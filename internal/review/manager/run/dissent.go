package run

// DISSENT AS PRODUCT — saying what the seats that RAN did with each finding, on the surfaces a
// person actually reads.
//
// THE GOVERNING INVARIANT, and it is the same one composition.go holds:
//
//	Nothing here changes a finding. No count is adjusted, nothing is dropped, downgraded, reordered
//	or gated. `contested` is a READING PROMPT, never a verdict.
//
// WHAT WAS ALREADY TRUE. The panel has recorded `dissentingSeats` — the seats that completed and did
// not report a finding — since the provenance ledger existed, and `agreementIndependence` now says
// what the agreement was worth. Both rode the run record. Neither was ever SHOWN to a human: they
// reach `--json` and MCP and stop there, so the reader with the best judgement and the strongest
// context — a person at a terminal, looking at their own tree — was the one reader who never saw
// that four seats ran and three of them stayed silent on the finding in front of them. That is
// backwards from every other disclosure in this tool, where exposure runs OPPOSITE to gating.
//
// THE THING THIS FILE MUST NOT LET ANYONE BELIEVE, stated first because every use of the word
// "dissent" pulls the other way:
//
//	A SILENT SEAT IS NOT A SEAT THAT DISAGREED.
//
// Seats are blind. Each is asked what it finds, not to vote on a list, so a seat that did not report
// a finding may have disagreed with it — or never reached that file, or spent its round elsewhere, or
// stopped when its own set stabilized. Those are indistinguishable from here, and the host will not
// guess between them. So `contested` says ONE checkable thing: fewer of the seats that ran reported
// this than did not. It is not evidence against the finding, and a contested finding is not less
// likely to be real than a unanimous one — it is the one worth reading yourself rather than skimming.
//
// WHY A LABEL AND NOT A SCORE. The tempting version weights a finding by its support ratio and sorts
// on it. That needs silence to mean disagreement, which is exactly what it does not mean, and it would
// bury a single-seat finding that happens to be the serious one — the failure mode a panel exists to
// prevent. The same reasoning that keeps `agreementCount` unadjusted keeps this a label.
//
// WHY THE ADVERSARIAL REFUTATION STAGE IS NOT HERE. The other half of the original finding was a
// second pass asking a seat to argue each surviving finding is wrong. It is deferred, not refused, on
// this project's standing bar: it spends a second round of real calls on the findings that survived,
// and its distinctive claim — that "argue this is wrong" surfaces something a second BLIND SEAT does
// not — is plausible and unmeasured. Making dissent visible is the precondition for ever measuring it
// honestly: until a reader can see what the panel already disagreed about, there is nothing to
// compare a rebuttal against.

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
)

// Consensus verdicts. STABLE MACHINE CODES, over the seats that COMPLETED — a halted or never-started
// seat is in neither the supporting nor the dissenting list, so it cannot move any of these.
const (
	// ConsensusUnanimous — every seat that completed reported this finding.
	ConsensusUnanimous = "unanimous"
	// ConsensusMajority — more of the completed seats reported it than did not.
	ConsensusMajority = "majority"
	// ConsensusContested — a minority reported it, or the seats split evenly. The finding stands
	// exactly as it would otherwise: this is a prompt to read it, not a vote against it.
	ConsensusContested = "contested"
)

// DissentNote is the fixed sentence carried on the run-level record. It leads with what silence is
// NOT, because that is the misreading the vocabulary invites and the one that would do damage.
const DissentNote = "A silent seat is not a seat that disagreed. Seats are blind and each reports what IT found, so a seat that did not report a finding may have disagreed with it, may never have reached that file, or may have stopped when its own set stabilized — indistinguishable from here, and not guessed at. `contested` therefore means one checkable thing: fewer of the seats that ran reported this than did not. It is NOT evidence against the finding and does not make it less likely to be real; it marks the finding worth reading yourself rather than skimming. No count was adjusted and nothing was dropped, downgraded or reordered by any of this."

// DissentSummaryFor tallies the per-finding consensus labels for the run-level record.
//
// It is a TALLY over labels already written onto the decisions, not a second source of truth: a
// surface that recounted the decisions itself could reach a different number than the run record it
// is displaying.
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
		// Nothing had a describable panel behind it. A record of three zeroes would suggest a panel
		// that agreed on nothing, when in fact there was no panel to agree.
		return nil
	}
	return &out
}

// consensusOf is the verdict for a finding `supporting` completed seats reported and `dissenting`
// completed seats did not.
//
// It is empty — no label at all — in the two cases where there is nothing honest to say:
//
//   - NO SUPPORTING SEATS. Not a panel finding (an evidence hook, the cross-check, the verifier).
//   - FEWER THAN TWO COMPLETED SEATS. One seat cannot be unanimous with itself, and labelling it so
//     would dress the thinnest possible evidence in the strongest available word.
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
