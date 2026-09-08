package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	runmgr "github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
)

// TestCLIJSON_ARealRunValidatesAgainstItsPublishedSchema closes the hole that let the published
// projection schema drift out of date without a single test failing.
//
// The only schema check that existed ran over a DRY RUN — a projection with no findings, no panel
// provenance, and none of the run-level qualifiers, because a dry run stops before its first model
// call. So `docs/schema/review-projection.schema.json` was being validated against the one shape that
// exercises almost none of it, while the schema is `additionalProperties: false` and would therefore
// REJECT every real run that gained a key. It had.
//
// This runs a real review and validates what a consumer actually receives.
func TestCLIJSON_ARealRunValidatesAgainstItsPublishedSchema(t *testing.T) {
	hermeticReview(t, "valid")
	root := repoRootFromPackage(t)
	sch, cerr := jsonschema.CompileFile(filepath.Join(root, "docs", "schema", "review-projection.schema.json"))
	if cerr != nil {
		t.Fatalf("compile review-projection.schema.json: %v", cerr)
	}
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	if verr := sch.ValidateJSON([]byte(out)); verr != nil {
		t.Fatalf("a REAL run's projection does not validate against docs/schema/review-projection.schema.json: %v\n%s", verr, out)
	}
	// Non-vacuity: the run must actually have produced the findings this schema is mostly about.
	// Without this the test would pass just as happily over an empty result.
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("stdout is not the projection: %v", err)
	}
	if len(view.Findings) == 0 {
		t.Fatal("the run projected no findings, so this validated almost none of the schema")
	}
}

// TestProjectionSchema_CoversEveryOptionalBlock validates a projection carrying ALL of them at once.
//
// The real-run test above cannot reach these: a single-seat fake panel produces no dissent, no shared
// model, no capacity loss, no narrowed scope and no verification pass, so the schema for those blocks
// would go unchecked until a user hit it. Assembling the view directly is the cheap, deterministic way
// to hold `additionalProperties: false` honest for every key this projection can emit.
func TestProjectionSchema_CoversEveryOptionalBlock(t *testing.T) {
	root := repoRootFromPackage(t)
	sch, cerr := jsonschema.CompileFile(filepath.Join(root, "docs", "schema", "review-projection.schema.json"))
	if cerr != nil {
		t.Fatalf("compile review-projection.schema.json: %v", cerr)
	}
	applyable := false
	view := runview.View{
		SchemaVersion: runview.SchemaVersion,
		Status:        "stable",
		Mode:          "apply",
		RequestedMode: "apply",
		RunID:         "20260101T000000-1",
		Outcome:       string(review.OutcomePartialRefusal),
		Refusals:      []runview.Refusal{{Fingerprint: "sha1:deadbeef", File: ".env", Reason: "protected_path"}},
		Findings: []runview.Finding{{
			ID: "f1", Fingerprint: "sha1:abc", Severity: "high", Kind: "fail",
			File: "a.go", Location: "12", Summary: "a finding carrying every qualifier",
			Disposition: string(review.StateReportedValid), Sources: []string{"reviewer"},
			Applyable: &applyable, ApplyRefusalReason: "authority_only",
			SupportingSeats:       []review.SeatRef{{SeatID: "reviewer", Adapter: "a", Model: "m1", IdentityTier: "verified"}},
			AgreementCount:        1,
			DistinctModels:        1,
			AgreementIndependence: runmgr.IndependenceSharedModel,
			DissentingSeats:       []string{"reviewer-2", "reviewer-3"},
			Consensus:             runmgr.ConsensusContested,
			Grounding: &review.CitationGrounding{
				File: "a.go", Location: "12", Status: runmgr.GroundedLineOutOfRange,
				Detail: "the cited line is past the end of the file", LineCount: 4,
			},
		}},
		IdentityCaveats: []runview.IdentityCaveat{},
		Withheld:        []runview.Withheld{},
		Authority:       []review.AuthorityInclusion{},
		Panel:           []runview.PanelSeat{{SeatID: "reviewer", Index: 1, Adapter: "a", Model: "m1", Status: "completed", Rounds: 1, Findings: 1}},
		Selection:       &review.ApplySelection{Requested: []string{"sha1:abc"}, Matched: []string{"sha1:abc"}, Unmatched: []string{}},
		Grounding: &review.GroundingSummary{
			Root: "/w", Checked: 1, Grounded: 0, Unresolved: 1, NoCitation: 0,
			ByStatus: map[string]int{runmgr.GroundedLineOutOfRange: 1}, Note: runmgr.GroundingNote,
		},
		Composition: &review.PanelComposition{
			Seats:          []review.SeatComposition{{SeatID: "reviewer", Index: 1, Adapter: "a", Model: "m1", Effort: "high"}},
			DistinctModels: 1, Independence: runmgr.IndependenceSharedModel, Note: runmgr.CompositionNote,
		},
		Dissent: &review.DissentSummary{Panelled: 1, Unanimous: 0, Majority: 0, Contested: 1, Note: runmgr.DissentNote},
		PartialPanel: &runmgr.PartialPanel{
			Configured: 3, Answered: 2,
			Lost: []runmgr.LostSeat{{SeatID: "reviewer-3", Adapter: "b", Model: "m2", Signal: "quota_exhausted", Detail: "out of quota"}},
			Note: runmgr.PartialPanelNote,
		},
		Scope: &runmgr.ScopeSummary{
			Selected: 1, Available: 9, Paths: []string{"a.go"}, ChangedSince: "2h", VCSRef: "HEAD",
			Files: []string{"a.go"}, Note: runmgr.ScopeNote,
		},
		Verification: &review.VerificationReport{
			Root: "/copy", Commands: []string{"go test ./..."},
			Before: []review.VerificationResult{{Command: "go test ./...", OK: false, ExitCode: 1, DurationMs: 10, Output: "FAIL"}},
			After:  []review.VerificationResult{{Command: "go test ./...", OK: false, ExitCode: 1, TimedOut: true, Unstartable: "", DurationMs: 20, Output: "FAIL"}},
			Delta:  runmgr.DeltaNotComparable, Note: runmgr.VerificationNote,
		},
		Counts: runview.Counts{
			Findings: 1, BySeverity: map[string]int{"high": 1}, ByDisposition: map[string]int{"reported_valid": 1},
			IdentityCaveats: 0, Withheld: 0, PanelSeats: 3, PanelSeatsCompleted: 2, Applied: 0, Refused: 1,
		},
	}
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if verr := sch.ValidateJSON(payload); verr != nil {
		t.Fatalf("a projection carrying every optional block does not validate: %v\n%s", verr, payload)
	}
}
