package cli

// Surface-level tests for `exploremesh export`'s DESTINATION GOVERNANCE. internal/evidence pins the
// behavior; these pin the two things only the surface can prove — that the refusals arrive as the SHARED
// halt taxonomy's exit codes, and that `--force` is a real flag a user can actually reach.
//
// The command is exploremesh's only write to a path a user types, and the README's exit table now claims
// exit 6 for it. A claim in a table nothing tests is a claim that rots.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// exportableRun captures a real run through the CLI and returns its run directory.
func exportableRun(t *testing.T) string {
	t.Helper()
	artifactDir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", artifactDir)
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "pick a store", "--criteria", "operability", "--dump-run"}, &out, &errb); code != 0 {
		t.Fatalf("explore --dump-run exit %d: %s", code, errb.String())
	}
	return latestRunDir(t, artifactDir)
}

// TestExport_ProtectedDestinationExitsContainment: `export --sqlite <something under .aimesh>` must never
// DELETE and replace the shared adapter configuration. It is a containment halt (exit 6) whose message
// names the protected-path rule, and the file is untouched.
func TestExport_ProtectedDestinationExitsContainment(t *testing.T) {
	runDir := exportableRun(t)
	dest := filepath.Join(t.TempDir(), ".aimesh", "adapters.yaml")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const original = "schemaVersion: 1\nadapters: {}\n"
	if err := os.WriteFile(dest, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out, errb bytes.Buffer
	// --force is passed on purpose: the strongest opt-in the CLI has must NOT lift the denylist.
	code := Run([]string{"export", "--sqlite", dest, "--run", runDir, "--force"}, &out, &errb)
	if code != int(fault.Containment) {
		t.Errorf("exit = %d, want %d (containment); stderr: %s", code, int(fault.Containment), errb.String())
	}
	if !strings.Contains(errb.String(), ".aimesh") {
		t.Errorf("the refusal must name the protected-path rule: %s", errb.String())
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != original {
		t.Fatalf("the protected file was modified by a refused export: %q (%v)", got, err)
	}
}

// TestExport_NoClobberWithoutForce: an existing destination exits with the POLICY code (7) and the message
// teaches the flag; with `--force` the same command succeeds.
func TestExport_NoClobberWithoutForce(t *testing.T) {
	runDir := exportableRun(t)
	dest := filepath.Join(t.TempDir(), "evidence.db")
	const original = "not a database — but it is mine\n"
	if err := os.WriteFile(dest, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"export", "--sqlite", dest, "--run", runDir}, &out, &errb); code != int(fault.Policy) {
		t.Errorf("exit = %d, want %d (policy); stderr: %s", code, int(fault.Policy), errb.String())
	}
	if !strings.Contains(errb.String(), "--force") {
		t.Errorf("the refusal must name the flag that allows it: %s", errb.String())
	}
	if got, _ := os.ReadFile(dest); string(got) != original {
		t.Fatalf("a refused export modified the destination: %q", got)
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"export", "--sqlite", dest, "--run", runDir, "--force"}, &out, &errb); code != 0 {
		t.Fatalf("export --force exit %d: %s", code, errb.String())
	}
	if got, _ := os.ReadFile(dest); string(got) == original {
		t.Fatal("--force did not replace the destination")
	}
	if !strings.Contains(out.String(), "Exported") {
		t.Errorf("export --force printed no summary: %s", out.String())
	}
}
