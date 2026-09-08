package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	runmgr "github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// The CLI half of the partial-refusal contract (design §13.4.3).
//
// A run that applied seven findings and refused one is not a success, and the CLI's ONLY
// machine-readable channel for saying so is its exit code — CI scripts key on it and on nothing
// else. Exit 0 would violate D8-C's load-bearing clause on the surface where violating it is
// cheapest, so a completed apply carrying a protected-path refusal exits 7.
//
// AGAINST THE OLD CODE EVERY TEST HERE FAILS AT ITS FIRST ASSERTION, because the run halted:
// exit 6 (containment), a halt record on disk, and nothing applied.

// refusalWorkspace is an ordinary project that also contains a protected file, which is what a
// real repository looks like.
func refusalWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("SECRET=super-secret-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// TestReview_Apply_ProtectedPathRefusal_Exit7 is the whole CLI contract in one test: the run
// completes, the valid finding is written, the protected one is not, the exit code is 7, the
// machine reason is named on stderr, and NO halt record exists — because there was no halt.
func TestReview_Apply_ProtectedPathRefusal_Exit7(t *testing.T) {
	hermeticReview(t, string(fake.ProtectedPath))
	ws := refusalWorkspace(t)

	code, out, errs := run(t, "review", "--apply", "--profile", "fake-smoke", ws)

	if code != int(fault.Policy) {
		t.Fatalf("exit = %d, want %d (policy) — a completed apply that refused a protected-path finding must NOT exit 0, and it is not a containment BREACH either\nstdout:\n%s\nstderr:\n%s",
			code, fault.Policy, out, errs)
	}
	// The run COMPLETED. Exit 7 here is not a halt, and nothing must have been written that says
	// it was.
	if strings.Contains(out, "halted") {
		t.Errorf("the run must complete, not halt:\n%s", out)
	}
	if !strings.Contains(out, runmgr.ReasonApplyRefusedProtectedPath) {
		t.Errorf("the human output must name the machine reason %q:\n%s", runmgr.ReasonApplyRefusedProtectedPath, out)
	}
	if !strings.Contains(out, ".env") {
		t.Errorf("the refusal must NAME the refused path — a count with no path is not actionable:\n%s", out)
	}
	// The valid finding was still applied. This is what D8-C exists for: one
	// out-of-bounds target must not discard the whole paid run.
	if !strings.Contains(readFile(t, filepath.Join(ws, "main.go")), "reviewmesh[") {
		t.Error("the ordinary finding was not applied — a protected-path refusal must not discard the rest of the run")
	}
	if strings.Contains(readFile(t, filepath.Join(ws, ".env")), "reviewmesh[") {
		t.Fatal(".env was written; the denylist is not overridable")
	}
}

// TestReview_Apply_ProtectedPathRefusal_JSONProjection pins the structured half: `outcome`,
// `counts.refused` beside `counts.applied`, and a `refusals[]` keyed on the HOST-COMPUTED
// fingerprint — plus the absence of a halt record, which is the surprising part and therefore
// the part worth pinning.
func TestReview_Apply_ProtectedPathRefusal_JSONProjection(t *testing.T) {
	hermeticReview(t, string(fake.ProtectedPath))
	ws := refusalWorkspace(t)

	code, out, errs := run(t, "review", "--apply", "--json", "--profile", "fake-smoke", ws)
	if code != int(fault.Policy) {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, fault.Policy, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("stdout is not the projection: %v\nstdout:\n%s", err, out)
	}
	if view.Status != "stable" {
		t.Errorf("status = %q, want stable — the run completed", view.Status)
	}
	if view.HaltRecord != nil {
		t.Errorf("a recorded refusal is NOT a halt; haltRecord = %+v", view.HaltRecord)
	}
	if view.Outcome != review.OutcomePartialRefusal {
		t.Errorf("outcome = %q, want %q", view.Outcome, review.OutcomePartialRefusal)
	}
	if view.Counts.Refused != 1 {
		t.Errorf("counts.refused = %d, want 1", view.Counts.Refused)
	}
	if view.Counts.Applied < 1 {
		t.Errorf("counts.applied = %d, want at least 1 — the refusal count is meaningless without it", view.Counts.Applied)
	}
	if len(view.Refusals) != 1 {
		t.Fatalf("refusals = %+v, want one", view.Refusals)
	}
	r := view.Refusals[0]
	if r.File != ".env" || r.Reason != review.ApplyRefusalProtectedPath {
		t.Errorf("refusal = %+v, want .env/protected_path", r)
	}
	if !strings.HasPrefix(r.Fingerprint, "sha1:") {
		t.Errorf("refusal fingerprint = %q, want the host-computed value (never the model-authored id)", r.Fingerprint)
	}
	// The finding itself carries the refusal in the fields every existing consumer already reads.
	marked := 0
	for _, f := range view.Findings {
		if f.ApplyRefusalReason == review.ApplyRefusalProtectedPath {
			marked++
			if f.Applyable == nil || *f.Applyable {
				t.Errorf("finding %s: applyable = %v, want a non-nil false", f.ID, f.Applyable)
			}
		}
	}
	if marked != 1 {
		t.Errorf("%d finding(s) carry applyRefusalReason=protected_path, want 1", marked)
	}
}

// TestReview_Report_ProtectedPathFinding_Exit0 pins the blast radius. Report mode writes
// nothing, so a finding that merely NAMES a protected path is reported and the run exits 0 —
// exit 7 belongs to a WRITE that was refused, not to the existence of the finding. The read path
// must not become newly fragile.
func TestReview_Report_ProtectedPathFinding_Exit0(t *testing.T) {
	hermeticReview(t, string(fake.ProtectedPath))
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke", refusalWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit = %d, want 0 — report mode writes nothing, so nothing can be refused\nstderr:\n%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("stdout is not the projection: %v", err)
	}
	if view.Outcome != "" {
		t.Errorf("outcome = %q, want absent on a report run — it is not a write", view.Outcome)
	}
	if len(view.Refusals) != 0 {
		t.Errorf("refusals = %+v, want none on a report run", view.Refusals)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
