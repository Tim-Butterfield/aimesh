package pipeline

// This file runs the canonicalizing terminal path:
//
//	blind round 1  →  canonicalize (single or dual)
//	               →  confirmation round (typed challenges → host rule → new revision)
//	               →  mediated later rounds (confirmed canonical entities as untrusted data)
//	               →  governance claims over the blind round-1 baseline
//	               →  the mode's terminal collate over the confirmed partition
//
// The mode's contract decides which middle stages run. With a single canonicalizer, no confirmation and
// one round, the path is canonicalize then collate.

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// canonicalizingPath runs the terminal path for a mode that sets Canonicalizing.
func (r *runner) canonicalizingPath(primary []schema.Envelope, payloadHash string, rounds int) (Result, error) {
	res := r.res
	noms := r.spec.Canonicalizing.Nominations(primary)
	ranked := r.spec.Canonicalization.Dual || r.spec.Canonicalization.Confirm
	emit(r.onEvent, "info", "canonicalize_start", fmt.Sprintf("canonicalizing %d nomination(s) from %d explorer(s)", len(noms), len(primary)), map[string]any{
		"nominations": len(noms), "primary": len(primary),
		"dual": r.spec.Canonicalization.Dual, "confirm": r.spec.Canonicalization.Confirm,
	})

	calls := make([]*canonicalizerCall, 0, len(r.canonicalizers))
	for i, c := range r.canonicalizers {
		a, ok := r.reg[c.explorer.Adapter]
		if !ok {
			return *res, fault.New(fault.Config, fmt.Sprintf("canonicalizer adapter %q not registered", c.explorer.Adapter))
		}
		calls = append(calls, &canonicalizerCall{
			contract: r.spec.Canonicalizing, adapter: a, role: c.role, identity: c.explorer,
			ids: r.ids, res: res, primary: i == 0,
		})
	}

	var cres canon.Result
	var cerr error
	if r.spec.Canonicalization.Dual {
		if len(calls) < 2 {
			return *res, fault.New(fault.Internal, "dual canonicalization requires two canonicalizer calls")
		}
		// Only merges both canonicalizers propose are held; a merge only one proposes is contested and split.
		cres, cerr = canon.CanonicalizeDual(r.ctx, noms, calls[0], calls[1])
	} else {
		cres, cerr = canon.Canonicalize(r.ctx, noms, calls[0])
	}
	if cerr != nil {
		// Keep a fault's own class (such as an identity halt); wrap a plain error as Internal.
		if _, ok := errors.AsType[*fault.Fault](cerr); !ok {
			cerr = fault.Wrap(fault.Internal, "canonicalization failed (raw explorer nominations preserved)", cerr)
		}
		emit(r.onEvent, "error", "halt", "canonicalization halt: "+cerr.Error(), map[string]any{"haltClass": haltClassOf(cerr)})
		res.Degraded = r.degrade(schema.DegradedIdentityHalt, cerr.Error())
		return *res, cerr
	}
	// A ranking-grade path keeps the nominations so a confirmation revision can re-partition the same inputs.
	if ranked && len(cres.Nominations) == 0 {
		cres.Nominations = append([]canon.Nomination(nil), noms...)
	}
	res.Canonicalization = &cres
	emit(r.onEvent, "info", "canonicalize_done", fmt.Sprintf("canonicalized into %d cluster(s) (partition %s)", len(cres.Clusters), cres.PartitionRevisionHash), map[string]any{
		"clusters": len(cres.Clusters), "ledgerRows": cres.Ledger.Len(), "partitionRevisionHash": cres.PartitionRevisionHash,
		"contestedMerges": len(cres.Ledger.Contested()), "agreementRule": cres.AgreementRuleVersion,
	})

	confirmed := cres
	if r.spec.Canonicalization.Confirm {
		conf, herr := r.confirmationRound(cres, payloadHash)
		if herr != nil {
			emit(r.onEvent, "error", "halt", "confirmation halt: "+herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			res.Degraded = r.degrade(schema.DegradedCollatorUnavailable, herr.Error())
			return *res, herr
		}
		provisional := cres
		res.Provisional = &provisional
		res.Confirmation = conf
		confirmed = conf.Revision
		res.Canonicalization = &confirmed
		emit(r.onEvent, "info", "confirm_done", fmt.Sprintf("confirmation round: %d challenge(s), %d resolution(s) → revision %s (%d contested mapping(s))",
			len(conf.Challenges), len(conf.Resolutions), confirmed.PartitionRevisionHash, len(conf.Contested)), map[string]any{
			"challenges": len(conf.Challenges), "resolutions": len(conf.Resolutions),
			"contested": len(conf.Contested), "ruleVersion": conf.RuleVersion,
			"priorRevisionHash": conf.PriorRevisionHash, "partitionRevisionHash": confirmed.PartitionRevisionHash,
		})
	}

	// The presentation order is seeded from persisted hashes, so it is reproducible. It orders both the
	// mediated digest and the ballot options, so position cannot encode anyone's preference.
	pres := canon.Present(confirmed, payloadHash+"\x00"+confirmed.PartitionRevisionHash)

	// Ballot inputs are frozen before the ballot round, so no criterion can be added after seeing which
	// candidate it favors.
	if r.spec.Ballot != nil {
		if herr := r.freezeDecision(confirmed, pres); herr != nil {
			emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			return *res, herr
		}
	}

	// Later rounds add depth, never counts. For a ballot-bearing mode the final round is the ballot.
	for k := 2; k <= rounds; k++ {
		if herr := r.laterRound(k, confirmed, pres); herr != nil {
			return *res, herr
		}
	}

	// The ranking is tallied by the host from the recorded ballots.
	if r.spec.Ballot != nil {
		if herr := r.tallyBallots(confirmed, payloadHash); herr != nil {
			emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			return *res, herr
		}
	}

	// Governance claims are emitted only for a ranking-grade policy.
	if ranked {
		if herr := r.emitClaims(confirmed, payloadHash); herr != nil {
			return *res, herr
		}
	}

	out, oerr := r.collate(confirmed)
	if oerr != nil {
		herr := fault.Wrap(fault.Internal, "terminal collate output invalid (merge-ledger preserved)", oerr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		res.Degraded = r.degrade(schema.DegradedCollatorUnavailable, "terminal collate output invalid: "+oerr.Error())
		return *res, herr
	}
	res.Output = out
	emit(r.onEvent, "info", "synthesize_done", "canonicalization complete", map[string]any{
		"mode": r.spec.Name, "summary": out.Summary(),
	})
	return *res, nil
}

// collate runs the mode's terminal collation over the confirmed partition. A mode implementing
// mode.GovernedCollator receives the whole governance record, so its counts and ranks are views over host
// artifacts; any other mode receives only the partition.
func (r *runner) collate(confirmed canon.Result) (mode.ModeOutput, error) {
	if gc, ok := r.spec.Canonicalizing.(mode.GovernedCollator); ok {
		return gc.CollateGoverned(mode.CollateInput{
			Raw:          r.raw,
			Partition:    confirmed,
			Confirmation: r.res.Confirmation,
			Governance:   r.res.Governance,
			Decision:     r.res.Decision,
			Rounds:       r.res.Rounds,
			Panel:        r.res.Panel,
		})
	}
	return r.spec.Canonicalizing.Collate(confirmed)
}

// confirmationRound shows every explorer the provisional partition with attribution, collects typed
// challenges, and resolves them with the versioned host rule. The canonicalizer does not judge challenges
// to its own partition. A failed or unparseable reply is recorded as no objection and does not halt.
func (r *runner) confirmationRound(prov canon.Result, payloadHash string) (*canon.Confirmation, error) {
	pres := canon.Present(prov, payloadHash+"\x00"+prov.PartitionRevisionHash)
	prompt, perr := canon.ConfirmationPrompt(prov, pres)
	if perr != nil {
		return nil, fault.Wrap(fault.Internal, "confirmation prompt", perr)
	}
	r.res.ConfirmationPrompt = prompt
	known := map[string]bool{}
	for _, c := range prov.Clusters {
		known[c.CanonicalID] = true
	}
	emit(r.onEvent, "info", "confirm_start", fmt.Sprintf("confirmation round: showing %d provisional entity(ies) to %d explorer(s) in a persisted randomized order",
		len(pres.Order), len(r.plan.Explorers)), map[string]any{
		"entities": len(pres.Order), "seed": pres.Seed, "ruleVersion": pres.RuleVersion,
	})

	type reply struct {
		challenges []canon.Challenge
		resolved   string
		note       string
	}
	replies := make([]reply, len(r.plan.Explorers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, explorerFanout(len(r.plan.Explorers), r.opts.MaxParallel))
	for i, ex := range r.plan.Explorers {
		wg.Add(1)
		go func(i int, ex roster.Explorer) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a, ok := r.reg[ex.Adapter]
			if !ok {
				replies[i].note = "adapter not registered"
				return
			}
			work, cleanup, werr := isolatedWorkDir()
			if werr != nil {
				replies[i].note = "isolated work dir: " + werr.Error()
				return
			}
			cres, ierr := a.Invoke(r.ctx, model.Call{
				Role: "explorer", Phase: schema.PhaseConfirm,
				Model: ex.Model, ModelArg: core.ModelArg(ex.Model), Effort: ex.Effort,
				WorkDir: work, Prompt: prompt,
			})
			cleanup()
			replies[i].resolved = cres.ActualModel
			if ierr != nil {
				replies[i].note = "confirmation call failed: " + ierr.Error()
				return
			}
			chs, cperr := canon.ParseChallenges(responseBody(cres), ex.Identity(), known)
			if cperr != nil {
				replies[i].note = "confirmation response unusable: " + cperr.Error()
				return
			}
			replies[i].challenges = chs
		}(i, ex)
	}
	wg.Wait()

	var challenges []canon.Challenge
	for i, rep := range replies {
		if ierr := r.ids.observe("explorer["+identityString(r.plan.Explorers[i].Identity())+"]", rep.resolved); ierr != nil {
			return nil, ierr
		}
		if rep.note != "" {
			emit(r.onEvent, "warn", "confirm_no_reply", "no usable confirmation reply from "+identityString(r.plan.Explorers[i].Identity())+": "+rep.note, map[string]any{
				"adapter": r.plan.Explorers[i].Adapter, "model": r.plan.Explorers[i].Model, "reason": rep.note,
			})
			continue
		}
		challenges = append(challenges, rep.challenges...)
	}

	conf, cerr := canon.Confirm(prov, pres, challenges)
	if cerr != nil {
		return nil, fault.Wrap(fault.Internal, "confirmation host rule", cerr)
	}
	return &conf, nil
}

// laterRound runs mediated explorer round k (k >= 2):
//
//  1. pool the confirmed canonical entities into a typed round artifact, checked to contain no raw peer
//     output;
//  2. validate the round edge against the contract before any model call;
//  3. render the artifact as delimited untrusted data inside the mode's prompt;
//  4. record the digest shown to each explorer, then fan out.
func (r *runner) laterRound(k int, confirmed canon.Result, pres canon.Presentation) error {
	res := r.res
	prior := res.Rounds[len(res.Rounds)-1]
	items, refs := pooledUniques(confirmed, pres.Order)
	art, aerr := round.NewArtifact(
		round.Discriminant{Mode: r.spec.Name, RoundIndex: prior.Index(), Kind: round.KindCanonicalUniques},
		prior.ID(), round.TrustHostDerived, refs, items, round.DefaultCaps())
	if aerr != nil {
		return fault.Wrap(fault.Internal, "build round artifact", aerr)
	}
	if gerr := round.AssertNoRawPeerOutput(art, res.Rounds[0].Envelopes()); gerr != nil {
		herr := fault.Wrap(fault.Internal, "mediation guard", gerr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return herr
	}
	if verr := round.ValidateEdge(art, r.spec.LaterRound.Accepts()); verr != nil {
		herr := fault.Wrap(fault.Config, fmt.Sprintf("round %d→%d edge rejected before any model call", prior.Index(), k), verr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return herr
	}
	prompt := r.spec.LaterRound.Prompt(r.raw, art.RenderAsUntrustedData())
	// A ballot round starts with the frozen decision header, added by the host outside both the untrusted
	// block and the mode's prompt, so a contract cannot reword the framing a vote is cast under.
	phase := schema.PhaseExplore
	if res.Decision != nil {
		prompt = res.Decision.Frozen.Render() + "\n" + prompt
		phase = schema.PhaseBallot
	}
	payload := schema.ExplorerTaskPayload{FinalPrompt: prompt, ExpandedSchema: r.spec.LaterRound.ExplorerSchema()}
	payloadHash, _ := payload.Hash()
	res.LaterRoundPrompts = append(res.LaterRoundPrompts, prompt)

	med := round.Mediation{RoundIndex: k, Artifact: art}
	for _, ex := range r.plan.Explorers {
		med.ShownTo = append(med.ShownTo, round.Shown{Explorer: ex.Identity(), RoundIndex: k, ContentHash: art.ContentHash})
	}
	res.Mediations = append(res.Mediations, med)
	emit(r.onEvent, "info", "mediate", fmt.Sprintf("round %d: redistributing %d confirmed-canonical unique(s) as untrusted data (digest %s)", k, len(art.Payload.Items), art.ContentHash[:12]), map[string]any{
		"round": k, "items": len(art.Payload.Items), "contentHash": art.ContentHash,
		"kind": string(art.Discriminant.Kind), "schemaVersion": art.SchemaVersion,
	})

	envs, drops, halt := r.fanout(k, phase, payload, payloadHash)
	res.Dropped = append(res.Dropped, drops...)
	if halt != nil {
		emit(r.onEvent, "error", "halt", "explorer identity halt: "+halt.Error(), map[string]any{"haltClass": haltClassOf(halt)})
		res.Degraded = r.degrade(schema.DegradedIdentityHalt, halt.Error())
		return halt
	}
	res.Rounds = append(res.Rounds, round.NewRound(k, false, payloadHash, envs, &art))
	return nil
}

// pooledUniques builds the mediation payload from the confirmed partition: one item per canonical entity
// with its label, the distinct raw variants and the attributed sources, in presentation order. It never
// reads an envelope's raw body.
func pooledUniques(confirmed canon.Result, order []string) (items []round.Item, sourceRefs []string) {
	seenRef := map[string]bool{}
	for _, c := range presentedClusters(confirmed, order) {
		var variants []string
		seenRaw := map[string]bool{}
		var sources []schema.ExplorerIdentity
		seenSrc := map[schema.ExplorerIdentity]bool{}
		for _, m := range c.Members {
			if !seenRaw[m.RawNomination] {
				seenRaw[m.RawNomination] = true
				variants = append(variants, m.RawNomination)
			}
			if !seenSrc[m.SourceExplorer] {
				seenSrc[m.SourceExplorer] = true
				sources = append(sources, m.SourceExplorer)
			}
			if !seenRef[m.EnvelopeRef] {
				seenRef[m.EnvelopeRef] = true
				sourceRefs = append(sourceRefs, m.EnvelopeRef)
			}
		}
		text := c.Name
		if len(variants) > 0 {
			text = c.Name + " (nominated as: " + joinComma(variants) + ")"
		}
		items = append(items, round.Item{Ref: c.CanonicalID, Text: text, Attribution: sources})
	}
	return items, sourceRefs
}

// presentedClusters returns the confirmed clusters in presentation order, followed by any cluster the
// order omits.
func presentedClusters(confirmed canon.Result, order []string) []canon.Cluster {
	byID := map[string]canon.Cluster{}
	for _, c := range confirmed.Clusters {
		byID[c.CanonicalID] = c
	}
	out := make([]canon.Cluster, 0, len(confirmed.Clusters))
	seen := map[string]bool{}
	for _, id := range order {
		if c, ok := byID[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, c)
		}
	}
	for _, c := range confirmed.Clusters {
		if !seen[c.CanonicalID] {
			out = append(out, c)
		}
	}
	return out
}

// joinComma joins v with ", ".
func joinComma(v []string) string {
	var out strings.Builder
	for i, s := range v {
		if i > 0 {
			out.WriteString(", ")
		}
		out.WriteString(s)
	}
	return out.String()
}

// emitClaims records one corroboration claim per confirmed canonical entity in the claim ledger. Claims
// are computed only over the blind first round (govern.NewBlindBaseline refuses any other), and model
// prose is kept in the narrative namespace rather than in a claim.
func (r *runner) emitClaims(confirmed canon.Result, formulationHash string) error {
	res := r.res
	if len(res.Rounds) == 0 {
		return fault.New(fault.Internal, "governance claims: no recorded round-1 baseline")
	}
	baseline, berr := govern.NewBlindBaseline(res.Rounds[0])
	if berr != nil {
		return fault.Wrap(fault.Internal, "governance claims", berr)
	}
	var contested []canon.ContestedMapping
	if res.Confirmation != nil {
		contested = res.Confirmation.Contested
	}
	ledger := &govern.ClaimLedger{}
	for _, c := range confirmed.Clusters {
		claim, cerr := govern.Corroboration(govern.CountInput{
			Baseline: baseline, Partition: confirmed, Contested: contested,
			CanonicalID: c.CanonicalID, Panel: res.Panel, FormulationHash: formulationHash,
		})
		if cerr != nil {
			return fault.Wrap(fault.Internal, "corroboration claim", cerr)
		}
		ledger.Emit(claim)
	}
	// Ballot tally claims join the same ledger: emergent salience and voted preference are reported side by
	// side.
	if res.Decision != nil {
		for _, e := range res.Decision.Entries {
			if e.Claim != nil {
				ledger.Emit(*e.Claim)
			}
		}
	}
	var narrative []govern.Narrative
	if notes := confirmed.CoverageNotes; notes != "" {
		src := schema.ExplorerIdentity{}
		if len(confirmed.Canonicalizers) > 0 {
			// Slot a labels the note's source; on the dual path the merged note already names both authors
			// inline. Nothing is selected here.
			src = confirmed.Canonicalizers[0].Identity
		}
		narrative = append(narrative, govern.Narrative{Source: src, Phase: schema.PhaseCanonicalize, Prose: notes})
	}
	narrative = append(narrative, res.BallotNarrative...)
	report := govern.NewReport(res.Panel, ledger, narrative)
	res.Governance = &report
	emit(r.onEvent, "info", "governance_claims", report.Summary(), map[string]any{
		"claims": len(report.Claims), "claimsHash": report.ClaimsHash, "rulesVersion": report.RulesVersion,
	})
	return nil
}
