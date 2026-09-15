package pipeline

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// readinessTarget is one adapter, with the model and effort a real call would pass it. Recipes build the
// model into the command line, so a probe must use the model the panel uses.
type readinessTarget struct {
	Adapter string
	Model   string
	Effort  string
}

func (t readinessTarget) String() string {
	s := t.Adapter + "/" + t.Model
	if t.Effort != "" {
		s += " effort=" + t.Effort
	}
	return s
}

// readinessTargets returns the distinct (adapter, model, effort) identities the panel names — explorers,
// collator and explicit canonicalizers — whose registered adapter supports a deep probe, sorted. Derived
// canonicalizers come from those same seats and add no target. Adapters without a deep probe are left out,
// so the result's length is exactly the number of calls verifyReadiness makes.
func readinessTargets(reg Registry, plan roster.Plan) []readinessTarget {
	seen := map[readinessTarget]bool{}
	add := func(adapter, m, effort string) {
		a, ok := reg[adapter]
		if !ok {
			return
		}
		if _, deep := a.(model.DeepProber); !deep {
			return
		}
		seen[readinessTarget{Adapter: adapter, Model: m, Effort: effort}] = true
	}
	for _, e := range plan.Explorers {
		add(e.Adapter, e.Model, e.Effort)
	}
	add(plan.Collator.Adapter, plan.Collator.Model, plan.Collator.Effort)
	for _, c := range plan.Canonicalizers {
		add(c.Adapter, c.Model, c.Effort)
	}
	out := make([]readinessTarget, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b readinessTarget) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// verifyReadiness probes every readiness target concurrently and returns an error if any cannot do real
// work. It runs before the identity pre-flight, which is the run's first model call.
//
// Available only proves a binary exists; login, folder trust and model access fail only when the CLI is
// invoked. A one-token deep probe finds those failures for one call per target instead of a panel's worth
// of spend. A passing probe says nothing about quota for later calls.
func verifyReadiness(ctx context.Context, reg Registry, plan roster.Plan, onEvent func(audit.EventLine)) error {
	targets := readinessTargets(reg, plan)
	if len(targets) == 0 {
		return nil
	}
	emit(onEvent, "info", "readiness_started", "deep readiness probe: asking every panel agent whether it can do real work", map[string]any{"targets": len(targets)})

	results := make([]model.ProbeResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		dp := reg[t.Adapter].(model.DeepProber)
		wg.Go(func() {
			results[i] = dp.ProbeDeep(ctx, model.DeepProbeSpec{ModelArg: core.ModelArg(t.Model), Effort: t.Effort})
		})
	}
	wg.Wait()

	// Report every failure, not only the first: two blocked agents need two different fixes.
	var failed []int
	for i, t := range targets {
		r := results[i]
		fields := map[string]any{"target": t.String(), "ok": r.OK, "stage": r.Stage, "detail": r.Detail}
		if r.Signal != "" {
			fields["signal"] = string(r.Signal)
		}
		if r.OK {
			emit(onEvent, "info", "readiness_ok", "agent is ready", fields)
			continue
		}
		emit(onEvent, "error", "readiness_failed", "agent is not ready", fields)
		failed = append(failed, i)
	}
	if len(failed) == 0 {
		emit(onEvent, "info", "readiness_complete", "every panel agent answered", nil)
		return nil
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "%d of %d panel agent(s) cannot do real work; nothing was dispatched", len(failed), len(targets))
	for _, i := range failed {
		fmt.Fprintf(&msg, "\n  %s: %s", targets[i], results[i].Detail)
	}
	f := fault.New(fault.Adapter, msg.String()).WithHalt("A").WithReason("adapter_not_ready")
	if sig := results[failed[0]].Signal; sig != "" {
		f = f.WithReason(string(sig))
	}
	return f
}
