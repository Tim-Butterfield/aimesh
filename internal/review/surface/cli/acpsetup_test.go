package cli

// `aimesh review setup --acp`: the CLI half of the user-defined ACP adapter flow. The
// detect/save/remove seams shipped with no CLI caller at all, so a headless install could not configure
// an ACP adapter; these tests pin the surface that fixes it.
//
// HERMETIC: `--acp detect` and `--acp add` LAUNCH the candidate CLI (the only way to confirm it speaks
// ACP), so they are covered by their usage guards only — no test here starts a process. `--acp remove`
// touches config without launching anything, so it is exercised for real.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// TestSetupACP_ShapeGuards pins the guards that fire BEFORE any config is read and, critically, before
// anything could be launched: an unknown action, a probe with no binary, and a remove with no key.
func TestSetupACP_ShapeGuards(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown action", []string{"setup", "--acp", "bogus"}, "unknown --acp action"},
		{"detect without path", []string{"setup", "--acp", "detect"}, "requires --path"},
		{"add without path", []string{"setup", "--acp", "add"}, "requires --path"},
		{"remove without name", []string{"setup", "--acp", "remove"}, "requires --name"},
		{"acp with adapter", []string{"setup", "--acp", "remove", "--name", "x", "--adapter", "claude-code"}, "cannot be combined"},
		{"acp with interactive", []string{"setup", "--acp", "detect", "--path", "/bin/true", "--interactive"}, "cannot be combined"},
		{"orphan acp flags", []string{"setup", "--name", "x"}, "apply only with --acp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errs := run(t, tc.args...)
			if code != int(fault.Usage) {
				t.Fatalf("code = %d, want %d (usage)", code, fault.Usage)
			}
			if !strings.Contains(errs, tc.want) {
				t.Errorf("error should contain %q:\n%s", tc.want, errs)
			}
		})
	}
}

// TestSetupACP_PathNoLongerRequiresAdapter: --path is shared with the adapter-capture surface, so the
// "--path requires --adapter" guard has to stand down for the ACP probes. It must still fire otherwise.
func TestSetupACP_PathNoLongerRequiresAdapter(t *testing.T) {
	code, _, errs := run(t, "setup", "--path", "/bin/true")
	if code != int(fault.Usage) || !strings.Contains(errs, "--path requires --adapter") {
		t.Fatalf("a bare --path must still be a usage error, got %d: %s", code, errs)
	}
	// With --acp remove the guard must not fire — the error must come from the ACP surface instead.
	code, _, errs = run(t, "setup", "--acp", "remove", "--name", "acp-nope")
	if strings.Contains(errs, "--path requires --adapter") {
		t.Errorf("the adapter-path guard must not apply to --acp:\n%s", errs)
	}
	if code == int(fault.OK) {
		t.Error("removing an unsaved ACP adapter should not succeed")
	}
}

// TestSetupACP_RemoveUnknown: removing a name that is not a saved ACP instance is reported as a refusal
// naming the scope, with a non-zero exit — and it launches nothing.
func TestSetupACP_RemoveUnknown(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir())

	code, _, errs := run(t, "setup", "--acp", "remove", "--name", "acp-nope")
	if code != int(fault.Config) {
		t.Fatalf("code = %d, want %d (config)", code, fault.Config)
	}
	if !strings.Contains(errs, "acp-nope") {
		t.Errorf("the refusal should name the adapter:\n%s", errs)
	}
}

// TestSetupACP_DetectValidatesThePathFirst: the binary path is checked (exists / not a directory / on
// POSIX executable) BEFORE the probe, so a typo'd path fails instantly instead of after a 90s launch
// attempt. It is also what makes `detect` safe to cover here — nothing is ever started.
func TestSetupACP_DetectValidatesThePathFirst(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir())

	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	code, _, errs := run(t, "setup", "--acp", "detect", "--path", missing)
	if code == int(fault.OK) {
		t.Fatal("detect on a missing binary must fail")
	}
	if !strings.Contains(errs, "not found") {
		t.Errorf("the error should say the path was not found:\n%s", errs)
	}
	// A directory is not a binary either.
	code, _, errs = run(t, "setup", "--acp", "add", "--path", t.TempDir())
	if code == int(fault.OK) {
		t.Fatal("add with a directory must fail")
	}
	if !strings.Contains(errs, "directory") {
		t.Errorf("the error should say the path is a directory:\n%s", errs)
	}
}

// TestHelp_DocumentsACPSetup: a config surface nobody can discover is not a config surface.
func TestHelp_DocumentsACPSetup(t *testing.T) {
	_, out, _ := run(t, "--help")
	for _, want := range []string{"setup --acp detect", "setup --acp add", "setup --acp remove", "--acp-arg"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not document %q:\n%s", want, out)
		}
	}
}
