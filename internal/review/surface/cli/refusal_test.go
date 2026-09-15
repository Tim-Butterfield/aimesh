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

// A completed apply that refused a finding for a protected path exits 7, since the exit code is the
// CLI's only machine-readable signal and CI scripts rely on it.

// refusalWorkspace returns an ordinary project that also contains a protected file.
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

// The run completes, the valid finding is written and the protected one is not, the exit code is 7,
// stderr names the reason, and no halt record exists.
func TestReview_Apply_ProtectedPathRefusal_Exit7(t *testing.T) {
	hermeticReview(t, string(fake.ProtectedPath))
	ws := refusalWorkspace(t)

	code, out, errs := run(t, "review", "--apply", "--profile", "fake-smoke", ws)

	if code != int(fault.Policy) {
		t.Fatalf("exit = %d, want %d (policy) — a completed apply that refused a protected-path finding must NOT exit 0, and it is not a containment BREACH either\nstdout:\n%s\nstderr:\n%s",
			code, fault.Policy, out, errs)
	}
	// The run completed; nothing may say it halted.
	if strings.Contains(out, "halted") {
		t.Errorf("the run must complete, not halt:\n%s", out)
	}
	if !strings.Contains(out, runmgr.ReasonApplyRefusedProtectedPath) {
		t.Errorf("the human output must name the machine reason %q:\n%s", runmgr.ReasonApplyRefusedProtectedPath, out)
	}
	if !strings.Contains(out, ".env") {
		t.Errorf("the refusal must NAME the refused path — a count with no path is not actionable:\n%s", out)
	}
	// The valid finding was still applied.
	if !strings.Contains(readFile(t, filepath.Join(ws, "main.go")), "reviewmesh[") {
		t.Error("the ordinary finding was not applied — a protected-path refusal must not discard the rest of the run")
	}
	if strings.Contains(readFile(t, filepath.Join(ws, ".env")), "reviewmesh[") {
		t.Fatal(".env was written; the denylist is not overridable")
	}
}

// The JSON projection carries outcome, counts.refused beside counts.applied, and refusals keyed by
// host-computed fingerprint, with no halt record.
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
	// The finding itself carries the refusal in the fields consumers already read.
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

// Report mode writes nothing, so a finding naming a protected path is reported and the run exits 0.
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
