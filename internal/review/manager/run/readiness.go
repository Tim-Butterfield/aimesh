package run

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// probeTarget is one adapter with the model argument and effort a real call would pass it.
type probeTarget struct {
	Adapter  string
	ModelArg core.ModelArg
	Effort   string
}

// String formats the target as adapter/model with any effort.
func (t probeTarget) String() string {
	s := t.Adapter + "/" + string(t.ModelArg)
	if t.Effort != "" {
		s += " effort=" + t.Effort
	}
	return s
}

// probeTargets returns every distinct adapter, model and effort this run would invoke, sorted.
// Readiness does not differ per seat, so duplicates are probed once.
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

// verifyReadiness probes every target concurrently with a one-token call and refuses the run, before
// any seat is dispatched, if any cannot work. It catches login, folder trust, model validity and update
// prompts, which fail only at invocation. Quota cannot be established ahead of time; it is caught when
// a real call hits it (see meshcore/clihint).
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
			// An adapter without a deep probe is recorded as unprobeable, not as a failure.
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

	// Report every failure, so each blocked agent can be fixed in one pass.
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
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("%d of %d configured agent(s) cannot do real work; nothing was dispatched", len(failed), len(targets)))
	for _, o := range failed {
		msg.WriteString(fmt.Sprintf("\n  %s: %s", o.target, o.res.Detail))
	}
	f := fault.New(fault.Adapter, msg.String()).WithHalt("A").WithReason("adapter_not_ready")
	if first.res.Signal != "" {
		f = f.WithReason(string(first.res.Signal))
	}
	return f
}
