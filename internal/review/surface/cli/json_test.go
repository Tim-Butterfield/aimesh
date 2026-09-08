package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// jsonWorkspace is a deterministic single-file workspace for the projection tests.
func jsonWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func hermeticReview(t *testing.T, scenario string) {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", t.TempDir())
	t.Setenv("REVIEWMESH_FAKE_SCENARIO", scenario)
}

// TestReview_JSON_Success pins the success projection AND the stdout-purity rule: stdout is
// exactly one JSON object — the `run:` line and the identity-caveat lines move to stderr, so
// a caller reading stdout can parse it without filtering.
func TestReview_JSON_Success(t *testing.T) {
	hermeticReview(t, "valid")
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	var view runview.View
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&view); err != nil {
		t.Fatalf("stdout is not a single JSON object: %v\nstdout:\n%s", err, out)
	}
	if dec.More() {
		t.Error("stdout carries more than the projection — it must be the ONLY thing on stdout")
	}
	if strings.Contains(out, "run: ") || strings.Contains(out, "review complete") {
		t.Errorf("progress text leaked onto stdout:\n%s", out)
	}
	if !strings.Contains(errs, "run: ") {
		t.Errorf("the run: line must still be reported, on stderr; stderr:\n%s", errs)
	}

	if view.SchemaVersion != runview.SchemaVersion {
		t.Errorf("schemaVersion = %d, want %d", view.SchemaVersion, runview.SchemaVersion)
	}
	if view.Status != "stable" {
		t.Errorf("status = %q, want stable", view.Status)
	}
	if view.Mode != "report" {
		t.Errorf("mode = %q, want report", view.Mode)
	}
	if view.RunID == "" || view.RunDir == "" {
		t.Errorf("runId/runDir missing: %+v", view)
	}
	if view.HaltRecord != nil {
		t.Errorf("a successful run must carry no haltRecord: %+v", view.HaltRecord)
	}
	if len(view.Findings) == 0 {
		t.Fatal("the `valid` scenario produces one finding; projection has none")
	}
	f := view.Findings[0]
	if f.ID == "" || f.Summary == "" || f.Severity == "" || f.Disposition == "" {
		t.Errorf("finding is missing required projection fields: %+v", f)
	}
	if f.File == "" {
		t.Errorf("finding has no file: %+v", f)
	}
	// Counts are host-computed and must agree with the arrays they summarize.
	if view.Counts.Findings != len(view.Findings) {
		t.Errorf("counts.findings = %d, want %d", view.Counts.Findings, len(view.Findings))
	}
	if view.Counts.BySeverity[f.Severity] == 0 {
		t.Errorf("counts.bySeverity missing %q: %+v", f.Severity, view.Counts.BySeverity)
	}
	if view.Counts.ByDisposition[f.Disposition] == 0 {
		t.Errorf("counts.byDisposition missing %q: %+v", f.Disposition, view.Counts.ByDisposition)
	}
}

// TestReview_JSON_Halted pins the halted projection: the object is STILL emitted (with the
// halt record populated) and the exit code is the taxonomy code, unchanged by --json. The halt is
// UNUSABLE OUTPUT (Class G) — output the host cannot parse is the kind of thing that stops a run.
func TestReview_JSON_Halted(t *testing.T) {
	hermeticReview(t, "malformed")
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.Internal) {
		t.Fatalf("exit = %d, want %d (malformed output); stderr=%s", code, fault.Internal, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("a halted run must still emit the projection: %v\nstdout:\n%s", err, out)
	}
	if view.Status != "halted" {
		t.Errorf("status = %q, want halted", view.Status)
	}
	if view.HaltRecord == nil {
		t.Fatal("halted run has no haltRecord")
	}
	h := view.HaltRecord
	if h.HaltClass != "G" {
		t.Errorf("haltClass = %q, want G", h.HaltClass)
	}
	if h.ExitCode != int(fault.Internal) {
		t.Errorf("haltRecord.exitCode = %d, want %d", h.ExitCode, fault.Internal)
	}
	if h.Message == "" {
		t.Error("haltRecord carries no human message")
	}
	if strings.ContainsAny(h.ReasonCode, " .") {
		t.Errorf("reasonCode %q must be code-shaped, not a sentence", h.ReasonCode)
	}
	// The human error line still goes to stderr.
	if !strings.Contains(errs, "aimesh review:") {
		t.Errorf("the human error line must stay on stderr; stderr:\n%s", errs)
	}
}

// TestReview_JSON_IdentityMismatchIsNotAHalt is the contrast: the same run shape with a mismatched
// model identity exits 0 and projects a NORMAL result carrying the caveat. A consumer reading --json
// must be able to see the mismatch without the run having been thrown away to tell them.
func TestReview_JSON_IdentityMismatchIsNotAHalt(t *testing.T) {
	hermeticReview(t, "identity_mismatch")
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("projection: %v\nstdout:\n%s", err, out)
	}
	if view.Status == "halted" || view.HaltRecord != nil {
		t.Errorf("an identity mismatch must not produce a halt: status=%q haltRecord=%+v", view.Status, view.HaltRecord)
	}
	if len(view.IdentityCaveats) == 0 {
		t.Error("the mismatch must be projected as an identity caveat")
	}
}

// TestReview_NoJSON_Unchanged pins that the default rendering is untouched — --json is
// strictly opt-in, so nothing that reads the human output regresses.
func TestReview_NoJSON_Unchanged(t *testing.T) {
	hermeticReview(t, "valid")
	code, out, errs := run(t, "review", "--report", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	if !strings.Contains(out, "run: ") || !strings.Contains(out, "review complete: mode=report") {
		t.Errorf("default stdout changed:\n%s", out)
	}
	if strings.Contains(out, "\"schemaVersion\"") {
		t.Error("the projection must not appear without --json")
	}
}

// TestReview_JSON_RefusesSecretWorkspace pins the surface-level confinement check: pointing a
// review at a read-denied path is refused before any run, with the machine reason code.
func TestReview_JSON_RefusesSecretWorkspace(t *testing.T) {
	hermeticReview(t, "valid")
	ws := t.TempDir()
	secret := filepath.Join(ws, ".env")
	if err := os.WriteFile(secret, []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "review", "--report", "--profile", "fake-smoke", secret)
	if code != int(fault.Config) {
		t.Errorf("exit = %d, want %d", code, fault.Config)
	}
	if !strings.Contains(errs, "scope_read_denied") {
		t.Errorf("the refusal must name the machine reason code; stderr:\n%s", errs)
	}
}

// TestDoctor_JSON pins the doctor projection and that exit semantics are unchanged by it.
func TestDoctor_JSON(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir())
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", t.TempDir())
	code, out, errs := run(t, "doctor", "--json")
	var view struct {
		SchemaVersion int  `json:"schemaVersion"`
		OK            bool `json:"ok"`
		Checks        []struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
			Probe  bool   `json:"probe"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("doctor --json is not parseable: %v\nstdout:\n%s\nstderr:%s", err, out, errs)
	}
	if view.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d, want 1", view.SchemaVersion)
	}
	if len(view.Checks) == 0 {
		t.Fatal("no checks in the projection")
	}
	for _, c := range view.Checks {
		if c.Name == "" {
			t.Errorf("check with no name: %+v", c)
		}
	}
	// Exit semantics are unchanged: ok ⇒ 0, not-ok ⇒ 3.
	wantCode := int(fault.OK)
	if !view.OK {
		wantCode = int(fault.Config)
	}
	if code != wantCode {
		t.Errorf("exit = %d, want %d for ok=%v", code, wantCode, view.OK)
	}
	if strings.Contains(out, "doctor: all static checks") || strings.Contains(out, "[ok  ]") {
		t.Error("the human report must not be mixed into the JSON stream")
	}
}

// TestDoctor_JSON_RejectsFix pins the guard: a read-only projection and an interactive repair
// flow cannot share one stream.
func TestDoctor_JSON_RejectsFix(t *testing.T) {
	code, _, errs := run(t, "doctor", "--json", "--fix")
	if code != int(fault.Usage) {
		t.Errorf("exit = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errs, "--json cannot be combined with --fix") {
		t.Errorf("stderr should explain the conflict:\n%s", errs)
	}
}
