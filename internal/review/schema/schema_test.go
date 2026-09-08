package schema

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

func TestParseReviewerResult_Valid(t *testing.T) {
	in := []byte(`{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"approve","findings":[]}`)
	r, err := ParseReviewerResult(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Verdict != "approve" {
		t.Errorf("verdict = %q", r.Verdict)
	}
}

// TestParseReviewerResult_ToleratesUnknownTopLevelField pins the H2 robustness fix: a real
// reviewer that emits a benign extra top-level key (observed in the wild: `"source":"reviewer"`)
// must NOT lose its findings. Before the fix, DisallowUnknownFields rejected the whole result,
// the one corrective retry replaced it with an empty approve, and 3 valid findings were lost.
func TestParseReviewerResult_ToleratesUnknownTopLevelField(t *testing.T) {
	in := []byte(`{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"request_changes","source":"reviewer","findings":[{"id":"F1","kind":"risk","severity":"high","title":"x"}]}`)
	r, err := ParseReviewerResult(in)
	if err != nil {
		t.Fatalf("an unknown top-level field must be tolerated, not fatal: %v", err)
	}
	if len(r.Findings) != 1 || r.Findings[0].ID != "F1" {
		t.Errorf("findings must be preserved through a benign extra field, got %+v", r.Findings)
	}
}

func TestParseReviewerResult_Malformed(t *testing.T) {
	if _, err := ParseReviewerResult([]byte(`{"role":"reviewer"`)); err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestParseReviewerResult_SchemaInvalid(t *testing.T) {
	in := []byte(`{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"request_changes","findings":[{"id":"F-1","kind":"bug","title":"x"}]}`)
	if _, err := ParseReviewerResult(in); err == nil {
		t.Error("expected error for invalid kind enum")
	}
}

func TestParseHostAdjudication(t *testing.T) {
	ids := []string{"F1", "F2"}
	valid := `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[` +
		`{"findingId":"F1","validity":"valid","decisionState":"reported_valid","severityAdjusted":"high","reasoning":"ok"},` +
		`{"findingId":"F2","validity":"invalid","decisionState":"reported_invalid","reasoning":"not supported"}]}`
	if _, err := ParseHostAdjudication([]byte(valid), ids); err != nil {
		t.Fatalf("valid host adjudication should parse: %v", err)
	}

	bad := map[string]string{
		"missing":     `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"F1","validity":"valid","decisionState":"reported_valid","reasoning":"ok"}]}`,
		"unknown":     `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"F1","validity":"valid","decisionState":"reported_valid","reasoning":"ok"},{"findingId":"F2","validity":"valid","decisionState":"reported_valid","reasoning":"ok"},{"findingId":"F9","validity":"valid","decisionState":"reported_valid","reasoning":"ok"}]}`,
		"duplicate":   `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"F1","validity":"valid","decisionState":"reported_valid","reasoning":"ok"},{"findingId":"F1","validity":"valid","decisionState":"reported_valid","reasoning":"ok"},{"findingId":"F2","validity":"valid","decisionState":"reported_valid","reasoning":"ok"}]}`,
		"badValidity": `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"F1","validity":"maybe","decisionState":"reported_valid","reasoning":"ok"},{"findingId":"F2","validity":"valid","decisionState":"reported_valid","reasoning":"ok"}]}`,
		"badState":    `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"F1","validity":"valid","decisionState":"frob","reasoning":"ok"},{"findingId":"F2","validity":"valid","decisionState":"reported_valid","reasoning":"ok"}]}`,
		"noReasoning": `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"F1","validity":"valid","decisionState":"reported_valid","reasoning":""},{"findingId":"F2","validity":"valid","decisionState":"reported_valid","reasoning":"ok"}]}`,
		"notArray":    `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":{}}`,
		"wrongRole":   `{"schemaVersion":1,"role":"reviewer","phase":"semantic_adjudicate","adjudications":[]}`,
	}
	for name, in := range bad {
		if _, err := ParseHostAdjudication([]byte(in), ids); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseReviewerResult_InvalidSource(t *testing.T) {
	in := `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"request_changes","findings":[{"id":"F1","kind":"fail","severity":"high","title":"x","source":"hacker"}]}`
	if _, err := ParseReviewerResult([]byte(in)); err == nil {
		t.Error("an invalid finding source must be rejected")
	}
}

// The report-only host self-review produces a reviewer-shaped result with role
// author_remediator, phase semantic_author_review, and source author_self_review — all
// must parse.
func TestParseReviewerResult_HostSelfReviewEnumsAccepted(t *testing.T) {
	in := `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_author_review","verdict":"request_changes","findings":[{"id":"H1","kind":"risk","severity":"medium","title":"note","source":"author_self_review"}]}`
	if _, err := ParseReviewerResult([]byte(in)); err != nil {
		t.Errorf("host self-review enums must be accepted: %v", err)
	}
}

func TestParseReviewerResult_FindingsRequired(t *testing.T) {
	for name, in := range map[string]string{
		"missing": `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"approve"}`,
		"null":    `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"approve","findings":null}`,
		"object":  `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"approve","findings":{}}`,
	} {
		if _, err := ParseReviewerResult([]byte(in)); err == nil {
			t.Errorf("%s findings should be rejected", name)
		}
	}
	ok := `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"approve","findings":[]}`
	if _, err := ParseReviewerResult([]byte(ok)); err != nil {
		t.Errorf("empty findings array should pass (approve path): %v", err)
	}
}

func TestNormalizeReviewerOutput(t *testing.T) {
	valid := `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"approve","findings":[]}`

	// raw valid → unchanged, parses
	if out, changed, err := NormalizeReviewerOutput([]byte(valid)); err != nil || changed || string(out) != valid {
		t.Errorf("raw valid: out=%s changed=%v err=%v", out, changed, err)
	}
	// fenced + surrounded → normalized, still valid
	for name, in := range map[string]string{
		"fenced":     "```json\n" + valid + "\n```",
		"surrounded": "Here is the result:\n" + valid + "\nThanks!",
	} {
		out, changed, err := NormalizeReviewerOutput([]byte(in))
		if err != nil || !changed {
			t.Errorf("%s: changed=%v err=%v", name, changed, err)
			continue
		}
		if _, perr := ParseReviewerResult(out); perr != nil {
			t.Errorf("%s: normalized output does not parse: %v", name, perr)
		}
	}
	// rejected forms
	for name, in := range map[string]string{
		"multiple": valid + "\n" + valid,
		"array":    "[1,2,3]",
		"prose":    "no json here",
		"partial":  `{"schemaVersion":1,`,
		"empty":    "   ",
	} {
		if _, _, err := NormalizeReviewerOutput([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestStabilityKey_IgnoresKind pins H1: set-stability keys on file+location WITHOUT kind, so a
// drifting kind on the same issue isn't a "new" finding.
//
// Dedup Fingerprint now excludes kind for the SAME reason (see below), so the two keys have
// converged — this asserts both, rather than the old expectation that they differ.
func TestStabilityKey_IgnoresKind(t *testing.T) {
	a := review.Finding{Kind: review.KindAmbiguity, File: "main.go", Location: "12"}
	b := review.Finding{Kind: review.KindInconsistent, File: "main.go", Location: "12"}
	if StabilityKey(a) != StabilityKey(b) {
		t.Error("kind drift at the same file+location must be the SAME stability key")
	}
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("kind drift at the same file+location must be the SAME fingerprint")
	}
}

// TestFingerprint_KindIsNotIdentity is the regression for the measured defect. On a real run
// (2026-08-11) ONE seat reported the same defect twice at a byte-identical file and location,
// labelling it `inconsistency` once and `fail` once — two fingerprints, two findings, agreement 1
// on each. A model relabelling its own finding must not be able to split it.
func TestFingerprint_KindIsNotIdentity(t *testing.T) {
	loc := "loadPriorDispositions (lines 163-225)"
	inconsistency := review.Finding{Kind: review.KindInconsistent, File: "dispositions.go", Location: loc}
	fail := review.Finding{Kind: review.KindFail, File: "dispositions.go", Location: loc}
	if Fingerprint(inconsistency) != Fingerprint(fail) {
		t.Error("the same defect relabelled by the model is still the same defect")
	}
}

// TestNormalizeLocation_PunctuationIsNotIdentity: the other measured fragmentation. The same seat
// wrote the same place two ways, differing only in how it punctuated the line range, and the
// trailing separator left by the closing parenthesis made them two identities.
func TestNormalizeLocation_PunctuationIsNotIdentity(t *testing.T) {
	for _, pair := range [][2]string{
		{"loadPriorDispositions (lines 163-225)", "loadPriorDispositions, lines 163-225"},
		{"runPanel halt aggregation (lines 208-245)", "runPanel halt aggregation, lines 208-245"},
		{"recordDecisionSet (lines 230-266)", "recordDecisionSet, lines 230-266"},
	} {
		if NormalizeLocation(pair[0]) != NormalizeLocation(pair[1]) {
			t.Errorf("%q and %q normalize differently (%q vs %q)",
				pair[0], pair[1], NormalizeLocation(pair[0]), NormalizeLocation(pair[1]))
		}
	}
}

// TestNormalizeLocation_LineRangesStillDiffer states the LIMIT of this change, so nobody reads the
// two tests above as "duplicate findings are solved". Reconciling different ranges for the same
// place is range arithmetic — deliberately NOT done here — and these pairs, all measured on the
// same real run, remain distinct identities.
func TestNormalizeLocation_LineRangesStillDiffer(t *testing.T) {
	for _, pair := range [][2]string{
		{"StoredDecisionSet.BindWorkspace (lines 177-181, 210-216)", "StoredDecisionSet.BindWorkspace (lines 177-217)"},
		{"runPanel halt aggregation (lines 231-245)", "runPanel halt aggregation (lines 208-245)"},
	} {
		if NormalizeLocation(pair[0]) == NormalizeLocation(pair[1]) {
			t.Errorf("%q and %q merged — range reconciliation is not part of this change and must not arrive by accident", pair[0], pair[1])
		}
	}
}

func TestFingerprint_DedupSameLocationBucket(t *testing.T) {
	a := review.Finding{Kind: review.KindFail, File: "main.go", Location: "1"}
	b := review.Finding{Kind: review.KindFail, File: "main.go", Location: "5"} // same 0-9 bucket
	c := review.Finding{Kind: review.KindFail, File: "main.go", Location: "42"}
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("findings in the same line bucket should share a fingerprint")
	}
	if Fingerprint(a) == Fingerprint(c) {
		t.Error("findings in different line buckets should differ")
	}
}
