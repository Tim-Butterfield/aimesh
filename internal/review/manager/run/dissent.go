package run

// Dissent reporting labels each panel finding with how the seats that completed treated it, so a
// reader can see which findings the panel agreed on.
//
// A silent seat is not a seat that disagreed. Seats are blind, so a seat that did not report a finding
// may have disagreed, never reached the file, or stabilized first. `contested` therefore means only
// that fewer completed seats reported the finding than did not. It is a label, not a score: no count
// is adjusted and nothing is dropped, downgraded or reordered, so a serious single-seat finding is
// never buried.

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
)

// Consensus labels over the seats that completed; halted or unstarted seats count in neither direction.
const (
	// ConsensusUnanimous — every seat that completed reported this finding.
	ConsensusUnanimous = "unanimous"
	// ConsensusMajority — more of the completed seats reported it than did not.
	ConsensusMajority = "majority"
	// ConsensusContested — a minority reported it, or the seats split evenly; the finding stands
	// unchanged.
	ConsensusContested = "contested"
)

// DissentNote is the fixed sentence on the run-level record.
const DissentNote = "A silent seat is not a seat that disagreed. Seats are blind and each reports what IT found, so a seat that did not report a finding may have disagreed with it, may never have reached that file, or may have stopped when its own set stabilized — indistinguishable from here, and not guessed at. `contested` therefore means one checkable thing: fewer of the seats that ran reported this than did not. It is NOT evidence against the finding and does not make it less likely to be real; it marks the finding worth reading yourself rather than skimming. No count was adjusted and nothing was dropped, downgraded or reordered by any of this."

// DissentSummaryFor tallies the consensus labels already written onto the decisions. It returns nil when
// no finding had a panel behind it.
func DissentSummaryFor(adj *adjudication.Result) *review.DissentSummary {
	out := review.DissentSummary{Note: DissentNote}
	for i := range adj.Decisions {
		switch adj.Decisions[i].Consensus {
		case ConsensusUnanimous:
			out.Unanimous++
		case ConsensusMajority:
			out.Majority++
		case ConsensusContested:
			out.Contested++
		default:
			continue // no panel behind it, or a panel too small to describe
		}
		out.Panelled++
	}
	if out.Panelled == 0 {
		return nil
	}
	return &out
}

// consensusOf returns the label for a finding that supporting completed seats reported and dissenting
// completed seats did not. It returns "" for a non-panel finding or when fewer than two seats
// completed.
func consensusOf(supporting, dissenting int) string {
	if supporting == 0 || supporting+dissenting < 2 {
		return ""
	}
	switch {
	case dissenting == 0:
		return ConsensusUnanimous
	case supporting > dissenting:
		return ConsensusMajority
	default:
		return ConsensusContested
	}
}

// ConsensusPhrase renders a finding's panel provenance for a human, as "2 of 5 seats (contested)".
// Empty when there is no panel behind the finding, so a caller can drop the whole clause rather than
// print an empty parenthesis.
func ConsensusPhrase(supporting, dissenting int, consensus string) string {
	if consensus == "" {
		return ""
	}
	return fmt.Sprintf("%d of %d seats (%s)", supporting, supporting+dissenting, consensus)
}
