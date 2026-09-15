package cli

// These tests cover `aimesh review setup --acp`, which configures user-defined ACP adapters. detect
// and add launch the candidate CLI, so only their usage guards are tested; remove touches config
// without launching anything and is exercised fully.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// Guards that fire before config is read or anything is launched: an unknown action, a probe with no
// binary, and a remove with no name.
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

// --path is shared with adapter capture, so "--path requires --adapter" must not fire for the ACP
// actions, but must still fire otherwise.
func TestSetupACP_PathNoLongerRequiresAdapter(t *testing.T) {
	code, _, errs := run(t, "setup", "--path", "/bin/true")
	if code != int(fault.Usage) || !strings.Contains(errs, "--path requires --adapter") {
		t.Fatalf("a bare --path must still be a usage error, got %d: %s", code, errs)
	}
	// With --acp remove the guard must not fire; the error comes from the ACP surface.
	code, _, errs = run(t, "setup", "--acp", "remove", "--name", "acp-nope")
	if strings.Contains(errs, "--path requires --adapter") {
		t.Errorf("the adapter-path guard must not apply to --acp:\n%s", errs)
	}
	if code == int(fault.OK) {
		t.Error("removing an unsaved ACP adapter should not succeed")
	}
}

// Removing a name that is not a saved ACP instance is a refusal naming the scope, with a non-zero
// exit.
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

// The binary path is validated before the probe, so a mistyped path fails immediately and nothing
// is started.
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

// The help text documents --acp.
func TestHelp_DocumentsACPSetup(t *testing.T) {
	_, out, _ := run(t, "--help")
	for _, want := range []string{"setup --acp detect", "setup --acp add", "setup --acp remove", "--acp-arg"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not document %q:\n%s", want, out)
		}
	}
}
