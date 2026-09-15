package run

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
)

// TestIsCapacityFailure_SplitsBudgetFromPremises checks that only capacity failures (a quota or a
// timeout) may degrade a panel. An identity mismatch, a containment refusal or an unresolvable adapter
// means the run's premises are broken, so no count over the remaining seats would mean anything.
func TestIsCapacityFailure_SplitsBudgetFromPremises(t *testing.T) {
	capacity := []clihint.Signal{clihint.QuotaExhausted, clihint.Timeout}
	for _, s := range capacity {
		if !IsCapacityFailure(&review.LaneFailure{Signal: string(s)}) {
			t.Errorf("%q is a provider running out, not a broken premise", s)
		}
	}
	// Integrity failures, and the default: an unconsidered signal halts, so a new clihint signal never
	// widens what a panel continues past.
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
	// The note has to refuse both conclusions: that the result is untrustworthy, and that it is as
	// strong as a full panel's.
	for _, phrase := range []string{"FEWER SEATS", "Nothing about the surviving findings changed", "re-run when capacity returns"} {
		if !strings.Contains(p.Note, phrase) {
			t.Errorf("the note must state %q:\n%s", phrase, p.Note)
		}
	}
}

// TestPartialPanel_NilIsSilent checks that a full panel produces no disclosure, so the disclosure's
// presence alone signals a degraded run.
func TestPartialPanel_NilIsSilent(t *testing.T) {
	var p *PartialPanel
	if p.Summary() != "" {
		t.Errorf("a full panel must produce no summary, got %q", p.Summary())
	}
}
