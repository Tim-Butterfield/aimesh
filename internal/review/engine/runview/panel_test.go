package runview

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// TestBuild_ProjectsThePanelAndItsProvenance: the machine projection exposes the executed roster
// and the host-computed provenance verbatim. A consumer must be able to see how many independent
// vantages stood behind a finding without recounting anything — and must never be told a different
// number than the host decided.
func TestBuild_ProjectsThePanelAndItsProvenance(t *testing.T) {
	no := false
	out := review.RunOutcome{
		Status: "stable", Mode: review.ModeReport,
		Panel: []review.SeatStatus{
			{SeatID: "reviewer", Index: 1, Adapter: "codex-cli", Model: "m", Status: "completed", Rounds: 2, Findings: 3, IdentityTier: "verified"},
			{SeatID: "reviewer-2", Index: 2, Adapter: "claude-code", Model: "m", Status: "completed", Rounds: 1, Findings: 1, IdentityTier: "self_reported"},
			{SeatID: "reviewer-3", Index: 3, Adapter: "agy-cli", Model: "m", Status: "halted", ReasonCode: "model_identity_mismatch"},
		},
		Findings: []review.Finding{{ID: "f1", Title: "t", Kind: review.KindFail, Severity: review.SeverityHigh}},
		Decisions: []review.Decision{{
			FindingID: "f1", Valid: true, State: review.StateReportedValid,
			SupportingSeats: []review.SeatRef{
				{SeatID: "reviewer", Adapter: "codex-cli", Model: "m", IdentityTier: "self_reported"},
			},
			AgreementCount:     1,
			DissentingSeats:    []string{"reviewer-2"},
			Applyable:          &no,
			ApplyRefusalReason: "no_workspace_evidence",
		}},
	}
	v := Build(Input{Outcome: out})
	if len(v.Panel) != 3 {
		t.Fatalf("projection panel = %d seats, want 3 (every REQUESTED seat, halted ones included)", len(v.Panel))
	}
	if v.Counts.PanelSeats != 3 || v.Counts.PanelSeatsCompleted != 2 {
		t.Errorf("counts = %d requested / %d completed, want 3/2 — the gap must be visible, not folded away",
			v.Counts.PanelSeats, v.Counts.PanelSeatsCompleted)
	}
	f := v.Findings[0]
	if f.AgreementCount != 1 || len(f.SupportingSeats) != 1 || f.SupportingSeats[0].SeatID != "reviewer" {
		t.Errorf("provenance was not carried through verbatim: %+v", f)
	}
	if f.DissentingSeats[0] != "reviewer-2" {
		t.Errorf("dissent = %v, want [reviewer-2]", f.DissentingSeats)
	}
	if f.ApplyRefusalReason != "no_workspace_evidence" || f.Applyable == nil || *f.Applyable {
		t.Errorf("a write-path refusal must survive into the projection, got %+v", f)
	}
	// The seat's identity tier rides along for the reader; it is not why anything was refused.
	if f.SupportingSeats[0].IdentityTier != "self_reported" {
		t.Errorf("the supporting seat's identity tier must be projected verbatim: %+v", f.SupportingSeats[0])
	}

	// `panel` is ALWAYS present (never null) so a consumer can index it without a null check.
	b, err := json.Marshal(Build(Input{}))
	if err != nil {
		t.Fatalf("marshal empty view: %v", err)
	}
	if !strings.Contains(string(b), `"panel":[]`) {
		t.Errorf("an empty projection must carry an empty panel array, got %s", b)
	}
}
