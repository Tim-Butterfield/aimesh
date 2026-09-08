package model

import (
	"context"
	"os"
	"sort"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

// ProbeResult is the typed outcome of an adapter readiness probe: a machine-readable Signal (empty
// when none) kept separate from the human Detail, plus the Stage reached, so a caller can act on the
// signal and render its own guidance. A probe spends no model tokens.
type ProbeResult struct {
	OK     bool           // the adapter handshaked / responded to a safe probe
	Stage  string         // how far it got (resolve | spawn | initialize | session_new | version)
	Signal clihint.Signal // classified blocker (folder_trust / login_required / …), or ""
	Detail string         // human-readable detail (for display; not for machine decisions)
}

// Prober is an OPTIONAL adapter capability: a safe, no-model readiness probe. Shell adapters run a
// bounded `<bin> --version`; ACP adapters run a bounded handshake (initialize → session/new). Adapters
// that do not implement it report as un-probeable.
type Prober interface {
	Probe(ctx context.Context) ProbeResult
}

// DeepProbeSpec parameterizes a deep probe: the model argument a real run would pass, the effort, and
// an optional budget override. The model argument matters because a recipe builds it into the argv —
// probing with a model the roster does not use answers a question nobody asked.
type DeepProbeSpec struct {
	ModelArg core.ModelArg
	Effort   string
	// Timeout bounds the whole deep probe. 0 → the adapter's own deep-probe default.
	Timeout time.Duration
}

// DeepProber is an OPTIONAL adapter capability that answers the question `Probe` structurally cannot:
// DOES THIS CLI DO REAL WORK WHERE THE RUN ACTUALLY HAPPENS?
//
// `Probe` runs `<bin> --version` with no working directory. That proves the binary starts. It does not
// exercise authentication, folder trust, the model argument, or the recipe's argv — and every run's
// real invocation happens in a FRESH ISOLATED DIRECTORY that the CLI has never seen. `gemini-cli`
// passed the cheap probe and then failed every real call (exit 55, no output) until `--skip-trust` was
// added to its recipe; only a real-token campaign caught it.
//
// A deep probe therefore performs one bounded REAL invocation, in a representative isolated directory,
// through the same `Invoke` path a run uses, and classifies the outcome. Because the answer depends on
// the CLI's own trust/auth posture rather than on any one project, it is stable per adapter.
//
// It SPENDS. Every caller must keep it behind an explicit opt-in.
//
// It must NEVER auto-answer an interactive prompt: a deep probe writes nothing to the CLI's stdin and
// adds no flag the recipe does not already carry. A CLI that blocks on a trust or login prompt is
// DETECTED, CLASSIFIED and REPORTED with the human fix — consenting on the human's behalf is exactly
// the thing a trust prompt exists to prevent.
type DeepProber interface {
	ProbeDeep(ctx context.Context, spec DeepProbeSpec) ProbeResult
}

// HardenedEnv returns the environment for a spawned provider CLI: the inherited process environment
// (so the CLI's own auth/config is preserved) plus non-interactive hints appended LAST — os/exec
// resolves duplicate keys to the last value, so an appended hint overrides an inherited one while
// leaving authentication intact.
//
// Only NO_UPDATE_NOTIFIER is applied universally: it is npm-specific and identity-inert (it merely
// suppresses an update nag). CI=1 is deliberately NOT set here — several CLIs change or suppress their
// stderr identity banner in CI mode (e.g. codex, whose model identity is parsed from that banner),
// which would silently break model-identity verification. Enable CI per-recipe only after confirming
// the banner/envelope survives it.
func HardenedEnv() []string {
	return append(os.Environ(), "NO_UPDATE_NOTIFIER=1")
}

// HardenedEnvWith returns HardenedEnv() with a PER-RECIPE environment merged on top: one `K=V` entry
// appended per override, so the recipe's value wins over both the inherited environment and the
// hardened base (os/exec resolves duplicate keys to the LAST value).
//
// It APPENDS rather than replacing the environment, which is the whole point: a provider CLI's
// authentication rides on inherited variables (API keys, credential-helper paths, HOME/PATH), and a
// recipe that built its environment from scratch would break auth for every user. Only the keys a
// recipe NAMES are overridden; everything else is inherited untouched.
//
// Overrides are a map rather than a []string so a recipe cannot accidentally declare the same key
// twice with different values (with a slice, the last one silently wins and the other looks live).
// Keys are appended in sorted order so the produced environment is deterministic and testable.
//
// This is the ONLY sanctioned way to give a recipe extra environment: see the CI=1 discussion on
// HardenedEnv — the hint is enabled per-recipe, never globally.
func HardenedEnvWith(overrides map[string]string) []string {
	env := HardenedEnv()
	if len(overrides) == 0 {
		return env
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+overrides[k])
	}
	return env
}
