package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
)

// repoRootFromPackage walks up to the directory holding docs/schema.
func repoRootFromPackage(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	for range 8 {
		if _, serr := os.Stat(filepath.Join(dir, "docs", "schema")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the repo root (no docs/schema above the package directory)")
	return ""
}

// --dry-run prints the shape and nothing implying a review happened; "findings=0" would read like a
// clean tree.
func TestCLIDryRun_PrintsTheShapeAndNeverAFindingsSummary(t *testing.T) {
	hermeticReview(t, "valid")
	code, out, errs := run(t, "review", "--report", "--dry-run", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	for _, want := range []string{"dry run: nothing was spent", "blind panel", "model calls:", "Drop --dry-run"} {
		if !strings.Contains(out, want) {
			t.Errorf("the disclosure does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "review complete") || strings.Contains(out, "findings=") {
		t.Errorf("a dry run printed a review summary — its zero findings would read as a clean tree:\n%s", out)
	}
}

// The disclosure says what the run would carry, which distinguishes runs over different workspaces
// that cost the same.
func TestCLIDryRun_DisclosesWhatTheRunWouldCarry(t *testing.T) {
	hermeticReview(t, "valid")
	ws := t.TempDir()
	for _, name := range []string{"main.go", "helper.go"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte("package main // "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errs := run(t, "review", "--report", "--dry-run", "--profile", "fake-smoke", ws)
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	if !strings.Contains(out, "workspace: 2 file(s)") {
		t.Errorf("the disclosure does not say what the run would carry:\n%s", out)
	}
	if !strings.Contains(out, "run-shape.json") {
		t.Errorf("the disclosure does not point at the full file list:\n%s", out)
	}
	// Nothing is clipped, so the disclosure must not hedge as if it were.
	if strings.Contains(out, "cut the walk short") || strings.Contains(out, "truncated") {
		t.Errorf("the disclosure hedges about a budget that does not exist:\n%s", out)
	}
}

// --dry-run --json carries the shape and status "planned", so a caller can tell "nothing found" from
// "nothing looked at".
func TestCLIDryRun_JSONProjectionCarriesTheShape(t *testing.T) {
	hermeticReview(t, "valid")
	code, out, errs := run(t, "review", "--report", "--dry-run", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("stdout is not the projection: %v\n%s", err, out)
	}
	if view.Status != "planned" {
		t.Errorf("status = %q, want \"planned\"", view.Status)
	}
	if view.Shape == nil {
		t.Fatal("the projection carries no shape — its presence IS the machine-readable form of \"planned\"")
	}
	if len(view.Shape.Seats) == 0 {
		t.Error("the shape names no seats, so it prices nothing")
	}
	if view.Shape.MinModelCalls < 1 || view.Shape.MaxModelCalls < view.Shape.MinModelCalls {
		t.Errorf("nonsensical call bounds: min=%d max=%d", view.Shape.MinModelCalls, view.Shape.MaxModelCalls)
	}
	if len(view.Findings) != 0 {
		t.Errorf("a dry run projected %d finding(s)", len(view.Findings))
	}
}

// A real run carries no shape.
func TestCLIDryRun_ARealRunCarriesNoShape(t *testing.T) {
	hermeticReview(t, "valid")
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("stdout is not the projection: %v", err)
	}
	if view.Shape != nil {
		t.Errorf("a real run projected a shape: %+v — a shape is a prediction, and this run has facts", *view.Shape)
	}
	if view.Status == "planned" {
		t.Error("a real run reported status \"planned\"")
	}
}

// The dry-run projection validates against the published schema, which forbids additional properties
// and enumerates status.
func TestCLIDryRun_ProjectionValidatesAgainstItsPublishedSchema(t *testing.T) {
	hermeticReview(t, "valid")
	root := repoRootFromPackage(t)
	sch, cerr := jsonschema.CompileFile(filepath.Join(root, "docs", "schema", "review-projection.schema.json"))
	if cerr != nil {
		t.Fatalf("compile review-projection.schema.json: %v", cerr)
	}
	code, out, errs := run(t, "review", "--report", "--dry-run", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	if verr := sch.ValidateJSON([]byte(out)); verr != nil {
		t.Fatalf("a dry run's projection does not validate against docs/schema/review-projection.schema.json: %v\n%s", verr, out)
	}
}

// A findings gate over a dry run would always pass, so the combination is refused.
func TestCLIDryRun_RefusesAFindingsGateOverAReviewThatNeverRan(t *testing.T) {
	hermeticReview(t, "valid")
	for _, gate := range []string{"--fail-on-findings", "--ci"} {
		code, _, errs := run(t, "review", "--report", "--dry-run", gate, "--profile", "fake-smoke", jsonWorkspace(t))
		if code != int(fault.Usage) {
			t.Errorf("%s with --dry-run exited %d, want %d (usage)", gate, code, int(fault.Usage))
		}
		if !strings.Contains(errs, "reviews nothing") {
			t.Errorf("%s refusal does not say why:\n%s", gate, errs)
		}
	}
}
