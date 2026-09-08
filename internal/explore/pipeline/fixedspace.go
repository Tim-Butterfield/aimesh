package pipeline

// This file drives the FIXED-SPACE terminal path (design §0 F-B, §3 Compare + Forecast rows):
//
//	blind round 1  →  HOST AGGREGATION (matrix + agreement + Pareto, or pooled estimate + dispersion)
//	               →  governance CLAIMS over the blind round-1 baseline
//	               →  the terminal collate call, which contributes NARRATIVE ONLY
//	               →  the mode's terminal view over the host aggregate
//
// It is the shortest path in the pipeline, and the omissions are the point. There is no canonicalizer call,
// no confirmation round, no mediation and no partition, because the option set / estimation target was
// DECLARED by the user before the fan-out — so there is no emergent universe to resolve and nothing for the
// governance ceremony next door to adjudicate (§0 F-B). Adding those stages here would not make the result
// safer; it would invent a contestable judgment and then stage a process to settle it.
//
// The ordering does the enforcement. Every number is computed BEFORE the only model call left in the run,
// so "the collator did not produce this value" is a property of the sequence rather than of the prompt.

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// fixedSpacePath runs the terminal path for a mode that sets FixedSpace. It returns the Result by value
// (like Run) so callers keep the existing shape.
func (r *runner) fixedSpacePath(primary []schema.Envelope, payloadHash string) (Result, error) {
	res := r.res
	if len(res.Rounds) == 0 {
		return *res, fault.New(fault.Internal, "fixed-space aggregation: no recorded round-1 baseline")
	}
	// The blind baseline is built through the constructor that REFUSES anything but a blind round 1 (§0 F-A).
	// A fixed-space mode has exactly one round, so this can only succeed — which is the point of asking: the
	// anti-echo invariant must hold by the same mechanism here as everywhere else, not by the mode contract
	// remembering that it declared a single round.
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

	// -- HOST AGGREGATION (design §3): the whole result, computed from the recorded blind envelopes before
	// any further model call. A failure here is a failure of the DECLARED space (an empty option set, a
	// panel that produced no poolable estimate) — not a lost collator — so it halts without a degraded
	// artifact, whose reason codes would misdescribe what happened.
	emit(r.onEvent, "info", "aggregate_start", fmt.Sprintf("fixed space (%s): host-aggregating %d blind response(s) — no canonicalizer, no confirmation round, no partition", r.spec.Name, len(primary)), map[string]any{
		"mode": r.spec.Name, "primary": len(primary), "class": string(r.spec.ModeClass()),
	})
	view, aerr := r.spec.FixedSpace.Aggregate(in)
	if aerr != nil {
		herr := fault.Wrap(fault.Internal, "fixed-space host aggregation (raw explorer responses preserved)", aerr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return *res, herr
	}

	// -- Governance CLAIMS (§0 F-C/§4/§9): the host's claims go into the same append-only ledger every other
	// mode's do, so a fixed-space claim is auditable by exactly the same machinery — it simply carries
	// FixedSpaceNoPartition where an emergent-space claim carries a partition revision.
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

	// -- Terminal collation: NARRATIVE ONLY. The collator is shown the finished numbers and its output can
	// only ever reach the quarantined collatorNarrative namespace.
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
		// The collator became UNAVAILABLE after the fan-out. §1 names this as a degraded-artifact case, and for
		// a FIXED-space mode the degraded artifact is a REAL host register keyed on the mode's declared key
		// field — a genuine comparison, because the key universe was given to the explorers.
		herr := fault.Wrap(fault.Internal, "collator narrative call failed (the HOST-COMPUTED result is preserved in the degraded artifact)", serr)
		emit(r.onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		res.Degraded = r.degrade(schema.DegradedCollatorUnavailable, serr.Error())
		return *res, herr
	}
	// Formulation-free modes skip the formulate-phase collator identity check (there is no formulate call),
	// so the collator identity is classified HERE at its only call. It is RECORDED and never gates.
	status, _ := classify(r.collAdapter, sres, r.plan.Collator.Model, r.plan.Collator.Adapter)
	res.CollatorStatus = status
	res.CollatorCaveat = collatorIdentityCaveat(status)
	// SAME-IDENTITY invariant (§1): the collate call must resolve to the model pinned at pre-flight.
	if ierr := r.ids.observe("collator", sres.ActualModel); ierr != nil {
		emit(r.onEvent, "error", "halt", ierr.Error(), map[string]any{"haltClass": haltClassOf(ierr)})
		res.Degraded = r.degrade(schema.DegradedIdentityHalt, ierr.Error())
		return *res, ierr
	}

	// The narrative is parsed into the quarantined namespace. An UNUSABLE narrative is deliberately NOT a
	// halt: the collator contributed no value to this result, so a body it garbled degrades the explanation
	// and nothing else — the contract records a host note saying so. Throwing away a complete host-computed
	// matrix because the narrator stumbled would be the opposite of the honesty rule it is meant to serve.
	narrative, nerr := r.spec.FixedSpace.ParseNarrative(responseBody(sres), r.plan.Collator.Identity())
	if nerr != nil {
		return *res, fault.Wrap(fault.Internal, "fixed-space narrative", nerr)
	}
	// Fold the prose into the governance report so collatorNarrative lives in exactly ONE place (§0 F-C).
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
