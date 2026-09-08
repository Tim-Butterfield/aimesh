package cli

// CLI self-description tests: the binary must describe itself TRUTHFULLY (usage covers every shipped command,
// flag and mode, and never claims the config-only UI can run explorations), report its version, and
// resolve `ui`/`list` through the SAME profile resolver `explore`/`doctor`/`acp` use.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/version"
)

// TestVersion_Line: `--version` prints the single user-facing line and exits 0.
func TestVersion_Line(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"--version"}, &out, &errb); code != 0 {
		t.Fatalf("--version exit %d, stderr: %s", code, errb.String())
	}
	want := version.Get().String() + "\n"
	if out.String() != want {
		t.Errorf("--version printed %q, want %q", out.String(), want)
	}
}

// TestVersion_JSON: `--version --json` emits the full build metadata object.
func TestVersion_JSON(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"--version", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("--version --json exit %d, stderr: %s", code, errb.String())
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("--version --json is not valid JSON: %v\n%s", err, out.String())
	}
	for _, k := range []string{"version", "commit", "date", "dirty", "goVersion", "os", "arch"} {
		if _, ok := m[k]; !ok {
			t.Errorf("--version --json missing %q", k)
		}
	}
}

// TestHelp_ExitsZero_AndABadInvocationExitsUsage is the CROSS-APP PARITY pin. Asking for help is not a
// usage error: `exploremesh --help` must print to STDOUT and exit 0, exactly as `reviewmesh --help`
// does (reviewmesh's test of the SAME NAME pins the other half — the pair IS the parity). It exited 2
// for a whole release, because help and "you invoked me wrong" shared one function that returned the
// usage code — so a script or a CI step that gated on `--help` succeeding failed against one binary
// and passed against the other.
//
// The other half of the same table matters just as much: a bad invocation must STILL exit 2, and its
// text must still go to stderr. A fix that made everything exit 0 would be the worse regression.
func TestHelp_ExitsZero_AndABadInvocationExitsUsage(t *testing.T) {
	for _, spelling := range []string{"-h", "--help", "help"} {
		var out, errb bytes.Buffer
		code := Run([]string{spelling}, &out, &errb)
		if code != 0 {
			t.Errorf("%q exit %d, want 0 — asking for help is not a usage error", spelling, code)
		}
		if !strings.Contains(out.String(), "exploremesh") {
			t.Errorf("%q must print the help text on STDOUT (a pipeable answer), got %q", spelling, out.String())
		}
		if errb.Len() != 0 {
			t.Errorf("%q wrote to stderr: %q", spelling, errb.String())
		}
	}
	for _, args := range [][]string{nil, {"frobnicate"}, {"--not-a-flag"}} {
		var out, errb bytes.Buffer
		code := Run(args, &out, &errb)
		if code != int(fault.Usage) {
			t.Errorf("%v exit %d, want %d — a bad invocation is still a usage error", args, code, fault.Usage)
		}
		if !strings.Contains(errb.String(), "usage:") {
			t.Errorf("%v must explain itself on STDERR, got %q", args, errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("%v wrote to stdout: %q", args, out.String())
		}
	}
}

// TestUsage_CoversTheShippedSurface: every subcommand, every explore flag and every REGISTERED mode
// appears in the help text. Help that omits a shipped flag is the same failure as help that invents one.
func TestUsage_CoversTheShippedSurface(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	s := out.String()

	// `ui` and its `--open` are deliberately absent: the web UI was retired, and this list is what
	// stops help from advertising surface the binary does not have. It was doing the opposite until
	// the usage line went — pinning a removed command in place.
	for _, cmd := range []string{"explore", "export", "list", "doctor", "acp", "mcp", "init", "repo init", "folder init", "--version"} {
		if !strings.Contains(s, cmd) {
			t.Errorf("usage does not mention the %q command", cmd)
		}
	}
	for _, fl := range []string{
		"--purpose", "--criteria", "--mode", "--profile", "--roster", "--count", "--artifact",
		"--prior-context", "--json", "--dump-run", "--debug", "--explorer", "--collator", "--canonicalizer",
		"--options", "--criterion", "--target", "--unit", "--horizon", "--conditioning",
		"--sqlite", "--run", "--verify", "--framing", "--turn-timeout", "--max-parallel",
	} {
		if !strings.Contains(s, fl) {
			t.Errorf("usage does not mention the %q flag", fl)
		}
	}
	// The mode list is rendered FROM the registry, so this can only fail if the rendering is dropped.
	for _, m := range mode.Names() {
		if !strings.Contains(s, m) {
			t.Errorf("usage does not list the registered mode %q", m)
		}
	}
}

// TestUsage_StatesTheFailClosedAdapterPosture: the help text once described an adapter FALLBACK that
// does not exist. Resolution is fail-closed — an unrecognized adapter is refused, never quietly
// swapped for another — and the help has to say so, because a user who believes in a fallback will
// not read a refusal as the intended behaviour.
func TestUsage_StatesTheFailClosedAdapterPosture(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	s := out.String()
	if strings.Contains(s, "falls back to a") {
		t.Error("usage must not describe any adapter fallback — resolution is fail-closed")
	}
	if !strings.Contains(s, "never silently substituted") {
		t.Error("usage should state that an unrecognized adapter is refused, never silently substituted")
	}
	// The workbench is gone; the help must not advertise a command that no longer exists.
	if strings.Contains(s, "  ui ") || strings.Contains(s, "workbench") {
		t.Error("usage still advertises the removed web workbench")
	}
}

// TestList_ResolvesThroughProfiles proves `list` reads the profile set through the SAME resolver a run
// uses: a persisted profiles.yaml whose DEFAULT profile is `base` must drive the reported roster. The
// legacy roster.Discover path cannot see profiles.yaml at all, so resolving through it would report the
// wrong roster.
func TestList_ResolvesThroughProfiles(t *testing.T) {
	writeProfiles(t, profileEnv(t))
	var out, errb bytes.Buffer
	if code := Run([]string{"list", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("list --json exit %d, stderr: %s", code, errb.String())
	}
	var view struct {
		Roster struct {
			Explorers []struct {
				Model string `json:"model"`
			} `json:"explorers"`
		} `json:"roster"`
		Profiles struct {
			DefaultProfile string `json:"defaultProfile"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out.String())
	}
	if view.Profiles.DefaultProfile != "base" {
		t.Errorf("defaultProfile = %q, want base", view.Profiles.DefaultProfile)
	}
	if len(view.Roster.Explorers) != 2 || view.Roster.Explorers[0].Model != "base-a" {
		t.Errorf("list must report the DEFAULT profile's roster, got %+v", view.Roster.Explorers)
	}
}

// `ui` is gone (the web workbench was removed whole), so it must be refused like any other unknown
// command rather than lingering as an accepted no-op.
func TestUI_IsNoLongerACommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"ui"}, &out, &errb); code == 0 {
		t.Fatal("`ui` must be refused — the workbench was removed")
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("the refusal should read as an unknown command:\n%s", errb.String())
	}
}
