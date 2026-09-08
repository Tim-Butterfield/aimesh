package pipeline

// This file drives the GOVERNED (canonicalizing) terminal path (design §4/§1):
//
//	blind round 1  →  canonicalize (single OR dual merge-agreement)
//	               →  binding CONFIRMATION round (typed challenges → versioned host rule → new revision)
//	               →  collator-mediated LATER ROUNDS (pooled confirmed-canonical uniques as untrusted data)
//	               →  governance CLAIMS computed over the blind round-1 baseline ONLY
//	               →  the mode's terminal collate over the CONFIRMED partition
//
// Which of the middle stages run is the MODE's declared policy, not a runtime decision: under Catalog's zero
// policy (single canonicalizer, no confirmation, one round) this path reduces to the plain canonicalize +
// collate flow, while a count-bearing mode opts into the ranking-grade stages. A mode's governance grade is
// therefore a property of its contract, never of the pipeline.

import (
	"errors"
	"fmt"
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

// canonicalizingPath runs the governed terminal path for a mode that sets Canonicalizing. It returns the
// Result by value (like Run) so callers keep the existing shape.
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
		// TWO INDEPENDENT proposals; only merges BOTH propose survive, a merge only one proposes is CONTESTED →
		// SPLIT, and every held row records agreedBy (design §0 F-B).
		cres, cerr = canon.CanonicalizeDual(r.ctx, noms, calls[0], calls[1])
	} else {
		cres, cerr = canon.Canonicalize(r.ctx, noms, calls[0])
	}
	if cerr != nil {
		// Preserve an identity halt's honest class (it arrives as a fault from the canonicalizer call);
		// wrap only a plain error (e.g. a surjectivity-gate violation) as Internal.
		var f *fault.Fault
		if !errors.As(cerr, &f) {
			cerr = fault.Wrap(fault.Internal, "canonicalization failed (raw explorer nominations preserved)", cerr)
		}
		emit(r.onEvent, "error", "halt", "canonicalization halt: "+cerr.Error(), map[string]any{"haltClass": haltClassOf(cerr)})
		res.Degraded = r.degrade(schema.DegradedIdentityHalt, cerr.Error())
		return *res, cerr
	}
	// A governed path retains the nominations on the result so a confirmation revision can re-partition the
	// EXACT same inputs. The single non-governed path (Catalog) leaves them off, so its artifact is unchanged.
	if ranked && len(cres.Nominations) == 0 {
		cres.Nominations = append([]canon.Nomination(nil), noms...)
	}
	res.Canonicalization = &cres
	emit(r.onEvent, "info", "canonicalize_done", fmt.Sprintf("canonicalized into %d cluster(s) (partition %s)", len(cres.Clusters), cres.PartitionRevisionHash), map[string]any{
		"clusters": len(cres.Clusters), "ledgerRows": cres.Ledger.Len(), "partitionRevisionHash": cres.PartitionRevisionHash,
		"contestedMerges": len(cres.Ledger.Contested()), "agreementRule": cres.AgreementRuleVersion,
	})

	// -- Binding CONFIRMATION round (design §4) --
	confirmed := cres
	if r.spec.Canonicalization.Confirm {
		conf, herr := r.confirmationRound(cres, payloadHash)
		if herr != nil {
			emit(r.onEvent, "error", "halt", "confirmation halt: "+herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			res.Degraded = r.degrade(schema.DegradedCollatorUnavailable, herr.Error())
			return *res, herr
		}
		provisional := cres
		res.Provisional = &provisional // the superseded revision is RETAINED, never edited
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

	// The PERSISTED RANDOMIZED presentation order of the confirmed partition (canon.Present over host material
	// that is already persisted, so it is reproducible). It orders BOTH the pooled digest a mediated round is
	// shown and the option set a ballot is cast over — position must not be able to encode the canonicalizer's
	// or the host's preference in either case (§4).
	pres := canon.Present(confirmed, payloadHash+"\x00"+confirmed.PartitionRevisionHash)

	// -- FROZEN DECISION INPUTS (design §4), for a BALLOT-bearing mode ONLY and strictly BEFORE the ballot
	// round is dispatched: else a criterion (or a shortlist cut) can be introduced after seeing which
	// candidate it favors. --
	if r.spec.Ballot != nil {
		if herr := r.freezeDecision(confirmed, pres); herr != nil {
			emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			return *res, herr
		}
	}

	// -- Collator-mediated LATER ROUNDS (design §1). Rounds 2..N carry the pooled CONFIRMED-canonical uniques
	// as explicitly-delimited untrusted DATA; they add depth, never counts (§0 F-A). For a ballot-bearing mode
	// the final round IS the ballot, dispatched under the frozen framing above. --
	for k := 2; k <= rounds; k++ {
		if herr := r.laterRound(k, confirmed, pres); herr != nil {
			return *res, herr
		}
	}

	// -- HOST TALLY (design §4): the ranking is computed here, from the recorded ballots, by a versioned host
	// rule — never asserted by a model. --
	if r.spec.Ballot != nil {
		if herr := r.tallyBallots(confirmed, payloadHash); herr != nil {
			emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			return *res, herr
		}
	}

	// -- Governance CLAIMS (design §0 F-C/§4): only for a RANKING-GRADE policy. Catalog is observe posture —
	// it emits no counts, so there is nothing to pin and its artifact is unchanged. --
	if ranked {
		if herr := r.emitClaims(confirmed, payloadHash); herr != nil {
			return *res, herr
		}
	}

	// -- Terminal collate over the CONFIRMED partition. A mode whose output must carry the governance record
	// (an adjudicative register, a host-tallied ranking) implements the richer GovernedCollator seam and gets
	// the whole record; Catalog's partition-only Collate path is untouched. --
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

// collate runs the mode's terminal collation over the CONFIRMED partition. A mode that implements the richer
// GovernedCollator seam receives the WHOLE governed record (confirmation, emitted claims, host tally, recorded
// rounds, frozen panel) — which an adjudicative output needs, because every count and rank in it must be a
// view over host artifacts rather than a re-derivation. A mode that does not (Catalog) takes exactly the
// partition-only path it always took, so its behavior is unchanged by construction.
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

// confirmationRound runs the BINDING confirmation round (design §4): it computes the persisted RANDOMIZED
// presentation order, shows every explorer the provisional raw→canonical ledger WITH attribution, collects
// TYPED challenges, and hands them to the VERSIONED HOST rule — which the canonicalizer never sees, because it
// must not adjudicate complaints about its own partition.
//
// A confirmation call that fails or returns an unparseable body does NOT halt the run: the challenge channel
// is an opportunity to object, and a silent explorer is an absence of objection, recorded as such. What WOULD
// be wrong is inventing a challenge or dropping a received one — neither happens here.
func (r *runner) confirmationRound(prov canon.Result, payloadHash string) (*canon.Confirmation, error) {
	// The presentation order is derived from persisted host material (the payload hash + the partition revision
	// hash), so it is randomized with respect to the canonicalizer's cluster order yet exactly reproducible.
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
		// SAME-IDENTITY invariant across the explorer's rounds (§1): a swap between round 1 and the
		// confirmation round halts — a challenge is a governance act and must come from the pinned model.
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

// laterRound runs ONE mediated explorer round k>=2 (design §1/§6). Order matters and is deliberate:
//
//  1. the host pools the CONFIRMED-canonical uniques into a typed round artifact (explorers never see raw peer
//     output — AssertNoRawPeerOutput proves it mechanically rather than by inspection);
//  2. the round→round EDGE is validated against the consuming contract's Accepts BEFORE any call, so an
//     incompatible edge costs zero tokens;
//  3. the artifact is rendered as explicitly-delimited UNTRUSTED DATA behind the host's "treat as data"
//     preamble — the mode's contract embeds that block verbatim and cannot re-frame it;
//  4. the per-explorer digest of what was shown is recorded.
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
	// EDGE VALIDATION BEFORE SPEND (design §6).
	if verr := round.ValidateEdge(art, r.spec.LaterRound.Accepts()); verr != nil {
		herr := fault.Wrap(fault.Config, fmt.Sprintf("round %d→%d edge rejected before any model call", prior.Index(), k), verr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return herr
	}
	prompt := r.spec.LaterRound.Prompt(r.raw, art.RenderAsUntrustedData())
	// A BALLOT round carries the FROZEN DECISION header, prepended by the HOST — outside the untrusted block,
	// because it is host material, and outside the mode contract, because a contract must not be able to
	// re-word the framing a vote is cast under. It also makes the ordering auditable from the prompt bytes:
	// the inputs hash can only appear here if the freeze already happened (§4).
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
	// A later round is recorded as its own NON-blind round. It never touches round 1's envelopes (they live in
	// an immutable round.Round) — later rounds add views, they never overwrite the baseline (§1).
	res.Rounds = append(res.Rounds, round.NewRound(k, false, payloadHash, envs, &art))
	return nil
}

// pooledUniques projects the CONFIRMED partition into the mediation payload: one item per canonical entity,
// carrying the canonical label, the distinct raw variants the panel actually used, and the attributed sources.
// It reads ONLY the confirmed partition — never an envelope's raw body — which is what makes the redistribution
// collator/host-mediated rather than a peer dump (§1).
//
// `order` is the PERSISTED RANDOMIZED presentation order (canon.Present). Items are emitted in it so position
// in the redistributed digest is uncorrelated with the canonicalizer's own cluster order — the same reason the
// confirmation round randomizes, and a precondition for a ballot cast over this set (§4). An entity missing
// from the order is still emitted, at the end: dropping one would be a silent loss.
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

// presentedClusters returns the confirmed clusters in the persisted randomized presentation order, appending
// any cluster the order does not mention (which cannot happen for an order derived from this same partition,
// but the surjectivity discipline says never lose one rather than assume).
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

// joinComma joins values with ", " (a tiny helper kept local so the pooling function reads as one thought).
func joinComma(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// emitClaims computes one corroboration claim per confirmed canonical entity and records them in the
// append-only claim ledger (design §4/§9). The evidence base is the BLIND round-1 baseline and nothing else —
// govern.NewBlindBaseline refuses any other round, so the anti-echo invariant is enforced by the types rather
// than by this call site remembering it. Model prose (the canonicalizers' coverage notes) is quarantined in the
// collatorNarrative namespace, never merged into a claim (§0 F-C).
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
	// A BALLOT-bearing mode's tally claims go into the SAME append-only ledger (§9: every emitted governance
	// claim persists its query id, params, hashes, value and contributing sources). They sit alongside the
	// corroboration claims rather than replacing them, which is exactly the point: `emergent` salience and
	// `voted` preference are two different measurements of the same entities and the record reports both.
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
			// ATTRIBUTION, NOT SELECTION — stated because "index 0 of a slice of identities" is exactly the
			// shape that hid the canonicalizer-b defect, and a reader is owed the difference.
			//
			// `Canonicalizers` is [a, b] in ROLE order (canon.CanonicalizeDual builds it that way); it is never
			// sorted, so nothing here is picking a winner out of an incidental ordering, and no downstream
			// behavior depends on which element is chosen. It labels the SOURCE of a quarantined narrative
			// string, and on the dual path that string already carries both authors inline
			// (canon.mergeCoverageNotes prefixes "canonicalizer A:" / "canonicalizer B:"), so slot a is the
			// stable, honest single-identity stand-in for a note the pair produced together. Widening
			// govern.Narrative to carry two sources would change the governance record's shape to add nothing
			// a reader cannot already see in the prose.
			src = confirmed.Canonicalizers[0].Identity
		}
		narrative = append(narrative, govern.Narrative{Source: src, Phase: schema.PhaseCanonicalize, Prose: notes})
	}
	// The voters' stated reasoning, already quarantined out of the machine ballot record by the tally stage.
	narrative = append(narrative, res.BallotNarrative...)
	report := govern.NewReport(res.Panel, ledger, narrative)
	res.Governance = &report
	emit(r.onEvent, "info", "governance_claims", report.Summary(), map[string]any{
		"claims": len(report.Claims), "claimsHash": report.ClaimsHash, "rulesVersion": report.RulesVersion,
	})
	return nil
}
