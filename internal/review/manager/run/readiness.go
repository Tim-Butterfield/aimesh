package run

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// probeTarget is one distinct thing worth asking "can you actually work?" — an adapter with the
// model argument and effort a real call would pass it. A recipe builds the model into its argv, so
// probing with a model the roster does not use answers a question nobody asked.
type probeTarget struct {
	Adapter  string
	ModelArg core.ModelArg
	Effort   string
}

func (t probeTarget) String() string {
	s := t.Adapter + "/" + string(t.ModelArg)
	if t.Effort != "" {
		s += " effort=" + t.Effort
	}
	return s
}

// probeTargets is every DISTINCT (adapter, model, effort) this run would invoke, sorted.
//
// Deduplicated because readiness is a property of the CLI's trust/auth posture, not of the seat: a
// three-seat panel on one adapter and model is one question, and asking it three times would spend
// three times to learn the same fact.
func probeTargets(plan review.RunPlan, seats []review.LaneResolution) []probeTarget {
	seen := map[probeTarget]bool{}
	add := func(adapter string, m core.ModelArg, effort, execution string) {
		// A host-execution lane runs in-process and invokes nothing, so it has no readiness to probe.
		if execution == "host" || adapter == "" || adapter == "fake" {
			return
		}
		seen[probeTarget{Adapter: adapter, ModelArg: m, Effort: effort}] = true
	}
	for _, l := range plan.Lanes {
		add(l.Adapter, l.ModelArg, l.Effort, l.Execution)
	}
	for _, s := range seats {
		add(s.Adapter, s.ModelArg, s.Effort, s.Execution)
	}
	out := make([]probeTarget, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// verifyReadiness asks EVERY configured agent whether it can do real work, CONCURRENTLY, and refuses
// the run if any cannot — before the first seat is dispatched.
//
// WHY IT IS WORTH A CALL. `Available()` proves a binary exists. It does not prove the CLI is logged
// in, that it will accept a fresh isolated directory, or that the account can use the model named in
// the roster — and every one of those fails only at invocation time. Without this, a panel dispatches
// seat 1 against a 900 KB prompt, pays for it, and then discovers at seat 2 that its CLI has been
// waiting on a folder-trust prompt all along. The deep probe asks the same question with a
// one-token prompt instead, so the expensive discovery becomes a cheap one.
//
// IT SPENDS, so it is opt-in (Request.VerifyReadiness) and it is PRICED: a dry run counts one call
// per target in both bounds and executes none of them, because a dry run that spent to prove it
// would not have to spend has answered a different question than the one asked.
//
// QUOTA IS NOT PREFLIGHTABLE, and this does not pretend otherwise. A probe that passes proves
// nothing about the next call — the window can empty between them — and the probe itself draws on
// the same window. Trust, login, model validity and update blocks are stable properties this can
// establish; quota is a moment, and it is caught where it happens (see meshcore/clihint's
// QuotaExhausted). A probe that failed on quota still reports it, which is strictly better than
// discovering it after a panel's worth of spend.
func (m *Manager) verifyReadiness(ctx context.Context, run *audit.Run, targets []probeTarget) error {
	if len(targets) == 0 {
		return nil
	}
	_ = run.Event(m.now(), "info", "readiness_started",
		"deep readiness probe: asking every configured agent whether it can do real work",
		map[string]any{"targets": len(targets)})

	type outcome struct {
		target probeTarget
		res    model.ProbeResult
		probed bool
	}
	results := make([]outcome, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		a, ok := m.Adapters[t.Adapter]
		if !ok {
			continue // preflightAdapter already refused an unregistered adapter
		}
		dp, ok := a.(model.DeepProber)
		if !ok {
			// An adapter with no deep probe is NOT a failure: it is un-probeable, and reporting it as
			// unready would refuse runs that work. The gap is recorded rather than guessed at.
			results[i] = outcome{target: t}
			continue
		}
		wg.Add(1)
		go func(i int, t probeTarget, dp model.DeepProber) {
			defer wg.Done()
			res := dp.ProbeDeep(ctx, model.DeepProbeSpec{ModelArg: t.ModelArg, Effort: t.Effort})
			results[i] = outcome{target: t, res: res, probed: true}
		}(i, t, dp)
	}
	wg.Wait()

	// EVERY failure is reported, not just the first. The seats were probed concurrently, exactly as
	// they would be dispatched, so a run blocked on two different agents needs two different fixes —
	// the same reason a halted panel records every seat's cause (see panel.go).
	var failed []outcome
	for _, o := range results {
		if o.target.Adapter == "" {
			continue
		}
		if !o.probed {
			_ = run.Event(m.now(), "info", "readiness_unprobeable",
				"adapter exposes no deep probe; readiness not established",
				map[string]any{"target": o.target.String()})
			continue
		}
		ev := map[string]any{"target": o.target.String(), "ok": o.res.OK, "stage": o.res.Stage, "detail": o.res.Detail}
		if o.res.Signal != "" {
			ev["signal"] = string(o.res.Signal)
		}
		if o.res.OK {
			_ = run.Event(m.now(), "info", "readiness_ok", "agent is ready", ev)
			continue
		}
		_ = run.Event(m.now(), "error", "readiness_failed", "agent is NOT ready", ev)
		failed = append(failed, o)
	}
	if len(failed) == 0 {
		_ = run.Event(m.now(), "info", "readiness_complete", "every configured agent answered", nil)
		return nil
	}

	first := failed[0]
	msg := fmt.Sprintf("%d of %d configured agent(s) cannot do real work; nothing was dispatched", len(failed), len(targets))
	for _, o := range failed {
		msg += fmt.Sprintf("\n  %s: %s", o.target, o.res.Detail)
	}
	f := fault.New(fault.Adapter, msg).WithHalt("A").WithReason("adapter_not_ready")
	if first.res.Signal != "" {
		f = f.WithReason(string(first.res.Signal))
	}
	return f
}
