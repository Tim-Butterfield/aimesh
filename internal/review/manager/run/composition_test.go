package run

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
)

func seat(id, adapter, model string) review.LaneResolution {
	return review.LaneResolution{Role: review.RoleReviewer, SeatID: id, Adapter: adapter, Model: model}
}

func ref(id, adapter, model string) review.SeatRef {
	return review.SeatRef{SeatID: id, Adapter: adapter, Model: model}
}

// TestComposeAgreement_NeverTouchesTheCount is the invariant this whole feature is defined by. An
// adjusted agreement figure would need a model-family taxonomy the host cannot verify; what it can do
// is say how many DISTINCT models are behind the number it already computed.
func TestComposeAgreement_NeverTouchesTheCount(t *testing.T) {
	adj := &adjudication.Result{
		Findings: []review.Finding{{ID: "1"}, {ID: "2"}},
		Decisions: []review.Decision{
			{
				AgreementCount:  3,
				SupportingSeats: []review.SeatRef{ref("reviewer", "a", "m1"), ref("reviewer-2", "b", "m1"), ref("reviewer-3", "c", "m2")},
			},
			{
				AgreementCount:  2,
				SupportingSeats: []review.SeatRef{ref("reviewer", "a", "m1"), ref("reviewer-3", "c", "m2")},
			},
		},
	}
	comp := composeAgreement([]review.LaneResolution{
		seat("reviewer", "a", "m1"), seat("reviewer-2", "b", "m1"), seat("reviewer-3", "c", "m2"),
	}, adj)

	if adj.Decisions[0].AgreementCount != 3 || adj.Decisions[1].AgreementCount != 2 {
		t.Fatalf("an agreement count was ADJUSTED: %d, %d", adj.Decisions[0].AgreementCount, adj.Decisions[1].AgreementCount)
	}
	// Three seats, two models: shared. Two seats, two models: distinct.
	if adj.Decisions[0].DistinctModels != 2 || adj.Decisions[0].AgreementIndependence != IndependenceSharedModel {
		t.Errorf("decision 0 = %d models / %q", adj.Decisions[0].DistinctModels, adj.Decisions[0].AgreementIndependence)
	}
	if adj.Decisions[1].DistinctModels != 2 || adj.Decisions[1].AgreementIndependence != IndependenceDistinctModels {
		t.Errorf("decision 1 = %d models / %q", adj.Decisions[1].DistinctModels, adj.Decisions[1].AgreementIndependence)
	}
	if comp == nil || comp.DistinctModels != 2 || comp.Independence != IndependenceSharedModel {
		t.Fatalf("panel record = %+v", comp)
	}
	if got := SharedModels(comp.Seats); len(got) != 1 || got[0] != "m1" {
		t.Errorf("the doubled model must be NAMED, got %v", got)
	}
}

// TestComposeAgreement_SharedModelAcrossDifferentAdaptersStillCounts. Two vendor CLIs wrapping one
// model are two seats and one vantage — the adapter differing is exactly the case that makes this
// invisible without the label.
func TestComposeAgreement_SharedModelAcrossDifferentAdaptersStillCounts(t *testing.T) {
	adj := &adjudication.Result{
		Findings: []review.Finding{{ID: "1"}},
		Decisions: []review.Decision{{
			AgreementCount:  2,
			SupportingSeats: []review.SeatRef{ref("reviewer", "vendor-a-cli", "m1"), ref("reviewer-2", "vendor-b-cli", "m1")},
		}},
	}
	composeAgreement([]review.LaneResolution{
		seat("reviewer", "vendor-a-cli", "m1"), seat("reviewer-2", "vendor-b-cli", "m1"),
	}, adj)
	if adj.Decisions[0].AgreementIndependence != IndependenceSharedModel {
		t.Fatalf("two adapters running one model must read as shared_model, got %q", adj.Decisions[0].AgreementIndependence)
	}
	if adj.Decisions[0].DistinctModels != 1 {
		t.Errorf("distinctModels = %d, want 1", adj.Decisions[0].DistinctModels)
	}
}

// TestComposeAgreement_NoSupportingSeatsIsNotAWeakAgreement. A finding from an evidence hook, the
// cross-check or the verifier makes no agreement claim at all. Labelling it `distinct_models: 1`
// would read as a weak agreement rather than as no agreement — a different and worse statement.
func TestComposeAgreement_NoSupportingSeatsIsNotAWeakAgreement(t *testing.T) {
	adj := &adjudication.Result{
		Findings:  []review.Finding{{ID: "1"}},
		Decisions: []review.Decision{{AgreementCount: 0}},
	}
	composeAgreement([]review.LaneResolution{seat("reviewer", "a", "m1")}, adj)
	if adj.Decisions[0].AgreementIndependence != "" || adj.Decisions[0].DistinctModels != 0 {
		t.Errorf("a finding with no supporting seats must carry no independence claim: %+v", adj.Decisions[0])
	}
}

// TestComposeAgreement_NoPanelNoRecord. A run with no blind panel has no agreement to qualify, and a
// record for it would suggest a panel that never existed.
func TestComposeAgreement_NoPanelNoRecord(t *testing.T) {
	if got := composeAgreement(nil, &adjudication.Result{}); got != nil {
		t.Fatalf("no panel must produce no composition record, got %+v", got)
	}
}

// TestCompositionNote_StatesBothMisreadings. They run in opposite directions — shared_model invites
// discarding a real agreement, distinct_models invites treating one as proof — and the sentence has
// to refuse both, in the artifact, where every surface picks it up rather than phrasing its own.
func TestCompositionNote_StatesBothMisreadings(t *testing.T) {
	comp := composeAgreement([]review.LaneResolution{seat("reviewer", "a", "m1")}, &adjudication.Result{})
	if comp == nil {
		t.Fatal("a one-seat panel is still a panel")
	}
	for _, phrase := range []string{
		"is NOT adjusted",           // the count is untouched
		"does not make the finding", // shared_model is not a refutation
		"are NOT asserted",          // no vendor/family taxonomy is claimed
	} {
		if !strings.Contains(comp.Note, phrase) {
			t.Errorf("the note must contain %q:\n%s", phrase, comp.Note)
		}
	}
}

// TestComposeAgreement_RunsOnARealReview — the wiring. Every assertion above calls composeAgreement
// directly and would keep passing if the manager never called it.
func TestComposeAgreement_RunsOnARealReview(t *testing.T) {
	m := rejectingManager(t)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "prior"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Composition == nil {
		t.Fatal("a review with a blind panel must carry the composition record — the pass is not wired in")
	}
	if len(out.Composition.Seats) == 0 || out.Composition.DistinctModels == 0 {
		t.Errorf("composition = %+v", out.Composition)
	}
	if out.Composition.Note != CompositionNote {
		t.Error("the record must carry the fixed note")
	}
}
