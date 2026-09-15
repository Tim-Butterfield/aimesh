package run

// Verification runs the project's own build/test commands, supplied by the operator, on containment
// copies and records the results. The result never invalidates, drops or downgrades a finding and never
// blocks a commit; nothing reads it back.
//
// Nothing a model wrote is executed: the commands come from the operator and run on a copy of the
// operator's own project. The baseline runs on a throwaway copy and the after-edit pass runs on the
// write copy only after the commit, so no build output reaches the live tree.
//
// The signal is the delta between the two passes, not absolute success, so a repository whose suite
// already fails can still be reviewed.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// DefaultVerifyTimeout bounds one command. The budget is per command so a slow build does not shorten
// the time left for the tests.
const DefaultVerifyTimeout = 5 * time.Minute

// maxVerifyOutput bounds one command's captured output. The tail is kept, because tools print errors
// last.
const maxVerifyOutput = 4 << 10

// MaxVerifyCommands caps how many commands one run executes, bounding wall-clock cost.
const MaxVerifyCommands = 8

// Delta values: the host's comparison of the two passes.
const (
	// DeltaUnchangedPass — every command passed before and after.
	DeltaUnchangedPass = "unchanged_pass"
	// DeltaUnchangedFail — at least one command failed, and the same ones failed both times. This is
	// an ordinary outcome.
	DeltaUnchangedFail = "unchanged_fail"
	// DeltaFixed — something that failed before passes now.
	DeltaFixed = "fixed"
	// DeltaBroken — something that passed before fails now. It is reported prominently and blocks
	// nothing.
	DeltaBroken = "broken"
	// DeltaBaselineOnly — only one pass ran, as on a report run.
	DeltaBaselineOnly = "baseline_only"
	// DeltaNotComparable — a command timed out or could not start on one side, so the passes cannot
	// be compared. It is not reported as broken.
	DeltaNotComparable = "not_comparable"
)

// VerificationNote is the fixed sentence carried on every report, stating the limits in both
// directions.
const VerificationNote = "These are the project's own commands, run on the containment copy — never on your working tree, and never on anything a model wrote. A pass does NOT mean the change is correct: the commands may not cover the code that changed. A failure does NOT invalidate any finding and did not block anything — nothing here drops, downgrades, reorders or refuses."

// runVerifyPass runs every command in order inside dir, including after a failure, so both passes
// describe the same commands. It returns one result per command.
func runVerifyPass(ctx context.Context, dir string, commands []string, timeout time.Duration) []review.VerificationResult {
	if timeout <= 0 {
		timeout = DefaultVerifyTimeout
	}
	out := make([]review.VerificationResult, 0, len(commands))
	for _, raw := range commands {
		cmd := strings.TrimSpace(raw)
		if cmd == "" {
			continue
		}
		out = append(out, runVerifyCommand(ctx, dir, cmd, timeout))
	}
	return out
}

// runVerifyCommand runs one command through the platform shell, so pipes, redirections and chains in
// the operator's command line behave as they would for the operator, and records the result.
func runVerifyCommand(ctx context.Context, dir, command string, timeout time.Duration) review.VerificationResult {
	res := review.VerificationResult{Command: command}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, flag := "/bin/sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd.exe", "/C"
	}
	c := exec.CommandContext(cctx, shell, flag, command)
	c.Dir = dir
	started := time.Now()
	// Combined output keeps diagnostics from both streams in order.
	body, err := c.CombinedOutput()
	res.DurationMs = time.Since(started).Milliseconds()
	res.Output = tailOf(body)

	switch {
	case cctx.Err() == context.DeadlineExceeded:
		// A timeout reflects the budget, not the code, so it makes the pass not comparable rather
		// than failed.
		res.TimedOut = true
		res.ExitCode = -1
		return res
	case err == nil:
		res.OK = true
		return res
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		res.ExitCode = ee.ExitCode()
		return res
	}
	// The command could not start (no shell, a bad directory); that is about this host, not the project.
	res.Unstartable = err.Error()
	res.ExitCode = -1
	return res
}

// tailOf keeps the last maxVerifyOutput bytes and marks the clip.
func tailOf(b []byte) string {
	s := strings.TrimRight(string(b), "\n")
	if len(s) <= maxVerifyOutput {
		return s
	}
	return "…(earlier output clipped)\n" + s[len(s)-maxVerifyOutput:]
}

// verificationDelta compares the two passes command by command. Both passes run the same list in the
// same order, so a length mismatch is reported as not comparable.
func verificationDelta(before, after []review.VerificationResult) string {
	switch {
	case len(before) == 0 && len(after) == 0:
		return DeltaBaselineOnly
	case len(after) == 0:
		return DeltaBaselineOnly
	case len(before) != len(after):
		return DeltaNotComparable
	}
	broke, fixed, anyFail := false, false, false
	for i := range before {
		b, a := before[i], after[i]
		if b.TimedOut || a.TimedOut || b.Unstartable != "" || a.Unstartable != "" {
			return DeltaNotComparable
		}
		if !a.OK {
			anyFail = true
		}
		switch {
		case b.OK && !a.OK:
			broke = true
		case !b.OK && a.OK:
			fixed = true
		}
	}
	switch {
	case broke:
		return DeltaBroken
	case fixed:
		return DeltaFixed
	case anyFail:
		return DeltaUnchangedFail
	default:
		return DeltaUnchangedPass
	}
}

// newVerificationReport assembles the record, echoing the commands as the operator gave them.
func newVerificationReport(root string, commands []string, before, after []review.VerificationResult) *review.VerificationReport {
	return &review.VerificationReport{
		Root:     root,
		Commands: append([]string(nil), commands...),
		Before:   before,
		After:    after,
		Delta:    verificationDelta(before, after),
		Note:     VerificationNote,
	}
}

// baselineOnAThrowawayCopy runs the commands once on a disposable copy. The commit writes back every
// file in the write copy that differs from live, so a baseline run there would commit build output.
// If the copy cannot be taken the baseline is omitted and the review is unaffected.
func (m *Manager) baselineOnAThrowawayCopy(ctx context.Context, run *audit.Run, ws *workspace.Access, root string, commands []string, timeout time.Duration) []review.VerificationResult {
	if len(commands) == 0 {
		return nil
	}
	h, err := ws.Copy(root, true, "verify")
	if err != nil {
		_ = run.Event(m.now(), "warn", "verification_copy_failed",
			"the verification baseline could not be taken (the containment copy failed); the review is unaffected",
			map[string]any{"error": err.Error()})
		return nil
	}
	defer ws.Cleanup(h)
	_ = run.Event(m.now(), "info", "verification_baseline_start",
		fmt.Sprintf("running %d project command(s) on a throwaway containment copy to record a baseline", len(commands)),
		map[string]any{"commands": commands})
	before := runVerifyPass(ctx, h.Root, commands, timeout)
	m.eventVerification(run, "verification_baseline_done", "baseline", before)
	return before
}

// reportBaseline runs the baseline for a report run when Request.VerifyBaseline is set. It is opt-in
// because, with no after pass, it costs a full suite run for a single result.
func (m *Manager) reportBaseline(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request) *review.VerificationReport {
	if len(req.VerifyCommands) == 0 || !req.VerifyBaseline {
		return nil
	}
	before := m.baselineOnAThrowawayCopy(ctx, run, ws, req.Workspace, req.VerifyCommands, req.VerifyTimeout)
	if before == nil {
		return nil
	}
	// A report run writes nothing, so there is no after pass and the delta is baseline_only.
	return newVerificationReport(req.Workspace, req.VerifyCommands, before, nil)
}

// ValidateVerifyCommands trims the operator's commands, drops blank ones, and enforces
// MaxVerifyCommands. Command content is the operator's business and is not inspected.
func ValidateVerifyCommands(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		if s := strings.TrimSpace(c); s != "" {
			out = append(out, s)
		}
	}
	if len(out) > MaxVerifyCommands {
		return nil, fault.New(fault.Usage, fmt.Sprintf(
			"%d verification command(s) requested; the ceiling is %d. Each one is a real process against a real toolchain, run twice on a write run — chain them in a single command if you need more",
			len(out), MaxVerifyCommands))
	}
	return out, nil
}
