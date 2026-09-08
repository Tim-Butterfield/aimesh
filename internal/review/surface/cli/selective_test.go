package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// SELECTIVE APPLY on the CLI (D8-A, design §13.3).
//
// AGAINST A TREE WITHOUT SELECTIVE APPLY every test here fails: `--select` was not a flag, so `flag.Parse`
// refused it and the command exited 3 with "flag provided but not defined". That is a behavioral
// failure rather than a compile one — `parseSelection` and `printSelection` are new symbols, but the
// tests reach them through `run(...)`, which existed.

// TestSelect_CLI_FingerprintIsDisclosedByTheProjection is the DISCLOSURE CHANNEL on this surface: a
// human runs `--report --json`, reads a fingerprint, and passes it back as `--select`. Without it
// the flag would take a value nothing tells you.
func TestSelect_CLI_FingerprintIsDisclosedByTheProjection(t *testing.T) {
	hermeticReview(t, "valid")
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("stdout is not the projection: %v", err)
	}
	if len(view.Findings) == 0 {
		t.Fatalf("no findings in the projection:\n%s", out)
	}
	f := view.Findings[0]
	if f.Fingerprint == "" {
		t.Fatal("the projection discloses no `fingerprint` — a caller cannot pass --select a value it was never told")
	}
	if !strings.HasPrefix(f.Fingerprint, "sha1:") {
		t.Errorf("fingerprint = %q, want the host-computed identity", f.Fingerprint)
	}
	// THE SECURITY PROPERTY: the selector is not the model-authored id.
	if f.Fingerprint == f.ID {
		t.Fatal("the disclosed selector IS the finding's id — a model that could relabel findings could then steer which one --select names")
	}
}

// TestSelect_CLI_IsRefusedInReportMode. `--select` narrows what is WRITTEN, and report mode writes
// nothing, so accepting it would report success for a narrowing that could not have had an effect.
func TestSelect_CLI_IsRefusedInReportMode(t *testing.T) {
	hermeticReview(t, "valid")
	code, _, errs := run(t, "review", "--report", "--profile", "fake-smoke", "--select", "sha1:abc", jsonWorkspace(t))
	if code != int(fault.Usage) {
		t.Fatalf("exit=%d, want %d (usage) — a filter over a write that will not happen is a usage error, not a no-op\nstderr:\n%s",
			code, int(fault.Usage), errs)
	}
	if !strings.Contains(errs, "writes nothing") {
		t.Errorf("the refusal must say why: %q", errs)
	}
}

// TestSelect_CLI_EmptyValueIsRefusedNotWidened. An empty narrowing filter names ZERO findings, and
// the one outcome a caller can neither detect nor survive is having that read as "apply everything".
func TestSelect_CLI_EmptyValueIsRefusedNotWidened(t *testing.T) {
	hermeticReview(t, "valid")
	code, _, errs := run(t, "review", "--apply", "--profile", "fake-smoke", "--select", "  ", jsonWorkspace(t))
	if code != int(fault.Usage) {
		t.Fatalf("exit=%d, want %d — an empty --select must be refused\nstderr:\n%s", code, int(fault.Usage), errs)
	}
	if !strings.Contains(errs, "apply everything") {
		t.Errorf("the refusal must name what it is NOT doing: %q", errs)
	}
}

// TestSelect_CLI_UnmatchedSelectorRefusesAndNamesIt. Every selector named nothing, so nothing would
// be written — refused rather than reported as a clean run that happened to apply zero findings.
// The projection still carries the selection, because "which of my selectors named nothing" is the
// question a caller has at that moment.
func TestSelect_CLI_UnmatchedSelectorRefusesAndNamesIt(t *testing.T) {
	hermeticReview(t, "valid")
	code, out, errs := run(t, "review", "--apply", "--json", "--profile", "fake-smoke",
		"--select", "sha1:names-no-finding", jsonWorkspace(t))
	if code == int(fault.OK) {
		t.Fatalf("a selection that matched nothing exited 0 — nothing was written, and a clean exit says otherwise\nstdout:\n%s", out)
	}
	if !strings.Contains(errs, "apply_selection_matched_nothing") && !strings.Contains(out, "apply_selection_matched_nothing") {
		t.Errorf("the machine reason code must be reported\nstderr:\n%s\nstdout:\n%s", errs, out)
	}
}

// TestSelect_CLI_SelectedApplyWritesAndReportsTheSelection is the capability end to end on the
// surface a human uses.
func TestSelect_CLI_SelectedApplyWritesAndReportsTheSelection(t *testing.T) {
	hermeticReview(t, "valid")
	ws := jsonWorkspace(t)

	// Turn 1: learn the fingerprint.
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke", ws)
	if code != int(fault.OK) {
		t.Fatalf("report exit=%d stderr=%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("projection: %v", err)
	}
	fp := view.Findings[0].Fingerprint

	// Turn 2: apply exactly it.
	code, out, errs = run(t, "review", "--apply", "--json", "--profile", "fake-smoke", "--select", fp, ws)
	if code != int(fault.OK) {
		t.Fatalf("selective apply exit=%d\nstderr:\n%s\nstdout:\n%s", code, errs, out)
	}
	var applied runview.View
	if err := json.Unmarshal([]byte(out), &applied); err != nil {
		t.Fatalf("projection: %v", err)
	}
	if applied.Selection == nil {
		t.Fatalf("the projection carries no `selection`:\n%s", out)
	}
	if len(applied.Selection.Matched) != 1 || applied.Selection.Matched[0] != fp {
		t.Errorf("selection.matched = %v, want the one selector", applied.Selection.Matched)
	}
	if len(applied.Selection.Unmatched) != 0 {
		t.Errorf("selection.unmatched = %v, want none", applied.Selection.Unmatched)
	}
	if applied.Counts.Applied != 1 {
		t.Errorf("counts.applied = %d, want 1", applied.Counts.Applied)
	}
	if applied.Outcome != review.OutcomeApplied {
		t.Errorf("outcome = %q, want %q", applied.Outcome, review.OutcomeApplied)
	}
	// The human channel says what was narrowed, on stderr under --json so stdout stays parseable.
	if !strings.Contains(errs, "selective apply") {
		t.Errorf("the human channel must state the narrowing:\n%s", errs)
	}
}

// TestSelect_CLI_NoSelectionIsUnchanged is the blast radius: an ordinary apply must be exactly what
// it was, or `--select` has changed the meaning of every run that does not use it.
func TestSelect_CLI_NoSelectionIsUnchanged(t *testing.T) {
	hermeticReview(t, string(fake.Valid))
	code, out, errs := run(t, "review", "--apply", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("projection: %v", err)
	}
	if view.Selection != nil {
		t.Errorf("`selection` must be absent when no --select was given: %+v", view.Selection)
	}
	if strings.Contains(errs, "selective apply") || strings.Contains(errs, "SELECTOR") {
		t.Errorf("the human channel must not manufacture a selection:\n%s", errs)
	}
}
