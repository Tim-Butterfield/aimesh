package run

// A capacity failure (quota, rate limit or wall-clock timeout) means one provider ran out; it says
// nothing about the seats that answered. Unlike an integrity failure (identity mismatch, containment
// refusal, wrong workspace), it does not halt the run: the seat is recorded as lost and the panel
// continues with a smaller denominator. Nothing is retried or substituted, and there is no minimum
// number of answering seats.

import (
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
)

// ReasonPanelDegraded is the reason code recorded on a seat the run continued without.
const ReasonPanelDegraded = "seat_capacity_exhausted"

// PartialPanelNote is the fixed sentence carried on the partial-panel disclosure.
const PartialPanelNote = "This review ran with FEWER SEATS than were configured: one or more providers ran out of capacity (quota, rate limit, or a wall-clock timeout) and the run continued rather than discarding the seats that had already answered. Nothing about the surviving findings changed — they came from the seats that ran, over a smaller denominator. Read every agreement count against `answered`, not against the panel you configured, and re-run when capacity returns if the full panel matters."

// capacitySignals are the clihint signals that count as capacity failures. The set is explicit, so a
// new signal halts the run until it is added here.
var capacitySignals = map[clihint.Signal]bool{
	clihint.QuotaExhausted: true,
	clihint.Timeout:        true,
}

// IsCapacityFailure reports whether a seat failed for capacity. It reads the signal laneFailure already
// classified, so the decision matches the audit record.
func IsCapacityFailure(f *review.LaneFailure) bool {
	if f == nil {
		return false
	}
	return capacitySignals[clihint.Signal(strings.TrimSpace(f.Signal))]
}

// PartialPanel discloses that fewer seats answered than were configured. It is present only when a
// seat was lost to capacity.
type PartialPanel struct {
	// Configured and Answered are the seat counts; agreement counts are over Answered.
	Configured int `json:"configured"`
	Answered   int `json:"answered"`
	// Lost names each seat the run continued without, with the signal that took it out.
	Lost []LostSeat `json:"lost"`
	// Note is PartialPanelNote.
	Note string `json:"note"`
}

// LostSeat is one seat a capacity failure removed from the panel.
type LostSeat struct {
	SeatID  string `json:"seatId"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	// Signal is the clihint classification (quota_exhausted | timeout).
	Signal string `json:"signal"`
	// Detail is the seat's own cause.
	Detail string `json:"detail,omitempty"`
}

// Summary renders the disclosure as one line.
func (p *PartialPanel) Summary() string {
	if p == nil {
		return ""
	}
	var who []string
	for _, l := range p.Lost {
		who = append(who, l.SeatID+" ("+l.Signal+")")
	}
	return "partial panel: " + itoa(p.Answered) + " of " + itoa(p.Configured) +
		" seat(s) answered; lost " + strings.Join(who, ", ")
}
