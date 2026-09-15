package shell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// This file implements the deep readiness probe. Probe runs `<bin> --version` with no working
// directory, which proves only that the binary starts. Real calls run in a fresh isolated directory,
// where authentication, folder trust, the model argument and the recipe's argv all matter, so the deep
// probe performs one bounded real invocation through Invoke in a representative isolated directory and
// classifies the outcome with meshcore/clihint. It spends tokens and is opt-in at every caller.

// deepProbeToken is what the probe asks the model to echo; an empty answer is not real work.
const deepProbeToken = "AIMESH-DEEP-PROBE-OK"

// deepProbePrompt is the smallest question that still requires a real model turn.
const deepProbePrompt = "Reply with exactly this text and nothing else: " + deepProbeToken

// defaultDeepProbeTimeout bounds one deep probe: enough for a cold CLI to authenticate and answer, and
// well below the adapter's per-call timeout.
const defaultDeepProbeTimeout = 120 * time.Second

// deepProbeFile is the content of the isolated directory, since some CLIs behave differently in an empty
// directory than in a project.
const deepProbeFile = "# aimesh readiness probe\n\nThis throwaway directory exists only to ask the CLI whether it can work here.\n"

// ProbeDeep implements model.DeepProber. It never answers an interactive prompt: Invoke gives the child
// no stdin and the probe adds no arguments. A CLI blocked on a trust or login prompt is reported with the
// fix a human must perform.
func (a *Adapter) ProbeDeep(ctx context.Context, spec model.DeepProbeSpec) model.ProbeResult {
	bin, err := a.resolveBinary()
	if err != nil {
		return model.ProbeResult{OK: false, Stage: "resolve", Detail: err.Error()}
	}
	dir, cleanup, derr := isolatedProbeDir(ctx)
	if derr != nil {
		return model.ProbeResult{OK: false, Stage: "isolate", Detail: "could not build an isolated probe directory: " + derr.Error()}
	}
	defer cleanup()

	budget := spec.Timeout
	if budget <= 0 {
		budget = defaultDeepProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	res, rerr := a.Invoke(pctx, model.Call{
		Role: "probe", Phase: "probe",
		Model: string(spec.ModelArg), ModelArg: spec.ModelArg, Effort: spec.Effort,
		WorkDir: dir, Prompt: deepProbePrompt,
	})
	stderr, stdout := string(res.Stderr), string(res.Stdout)
	// The prompt is removed from the output before classifying (see meshcore/clihint).
	sig := clihint.ForFailure(clihint.Failure{
		Stderr: stderr, Stdout: stdout, Prompt: deepProbePrompt, ExitCode: res.ExitCode,
		Adapter: a.Recipe.Name,
	})

	// On a timeout the report names what was tried and what to do.
	if pctx.Err() == context.DeadlineExceeded {
		return model.ProbeResult{
			OK: false, Stage: "invoke", Signal: sig,
			Detail: fmt.Sprintf(
				"deep probe timed out after %s: a REAL invocation in a throwaway directory produced no answer. %s",
				budget.Round(time.Second), fixFor(sig, bin, spec)),
		}
	}
	if rerr != nil && res.ExitCode == 0 {
		// Could not start / an I/O error — not a CLI verdict.
		return model.ProbeResult{OK: false, Stage: "invoke", Signal: sig, Detail: "deep probe could not run: " + rerr.Error()}
	}
	if res.ExitCode != 0 {
		return model.ProbeResult{
			OK: false, Stage: "invoke", Signal: sig,
			Detail: fmt.Sprintf("deep probe FAILED in a throwaway directory: %s exited %d%s. %s",
				a.Recipe.Name, res.ExitCode, excerpt(stderr, stdout), fixFor(sig, bin, spec)),
		}
	}

	// Exit 0 without an answer is not readiness.
	answer := string(res.Payload)
	if strings.TrimSpace(answer) == "" {
		answer = stdout
	}
	if strings.TrimSpace(answer) == "" {
		return model.ProbeResult{
			OK: false, Stage: "invoke", Signal: sig,
			Detail: fmt.Sprintf("deep probe FAILED: %s exited 0 in a throwaway directory but produced NO answer%s. %s",
				a.Recipe.Name, excerpt(stderr, ""), fixFor(sig, bin, spec)),
		}
	}
	detail := fmt.Sprintf("deep probe ok: a real invocation in a throwaway directory answered (%d bytes)", len(strings.TrimSpace(answer)))
	if strings.Contains(answer, deepProbeToken) {
		detail += "; the probe token was echoed"
	}
	return model.ProbeResult{OK: true, Stage: "invoke", Detail: detail}
}

var _ model.DeepProber = (*Adapter)(nil)

// isolatedProbeDir builds a directory like the one a real call gets: a throwaway temp directory holding
// a file, made a git repository with GitInitIsolatedDir so a CLI that requires a repository is not
// failed by the probe itself.
func isolatedProbeDir(ctx context.Context) (string, func(), error) {
	dir, err := os.MkdirTemp("", "aimesh-deep-probe-")
	if err != nil {
		return "", func() {}, err
	}
	if werr := os.WriteFile(filepath.Join(dir, "README.md"), []byte(deepProbeFile), 0o644); werr != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, werr
	}
	model.GitInitIsolatedDir(ctx, dir) // best-effort; a residual trust refusal is classified below
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// fixFor renders the human step for a classified blocker. The fix is always something the operator does
// once in their own terminal; nothing in this process answers a prompt.
func fixFor(sig clihint.Signal, bin string, spec model.DeepProbeSpec) string {
	switch sig {
	case clihint.FolderTrust:
		return "The CLI refused the throwaway directory under its OWN folder-trust / git-repo policy. " +
			"Nothing was auto-answered — a trust prompt is the operator's to answer. FIX: run `" + bin + "` yourself once " +
			"and complete its folder-trust setup so it stops gating a fresh directory, or configure this adapter's " +
			"recipe to pass the CLI's documented skip-trust flag. Trusting your project alone does NOT clear this: " +
			"every run happens in a fresh isolated copy, not in your project."
	case clihint.LoginRequired:
		return "The CLI is not authenticated. FIX: run `" + bin + "`'s own login command in your terminal, then re-run this probe."
	case clihint.ModelInvalid:
		return fmt.Sprintf("The CLI rejected the model argument %q. FIX: choose a model this account can use (see the adapter's model discovery) and update the roster.", string(spec.ModelArg))
	case clihint.UpdatePrompt:
		return "The CLI is asking to update, which can block a non-interactive run. FIX: update it once in your terminal, then re-run this probe."
	case clihint.Timeout:
		return "No output matched a known blocker, so the hang itself is the only signal. FIX: run `" + bin +
			"` yourself in an EMPTY directory and complete whatever it asks for (login, folder trust, first-run setup); nothing here will answer it for you."
	default:
		return "No recognized blocker was in the output — this is the shape `gemini-cli` had before `--skip-trust` (a non-zero exit with nothing to read). " +
			"FIX: run the same command yourself in an empty directory to see what the CLI wants; `doctor --probe-deep --json` reports the stage and signal."
	}
}

// excerpt renders a short, single-line excerpt of whatever the CLI said, so a failure report is not
// merely an exit code. It is bounded because this string reaches a terminal and a JSON projection.
func excerpt(stderr, stdout string) string {
	text := strings.TrimSpace(stderr)
	if text == "" {
		text = strings.TrimSpace(stdout)
	}
	if text == "" {
		return " with no output at all"
	}
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return " saying: " + text
}
