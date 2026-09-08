package run

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
)

// TestIsCapacityFailure_SplitsBudgetFromPremises is the distinction the whole feature rests on.
//
// A quota wall or a wall clock is a fact about a billing relationship or a timer. A model-identity
// mismatch, a containment refusal or an unresolvable adapter says the run's PREMISES are broken, and
// no count over the survivors would mean anything. Only the first kind may degrade a panel.
func TestIsCapacityFailure_SplitsBudgetFromPremises(t *testing.T) {
	capacity := []clihint.Signal{clihint.QuotaExhausted, clihint.Timeout}
	for _, s := range capacity {
		if !IsCapacityFailure(&review.LaneFailure{Signal: string(s)}) {
			t.Errorf("%q is a provider running out, not a broken premise", s)
		}
	}
	// INTEGRITY, and the default. A signal this file has not considered must HALT: adding a failure
	// mode to clihint should never silently widen what a panel is willing to continue past.
	integrity := []clihint.Signal{
		clihint.FolderTrust, clihint.LoginRequired, clihint.ModelInvalid, clihint.UpdatePrompt,
	}
	for _, s := range integrity {
		if IsCapacityFailure(&review.LaneFailure{Signal: string(s)}) {
			t.Errorf("%q must halt: it says the run's premises are broken", s)
		}
	}
	if IsCapacityFailure(&review.LaneFailure{Signal: "some_signal_added_later"}) {
		t.Error("an unrecognized signal must default to HALT, never to degrade")
	}
	if IsCapacityFailure(nil) {
		t.Error("no failure is not a capacity failure")
	}
	if IsCapacityFailure(&review.LaneFailure{Signal: ""}) {
		t.Error("an unclassified failure must default to HALT")
	}
}

// TestPartialPanel_ReportsTheDenominatorItIsActuallyOver. The disclosure's whole job is to stop a
// reader measuring an agreement count against the panel they configured rather than the one that
// answered.
func TestPartialPanel_ReportsTheDenominatorItIsActuallyOver(t *testing.T) {
	p := &PartialPanel{
		Configured: 4, Answered: 2, Note: PartialPanelNote,
		Lost: []LostSeat{
			{SeatID: "reviewer-3", Adapter: "vendor-a", Model: "m3", Signal: string(clihint.QuotaExhausted)},
			{SeatID: "reviewer-4", Adapter: "vendor-b", Model: "m4", Signal: string(clihint.Timeout)},
		},
	}
	s := p.Summary()
	for _, want := range []string{"2 of 4", "reviewer-3", "quota_exhausted", "reviewer-4", "timeout"} {
		if !strings.Contains(s, want) {
			t.Errorf("the summary must contain %q: %s", want, s)
		}
	}
	// The note has to refuse BOTH conclusions: that the result is untrustworthy, and that it is as
	// strong as a full panel's.
	for _, phrase := range []string{"FEWER SEATS", "Nothing about the surviving findings changed", "re-run when capacity returns"} {
		if !strings.Contains(p.Note, phrase) {
			t.Errorf("the note must state %q:\n%s", phrase, p.Note)
		}
	}
}

// TestPartialPanel_NilIsSilent. A full panel produces no disclosure at all, so its PRESENCE is the
// signal — a consumer never has to compare two counts to notice a degraded run.
func TestPartialPanel_NilIsSilent(t *testing.T) {
	var p *PartialPanel
	if p.Summary() != "" {
		t.Errorf("a full panel must produce no summary, got %q", p.Summary())
	}
}
