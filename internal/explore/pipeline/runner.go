package pipeline

// This file holds the per-run state carrier + the two stages every round shares: the identity PRE-FLIGHT
// that runs before the fan-out, and the fan-out itself (used by the blind round 1 and by every later round,
// so a later round cannot accidentally acquire different drop/halt/identity semantics). It also builds the
// PER MODE-CLASS degraded terminal artifact (design §1).

import (
	"context"
	"fmt"
	"sync"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// runner carries one run's immutable inputs plus its mutable Result and identity ledger through the governed
// stages (pre-flight → blind round 1 → canonicalize+confirm → mediate → later rounds → collate). It exists
// because those stages all need the same inputs; threading nine parameters through each of them made every
// helper signature a paragraph and invited them to drift apart.
type runner struct {
	ctx         context.Context
	reg         Registry
	plan        roster.Plan
	raw         schema.RawTask
	spec        mode.ModeSpec
	opts        Options
	onEvent     func(audit.EventLine)
	ids         *identityLedger
	collAdapter model.Adapter
	res         *Result
	// canonicalizers are the resolved canonicalizer identities for this run (one, or two on the dual path),
	// fixed at pre-flight so the same identities are used for the proposal calls.
	canonicalizers []canonicalizerIdentity
}

// panelIdentities returns the plan's explorer identities in stable attribution order — the membership the
// panel is FROZEN on (design §1).
func panelIdentities(p roster.Plan) []schema.ExplorerIdentity {
	out := make([]schema.ExplorerIdentity, 0, len(p.Explorers))
	for _, e := range p.Explorers {
		out = append(out, e.Identity())
	}
	return out
}

// preflightRoles probes every GOVERNED role BEFORE the explorer fan-out (design §1): the collator, and each
// canonicalizer a canonicalizing mode will use. A role that cannot be invoked fails here, costing one cheap
// call per role instead of a whole panel. It also PINS each role's resolved model in the identity ledger, so
// the same-identity invariant has a baseline to compare later calls against.
func (r *runner) preflightRoles() error {
	roles := []struct {
		role    string
		adapter model.Adapter
		id      schema.ExplorerIdentity
	}{{"collator", r.collAdapter, r.plan.Collator.Identity()}}

	if r.spec.Canonicalizing != nil {
		choice, err := canonicalizerIdentities(r.plan, r.spec.Canonicalization.Dual, r.spec.Name, r.opts)
		if err != nil {
			return err
		}
		cids := choice.ids
		r.canonicalizers = cids
		// Record HOW the canonicalizers were chosen before any of them is called. It is a governance fact:
		// "explicit" says a human named these identities, "derived" says the host picked them from the panel.
		r.res.CanonicalizerProvenance = choice.provenance
		// And WHAT THE CHOICE BOUGHT. A dual pair on one model is allowed — a panel is configured
		// deliberately, and refusing it would override a choice on an assumption about what was meant — but
		// it makes every merge-agreement on this run weaker evidence, so it is warned once and recorded
		// where a reader of the count will find it.
		r.res.CanonicalizerIndependence = choice.independence
		if choice.independence == IndependenceSharedModel {
			emit(r.onEvent, "warn", "canonicalizer_shared_model", fmt.Sprintf(
				"both canonicalizers run model %q (%s and %s): a merge held by agreement between them is weaker evidence than one held across two different models — their errors are correlated, because the priors are the same",
				cids[0].explorer.Model, cids[0].explorer.Adapter, cids[1].explorer.Adapter), map[string]any{
				"independence": choice.independence, "model": cids[0].explorer.Model,
				"adapterA": cids[0].explorer.Adapter, "adapterB": cids[1].explorer.Adapter,
			})
		}
		for _, c := range cids {
			a, ok := r.reg[c.explorer.Adapter]
			if !ok {
				return fault.New(fault.Config, fmt.Sprintf("canonicalizer adapter %q not registered", c.explorer.Adapter))
			}
			roles = append(roles, struct {
				role    string
				adapter model.Adapter
				id      schema.ExplorerIdentity
			}{c.role, a, c.explorer.Identity()})
		}
	}

	for _, role := range roles {
		rec, err := preflight(r.ctx, role.adapter, role.role, role.id)
		r.res.Preflight = append(r.res.Preflight, rec)
		if err != nil {
			return err
		}
		if ierr := r.ids.observe(role.role, rec.ResolvedModel); ierr != nil {
			return ierr
		}
	}
	emit(r.onEvent, "info", "preflight_done", fmt.Sprintf("pre-flight reached %d governed role(s) BEFORE the fan-out", len(roles)), map[string]any{
		"roles": len(roles),
	})
	return nil
}

// fanout dispatches ONE explorer round: every explorer receives the byte-identical payload, concurrency is
// bounded, and each outcome becomes an envelope, a drop, or a halt. It is shared by the blind round 1 and by
// every later round so their semantics cannot diverge; roundIndex only affects the drop reason prefix and the
// same-identity role label (an explorer is pinned across the rounds it participates in). `phase` is the
// adapter-facing phase label — PhaseExplore for a research round, PhaseBallot for a ballot round (design §4):
// soliciting a preference is a governance act, and a recording must be able to tell it from a research call.
//
// A returned halt means the run must stop: a proven explorer identity mismatch (§6.2) or a mid-exploration
// model swap (§1). Envelopes/drops gathered before the halt are still returned, so the audit record is
// complete.
func (r *runner) fanout(roundIndex int, phase string, payload schema.ExplorerTaskPayload, payloadHash string) ([]schema.Envelope, []Dropped, error) {
	outs := make([]explorerOutcome, len(r.plan.Explorers))
	var wg sync.WaitGroup
	// Bound concurrency to what the CALLER asked for: each explorer is a heavy model-CLI subprocess
	// under a long per-call timeout, and only the caller knows what their machine can host at once.
	// Unset runs the whole panel in parallel.
	sem := make(chan struct{}, explorerFanout(len(r.plan.Explorers), r.opts.MaxParallel))
	for i, ex := range r.plan.Explorers {
		wg.Add(1)
		go func(i int, ex roster.Explorer) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			emit(r.onEvent, "info", "explorer_dispatch", "dispatching explorer "+identityString(ex.Identity()), map[string]any{
				"adapter": ex.Adapter, "model": ex.Model, "effort": ex.Effort, "order": i,
				"round": roundIndex, "phase": phase,
			})
			outs[i] = runExplorer(r.ctx, r.reg, ex, phase, payload, payloadHash, i)
			if d := outs[i].drop; d != nil {
				emit(r.onEvent, "warn", "explorer_drop", "dropped explorer "+identityString(d.Explorer)+": "+d.Reason, map[string]any{
					"adapter": d.Explorer.Adapter, "model": d.Explorer.Model, "reason": d.Reason,
				})
			} else if outs[i].halt == nil {
				emit(r.onEvent, "info", "explorer_verify", "verified explorer "+identityString(ex.Identity())+" ("+string(outs[i].env.IdentityStatus)+")", map[string]any{
					"adapter": ex.Adapter, "model": ex.Model, "identityStatus": string(outs[i].env.IdentityStatus),
				})
			}
		}(i, ex)
	}
	wg.Wait()

	var envs []schema.Envelope
	var drops []Dropped
	var halt error
	for i, o := range outs {
		// SAME-IDENTITY invariant per explorer, across the rounds it participates in (§1).
		if ierr := r.ids.observe("explorer["+identityString(r.plan.Explorers[i].Identity())+"]", o.resolvedModel); ierr != nil && halt == nil {
			halt = ierr
		}
		switch {
		case o.drop != nil:
			d := *o.drop
			if roundIndex > 1 {
				d.Reason = fmt.Sprintf("round %d: %s", roundIndex, d.Reason)
			}
			drops = append(drops, d)
		case o.halt != nil:
			if halt == nil {
				halt = o.halt
			}
		default:
			envs = append(envs, o.env)
		}
	}
	return envs, drops, halt
}

// degrade builds the PER MODE-CLASS degraded terminal artifact from the BLIND round-1 envelopes (design §1).
// For an EMERGENT-space mode that is the raw attributed envelopes + a mechanical typed-claim index labeled
// `uncollated — no entity resolution performed`; for a FIXED-space mode it is a real host register keyed on
// the mode's declared key field. It returns nil when there are no envelopes to carry (nothing was paid for
// yet, so there is nothing to salvage).
func (r *runner) degrade(reason schema.DegradedReason, detail string) *schema.DegradedOutput {
	envs := r.res.Envelopes
	if len(r.res.Rounds) > 0 {
		envs = r.res.Rounds[0].Envelopes() // the immutable blind round-1 view, whenever it exists
	}
	if len(envs) == 0 {
		return nil
	}
	out := schema.Degrade(r.spec.ModeClass(), r.spec.Name, reason, detail, envs, r.spec.FixedSpaceKeyField)
	emit(r.onEvent, "warn", "degraded", out.Summary(), map[string]any{
		"class": string(out.Class), "reason": string(reason), "envelopes": len(envs),
	})
	return &out
}
