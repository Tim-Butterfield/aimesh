package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// TestDryRun_TheProtectedPathWaiverAppliesToTheDryRunToo checks that a dry run does not refuse a root
// the real run would accept: the dry-run admissibility check honours `--allow-protected-paths` exactly
// as the run does.
func TestDryRun_TheProtectedPathWaiverAppliesToTheDryRunToo(t *testing.T) {
	m, _ := countingPanel(t)
	ws := filepath.Join(t.TempDir(), ".vscode", "sub")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel", DryRun: true}

	// Without the waiver the root is refused.
	if _, err := m.RunContext(context.Background(), base); err == nil {
		t.Fatal("a protected root must still be refused without the waiver")
	}

	// With it, the dry run reports a shape, as the real run would proceed.
	withWaiver := base
	withWaiver.AllowProtectedPaths = true
	out, err := m.RunContext(context.Background(), withWaiver)
	if err != nil {
		t.Fatalf("the waiver must admit the root on a dry run too: %v", err)
	}
	if out.Shape == nil {
		t.Fatal("no shape — the dry run refused a root the run would have accepted")
	}
	if out.Status != "planned" {
		t.Errorf("status = %q, want planned", out.Status)
	}
}

// TestDryRun_TheWaiverStillDoesNotAdmitASecret checks that the waiver covers protected configuration
// only: a `.ssh` root is refused with or without it.
func TestDryRun_TheWaiverStillDoesNotAdmitASecret(t *testing.T) {
	m, _ := countingPanel(t)
	ws := filepath.Join(t.TempDir(), ".ssh")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "config"), []byte("Host x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel",
		DryRun: true, AllowProtectedPaths: true,
	})
	if err == nil {
		t.Fatal("the waiver admitted a SECRET root; it must reach protected config only")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "ssh") {
		t.Errorf("the refusal does not name what it refused: %v", err)
	}
}
