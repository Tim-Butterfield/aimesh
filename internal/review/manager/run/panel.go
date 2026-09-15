package run

// The blind primary panel runs N ordered reviewer seats (1..review.MaxReviewerSeats), each an
// independent adapter, model and effort. It enforces three rules:
//
//  1. Blindness: between rounds a seat sees only its own prior findings, never another seat's output
//     or host decisions. runSeat keeps reported local and passes no decisions, and adjudication
//     starts only after every seat has finished.
//  2. No silent degradation: every requested seat runs or the run fails with a reason, and the roster
//     has one entry per requested seat. A capacity failure is recorded as a lost seat instead (see
//     capacity.go).
//  3. Host-computed counts: the host counts agreement over fingerprints; no model output carries a
//     count.

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

// defaultPanelRoundCeiling caps total blind-primary rounds when config sets no limit, so seats cannot
// multiply the per-seat cap. It is at least review.MaxReviewerSeats, so every seat can run its first
// round.
const defaultPanelRoundCeiling = 64

// seatFanout returns how many seats run concurrently: all n unless maxParallel is a smaller positive
// number, and never below 1. There is no built-in ceiling, because the right value depends on the
// caller's machine and provider limits.
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

// panelBudget is the round budget shared by every seat. Each seat's first round is guaranteed and not
// drawn from the pool, so the budget only shortens stabilization and never removes a seat.
type panelBudget struct {
	mu    sync.Mutex
	extra int // rounds available beyond each seat's guaranteed first round
	total int // the configured or derived ceiling, for reporting
}

// take draws one round from the shared pool, reporting whether one was available.
func (b *panelBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.extra <= 0 {
		return false
	}
	b.extra--
	return true
}

// newPanelBudget derives the round budget: review.maxPanelRounds when set, else maxInner rounds per
// seat capped at defaultPanelRoundCeiling. A budget below the seat count is a config error, because a
// requested seat could not run at all.
func newPanelBudget(configured *int, maxInner, seats int) (*panelBudget, error) {
	total := min(maxInner*seats, defaultPanelRoundCeiling)
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

// withSeatCause records a failing seat's own cause on its roster entry: the error text as detail, and
// the LaneFailure's signal and reason code when there is one.
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
	// findings is the union of every seat's findings, in seat order; the Judge deduplicates them.
	findings []review.Finding
	// shown is the union of files any seat was shown.
	shown map[string]bool
	// roster has one entry per requested seat, in requested order.
	roster []review.SeatStatus
	// support maps a finding fingerprint to the seats that reported it, keyed like the Judge's
	// deduplication so it survives it.
	support map[string][]review.SeatRef
	// completed lists seats that returned a valid result; only their silence counts as dissent.
	completed []review.SeatRef
	// caveats are the identity caveats collected across seats, in seat order.
	caveats []review.IdentityCaveat
	// lost are seats removed by a capacity failure (quota or wall clock); the panel continued without
	// them. See capacity.go.
	lost []LostSeat
	// withheld are containment caveats from every seat, deduplicated.
	withheld []review.WithheldFile
}

// seatOutput is one seat's private result, assembled inside its own goroutine.
type seatOutput struct {
	status   review.SeatStatus
	findings []review.Finding
	// fingerprints is the set of finding fingerprints this seat reported.
	fingerprints map[string]bool
	shown        map[string]bool
	caveats      []review.IdentityCaveat
	withheld     []review.WithheldFile
	failure      *review.LaneFailure
	err          error
	failedCallID string
}

// seatCallID returns the audit call id for one seat's round: the bare cycle id for seat 1, an -sN
// suffix for later seats, and an -iN suffix for rounds after the first. The golden run checks the
// resulting call directories.
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

// runPanel runs every seat concurrently, with bounded fan-out, and returns the union of findings, the
// provenance ledger and the executed roster. A non-capacity seat failure halts the run; when several
// seats fail, the lowest seat index wins, so the halt record is deterministic. Every seat runs to
// completion first.
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

	// Aggregate single-threaded in seat order, so shared state never races and the winning halt is
	// decided by index.
	res := panelResult{shown: map[string]bool{}, support: map[string][]review.SeatRef{}}
	var haltErr error
	var haltFailure *review.LaneFailure
	haltCallID := ""
	failedSeats := 0
	for i := range outs {
		o := outs[i]
		// Every failing seat records its own cause. A capacity failure (quota, wall clock) says
		// nothing about the seats that answered, so the seat is recorded as lost and the panel
		// continues; any other failure halts. See capacity.go.
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
		// Collect withheld files from every seat, including one that halted.
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
	outcome.Withheld = appendWithheld(outcome.Withheld, res.withheld...)
	if haltErr != nil {
		_ = run.WriteJSON("panel/roster.json", rosterRecord(run, res.roster))
		// Log every failing seat's cause, so all of them can be fixed in one pass.
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
	// Report a partial panel explicitly, so a consumer need not compare counts to notice lost seats.
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

// runSeat runs one seat's private stabilization loop. reported holds only this seat's own findings
// and no host decisions are passed, which keeps the seat blind. The loop stops when the seat
// approves, when a round adds no new finding, or when the shared round budget is exhausted.
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

	var reported []string // this seat's own prior findings only
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
			auth, addressedLines, reported, nil /* blind: no peer output or host decisions */, callID)
		// Keep withheld files on the halt path too.
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
			c.Role = seat.SeatID // attribute the caveat to the seat, not the role
			out.caveats = append(out.caveats, c)
		}
		mergeShown(out.shown, po.shown)

		newCount := 0
		for _, f := range po.result.Findings {
			out.findings = append(out.findings, f) // keep all; the Judge dedups
			out.fingerprints[schema.Fingerprint(f)] = true
			// Stability is keyed without kind, so a drifting kind for the same file and location is
			// not a new finding.
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

// attachPanelProvenance writes host-computed provenance onto each deduplicated decision: the
// supporting seats with their identity tiers, the agreement count, the dissenting seats and a
// consensus label. It does not gate on identity (see docs/model-identity.md): whether a finding is
// applied depends on its evidence, not on which model reported it.
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
		// A label over the two lists above; a silent seat is not a disagreeing one (see dissent.go).
		d.Consensus = consensusOf(len(support), len(d.DissentingSeats))
	}
}

// dissenters lists seats that completed without reporting the finding. Halted or unstarted seats are
// excluded, because their silence means nothing.
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

// weakestTier returns the weaker of two identity tiers, so a seat is recorded at its weakest tier
// across rounds.
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

// mergePanelStatus folds a cycle's roster into the run-level roster: identity and status come from the
// latest cycle, while rounds and findings accumulate across cycles.
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
