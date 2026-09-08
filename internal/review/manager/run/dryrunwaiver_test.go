package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// A DRY RUN MUST NOT REFUSE A ROOT THE REAL RUN WOULD ACCEPT.
//
// The dry-run stop judges workspace admissibility so it cannot answer "here is your plan, 2..10
// calls" for a root the next step refuses. That check kept the STRICT form after
// `--allow-protected-paths` landed, so the waiver was honoured by the run and ignored by the dry
// run: `--dry-run --allow-protected-paths` was refused while the same command without `--dry-run`
// went ahead. Exactly backwards — the free preview was stricter than the thing it previews.
//
// No test paired the two flags; driving the real binary is what caught it.
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

	// WITHOUT the waiver the refusal stands — the guard still guards.
	if _, err := m.RunContext(context.Background(), base); err == nil {
		t.Fatal("a protected root must still be refused without the waiver")
	}

	// WITH it, the dry run reports a shape, exactly as the real run would proceed.
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

// TestDryRun_TheWaiverStillDoesNotAdmitASecret. The waiver reaches protected CONFIG only; a `.ssh`
// root is refused on both paths, waiver or not.
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
