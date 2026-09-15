package pipeline

// This file holds the host steps around a ballot round:
//
//	freezeDecision   before the ballot: fix and hash the candidates, presented order, criteria, method,
//	                 shortlist size and panel policy; the hash is rendered into the ballot prompt
//	tallyBallots     after the ballot: parse each ballot, reject entries outside the confirmed universe, and
//	                 compute the ranking with govern.Tally
//
// govern.Tally refuses inputs that were not frozen or whose hash no longer matches.

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// freezeDecision hashes the decision inputs and records them on the Result before the ballot round. Only
// modes with a Ballot contract call it.
func (r *runner) freezeDecision(confirmed canon.Result, pres canon.Presentation) error {
	criteria := r.spec.Ballot.Criteria(r.raw)
	frozen, err := govern.FreezeDecision(
		r.res.Panel, confirmed, pres, criteria,
		r.spec.Ballot.Method(), r.spec.Ballot.ShortlistSize(len(pres.Order)))
	if err != nil {
		return fault.Wrap(fault.Config, "freeze decision inputs", err)
	}
	// Record the frozen inputs now so a run that halts later still shows them.
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

// tallyBallots parses the recorded ballot round and computes the host tally.
//
// An unusable ballot (unparseable, or naming a candidate outside the confirmed universe) counts as no ballot
// rather than halting the run or silently editing the vote. The dual denominators show the absence.
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
		// The voter's rationale is kept as narrative, not on the ballot record.
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

// lastRound returns the last recorded round, which is the ballot round in a ballot-bearing mode.
func lastRound(rounds []round.Round) (round.Round, bool) {
	if len(rounds) == 0 {
		return round.Round{}, false
	}
	return rounds[len(rounds)-1], true
}
