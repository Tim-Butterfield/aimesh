package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// SELECTIVE APPLY, in the one governed write path every surface reaches (D8-A, design §13.3).
//
// AGAINST A TREE WITHOUT SELECTIVE APPLY every test here fails to COMPILE: `writeRequest.Select`,
// `RemediateRequest.Select`, `review.ApplySelection`, `selectAccepted`, `ReasonSelectionEmpty`
// and `ReasonSelectionMatchedNothing` did not exist. That is stated plainly rather than dressed up.
// The MUTATION each test would catch is named on it, and each was run.
//
// The security property is the reason the write set is keyed the way it is, and it is asserted directly
// by TestSelect_ModelAuthoredFindingIDIsNotASelector: the key is the HOST-COMPUTED fingerprint and
// never `Finding.ID`. A model-authored identifier is model-controlled, so keying the write set on
// one would let a model relabel findings until "apply only this one" selected something else.

// twoAccepted is a two-finding accepted set across two files, which is the smallest set a selection
// can meaningfully narrow.
func twoAccepted(t *testing.T, ws string) ([]review.Finding, []review.Decision) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, "other.go"), []byte("package sample\n\nfunc Other() int { return 2 }\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	fs := []review.Finding{
		{ID: "F1", Title: "sample returns a magic number", Kind: review.KindRisk,
			Severity: review.SeverityMedium, File: "sample.go"},
		{ID: "F2", Title: "other returns a magic number", Kind: review.KindRisk,
			Severity: review.SeverityMedium, File: "other.go"},
	}
	ds := []review.Decision{
		{FindingID: "F1", Valid: true, State: review.StateReportedValid},
		{FindingID: "F2", Valid: true, State: review.StateReportedValid},
	}
	return fs, ds
}

// selectiveRequest is baseRequest widened to the two-finding set.
func selectiveRequest(t *testing.T, ws string, mode review.Mode) RemediateRequest {
	t.Helper()
	req := baseRequest(t, ws, mode)
	req.Findings, req.Decisions = twoAccepted(t, ws)
	req.Shown = map[string]bool{"sample.go": true, "other.go": true}
	req.BaseHashes = BaseHashes(ws, []string{"sample.go", "other.go"})
	// The identity is re-captured because the fixture file was added after baseRequest looked.
	id, err := CaptureWorkspaceIdentity(ws)
	if err != nil {
		t.Fatalf("capture workspace identity: %v", err)
	}
	req.WorkspaceIdentity = id
	return req
}

func fingerprintOf(f review.Finding) string { return schema.Fingerprint(f) }

// TestSelect_WritesOnlyTheSelectedFinding is the capability itself: two accepted findings, one
// selector, one file written and one file byte-for-byte untouched.
// MUTATION: ignore req.Select in governedWrite (use `accepted` unnarrowed).
func TestSelect_WritesOnlyTheSelectedFinding(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := selectiveRequest(t, ws, review.ModeApply)
	before := read(t, filepath.Join(ws, "other.go"))
	req.Select = []string{fingerprintOf(req.Findings[0])}

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if got := read(t, filepath.Join(ws, "sample.go")); !strings.Contains(got, "reviewmesh[F1]") {
		t.Fatalf("the SELECTED finding was not applied:\n%s", got)
	}
	if got := read(t, filepath.Join(ws, "other.go")); got != before {
		t.Fatalf("an UNSELECTED finding was written — a selection can only narrow:\n%s", got)
	}
	if n := len(out.Receipt.Applied); n != 1 {
		t.Errorf("receipt.applied = %d finding(s), want 1", n)
	}
	if out.Selection == nil || len(out.Selection.Matched) != 1 || len(out.Selection.Unmatched) != 0 {
		t.Errorf("selection = %+v, want one matched and none unmatched", out.Selection)
	}
	// The DURABLE record carries it too: a caller whose response was cancelled must still be able to
	// answer "what did I ask to narrow to" from the run directory alone.
	if r := readReceipt(t, out.RunDir); r.Selection == nil || len(r.Selection.Matched) != 1 {
		t.Errorf("receipt.selection = %+v, want the selection on disk", r.Selection)
	}
}

// TestSelect_ModelAuthoredFindingIDIsNotASelector IS THE SECURITY PROPERTY. `F1` is a perfectly good
// identifier for the finding — and it must select nothing, because a model that could relabel
// findings could otherwise steer which one a caller's "apply only this" names.
// MUTATION: index byFP on `t.finding.ID` as well as the fingerprint.
func TestSelect_ModelAuthoredFindingIDIsNotASelector(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := selectiveRequest(t, ws, review.ModeApply)
	before := map[string]string{
		"sample.go": read(t, filepath.Join(ws, "sample.go")),
		"other.go":  read(t, filepath.Join(ws, "other.go")),
	}
	req.Select = []string{"F1"} // the model-authored id, which must name nothing

	out, err := m.Remediate(context.Background(), req)
	if err == nil {
		t.Fatal("a model-authored finding id must not select anything; the call must refuse rather than write")
	}
	if got := fault.ReasonOf(err); got != ReasonSelectionMatchedNothing {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonSelectionMatchedNothing, err)
	}
	for name, want := range before {
		if got := read(t, filepath.Join(ws, name)); got != want {
			t.Errorf("%s was written by a run whose selection matched nothing:\n%s", name, got)
		}
	}
	if out.Selection == nil || len(out.Selection.Unmatched) != 1 || out.Selection.Unmatched[0] != "F1" {
		t.Errorf("selection = %+v, want F1 reported as unmatched — a caller that mistyped a selector must find out", out.Selection)
	}
}

// TestSelect_EmptySelectionIsARefusalNotEverything. The one outcome a caller can neither detect nor
// survive is a narrowing filter that reads as "apply everything".
// MUTATION: treat `len(sel.Requested) == 0` as "no selection" and fall through.
func TestSelect_EmptySelectionIsARefusalNotEverything(t *testing.T) {
	for _, empty := range [][]string{{}, {""}, {"  "}} {
		m, ws := remediateFixture(t, true)
		req := selectiveRequest(t, ws, review.ModeApply)
		req.Select = empty
		before := read(t, filepath.Join(ws, "sample.go"))

		_, err := m.Remediate(context.Background(), req)
		if err == nil {
			t.Fatalf("select=%v must be refused, never widened to the whole accepted set", empty)
		}
		if got := fault.ReasonOf(err); got != ReasonSelectionEmpty {
			t.Fatalf("select=%v reasonCode = %q, want %q", empty, got, ReasonSelectionEmpty)
		}
		if got := read(t, filepath.Join(ws, "sample.go")); got != before {
			t.Fatalf("select=%v wrote to the workspace:\n%s", empty, got)
		}
	}
}

// TestSelect_UnmatchedIsDroppedAndReportedNotFetched — a PARTIAL match proceeds. The unmatched
// selector writes nothing, is never resolved against another run, and is reported so a caller that
// mistyped one finds out rather than reading a smaller apply as a smaller finding set.
// MUTATION: drop the Unmatched accumulation in selectAccepted.
func TestSelect_UnmatchedIsDroppedAndReportedNotFetched(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := selectiveRequest(t, ws, review.ModeApply)
	before := read(t, filepath.Join(ws, "other.go"))
	req.Select = []string{fingerprintOf(req.Findings[0]), "sha1:not-a-finding-in-this-run"}

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("a PARTIAL match must proceed, not refuse: %v", err)
	}
	if out.Selection == nil {
		t.Fatal("the selection must be reported")
	}
	if len(out.Selection.Matched) != 1 || len(out.Selection.Unmatched) != 1 {
		t.Fatalf("selection = %+v, want one matched and one unmatched", out.Selection)
	}
	if out.Selection.Unmatched[0] != "sha1:not-a-finding-in-this-run" {
		t.Errorf("unmatched = %v, want the selector that named nothing", out.Selection.Unmatched)
	}
	if got := read(t, filepath.Join(ws, "other.go")); got != before {
		t.Fatal("an unmatched selector reached a finding it does not name")
	}
	if n := len(out.Receipt.Applied); n != 1 {
		t.Errorf("receipt.applied = %d, want 1 — only the matched finding was written", n)
	}
}

// TestSelect_AbsentSelectionAppliesEverything is the blast radius. `Select` must mean something,
// which requires the un-narrowed case to be exactly what it was.
// MUTATION: make `req.Select == nil` take the narrowing branch.
func TestSelect_AbsentSelectionAppliesEverything(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := selectiveRequest(t, ws, review.ModeApply)
	req.Select = nil

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if n := len(out.Receipt.Applied); n != 2 {
		t.Fatalf("receipt.applied = %d, want both findings — an absent selection narrows nothing", n)
	}
	if out.Selection != nil {
		t.Errorf("selection = %+v, want nil when none was supplied", out.Selection)
	}
}

// TestSelect_PatchModeIsNarrowedToo — the design keys the partial-refusal signal on `refused > 0`
// "uniformly across both write modes" for the same reason a selection must narrow both: a caller of a
// patch-mode run needs the diff to contain what it asked for and nothing else.
// MUTATION: apply the selection only when req.Mode == ModeApply.
func TestSelect_PatchModeIsNarrowedToo(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := selectiveRequest(t, ws, review.ModePatch)
	req.Select = []string{fingerprintOf(req.Findings[1])}

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if len(out.Receipt.Applied) != 1 || out.Receipt.Applied[0].File != "other.go" {
		t.Fatalf("receipt.applied = %+v, want only the selected finding in the diff", out.Receipt.Applied)
	}
	for _, f := range out.Receipt.Files {
		if f == "sample.go" {
			t.Fatal("the diff contains a file whose finding was not selected")
		}
	}
}

// --- selectAccepted, as a unit ---

// TestSelectAccepted_NormalizesAndPreservesOrder pins the three normalizations, each of which is a
// decision rather than a convenience: blanks name nothing, a repeat is a typo rather than a request
// to write twice, and the WRITE order stays the accepted set's — the journal and the per-finding call
// ids are ordered by it, and letting a caller reorder the write sequence would be a capability
// nobody asked for.
// MUTATION: build `narrowed` by iterating sel.Requested instead of `accepted`.
func TestSelectAccepted_NormalizesAndPreservesOrder(t *testing.T) {
	f1 := review.Finding{ID: "F1", Kind: review.KindRisk, File: "a.go", Location: "L10"}
	f2 := review.Finding{ID: "F2", Kind: review.KindRisk, File: "b.go", Location: "L20"}
	accepted := []acceptedTarget{{finding: f1, index: 0}, {finding: f2, index: 1}}

	// The caller names them in the reverse order, with a blank and a duplicate mixed in.
	sel, narrowed := selectAccepted(accepted, []string{
		"  " + schema.Fingerprint(f2) + "  ", "", schema.Fingerprint(f1), schema.Fingerprint(f2),
	})
	if len(sel.Requested) != 2 {
		t.Fatalf("requested = %v, want the blank dropped and the duplicate collapsed", sel.Requested)
	}
	if len(sel.Matched) != 2 || len(sel.Unmatched) != 0 {
		t.Fatalf("selection = %+v, want both matched", sel)
	}
	if len(narrowed) != 2 || narrowed[0].finding.ID != "F1" || narrowed[1].finding.ID != "F2" {
		t.Fatalf("narrowed order = %+v, want the ACCEPTED set's order, not the caller's", narrowed)
	}
}

// TestSelectAccepted_NarrowsNeverExtends. There is no branch that adds a finding back, and this is
// the assertion that says so: a selection over an empty accepted set matches nothing and produces
// nothing, rather than producing what it named.
// MUTATION: fall back to `accepted` when `narrowed` is empty.
func TestSelectAccepted_NarrowsNeverExtends(t *testing.T) {
	sel, narrowed := selectAccepted(nil, []string{"sha1:whatever"})
	if len(narrowed) != 0 {
		t.Fatalf("narrowed = %+v over an EMPTY accepted set — a selection can only reduce", narrowed)
	}
	if len(sel.Unmatched) != 1 {
		t.Fatalf("selection = %+v, want the selector reported as unmatched", sel)
	}
}

// TestSelect_BothGovernedWriteCallersHonourIt is the PARITY assertion at the level parity actually
// lives: `Manager.Remediate` (MCP's from-run form) and `handleMode` (the CLI and ACP full cycle) are
// the only two callers of `governedWrite`, and both must thread `Select` into it. A surface that
// accepted the argument and dropped it would silently widen its write set to everything.
//
// It reads the source rather than running both paths because the two produce different runs by
// design — a from-run remediation replays a stored decision set, a full cycle adjudicates its own —
// so "the same receipt" is not a comparison the two can be held to. What they CAN be held to, and
// what matters, is that neither drops the argument on the floor. TestOneGovernedWritePath already
// proves there is no third caller to check.
// MUTATION: delete `Select: req.Select` from either call site.
func TestSelect_BothGovernedWriteCallersHonourIt(t *testing.T) {
	for _, f := range []string{"remediate.go", "review.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(b), "Select: req.Select") {
			t.Errorf("%s calls governedWrite without threading `Select` — a surface that accepted a narrowing filter and then wrote everything is the one outcome a caller can neither detect nor survive", f)
		}
	}
}
