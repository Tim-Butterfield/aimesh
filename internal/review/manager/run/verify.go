package run

// BOUNDED EXECUTION — the project's OWN build/test commands, run on the containment copy, recorded.
//
// THE GOVERNING INVARIANT, which settles every question this file could otherwise raise:
//
//	The result is RECORDED. It never invalidates a finding, never drops one, never downgrades one,
//	and never blocks a commit. Nothing in the engine reads it back.
//
// That is the same rule task #41 established for model identity and grounding.go restates for
// citations, and this is the third and most tempting place to break it: a failing test suite feels
// like a reason to stop. It is not. A suite does not know whether a reviewer's SQL-injection finding
// is real, and it does not know whether the code path it failed on is the one the patch touched. What
// it knows is whether the commands the operator already runs behave differently before and after —
// and that fact, stated plainly, is worth more than a verdict it is not entitled to reach.
//
// WHY THIS IS NOT "EXECUTING MODEL OUTPUT", which is the question the whole design turns on. Nothing
// here runs anything a model wrote. The commands come from the OPERATOR's own command line; the tree
// they run in is the containment copy of the operator's own project. These are commands the user
// already runs on that tree, so the threat model is exactly unchanged. The rejected alternative —
// executing seat-proposed reproducers — inverts the containment guarantee and needs a real sandbox
// this project does not have, and `docs/adapters.md` already concedes that a working directory is not
// one. A half-sandbox would be worse than none, because the run record would then assert a safety
// property the implementation lacks.
//
// WHERE IT RUNS, and why that placement is load-bearing. Both passes execute inside `rcopy.Root` —
// the write copy, before any edit for the baseline and after every edit for the second pass, and the
// second pass finishes BEFORE the commit window opens. So the measurement never touches the live
// tree, and a red result cannot block the commit even by accident, because the commit is downstream of
// a function whose return value nothing branches on.
//
// THE SIGNAL IS THE DELTA, NEVER ABSOLUTE GREEN. Real repositories routinely have a red suite —
// a flaky integration test, a known-failing case, a half-finished migration. Requiring green-before
// would refuse to run on exactly the repositories most in need of review. A red-before tree is an
// ordinary run; what this reports is whether the commands answered DIFFERENTLY afterwards.

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

// DefaultVerifyTimeout bounds ONE command. It is per command rather than for the set because a
// project's build and its test suite are different lengths, and a shared budget would make the last
// command's failure depend on how long the first one took.
const DefaultVerifyTimeout = 5 * time.Minute

// maxVerifyOutput bounds the captured output of one command. The TAIL is kept, not the head: a build
// tool prints its banner and its progress first and its error last, so the end is the part that says
// what happened.
const maxVerifyOutput = 4 << 10

// MaxVerifyCommands caps how many commands one run will execute. It is a spend/wall-clock bound of the
// same kind as the panel-seat ceiling: each command is a real process against a real toolchain, and an
// unbounded list is a way to make one review take an afternoon.
const MaxVerifyCommands = 8

// Delta values — the HOST's comparison of the two passes, and the thing a reader should look at
// first. They are STABLE MACHINE CODES.
const (
	// DeltaUnchangedPass — every command passed before and after.
	DeltaUnchangedPass = "unchanged_pass"
	// DeltaUnchangedFail — at least one command failed, and the same ones failed both times. This is
	// an ORDINARY outcome, not an error: a repository with a red suite is still a repository worth
	// reviewing, and the change did not make it worse.
	DeltaUnchangedFail = "unchanged_fail"
	// DeltaFixed — something that failed before passes now.
	DeltaFixed = "fixed"
	// DeltaBroken — something that passed before fails now. The headline result, and still not a
	// verdict on any finding: it is reported loudly and it stops nothing.
	DeltaBroken = "broken"
	// DeltaBaselineOnly — only one pass ran, so there is nothing to compare. This is what a REPORT
	// run produces: it writes nothing, so there is no "after".
	DeltaBaselineOnly = "baseline_only"
	// DeltaNotComparable — a command timed out or could not be started on one side, so the two passes
	// do not describe the same thing. Reported as its own state rather than folded into `broken`,
	// which would blame the change for a wall-clock budget.
	DeltaNotComparable = "not_comparable"
)

// VerificationNote is the fixed sentence carried on every report. It states both limits at once,
// because the two misreadings run in opposite directions and a reader is prone to whichever suits
// them.
const VerificationNote = "These are the project's own commands, run on the containment copy — never on your working tree, and never on anything a model wrote. A pass does NOT mean the change is correct: the commands may not cover the code that changed. A failure does NOT invalidate any finding and did not block anything — nothing here drops, downgrades, reorders or refuses."

// runVerifyPass executes every command in order inside dir and returns one result each.
//
// EVERY COMMAND RUNS, even after one fails. Stopping at the first failure would save wall clock and
// cost the thing this feature exists for: the two passes have to describe the same set of commands or
// the per-command delta is not computable, and "the build broke so we never learned about the tests"
// is exactly the report a reader would have to guess at.
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

// runVerifyCommand executes one command through the platform shell and records what happened.
//
// It goes through a SHELL deliberately. The value is an operator's own command line — `make test`,
// `npm run build && npm test`, `pytest -q tests/` — and a tokenizer of our own would silently mangle
// the pipes, redirections and chains people actually write. The shell is the one the operator already
// uses, so what runs is what they would have run.
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
	// CombinedOutput, because a build tool splits its diagnosis across both streams and a reader
	// reconstructing the order from two buffers is doing the shell's job by hand.
	body, err := c.CombinedOutput()
	res.DurationMs = time.Since(started).Milliseconds()
	res.Output = tailOf(body)

	switch {
	case cctx.Err() == context.DeadlineExceeded:
		// A TIMEOUT IS NOT A FAILURE OF THE CODE. It is a statement about the budget, so it is its own
		// field and it makes the pass not-comparable rather than red — blaming a change for a
		// wall-clock ceiling would be the single easiest way for this feature to lie.
		res.TimedOut = true
		res.ExitCode = -1
		return res
	case err == nil:
		res.OK = true
		return res
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res
	}
	// The command could not be started at all (no shell, a bad directory). That is a fact about this
	// host, not about the project, so it is recorded as unstartable rather than as a red suite.
	res.Unstartable = err.Error()
	res.ExitCode = -1
	return res
}

// tailOf keeps the last maxVerifyOutput bytes and marks the clip. The tail is where a build tool puts
// the error; a clipped head that read like a whole log would be worse than either.
func tailOf(b []byte) string {
	s := strings.TrimRight(string(b), "\n")
	if len(s) <= maxVerifyOutput {
		return s
	}
	return "…(earlier output clipped)\n" + s[len(s)-maxVerifyOutput:]
}

// verificationDelta is the HOST's comparison of the two passes, per command and overall.
//
// It compares POSITIONALLY, because runVerifyPass runs the same command list in the same order on
// both sides and never skips one. A length mismatch therefore means something upstream changed the
// list between passes, which is not a delta anyone should be shown a number for.
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

// newVerificationReport assembles the record. `commands` is echoed as the operator gave it, so a
// reader can see what was run without inferring it from the results.
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

// baselineOnAThrowawayCopy runs the commands once, on a copy this function OWNS and destroys.
//
// THE THROWAWAY COPY IS THE POINT, and it is the second half of the same hazard the after-pass
// placement addresses. A build writes: object files, binaries, `node_modules`, coverage directories,
// `__pycache__`. `Access.CommitExpecting` re-enumerates the WHOLE copy and writes back every file that
// differs from live — so a baseline run inside the copy the commit is derived from would put build
// output into the user's repository. No denylist would reliably catch it either, because build
// artifacts look exactly like source files.
//
// So the baseline runs somewhere disposable, and the after-pass runs in the write copy only once the
// commit is already done. Between the two rules, nothing this feature executes can leave a trace in
// anybody's tree. Both copies are byte-identical to the live tree at the moment they are taken, so
// they remain a valid comparison.
//
// A copy that cannot be taken is REPORTED ABSENT, never raised: the review is a complete, correct
// answer whether or not a diagnostic could be collected beside it.
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

// reportBaseline is the REPORT-run half: the commands run once and there is no "after".
//
// It is opt-in (Request.VerifyBaseline) because it buys one fact — "here is what your suite did before
// anybody touched anything" — at the full wall-clock cost of running the suite, with nothing to
// compare it against. On a write run that trade is obviously worth making and the commands run
// without asking; here it is a judgement about the operator's own time, so they make it.
func (m *Manager) reportBaseline(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request) *review.VerificationReport {
	if len(req.VerifyCommands) == 0 || !req.VerifyBaseline {
		return nil
	}
	before := m.baselineOnAThrowawayCopy(ctx, run, ws, req.Workspace, req.VerifyCommands, req.VerifyTimeout)
	if before == nil {
		return nil
	}
	// No "after" pass: a report run writes nothing, so there is nothing to compare. The delta says
	// exactly that rather than leaving a reader to infer it from an empty array.
	return newVerificationReport(req.Workspace, req.VerifyCommands, before, nil)
}

// ValidateVerifyCommands checks an operator's command list before anything is spent: non-blank, and
// within the ceiling. It returns the trimmed list.
//
// It is a USAGE check rather than a policy one — the content of a command is the operator's business,
// and a tool that second-guessed which of the user's own commands it was willing to run would be
// inventing a security boundary it does not have.
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
