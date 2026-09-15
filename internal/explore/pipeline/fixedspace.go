package pipeline

// This file runs the fixed-space terminal path used by compare and forecast:
//
//	blind round 1  →  host aggregation
//	               →  governance claims over the blind round-1 baseline
//	               →  collator call for narrative only
//	               →  the mode's output view
//
// The user declares the space, so there is no canonicalization, confirmation or mediation. Every number is
// computed before the collator is called.

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// fixedSpacePath runs the terminal path for a mode that sets FixedSpace.
func (r *runner) fixedSpacePath(primary []schema.Envelope, payloadHash string) (Result, error) {
	res := r.res
	if len(res.Rounds) == 0 {
		return *res, fault.New(fault.Internal, "fixed-space aggregation: no recorded round-1 baseline")
	}
	baseline, berr := govern.NewBlindBaseline(res.Rounds[0])
	if berr != nil {
		herr := fault.Wrap(fault.Internal, "fixed-space baseline", berr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return *res, herr
	}
	in := mode.FixedSpaceInput{
		Raw: r.raw, Baseline: baseline, Primary: primary, Rounds: res.Rounds,
		Panel: res.Panel, FormulationHash: payloadHash,
	}

	// An aggregation failure reflects the declared space or the responses, not the collator, so it halts
	// without a degraded artifact.
	emit(r.onEvent, "info", "aggregate_start", fmt.Sprintf("fixed space (%s): host-aggregating %d blind response(s) — no canonicalizer, no confirmation round, no partition", r.spec.Name, len(primary)), map[string]any{
		"mode": r.spec.Name, "primary": len(primary), "class": string(r.spec.ModeClass()),
	})
	view, aerr := r.spec.FixedSpace.Aggregate(in)
	if aerr != nil {
		herr := fault.Wrap(fault.Internal, "fixed-space host aggregation (raw explorer responses preserved)", aerr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return *res, herr
	}

	ledger := &govern.ClaimLedger{}
	for _, c := range view.Claims {
		ledger.Emit(c)
	}
	report := govern.NewReport(res.Panel, ledger, nil)
	res.Governance = &report
	emit(r.onEvent, "info", "governance_claims", report.Summary(), map[string]any{
		"claims": len(report.Claims), "claimsHash": report.ClaimsHash, "rulesVersion": report.RulesVersion,
		"partition": govern.FixedSpaceNoPartition,
	})

	prompt, perr := r.spec.FixedSpace.CollatorPrompt(in, view)
	if perr != nil {
		return *res, fault.Wrap(fault.Internal, "fixed-space collator prompt", perr)
	}
	res.SynthesizePrompt = prompt
	work, cleanup, werr := isolatedWorkDir()
	if werr != nil {
		return *res, fault.Wrap(fault.Internal, "isolated work dir (synthesize)", werr)
	}
	sres, serr := r.collAdapter.Invoke(r.ctx, model.Call{
		Role: "collator", Phase: schema.PhaseSynthesize,
		Model: r.plan.Collator.Model, ModelArg: core.ModelArg(r.plan.Collator.Model), Effort: r.plan.Collator.Effort,
		WorkDir: work, Prompt: prompt,
	})
	cleanup()
	res.RawSynthesis = append([]byte(nil), responseBody(sres)...) // captured even on serr / parse failure
	if serr != nil {
		herr := fault.Wrap(fault.Internal, "collator narrative call failed (the HOST-COMPUTED result is preserved in the degraded artifact)", serr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		res.Degraded = r.degrade(schema.DegradedCollatorUnavailable, serr.Error())
		return *res, herr
	}
	// The collator's identity status is recorded but does not gate the run.
	status, _ := classify(r.collAdapter, sres, r.plan.Collator.Model, r.plan.Collator.Adapter)
	res.CollatorStatus = status
	res.CollatorCaveat = collatorIdentityCaveat(status)
	// The collator must resolve to the model pinned at pre-flight.
	if ierr := r.ids.observe("collator", sres.ActualModel); ierr != nil {
		emit(r.onEvent, "error", "halt", ierr.Error(), map[string]any{"haltClass": haltClassOf(ierr)})
		res.Degraded = r.degrade(schema.DegradedIdentityHalt, ierr.Error())
		return *res, ierr
	}

	// An unusable narrative does not halt; the contract records a host note instead.
	narrative, nerr := r.spec.FixedSpace.ParseNarrative(responseBody(sres), r.plan.Collator.Identity())
	if nerr != nil {
		return *res, fault.Wrap(fault.Internal, "fixed-space narrative", nerr)
	}
	folded := govern.NewReport(res.Panel, ledger, narrative)
	res.Governance = &folded

	out, oerr := r.spec.FixedSpace.Collate(in, view, narrative)
	if oerr != nil {
		herr := fault.Wrap(fault.Internal, "fixed-space terminal collation invalid (raw explorer responses preserved)", oerr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		res.Degraded = r.degrade(schema.DegradedCollatorUnavailable, "fixed-space collation invalid: "+oerr.Error())
		return *res, herr
	}
	res.Output = out
	emit(r.onEvent, "info", "synthesize_done", "fixed-space aggregation complete", map[string]any{
		"mode": r.spec.Name, "summary": out.Summary(),
	})
	return *res, nil
}
