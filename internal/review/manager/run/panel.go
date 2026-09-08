package run

// This file is the BLIND PRIMARY PANEL: the variable-count first stage of a review.
//
// A panel is N ordered seats (1..review.MaxReviewerSeats), each an independent
// (adapter, model, effort) vantage. The count is variable BY CONSTRUCTION — nothing here, in
// the config schema, or in any surface encodes a particular N, and a panel of one is exactly
// the historical single-reviewer run (same call ids, same artifacts, same prompts).
//
// THE THREE RULES THIS FILE EXISTS TO ENFORCE
//
//  1. BLINDNESS. Between its own stabilization rounds a seat sees ONLY its own prior findings.
//     It never sees another seat's output and never sees host decisions. The enforcement point
//     is structural, not documentary: `reported` is a seat-LOCAL slice built inside runSeat, and
//     the `decisions` argument to runSemanticPass is nil for every seat — there is no variable
//     in scope that could carry a peer's findings into a seat prompt. Host adjudication does not
//     begin until every seat has completed or halted.
//
//  2. NO SILENT DEGRADATION. N seats requested is N seats executed, or the run fails with a
//     clear reason. Nothing is clamped, dropped, or "best-effort skipped": a seat that cannot
//     resolve fails config validation before spend; a seat that fails at runtime HALTS the run;
//     and the executed-roster echo carries one entry per requested seat, including the ones that
//     halted or never started.
//
//  3. HOST-COMPUTED COUNTS. Agreement is arithmetic the host does over which seats reported a
//     fingerprint. No model is asked to count, and the model-facing schema has no field a count
//     could ride in — the provenance ledger lives on review.Decision, which is never parsed
//     from model output.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// defaultPanelRoundCeiling is the RUN-LEVEL ceiling on total blind-primary rounds when the
// config does not set one. It is what stops N seats from multiplying the per-seat round cap into
// an unbounded budget: the default allowance is maxInner rounds per seat, but never more than
// this in total. It is deliberately >= review.MaxReviewerSeats, so a full-size panel can
// always run every seat's first round.
const defaultPanelRoundCeiling = 64

// seatFanout returns how many seats invoke their model CLI concurrently: every seat at once unless
// the CALLER asked for fewer, and never below 1 or above n.
//
// There is no built-in ceiling. Each seat is a real provider CLI under a long per-call timeout, so
// how many may run at once is a fact about the operator's machine — RAM and process count for a
// cloud CLI, loaded weights for a local model — and about their provider rate limits. aimesh knows
// none of those, so any constant it picked would be a guess binding people it knows nothing about.
// maxParallel is therefore stated per invocation by whoever is making it, and 0 means "all of them".
//
// It bounds PARALLELISM only: every requested seat still runs. (exploremesh's explorerFanout is the
// same rule for explorers.)
func seatFanout(n, maxParallel int) int {
	switch {
	case n < 1:
		return 1
	case maxParallel > 0 && maxParallel < n:
		return maxParallel
	default:
		return n
	}
}

// panelBudget is the RUN-LEVEL round budget, shared by every seat.
//
// Round 1 of every seat is GUARANTEED and never drawn from the pool (the budget is validated to
// be at least one round per seat), so the budget can only ever shorten stabilization — it can
// never silently remove a seat. Later rounds draw from the shared pool, so a chatty seat cannot
// starve the run and N seats cannot multiply the spend without bound.
type panelBudget struct {
	mu    sync.Mutex
	extra int // rounds available BEYOND each seat's guaranteed first round
	total int // the configured/derived ceiling, for reporting
}

func (b *panelBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.extra <= 0 {
		return false
	}
	b.extra--
	return true
}

// newPanelBudget derives the run-level ceiling. An explicit `review.maxPanelRounds` wins; the
// default is maxInner rounds per seat, capped at defaultPanelRoundCeiling.
//
// FAIL-CLOSED: a configured ceiling below the seat count is a CONFIG ERROR before any spend,
// because it would mean a requested seat could not run even once — silent degradation by budget
// is still silent degradation.
func newPanelBudget(configured *int, maxInner, seats int) (*panelBudget, error) {
	total := maxInner * seats
	if total > defaultPanelRoundCeiling {
		total = defaultPanelRoundCeiling
	}
	if configured != nil && *configured > 0 {
		total = *configured
	}
	if total < seats {
		return nil, fault.New(fault.Config, fmt.Sprintf(
			"review.maxPanelRounds is %d but the reviewer panel has %d seats — every seat must be able to run at least one round (a budget that silently drops a seat is the degradation this refuses); raise the budget or remove seats",
			total, seats)).WithReason("panel_budget_below_seat_count")
	}
	return &panelBudget{extra: total - seats, total: total}, nil
}

// withSeatCause stamps a failing seat's OWN actionable cause onto its roster entry.
//
// It prefers the LaneFailure, because that is where the clihint Signal was derived once (with the
// prompt echo subtracted — see meshcore/clihint), and falls back to the error's own text for a
// failure that produced no LaneFailure at all (a containment breach, a cancellation). The error
// string is the fallback rather than the primary so the seat's detail and the run's halt message
// stay the same sentence when both exist.
func withSeatCause(s review.SeatStatus, f *review.LaneFailure, err error) review.SeatStatus {
	if err == nil {
		return s
	}
	s.Detail = err.Error()
	if f != nil {
		s.Signal = f.Signal
		if s.ReasonCode == "" {
			s.ReasonCode = f.ReasonCode
		}
	}
	return s
}

// seatCauses projects the roster's failed seats for the halt event: seat id → its own reason and
// signal. Completed seats are omitted, so an empty map means the halt came from somewhere other
// than a seat.
func seatCauses(roster []review.SeatStatus) map[string]any {
	out := map[string]any{}
	for _, s := range roster {
		if s.Detail == "" && s.ReasonCode == "" {
			continue
		}
		if s.Status != "halted" {
			continue
		}
		e := map[string]any{"adapter": s.Adapter, "model": s.Model, "reasonCode": s.ReasonCode}
		if s.Signal != "" {
			e["signal"] = s.Signal
		}
		out[s.SeatID] = e
	}
	return out
}

// panelResult is the blind stage's output for one outer cycle.
type panelResult struct {
	// findings is the UNION of every seat's findings, in seat order then report order. The
	// Judge dedups it by fingerprint; nothing is dropped here.
	findings []review.Finding
	// shown is the union of files any seat was shown (the apply-safety gate).
	shown map[string]bool
	// roster is the executed-roster echo: one entry per REQUESTED seat, in requested order.
	roster []review.SeatStatus
	// support maps a finding FINGERPRINT to the seats that reported it — the host-computed
	// provenance ledger, keyed on the same fingerprint the Judge dedups by, which is what makes
	// the ledger (and the weak-identity quarantine built on it) survive dedup by construction.
	support map[string][]review.SeatRef
	// completed lists the seats that ran to a valid result; they are the only seats whose
	// SILENCE on a finding is meaningful, so they are the dissent denominator.
	completed []review.SeatRef
	// caveats are the identity caveats collected across seats, in seat order.
	caveats []review.IdentityCaveat
	// lost are the seats a CAPACITY failure removed — a provider out of quota, or a wall clock
	// reached. The panel continued without them at a smaller denominator; see capacity.go for why
	// that is not the same event as an integrity failure.
	lost []LostSeat
	// withheld are the containment caveats collected across seats (deduped): files a rule
	// kept out of the reviewed set. Seats copy the same workspace, so the same file is
	// normally withheld from all of them — the union is what a reader needs, once each.
	withheld []review.WithheldFile
}

// seatOutput is one seat's private result, assembled inside its own goroutine.
type seatOutput struct {
	status   review.SeatStatus
	findings []review.Finding
	// fingerprints is the set of finding fingerprints THIS seat reported (dedup-stable).
	fingerprints map[string]bool
	shown        map[string]bool
	caveats      []review.IdentityCaveat
	withheld     []review.WithheldFile
	failure      *review.LaneFailure
	err          error
	failedCallID string
}

// seatCallID is the audit call id for one seat's round.
//
// Seat 1 round 1 keeps the BARE cycle call id and its later rounds keep the historical `-iN`
// suffix, so a panel of one writes exactly the call directories a pre-panel build wrote — that
// is what makes "a panel of one is today's behavior" a checkable fact (the golden run diffs
// those very files) rather than a claim.
func seatCallID(base string, seatIdx, round int) string {
	id := base
	if seatIdx > 0 {
		id = fmt.Sprintf("%s-s%d", base, seatIdx+1)
	}
	if round > 1 {
		id = fmt.Sprintf("%s-i%d", id, round)
	}
	return id
}

// runPanel executes every seat of the blind primary stage CONCURRENTLY (bounded fan-out) and
// returns the union of their findings plus the provenance ledger and the executed roster.
//
// Halt semantics: a seat failure (identity mismatch, adapter failure, containment breach,
// unparseable output after the corrective retry) halts the RUN — there is no partial-panel
// "success". When more than one seat fails, the FIRST BY SEAT INDEX wins, never the first by
// wall clock, so the same panel and the same failures always produce the same halt record. Every
// seat runs to completion before adjudication either way, so the audit record is complete.
func (m *Manager) runPanel(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, seats []review.LaneResolution, auth authority.Set, addressedLines []string, baseCallID string, maxInner int, budget *panelBudget, startedAt time.Time, outcome *review.RunOutcome) (panelResult, error) {
	fanout := seatFanout(len(seats), req.MaxParallel)
	_ = run.Event(m.now(), "info", "panel_started", "blind reviewer panel dispatched",
		map[string]any{"seats": len(seats), "fanout": fanout, "roundBudget": budget.total, "maxRoundsPerSeat": maxInner})

	outs := make([]seatOutput, len(seats))
	var wg sync.WaitGroup
	sem := make(chan struct{}, fanout)
	for i := range seats {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outs[i] = m.runSeat(ctx, run, ws, req, seats[i], i, len(seats), auth, addressedLines, baseCallID, maxInner, budget)
		}(i)
	}
	wg.Wait()

	// AGGREGATION — single-threaded, in seat order. Every shared-state mutation (the outcome's
	// failure/caveats, the halt record) happens here, so seat concurrency can never race and the
	// halt that wins is decided by index rather than by scheduling.
	res := panelResult{shown: map[string]bool{}, support: map[string][]review.SeatRef{}}
	var haltErr error
	var haltFailure *review.LaneFailure
	haltCallID := ""
	failedSeats := 0
	for i := range outs {
		o := outs[i]
		// EVERY failing seat records its OWN cause on its roster entry before the first-by-index
		// halt is chosen. The run-level Failure can only carry one seat; a panel that dispatched N
		// seats and was paid for N failures must not report one of them and drop the rest (that is
		// the same defect ResolvePanel was fixed for, one layer later and no longer free).
		// CAPACITY IS NOT INTEGRITY. A seat that ran out of quota, or hit its wall clock, is a fact
		// about a billing relationship or a timer — it says nothing about whether the seats that DID
		// answer were sound. Halting on it would discard everything already paid for, so such a seat
		// is recorded as LOST and the panel continues at a smaller denominator. An integrity failure
		// (identity, containment, an unresolvable adapter) still halts: there the run's premises are
		// broken and no count over the survivors would mean anything. See capacity.go.
		if o.err != nil && IsCapacityFailure(o.failure) {
			entry := withSeatCause(o.status, o.failure, o.err)
			entry.ReasonCode = ReasonPanelDegraded
			res.roster = append(res.roster, entry)
			res.lost = append(res.lost, LostSeat{
				SeatID: entry.SeatID, Adapter: entry.Adapter, Model: entry.Model,
				Signal: o.failure.Signal, Detail: entry.Detail,
			})
			_ = run.Event(m.now(), "warn", "seat_capacity_exhausted",
				"a seat's provider ran out of capacity; the panel continues WITHOUT it rather than discarding the seats that answered",
				map[string]any{
					"seat": entry.SeatID, "adapter": entry.Adapter, "signal": o.failure.Signal,
					"reasonCode": ReasonPanelDegraded,
				})
			continue
		}
		if o.err != nil {
			res.roster = append(res.roster, withSeatCause(o.status, o.failure, o.err))
			failedSeats++
		} else {
			res.roster = append(res.roster, o.status)
		}
		// Before the halt branch: the withheld set is collected from EVERY seat, including one
		// that halted, so a halted run still records what was kept out of the reviewed set.
		res.withheld = appendWithheld(res.withheld, o.withheld...)
		if o.err != nil {
			if haltErr == nil {
				haltErr, haltFailure, haltCallID = o.err, o.failure, o.failedCallID
			}
			continue
		}
		ref := seatRef(seats[i], o.status.IdentityTier)
		res.completed = append(res.completed, ref)
		res.findings = append(res.findings, o.findings...)
		mergeShown(res.shown, o.shown)
		res.caveats = append(res.caveats, o.caveats...)
		for _, fp := range sortedSet(o.fingerprints) {
			res.support[fp] = append(res.support[fp], ref)
		}
	}
	// Seats after the halting one may never have produced a result; label them honestly rather
	// than leaving them out of the roster.
	outcome.Withheld = appendWithheld(outcome.Withheld, res.withheld...)
	if haltErr != nil {
		_ = run.WriteJSON("panel/roster.json", rosterRecord(run, res.roster))
		// `failedSeats` and `causes` are logged alongside the winning reasonCode so the audit trail
		// says how many seats failed and why EACH did — a reader of the event stream alone can then
		// fix every blocker in one pass instead of re-running to meet the next one.
		_ = run.Event(m.now(), "error", "panel_halted", "a reviewer seat halted the run (no partial-panel success)",
			map[string]any{
				"seats": len(seats), "reasonCode": fault.ReasonOf(haltErr),
				"failedSeats": failedSeats, "causes": seatCauses(res.roster),
			})
		if haltFailure != nil {
			outcome.Failure = haltFailure
		}
		outcome.Panel = mergePanelStatus(outcome.Panel, res.roster)
		_, e := m.halt(run, outcome, startedAt, haltCallID, haltErr)
		return panelResult{}, e
	}
	for _, c := range res.caveats {
		outcome.IdentityCaveats = appendIdentityCaveat(outcome.IdentityCaveats, c)
	}
	outcome.Panel = mergePanelStatus(outcome.Panel, res.roster)
	// THE REDUCED DENOMINATOR, stated as loudly as the identity caveats are. Its PRESENCE is the
	// signal: a consumer never has to compare two counts to notice that the panel it configured is
	// not the panel that answered.
	if len(res.lost) > 0 {
		partial := &PartialPanel{
			Configured: len(seats), Answered: len(res.completed),
			Lost: res.lost, Note: PartialPanelNote,
		}
		outcome.PartialPanel = partial
		_ = run.Event(m.now(), "warn", "panel_degraded", partial.Summary(),
			map[string]any{
				"configured": len(seats), "answered": len(res.completed),
				"lost": len(res.lost), "reasonCode": ReasonPanelDegraded,
			})
	}
	_ = run.WriteJSON("panel/roster.json", rosterRecord(run, outcome.Panel))
	_ = run.Event(m.now(), "info", "panel_completed", "blind reviewer panel complete",
		map[string]any{"seats": len(seats), "answered": len(res.completed), "findings": len(res.findings), "distinct": len(res.support)})
	return res, nil
}

// runSeat runs ONE seat's private stabilization loop.
//
// BLINDNESS ENFORCEMENT POINT: `reported` below is seat-local and is fed ONLY from this seat's
// own findings; the `decisions` argument to runSemanticPass is nil. Nothing in this function's
// scope can reach another seat's output or the host's decisions, so a seat's later rounds see
// only what it itself said. Stabilization semantics are the historical ones (approve → stop; a
// round that adds no new finding → stop) plus the run-level round budget.
func (m *Manager) runSeat(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, seat review.LaneResolution, seatIdx, seatCount int, auth authority.Set, addressedLines []string, baseCallID string, maxInner int, budget *panelBudget) seatOutput {
	out := seatOutput{
		status: review.SeatStatus{
			SeatID: seat.SeatID, Index: seatIdx + 1, Adapter: seat.Adapter,
			Model: seat.Model, Effort: seat.Effort, Status: "not_started",
		},
		fingerprints: map[string]bool{},
		shown:        map[string]bool{},
	}
	adapter, aerr := m.laneAdapter(seat)
	if aerr != nil {
		out.status.Status, out.status.ReasonCode = "halted", fault.ReasonOf(aerr)
		out.err, out.failedCallID = aerr, seatCallID(baseCallID, seatIdx, 1)
		return out
	}
	_ = run.Event(m.now(), "info", "seat_started", "blind reviewer seat dispatched",
		map[string]any{"seat": seat.SeatID, "index": seatIdx + 1, "of": seatCount, "adapter": seat.Adapter, "model": seat.Model})

	var reported []string // THIS seat's own prior findings — never a peer's, never a host decision
	seen := map[string]bool{}
	for round := 1; round <= maxInner; round++ {
		if round > 1 && !budget.take() {
			out.status.Status = "budget_capped"
			out.status.ReasonCode = "panel_round_budget_exhausted"
			_ = run.Event(m.now(), "warn", "panel_round_budget_exhausted",
				"run-level reviewer-round budget reached; this seat stops stabilizing (its findings so far are kept)",
				map[string]any{"seat": seat.SeatID, "roundsRun": round - 1, "roundBudget": budget.total})
			return out
		}
		callID := seatCallID(baseCallID, seatIdx, round)
		po := m.runSemanticPass(ctx, run, ws, req, seat, adapter, review.RoleReviewer, review.PhaseIterate,
			auth, addressedLines, reported, nil /* BLIND: no peer output, no host decisions */, callID)
		// Carried on the halt path too — a file this seat was never shown is a fact about the
		// run regardless of how the seat ended.
		out.withheld = appendWithheld(out.withheld, po.withheld...)
		if po.err != nil {
			out.status.Status, out.status.ReasonCode = "halted", fault.ReasonOf(po.err)
			out.err, out.failure, out.failedCallID = po.err, po.failure, callID
			return out
		}
		out.status.Rounds = round
		out.status.IdentityTier = weakestTier(out.status.IdentityTier, po.identity)
		if po.caveat != nil {
			c := *po.caveat
			c.Role = seat.SeatID // attribute the caveat to the SEAT, not the bare role
			out.caveats = append(out.caveats, c)
		}
		mergeShown(out.shown, po.shown)

		newCount := 0
		for _, f := range po.result.Findings {
			out.findings = append(out.findings, f) // keep all; the Judge dedups
			out.fingerprints[schema.Fingerprint(f)] = true
			// Set-stability is keyed WITHOUT kind (schema.StabilityKey): a drifting kind on the
			// same file+location is the same underlying issue, so it must not read as a "new"
			// finding and keep this seat's loop spinning.
			sk := schema.StabilityKey(f)
			if !seen[sk] {
				seen[sk] = true
				reported = append(reported, findingDesc(f))
				newCount++
			}
		}
		out.status.Findings = len(out.findings)
		out.status.Status = "completed"
		if review.Verdict(po.result.Verdict) == review.VerdictApprove {
			break // this seat approves → stable for this seat
		}
		if newCount == 0 {
			break // this round added no new finding → this seat's set is stable
		}
	}
	return out
}

// attachPanelProvenance writes the HOST-COMPUTED provenance ledger onto each decision.
//
// It is computed over the DEDUPED result: the ledger is keyed on the same fingerprint the Judge
// dedups by, so a finding three seats reported carries three supporting seats on ONE decision. Each
// SeatRef carries that seat's identity tier, so a reader can see exactly what the support consists of.
//
// Nothing here consults model output: agreement is arithmetic over which seats reported the
// fingerprint, exactly as exploremesh computes corroboration host-side.
//
// It does NOT gate on identity. A finding supported only by weak-identity seats was once refused for
// apply ("weak_identity_only"), and that is gone: identity is recorded, never acted on
// (../../../../docs/model-identity.md). The refusal presumed we could tell a real model from a claimed
// one, which the codex echo test showed we cannot — so it withheld fixes for genuine defects on the
// strength of a tier that could itself be an argument echoed back at us. What a reviewer FOUND, and
// whether the write path can trace it to evidence in the workspace copy, decide whether it is applied.
func attachPanelProvenance(adj *adjudication.Result, p panelResult) {
	if len(p.completed) == 0 {
		return
	}
	for i := range adj.Findings {
		fp := schema.Fingerprint(adj.Findings[i])
		support := p.support[fp]
		if len(support) == 0 {
			continue // not a panel finding (evidence hook, cross-check, verifier)
		}
		d := &adj.Decisions[i]
		d.SupportingSeats = append([]review.SeatRef(nil), support...)
		d.AgreementCount = len(support)
		d.DissentingSeats = dissenters(p.completed, support)
		// What the seats that RAN did with it — a label over the two lists above and nothing else.
		// See dissent.go: a silent seat is not a seat that disagreed, so this is a reading prompt and
		// never a verdict.
		d.Consensus = consensusOf(len(support), len(d.DissentingSeats))
	}
}

// dissenters lists the seats that COMPLETED and did not report the finding. A seat that halted
// or never started is in neither list: its silence is unknowable, and recording it as dissent
// would manufacture disagreement out of a failure.
func dissenters(completed, support []review.SeatRef) []string {
	sup := map[string]bool{}
	for _, r := range support {
		sup[r.SeatID] = true
	}
	var out []string
	for _, r := range completed {
		if !sup[r.SeatID] {
			out = append(out, r.SeatID)
		}
	}
	return out
}

func seatRef(seat review.LaneResolution, tier string) review.SeatRef {
	if tier == "" {
		tier = review.SeatIdentityUnknown
	}
	return review.SeatRef{SeatID: seat.SeatID, Adapter: seat.Adapter, Model: seat.Model, IdentityTier: tier}
}

// weakestTier keeps the WEAKEST identity tier a seat produced across its rounds — fail closed,
// so a seat that verified once and went unknown later is recorded at the weaker tier.
func weakestTier(a, b string) string {
	rank := map[string]int{review.SeatIdentityVerified: 2, review.SeatIdentitySelfReported: 1}
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if rank[b] < rank[a] {
		return b
	}
	return a
}

// mergePanelStatus folds a cycle's roster into the run-level echo: identity/status come from the
// latest cycle, while rounds and findings ACCUMULATE across outer cycles (the echo answers "what
// did this run spend on the panel", which a per-cycle snapshot would understate).
func mergePanelStatus(prev, cur []review.SeatStatus) []review.SeatStatus {
	if len(prev) == 0 {
		return append([]review.SeatStatus(nil), cur...)
	}
	byID := map[string]int{}
	for i, s := range prev {
		byID[s.SeatID] = i
	}
	out := append([]review.SeatStatus(nil), prev...)
	for _, s := range cur {
		i, ok := byID[s.SeatID]
		if !ok {
			out = append(out, s)
			continue
		}
		s.Rounds += out[i].Rounds
		s.Findings += out[i].Findings
		out[i] = s
	}
	return out
}

func rosterRecord(run *audit.Run, roster []review.SeatStatus) map[string]any {
	return map[string]any{"schemaVersion": 1, "runId": run.ID, "seats": roster}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
