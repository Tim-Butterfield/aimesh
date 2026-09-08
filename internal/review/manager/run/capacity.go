package run

// CAPACITY IS NOT INTEGRITY — a seat that ran out of quota does not invalidate the seats that
// answered.
//
// THE DISTINCTION, which is the whole of this file:
//
//	An INTEGRITY failure says the run's PREMISES are broken — a proven model-identity mismatch, a
//	containment refusal, a workspace that is not the tree we thought. Nothing the panel produced can
//	be trusted, so the run halts.
//
//	A CAPACITY failure says one PROVIDER ran out — a quota wall, a rate limit, a wall-clock timeout.
//	It is a fact about a billing relationship or a clock. It says nothing whatever about whether the
//	seats that did answer were sound.
//
// Halting on the second destroys what was already paid for. With several billing mechanisms in play
// at once — an enterprise quota, a flat subscription, purchased tokens, standalone API keys — one
// seat hitting its wall late in a panel is ordinary, and throwing away three completed seats because
// the fourth was out of credit is the tool wasting the user's money on the user's behalf.
//
// WHAT THIS DOES NOT DO. It does not retry, does not substitute another model, does not top anything
// up, and does not decide the review is fine. It reports a SMALLER PANEL, names which seats were lost
// and why, and lets the denominator speak. The findings that survive came from the seats that ran;
// that is exactly as true as it was before, over a smaller number.
//
// WHY THE FLOOR IS NOT A THRESHOLD. An earlier shape of this refused to degrade below N answering
// seats. There is no N that can be justified from anything: the honest denominator is however many
// answered, reported plainly, and the panel echo already accounts for every requested seat by name.
// A threshold would be a number we made up standing where a fact belongs.

import (
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
)

// ReasonPanelDegraded is the STABLE MACHINE code recorded on a seat the run continued without.
const ReasonPanelDegraded = "seat_capacity_exhausted"

// PartialPanelNote is the fixed sentence carried on the disclosure. It states the two things a reader
// must not conclude, because they run in opposite directions: that the result is untrustworthy, and
// that it is as strong as a full panel's.
const PartialPanelNote = "This review ran with FEWER SEATS than were configured: one or more providers ran out of capacity (quota, rate limit, or a wall-clock timeout) and the run continued rather than discarding the seats that had already answered. Nothing about the surviving findings changed — they came from the seats that ran, over a smaller denominator. Read every agreement count against `answered`, not against the panel you configured, and re-run when capacity returns if the full panel matters."

// capacitySignals are the clihint classifications that mean "a provider ran out", as opposed to "the
// run's premises are broken".
//
// The set is deliberately SMALL and explicit rather than a default-allow. A signal this file has not
// considered must halt: adding a new failure mode to clihint should not silently widen what a panel
// is willing to continue past. Everything here is a fact about a budget or a clock — never about the
// code, the identity of a model, or the tree being reviewed.
var capacitySignals = map[clihint.Signal]bool{
	clihint.QuotaExhausted: true,
	clihint.Timeout:        true,
}

// IsCapacityFailure reports whether a seat's failure was a provider running out rather than a broken
// premise.
//
// It reads the signal the run ALREADY classified (laneFailure does it once, at the failure, with the
// prompt echo subtracted) rather than re-inspecting stderr here. Re-deriving it would create a second
// classifier that can disagree with the one in the audit record — and this decides whether a run
// halts, so the two disagreeing would be the difference between a delivered review and a discarded
// one.
func IsCapacityFailure(f *review.LaneFailure) bool {
	if f == nil {
		return false
	}
	return capacitySignals[clihint.Signal(strings.TrimSpace(f.Signal))]
}

// PartialPanel is the disclosure that this run convened fewer seats than were configured.
//
// It is present whenever a seat was lost to capacity and ABSENT otherwise, so its presence is the
// signal — a consumer never has to compare two counts to notice. It rides the outcome for the same
// reason the identity caveats do: the alternative is that a quieter panel is indistinguishable from
// a full one, and a reviewer's silence about a file it was never shown reads as approval.
type PartialPanel struct {
	// Configured / Answered are the denominators. Answered is what every agreement count in this
	// result is actually over.
	Configured int `json:"configured"`
	Answered   int `json:"answered"`
	// Lost names each seat the run continued without, with the signal that took it out.
	Lost []LostSeat `json:"lost"`
	// Note is the fixed sentence stating what must not be concluded in either direction.
	Note string `json:"note"`
}

// LostSeat is one seat a capacity failure removed from the panel.
type LostSeat struct {
	SeatID  string `json:"seatId"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	// Signal is the clihint classification (quota_exhausted | timeout).
	Signal string `json:"signal"`
	// Detail is the seat's own actionable cause, so an operator fixing this does not have to go
	// find which provider it was.
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
