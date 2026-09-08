package run

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// blockedAdapter is installed and Available() — the exact posture that makes this worth doing. It
// fails only when actually invoked, which is where login, folder-trust and model-validity failures
// really live.
type blockedAdapter struct {
	model.Adapter
	name   string
	signal clihint.Signal
	detail string
	probes *int32
}

func (b blockedAdapter) Name() string              { return b.name }
func (b blockedAdapter) Available() (bool, string) { return true, "installed" }
func (b blockedAdapter) ProbeDeep(context.Context, model.DeepProbeSpec) model.ProbeResult {
	atomic.AddInt32(b.probes, 1)
	return model.ProbeResult{OK: false, Stage: "invoke", Signal: b.signal, Detail: b.detail}
}

// TestVerifyReadiness_RefusesBeforeDispatchingAnySeat is the defect this exists to prevent, in one
// assertion: the LAST seat is the broken one, and no seat runs at all.
//
// Without it, a panel dispatches concurrently, seat 1 pays for a full workspace-sized prompt, and
// the run then halts on seat 2's CLI having been blocked on a folder-trust prompt the whole time.
// `Available()` cannot see that — the binary is installed and starts fine.
func TestVerifyReadiness_RefusesBeforeDispatchingAnySeat(t *testing.T) {
	m, calls := countingPanel(t)
	ws, _ := makeWorkspace(t)
	var probes int32
	m.Adapters["seat-c"] = blockedAdapter{
		Adapter: m.Adapters["seat-c"], name: "seat-c", probes: &probes,
		signal: clihint.FolderTrust,
		detail: "deep probe FAILED in a throwaway directory: exited 55 with no output",
	}

	_, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel",
		VerifyReadiness: true,
	})
	if err == nil {
		t.Fatal("the run proceeded with an agent that cannot do real work")
	}
	if !strings.Contains(err.Error(), "cannot do real work") {
		t.Errorf("refusal does not say what happened: %v", err)
	}
	// THE POINT: not one seat was dispatched, including the two that were perfectly ready.
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("%d seat call(s) were paid for before the blocked agent was found, want 0", n)
	}
	if atomic.LoadInt32(&probes) == 0 {
		t.Error("the blocked adapter was never probed")
	}
}

// TestVerifyReadiness_ReportsEveryBlockedAgent: two agents blocked for two different reasons need
// two different fixes, and a report naming one of them sends the user round the loop twice. Same
// argument as the halted-panel seat causes (#50) — probes run concurrently, exactly like seats.
func TestVerifyReadiness_ReportsEveryBlockedAgent(t *testing.T) {
	m, _ := countingPanel(t)
	ws, _ := makeWorkspace(t)
	var probes int32
	m.Adapters["seat-b"] = blockedAdapter{
		Adapter: m.Adapters["seat-b"], name: "seat-b", probes: &probes,
		signal: clihint.LoginRequired, detail: "not authenticated",
	}
	m.Adapters["seat-c"] = blockedAdapter{
		Adapter: m.Adapters["seat-c"], name: "seat-c", probes: &probes,
		signal: clihint.ModelInvalid, detail: "the CLI rejected the model argument",
	}

	_, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel",
		VerifyReadiness: true,
	})
	if err == nil {
		t.Fatal("the run proceeded with two blocked agents")
	}
	for _, want := range []string{"not authenticated", "rejected the model argument"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits a blocked agent's cause %q:\n%v", want, err)
		}
	}
}

// TestVerifyReadiness_IsOptInAndOffByDefault. The probe spends, so a run that did not ask for it
// must not make the call — the same discipline meshcore/model.DeepProber states for every caller.
func TestVerifyReadiness_IsOptInAndOffByDefault(t *testing.T) {
	m, _ := countingPanel(t)
	ws, _ := makeWorkspace(t)
	var probes int32
	m.Adapters["seat-c"] = blockedAdapter{
		Adapter: m.Adapters["seat-c"], name: "seat-c", probes: &probes,
		signal: clihint.FolderTrust, detail: "blocked",
	}

	if _, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel",
	}); err != nil {
		t.Fatalf("a run without VerifyReadiness should not be gated by it: %v", err)
	}
	if n := atomic.LoadInt32(&probes); n != 0 {
		t.Errorf("%d probe(s) spent on a run that never asked for them", n)
	}
}

// TestVerifyReadiness_DryRunPricesThemAndSpendsNothing: probes are model calls, so a dry run must
// count them — and must not make them. A dry run that spent to prove it would not have to spend has
// answered a different question than the one asked.
func TestVerifyReadiness_DryRunPricesThemAndSpendsNothing(t *testing.T) {
	m, calls := countingPanel(t)
	ws, _ := makeWorkspace(t)
	var probes int32
	m.Adapters["seat-c"] = blockedAdapter{
		Adapter: m.Adapters["seat-c"], name: "seat-c", probes: &probes,
		signal: clihint.FolderTrust, detail: "blocked",
	}

	out, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel",
		DryRun: true, VerifyReadiness: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if out.Shape == nil {
		t.Fatal("no shape")
	}
	if out.Shape.ReadinessProbes == 0 {
		t.Error("the shape prices no readiness probes for a run that would make them")
	}
	if out.Shape.MinModelCalls < out.Shape.ReadinessProbes {
		t.Errorf("minModelCalls=%d does not include the %d unavoidable probes",
			out.Shape.MinModelCalls, out.Shape.ReadinessProbes)
	}
	if n := atomic.LoadInt32(&probes) + atomic.LoadInt32(calls); n != 0 {
		t.Errorf("a dry run spent %d call(s) pricing a probe it was supposed to only price", n)
	}
}
