package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
)

// groundingTree writes a small tree and returns its root.
func groundingTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"a.go":       "package a\n\nfunc Alpha() {}\n", // 3 newlines → 4 lines by our count
		"sub/b.go":   "package b\n",
		"big.txt":    strings.Repeat("x", maxGroundedFileBytes+1),
		"empty.go":   "",
		"nolf.go":    "package n",
		"symbols.go": "package s\n\nfunc DoTheThing() {}\n",
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

// adjOf builds an adjudication result whose Decisions are index-aligned with its Findings, which is
// the shape the annotation passes require.
func adjOf(findings ...review.Finding) *adjudication.Result {
	adj := &adjudication.Result{Findings: findings}
	for range findings {
		adj.Decisions = append(adj.Decisions, review.Decision{Valid: true, State: review.StateReportedValid})
	}
	return adj
}

// TestGrounding_ReportsWhatItChecked walks the statuses that matter, each against a real file, so the
// codes mean what they say rather than what a fixture asserted.
func TestGrounding_ReportsWhatItChecked(t *testing.T) {
	root := groundingTree(t)
	cases := []struct {
		name       string
		file, loc  string
		wantStatus string
	}{
		{"file and line both there", "a.go", "3", GroundedOK},
		{"a range that fits", "a.go", "1-3", GroundedOK},
		{"no location at all", "a.go", "", GroundedOK},
		{"a symbol that is present", "symbols.go", "DoTheThing", GroundedOK},
		{"a file that is not there", "ghost.go", "12", GroundedFileMissing},
		{"a nested file that is there", "sub/b.go", "1", GroundedOK},
		{"a line past the end", "a.go", "4000", GroundedLineOutOfRange},
		{"a symbol that is absent", "symbols.go", "NotHere", GroundedSymbolAbsent},
		{"a prose location", "a.go", "the error handling block near the top", GroundedLocationUnchecked},
		{"a file too big to load", "big.txt", "1", GroundedUnreadable},
		{"a path escaping the root", "../outside.go", "1", GroundedFileMissing},
		{"no citation", "", "", GroundedNoCitation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adj := adjOf(review.Finding{ID: "F1", Title: "t", File: tc.file, Location: tc.loc})
			rep := groundFindings(root, adj)
			if rep == nil {
				t.Fatal("a run with a workspace must produce a summary")
			}
			g := adj.Decisions[0].Grounding
			if g == nil {
				t.Fatal("every finding must be labelled")
			}
			if g.Status != tc.wantStatus {
				t.Errorf("status = %q (%s), want %q", g.Status, g.Detail, tc.wantStatus)
			}
			if g.Detail == "" {
				t.Error("a status without a detail sentence makes a reader guess what was checked")
			}
		})
	}
}

// TestGrounding_NeverTouchesTheFinding is the invariant, asserted directly.
//
// A finding whose file does not exist is the one a reader is most tempted to suppress, and #41 removed
// exactly this class of gate for model identity. The host knows the citation did not resolve; it does
// not know whether the defect is real one file over, and it has no standing to decide.
func TestGrounding_NeverTouchesTheFinding(t *testing.T) {
	root := groundingTree(t)
	adj := adjOf(
		review.Finding{ID: "F1", Title: "real", File: "a.go", Location: "1", Severity: review.SeverityHigh},
		review.Finding{ID: "F2", Title: "points nowhere", File: "ghost.go", Location: "99", Severity: review.SeverityHigh},
	)
	before := len(adj.Findings)
	beforeState, beforeSeverity := adj.Decisions[1].State, adj.Findings[1].Severity

	groundFindings(root, adj)

	if len(adj.Findings) != before {
		t.Fatalf("grounding dropped a finding: %d → %d", before, len(adj.Findings))
	}
	if adj.Findings[1].Title != "points nowhere" {
		t.Error("grounding reordered the findings")
	}
	if adj.Decisions[1].State != beforeState {
		t.Errorf("grounding changed a decision state: %q → %q", beforeState, adj.Decisions[1].State)
	}
	if adj.Findings[1].Severity != beforeSeverity {
		t.Errorf("grounding changed a severity: %q → %q", beforeSeverity, adj.Findings[1].Severity)
	}
	if adj.Decisions[1].Valid != true {
		t.Error("grounding invalidated a finding — a test suite does not know whether a reviewer is right, and neither does a file check")
	}
}

// TestGrounding_ProseLocationsAreNotAccused. The conservative direction is the design choice: an
// unchecked location costs a reader nothing, while a false symbol_absent discredits a correct finding.
// Anything wordier than one identifier is a section description, not a symbol.
func TestGrounding_ProseLocationsAreNotAccused(t *testing.T) {
	root := groundingTree(t)
	for _, loc := range []string{
		"the retry loop",
		"near the bottom of the file",
		"func Alpha, second branch",
		"§3.2",
	} {
		adj := adjOf(review.Finding{ID: "F", File: "a.go", Location: loc})
		groundFindings(root, adj)
		if got := adj.Decisions[0].Grounding.Status; got == GroundedSymbolAbsent {
			t.Errorf("location %q was ACCUSED of naming a missing symbol; a prose location must be reported unchecked", loc)
		}
	}
}

// TestGrounding_SummaryCountsAndDenominator. `noCitation` is deliberately outside `checked`: a finding
// about the change as a whole has nothing to resolve, and folding it into either column would turn the
// ratio into a statement about a different population.
func TestGrounding_SummaryCountsAndDenominator(t *testing.T) {
	root := groundingTree(t)
	adj := adjOf(
		review.Finding{ID: "1", File: "a.go", Location: "1"},
		review.Finding{ID: "2", File: "a.go", Location: "2"},
		review.Finding{ID: "3", File: "ghost.go", Location: "1"},
		review.Finding{ID: "4"}, // no citation at all
	)
	rep := groundFindings(root, adj)

	if rep.Checked != 3 || rep.Grounded != 2 || rep.Unresolved != 1 || rep.NoCitation != 1 {
		t.Errorf("summary = %+v", rep)
	}
	if rep.Checked != rep.Grounded+rep.Unresolved {
		t.Errorf("checked must be exactly grounded + unresolved: %+v", rep)
	}
	if rep.ByStatus[GroundedFileMissing] != 1 || rep.ByStatus[GroundedNoCitation] != 1 {
		t.Errorf("byStatus = %v", rep.ByStatus)
	}
	if rep.Note != GroundingNote {
		t.Error("the summary must carry the fixed note; a surface that reports the ratio without it makes a stronger claim than the run did")
	}
	if !strings.Contains(rep.Note, "does NOT") {
		t.Errorf("the note must state what grounding does not establish: %q", rep.Note)
	}
	if rep.Root != root {
		t.Errorf("root = %q, want %q — a check must say what tree it read", rep.Root, root)
	}
}

// TestGrounding_NoWorkspaceProducesNoReport. A caller with no reviewed tree (the ACP validation probe)
// has nothing a citation could resolve against, and an empty report would assert a check that never
// ran — the same reason the identity work refuses to report a tier it did not establish.
func TestGrounding_NoWorkspaceProducesNoReport(t *testing.T) {
	adj := adjOf(review.Finding{ID: "1", File: "a.go", Location: "1"})
	if rep := groundFindings("", adj); rep != nil {
		t.Fatalf("no workspace must produce no summary, got %+v", rep)
	}
	if adj.Decisions[0].Grounding != nil {
		t.Error("with nothing to check against, a finding must carry no grounding label at all")
	}
}

// TestGrounding_LineCountingIsExact pins the boundary, because an off-by-one here is a false
// line_out_of_range on the last line of every file that does not end in a newline.
func TestGrounding_LineCountingIsExact(t *testing.T) {
	root := groundingTree(t)
	cases := map[string]struct {
		file, loc  string
		wantStatus string
	}{
		"last line of a file ending in a newline": {"a.go", "3", GroundedOK},
		"one past it": {"a.go", "5", GroundedLineOutOfRange},
		"the only line of a file with no terminator":  {"nolf.go", "1", GroundedOK},
		"line 1 of an empty file":                     {"empty.go", "1", GroundedOK},
		"line 2 of an empty file":                     {"empty.go", "2", GroundedLineOutOfRange},
		"a range whose HIGH end is what gets checked": {"a.go", "1-4000", GroundedLineOutOfRange},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			adj := adjOf(review.Finding{ID: "F", File: tc.file, Location: tc.loc})
			groundFindings(root, adj)
			if got := adj.Decisions[0].Grounding.Status; got != tc.wantStatus {
				t.Errorf("status = %q (%s), want %q", got, adj.Decisions[0].Grounding.Detail, tc.wantStatus)
			}
		})
	}
}

// TestGrounding_RunsOnARealReview is the wiring test, and it is the one the unit tests above cannot
// substitute for: every assertion in this file so far calls groundFindings directly, so all of them
// would keep passing if the manager never called it. This drives a whole review through the Manager
// and requires the label to come out the other end.
func TestGrounding_RunsOnARealReview(t *testing.T) {
	m := rejectingManager(t)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "prior"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(out.Findings) == 0 {
		t.Fatal("the fixture must produce a finding, or this test asserts nothing")
	}
	if out.Grounding == nil {
		t.Fatal("a review of a real workspace must carry the grounding summary — the pass is not wired into the run")
	}
	if out.Grounding.Checked+out.Grounding.NoCitation != len(out.Findings) {
		t.Errorf("every finding must be accounted for: %+v over %d finding(s)", out.Grounding, len(out.Findings))
	}
	for i, d := range out.Decisions {
		if d.Grounding == nil {
			t.Fatalf("decision %d carries no grounding label", i)
		}
	}
	// The fixture's finding points at a file this workspace really has, so it grounds. Asserting the
	// POSITIVE outcome (rather than merely "a label exists") is what stops the pass from passing this
	// test while reporting file_missing for everything.
	if got := out.Decisions[0].Grounding.Status; got != GroundedOK {
		t.Errorf("status = %q (%s), want %q — the fixture cites a file the workspace has",
			got, out.Decisions[0].Grounding.Detail, GroundedOK)
	}
}

// TestGroundingRows_ProjectsInFindingOrder — the one derivation every surface builds from, so three
// surfaces cannot describe one run three ways.
func TestGroundingRows_ProjectsInFindingOrder(t *testing.T) {
	root := groundingTree(t)
	adj := adjOf(
		review.Finding{ID: "1", File: "a.go", Location: "1"},
		review.Finding{ID: "2", File: "ghost.go", Location: "1"},
	)
	groundFindings(root, adj)
	rows := GroundingRows(review.RunOutcome{Decisions: adj.Decisions})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Status != GroundedOK || rows[1].Status != GroundedFileMissing {
		t.Errorf("rows are out of finding order: %+v", rows)
	}
}
