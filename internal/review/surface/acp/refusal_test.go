package acp

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
)

// The ACP half of the partial-refusal contract (design §13.4.3).
//
// `refusal` is an official ACP StopReason and this surface has always documented it as legal;
// what it did not do was ever emit one — only `end_turn` and `cancelled`. A partially-refused
// apply is exactly the case it is for.
//
// Why not `end_turn`: it is the word for a turn that ended normally, and a host keying on
// stopReason alone would read it as clean. Why not a `-32000` halt: a halt is an ERROR response,
// and telling a caller the turn failed over a write that committed is the worst of both — the
// receipt exists and the caller has been told it does not.
//
// AGAINST THE OLD CODE BOTH TESTS FAIL: the surface emitted `end_turn`, and `_meta.reviewmesh`
// carried no `outcome`, `applied`, `refused` or `refusals`.
//
// The turns below are FROM-RUN writes, because that is what an ACP apply turn now is: it applies the
// decision set the `fromRun` run recorded rather than re-adjudicating. The rendering contract these
// tests pin is unchanged by that; what changed is which half of the Manager produces the outcome.

// refusedRemediation is a completed apply that wrote seven findings and refused one.
func refusedRemediation() run.RemediateOutcome {
	applied := make([]run.AppliedFinding, 0, 7)
	for i := 0; i < 7; i++ {
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

// applyTurn is the two-turn shape: a handle from an earlier report turn, and an apply turn keyed on
// it. The handle is resolved by the fake, so these tests stay about the RESPONSE contract.
const applyTurn = `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":` +
	`{"sessionId":"s-0001","workspace":` + wsPlaceholder + `,"fromRun":"/artifacts/prior-report-run","mode":"apply"}}`

// TestPrompt_PartialRefusal_StopReasonRefusal is the coarse signal: a host that reads nothing but
// `stopReason` must not conclude the turn was clean.
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

// TestPrompt_PartialRefusal_MetaCarriesTheNumbers is the other half, and it is what keeps
// `refusal` from being MIS-read. `refusal` over-signals on purpose; the risk it introduces is a
// host concluding that NOTHING was applied, and the answer to that risk is `applied: 7` sitting
// beside it. A stopReason with no numbers would trade one wrong reading for another.
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
	// Keyed on the HOST-COMPUTED fingerprint, never the model-authored finding id.
	if row["fingerprint"] != "sha1:deadbeef" {
		t.Errorf("refusal fingerprint = %v, want the host-computed value", row["fingerprint"])
	}
	// The two handles are BOTH present and are different runs: `runDir` is this write's own record
	// (its journal and receipt), `sourceRunDir` the run whose decisions it applied. A response that
	// carried one for both would make the receipt unfindable.
	if rm["runDir"] == rm["sourceRunDir"] {
		t.Errorf("runDir and sourceRunDir must name different runs, both were %v", rm["runDir"])
	}
	if rm["sourceRunDir"] == nil {
		t.Error("a from-run write must name the run it applied")
	}
}

// TestPrompt_CleanApply_StillEndTurn pins the blast radius: an apply that refused nothing is
// unchanged. `refusal` must mean something, which requires that the ordinary turn keeps saying
// `end_turn`.
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
