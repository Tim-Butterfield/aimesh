package acp

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
)

// A partially refused apply answers with stopReason refusal: end_turn would read as clean, and an
// error response would contradict a committed write. These turns are fromRun writes, as every ACP
// apply turn is.

// refusedRemediation is a completed apply that wrote seven findings and refused one.
func refusedRemediation() run.RemediateOutcome {
	applied := make([]run.AppliedFinding, 0, 7)
	for i := range 7 {
		applied = append(applied, run.AppliedFinding{
			FindingID: "F-00" + string(rune('1'+i)), File: "a.go",
			State: string(review.StateApplied),
		})
	}
	return run.RemediateOutcome{
		RunID: "run-remediate", RunDir: "/tmp/run-x",
		Receipt: run.Receipt{Status: "complete", Committed: true, Applied: applied},
		Refusals: []review.ApplyRefusal{{
			Fingerprint: "sha1:deadbeef", File: ".env",
			Reason: review.ApplyRefusalProtectedPath,
		}},
	}
}

// applyTurn is an apply turn keyed on a handle from an earlier report turn. The fake resolves the
// handle, so these tests cover only the response.
const applyTurn = `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":` +
	`{"sessionId":"s-0001","workspace":` + wsPlaceholder + `,"fromRun":"/artifacts/prior-report-run","mode":"apply"}}`

// A host that reads only stopReason must not conclude the turn was clean.
func TestPrompt_PartialRefusal_StopReasonRefusal(t *testing.T) {
	r := serveCaps(t, &fakeRemediator{out: refusedRemediation()},
		review.SurfaceCaps{FileRead: true, FileWrite: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+wsPlaceholder+`}}`,
		applyTurn)
	last := r[len(r)-1]
	if e, ok := last["error"]; ok {
		t.Fatalf("a partial refusal is a COMPLETED write, not an error response: %v", e)
	}
	if sr := stopReasonOf(t, last); sr != "refusal" {
		t.Fatalf("stopReason = %q, want refusal", sr)
	}
}

// refusal must not read as nothing applied, so the response carries applied: 7 beside it.
func TestPrompt_PartialRefusal_MetaCarriesTheNumbers(t *testing.T) {
	r := serveCaps(t, &fakeRemediator{out: refusedRemediation()},
		review.SurfaceCaps{FileRead: true, FileWrite: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+wsPlaceholder+`}}`,
		applyTurn)
	rm := promptMeta(t, r[len(r)-1])

	if rm["outcome"] != review.OutcomePartialRefusal {
		t.Errorf("_meta.reviewmesh.outcome = %v, want %q", rm["outcome"], review.OutcomePartialRefusal)
	}
	if rm["applied"] != float64(7) {
		t.Errorf("_meta.reviewmesh.applied = %v, want 7 — without it `refusal` reads as \"nothing was written\"", rm["applied"])
	}
	if rm["refused"] != float64(1) {
		t.Errorf("_meta.reviewmesh.refused = %v, want 1", rm["refused"])
	}
	refusals, ok := rm["refusals"].([]any)
	if !ok || len(refusals) != 1 {
		t.Fatalf("_meta.reviewmesh.refusals = %v, want one entry", rm["refusals"])
	}
	row, _ := refusals[0].(map[string]any)
	if row["file"] != ".env" || row["reason"] != review.ApplyRefusalProtectedPath {
		t.Errorf("refusal = %v, want .env/protected_path", row)
	}
	// Keyed on the host-computed fingerprint, never the model-authored finding id.
	if row["fingerprint"] != "sha1:deadbeef" {
		t.Errorf("refusal fingerprint = %v, want the host-computed value", row["fingerprint"])
	}
	// runDir is this write's own record and sourceRunDir the run whose decisions it applied; both must
	// be present and differ.
	if rm["runDir"] == rm["sourceRunDir"] {
		t.Errorf("runDir and sourceRunDir must name different runs, both were %v", rm["runDir"])
	}
	if rm["sourceRunDir"] == nil {
		t.Error("a from-run write must name the run it applied")
	}
}

// An apply that refused nothing still ends with end_turn.
func TestPrompt_CleanApply_StillEndTurn(t *testing.T) {
	clean := refusedRemediation()
	clean.Refusals = nil
	r := serveCaps(t, &fakeRemediator{out: clean},
		review.SurfaceCaps{FileRead: true, FileWrite: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+wsPlaceholder+`}}`,
		applyTurn)
	last := r[len(r)-1]
	if sr := stopReasonOf(t, last); sr != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn on a clean apply", sr)
	}
	if rm := promptMeta(t, last); rm["outcome"] != review.OutcomeApplied {
		t.Errorf("outcome = %v, want %q", rm["outcome"], review.OutcomeApplied)
	}
}
