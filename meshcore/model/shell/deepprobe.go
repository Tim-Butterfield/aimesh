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

// This file is the DEEP probe: the readiness question `Probe` structurally cannot answer.
//
// `Probe` is `<bin> --version`, with NO working directory. It proves a binary starts. It exercises no
// authentication, no folder trust, no model argument and none of the recipe's argv — and its own
// timeout message can only GUESS ("may be waiting on native login or a folder-trust prompt").
//
// Every real call runs in a FRESH ISOLATED DIRECTORY the CLI has never seen (`model.Call.WorkDir`;
// both apps create one per call). That is where CLIs actually fail: `gemini-cli` passed `--version`
// and then failed every single real invocation — exit 55, no output at all — until `--skip-trust`
// entered its recipe. A real-token campaign found that; no probe in this repo could have.
//
// So the deep probe performs ONE bounded REAL invocation, in a representative isolated directory
// (throwaway, with a file in it, git-initialized the same hermetic way the ACP validator does it),
// through the SAME `Invoke` path a run takes, and classifies the outcome through meshcore/clihint.
//
// It spends. It is opt-in at every caller.

// deepProbeToken is what the probe asks the model to echo. A CLI that answers with it did real work;
// one that exits 0 with an empty answer did not, and that distinction is the whole point.
const deepProbeToken = "AIMESH-DEEP-PROBE-OK"

// deepProbePrompt is deliberately the smallest question that still requires a real model turn.
const deepProbePrompt = "Reply with exactly this text and nothing else: " + deepProbeToken

// defaultDeepProbeTimeout bounds one deep probe. It is generous enough for a cold provider CLI to
// authenticate and answer a one-token question, and far below the adapter's own per-call timeout —
// a probe that ran for ten minutes would be a worse diagnostic than no probe.
const defaultDeepProbeTimeout = 120 * time.Second

// deepProbeFile is the representative content the isolated directory holds. A CLI handed a completely
// empty directory sometimes behaves differently from one handed a project, and the probe's job is to
// resemble a run.
const deepProbeFile = "# aimesh readiness probe\n\nThis throwaway directory exists only to ask the CLI whether it can work here.\n"

// ProbeDeep implements model.DeepProber.
//
// It NEVER auto-answers an interactive prompt. `Invoke` gives the child no stdin (os/exec connects
// /dev/null), and this probe adds no argument the recipe does not already build. A CLI that blocks on
// a trust or login prompt is detected, classified and reported WITH the fix a human must perform —
// consenting on the operator's behalf is precisely what a trust prompt exists to prevent, and a probe
// that clicked "yes" would make the whole diagnostic a lie.
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
	// deepProbePrompt is what we sent, so it is subtracted before classifying — a CLI that echoes its
	// prompt must not be able to classify itself off our text (see meshcore/clihint).
	sig := clihint.ForFailure(clihint.Failure{
		Stderr: stderr, Stdout: stdout, Prompt: deepProbePrompt, ExitCode: res.ExitCode,
		Adapter: a.Recipe.Name,
	})

	// TIMEOUT. The cheap probe guesses here; this one has a real invocation behind the guess, so the
	// report names what was tried and what a human must do.
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

	// Exit 0 is not the same as an answer. A CLI that exits clean having written nothing has not done
	// real work, and reporting that as ready is the failure mode this probe exists to close.
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

// isolatedProbeDir builds the representative directory a real call would get: a throwaway temp
// directory holding a file, made a git repository the same hermetic way the ACP validator does it, so
// a CLI whose gate is "am I inside a repository?" is not failed by the probe's own austerity.
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

// fixFor renders the EXACT human step for a classified blocker. It never suggests that the probe (or
// anything else in this process) answer the prompt: the fix is always something the operator does in
// their own terminal, once, so the CLI stops prompting on a fresh directory.
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
