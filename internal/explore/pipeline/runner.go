package pipeline

// This file holds the per-run state, the pre-flight identity probe, the explorer fan-out shared by every
// round, and the degraded-output builder.

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

// runner holds one run's inputs, its Result and its identity ledger across the pipeline stages.
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
	// canonicalizers are the canonicalizer identities resolved at pre-flight.
	canonicalizers []canonicalizerIdentity
}

// panelIdentities returns the plan's explorer identities in attribution order.
func panelIdentities(p roster.Plan) []schema.ExplorerIdentity {
	out := make([]schema.ExplorerIdentity, 0, len(p.Explorers))
	for _, e := range p.Explorers {
		out = append(out, e.Identity())
	}
	return out
}

// preflightRoles probes the collator and any canonicalizers before the fan-out, so an unusable role fails
// before the panel is paid for. It pins each role's resolved model in the identity ledger.
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
		r.res.CanonicalizerProvenance = choice.provenance
		// A dual pair sharing one model is allowed, but its agreement is weaker evidence, so it is warned
		// about and recorded.
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

// fanout sends payload to every explorer with bounded concurrency and sorts each outcome into an envelope, a
// drop or a halt. Every round uses it. phase labels the call for the adapter (for example PhaseExplore or
// PhaseBallot), and roundIndex prefixes drop reasons after round 1.
//
// A non-nil error means the run must stop because of an identity mismatch or model change. Envelopes and
// drops gathered so far are still returned.
func (r *runner) fanout(roundIndex int, phase string, payload schema.ExplorerTaskPayload, payloadHash string) ([]schema.Envelope, []Dropped, error) {
	outs := make([]explorerOutcome, len(r.plan.Explorers))
	var wg sync.WaitGroup
	// Concurrency follows Options.MaxParallel; unset runs the whole panel at once.
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
		// Each explorer must resolve to the same model in every round.
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

// degrade builds the degraded output for the mode's class from the blind round-1 envelopes: raw envelopes
// with an uncollated claim index for emergent-space modes, or a register keyed on FixedSpaceKeyField for
// fixed-space modes. It returns nil when there are no envelopes.
func (r *runner) degrade(reason schema.DegradedReason, detail string) *schema.DegradedOutput {
	envs := r.res.Envelopes
	if len(r.res.Rounds) > 0 {
		envs = r.res.Rounds[0].Envelopes()
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
