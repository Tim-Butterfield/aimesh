package pipeline

// This file is the BALLOT STAGE (design §3 Shortlist row / §4) — the two host steps that bracket the
// ballot round a ballot-bearing mode declares:
//
//	freezeDecision   BEFORE the ballot round is dispatched: fix + hash the candidate-universe revision, the
//	                 presented order, the criterion set (each with its origin + aggregation method), the tally
//	                 method, the shortlist cut, and the quorum / tie / missing-response policy already frozen
//	                 with the panel. The hash is then rendered into the ballot prompt by the host, so the
//	                 framing a voter voted under is recoverable from the persisted prompt bytes.
//	tallyBallots     AFTER it: parse each recorded ballot, reject an entry outside the confirmed universe, and
//	                 hand the ballots to govern.Tally. The ranking is arithmetic; no model produces one.
//
// The ordering is the whole governance property, and it is structural rather than remembered: the ballot
// prompt cannot be built without a frozen record (the pipeline prepends its rendering), and govern.Tally
// REFUSES a decision whose inputs were not frozen or whose hash no longer matches.

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// freezeDecision fixes + hashes the decision inputs and records them on the Result BEFORE the ballot round is
// dispatched (design §4). It is called only for a mode that declares a Ballot contract; every other mode
// leaves Result.Decision nil and never acquires a decision by accident.
func (r *runner) freezeDecision(confirmed canon.Result, pres canon.Presentation) error {
	criteria := r.spec.Ballot.Criteria(r.raw)
	frozen, err := govern.FreezeDecision(
		r.res.Panel, confirmed, pres, criteria,
		r.spec.Ballot.Method(), r.spec.Ballot.ShortlistSize(len(pres.Order)))
	if err != nil {
		return fault.Wrap(fault.Config, "freeze decision inputs", err)
	}
	// Recorded as a decision carrying ONLY its frozen inputs: the entries/ballots arrive at the tally. A
	// reader of a halted run can therefore still see exactly what framing was about to be put to the panel.
	r.res.Decision = &govern.Decision{Frozen: frozen, RuleVersion: govern.DecisionRuleVersion}
	emit(r.onEvent, "info", "decision_frozen", fmt.Sprintf(
		"decision inputs FROZEN + hashed BEFORE the ballot: %d criterion(s), method %s, %d candidate(s), shortlist %d, quorum %d, tie rule %s (inputs %s)",
		len(criteria), frozen.Inputs.Method, len(pres.Order), frozen.Inputs.ShortlistSize,
		frozen.Inputs.Quorum, frozen.Inputs.TieRule, frozen.InputsHash[:12]), map[string]any{
		"inputsHash": frozen.InputsHash, "method": string(frozen.Inputs.Method),
		"criteria": len(criteria), "universe": len(pres.Order),
		"shortlistSize": frozen.Inputs.ShortlistSize, "policyHash": frozen.Inputs.PolicyHash,
		"universeRevisionHash": frozen.Inputs.UniverseRevisionHash,
	})
	return nil
}

// tallyBallots parses the recorded ballot round and computes the HOST tally (design §4). It runs after the
// ballot round, so it can only ever read ballots that were cast under the already-frozen framing.
//
// An explorer whose ballot is unusable — no ballot round envelope, an unparseable body, or an entry naming a
// candidate outside the confirmed universe — is recorded as having cast no ballot and is excluded from the
// tally. That is deliberately the same posture the confirmation round takes toward a silent explorer: the
// alternative (halting the run, or quietly deleting the offending entry) would either throw away a paid-for
// panel or change a voter's expressed preference without saying so. The dual denominators keep the absence
// visible either way.
func (r *runner) tallyBallots(confirmed canon.Result, formulationHash string) error {
	res := r.res
	if res.Decision == nil {
		return fault.New(fault.Internal, "ballot tally: the decision inputs were never frozen")
	}
	ballotRound, ok := lastRound(res.Rounds)
	if !ok {
		return fault.New(fault.Internal, "ballot tally: no ballot round was recorded")
	}
	universe := map[string]bool{}
	for _, id := range res.Decision.Frozen.Inputs.Presentation.Order {
		universe[id] = true
	}
	var ballots []govern.Ballot
	var narrative []govern.Narrative
	for _, env := range ballotRound.Envelopes() {
		if schema.IsAbstention(env.Response) {
			emit(r.onEvent, "warn", "ballot_abstain", "explorer "+identityString(env.Identity)+" deliberately abstained from the ballot", map[string]any{
				"adapter": env.Identity.Adapter, "model": env.Identity.Model,
			})
			continue
		}
		b, perr := r.spec.Ballot.ParseBallot(env, universe)
		if perr != nil {
			emit(r.onEvent, "warn", "ballot_unusable", "no usable ballot from "+identityString(env.Identity)+": "+perr.Error(), map[string]any{
				"adapter": env.Identity.Adapter, "model": env.Identity.Model, "reason": perr.Error(),
			})
			continue
		}
		b.EnvelopeRef = schema.EnvelopeRef(ballotRound.Index(), env.Order)
		// The voter's stated reasoning is MODEL PROSE: it is moved into the collatorNarrative namespace here and
		// never travels on the machine record of the ballot (§0 F-C).
		if b.Rationale != "" {
			narrative = append(narrative, govern.Narrative{Source: env.Identity, Phase: schema.PhaseBallot, Prose: b.Rationale})
		}
		ballots = append(ballots, b)
	}

	baseline, berr := govern.NewBlindBaseline(res.Rounds[0])
	if berr != nil {
		return fault.Wrap(fault.Internal, "ballot tally", berr)
	}
	var contested []canon.ContestedMapping
	if res.Confirmation != nil {
		contested = res.Confirmation.Contested
	}
	dec, terr := govern.Tally(govern.TallyInput{
		Frozen: res.Decision.Frozen, Ballots: ballots, Partition: confirmed, Contested: contested,
		Baseline: baseline, Panel: res.Panel, FormulationHash: formulationHash,
		BallotRoundID: ballotRound.ID(),
	})
	if terr != nil {
		return fault.Wrap(fault.Internal, "ballot tally", terr)
	}
	res.Decision = &dec
	res.BallotNarrative = narrative
	emit(r.onEvent, "info", "ballot_tallied", dec.Rendering, map[string]any{
		"ballots": dec.Cast, "ranked": len(dec.Shortlisted()), "rejected": len(dec.Rejected()),
		"quorumMet": dec.QuorumMet, "ruleVersion": dec.RuleVersion, "inputsHash": dec.Frozen.InputsHash,
	})
	return nil
}

// lastRound returns the final recorded round (the ballot round for a ballot-bearing mode, whose ballot is
// always the last declared round).
func lastRound(rounds []round.Round) (round.Round, bool) {
	if len(rounds) == 0 {
		return round.Round{}, false
	}
	return rounds[len(rounds)-1], true
}
