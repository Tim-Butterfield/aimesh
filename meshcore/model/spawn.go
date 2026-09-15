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

// Prober is an optional adapter capability: a safe, no-model readiness probe. Shell adapters run a
// bounded `<bin> --version`; ACP adapters run a bounded handshake (initialize → session/new). Adapters
// that do not implement it report as un-probeable.
type Prober interface {
	Probe(ctx context.Context) ProbeResult
}

// DeepProbeSpec parameterizes a deep probe: the model argument and effort a real run would pass, and
// an optional timeout. The model argument matters because a recipe builds it into the argv.
type DeepProbeSpec struct {
	ModelArg core.ModelArg
	Effort   string
	// Timeout bounds the whole deep probe. 0 → the adapter's own deep-probe default.
	Timeout time.Duration
}

// DeepProber is an optional adapter capability that checks whether a CLI does real work where runs
// happen. Probe only proves the binary starts; it does not exercise authentication, folder trust,
// the model argument or the recipe's argv, and real calls run in a fresh isolated directory.
//
// A deep probe performs one bounded real invocation through Invoke in a representative isolated
// directory and classifies the outcome. It spends tokens, so callers must keep it behind an explicit
// opt-in. It never answers an interactive prompt: a CLI blocked on a trust or login prompt is detected
// and reported with the fix.
type DeepProber interface {
	ProbeDeep(ctx context.Context, spec DeepProbeSpec) ProbeResult
}

// HardenedEnv returns the environment for a spawned provider CLI: the inherited environment, which
// carries the CLI's authentication, plus NO_UPDATE_NOTIFIER=1 appended last so it takes precedence.
// CI=1 is not set here because some CLIs change their stderr output under it; recipes enable it
// individually.
func HardenedEnv() []string {
	return append(os.Environ(), "NO_UPDATE_NOTIFIER=1")
}

// HardenedEnvWith returns HardenedEnv with one `K=V` entry per override appended in sorted key order,
// so each override takes precedence while inherited authentication variables are kept. It is the only
// way a recipe adds environment.
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
