package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// countingAdapter wraps an adapter and counts every Invoke. It is the only instrument that can
// prove a dry run is a dry run: a shape that looks right but was produced after three model calls
// is worse than no shape at all, because it charges for the estimate.
type countingAdapter struct {
	model.Adapter
	calls *int32
}

func (c countingAdapter) Invoke(ctx context.Context, call model.Call) (model.Result, error) {
	atomic.AddInt32(c.calls, 1)
	return c.Adapter.Invoke(ctx, call)
}

// countingPanel builds the 3-seat panel manager with every adapter — seats AND the host —
// instrumented, and returns the shared call counter.
func countingPanel(t *testing.T) (*Manager, *int32) {
	t.Helper()
	m := panelManager(t,
		seatFake{name: "seat-a", title: "issue A", file: "a.go"},
		seatFake{name: "seat-b", title: "issue B", file: "b.go"},
		seatFake{name: "seat-c", title: "issue C", file: "c.go"},
	)
	var calls int32
	for name, a := range m.Adapters {
		m.Adapters[name] = countingAdapter{Adapter: a, calls: &calls}
	}
	return m, &calls
}

// TestDryRun_SpendsNothingAndDisclosesTheShape is the whole feature in one assertion pair: zero
// adapter invocations, and a shape that names every seat that WOULD have been invoked. Without the
// stop, the same request runs a real three-seat review.
func TestDryRun_SpendsNothingAndDisclosesTheShape(t *testing.T) {
	m, calls := countingPanel(t)
	ws, _ := makeWorkspace(t)

	out, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel", DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("a dry run made %d model call(s), want 0 — the stop is not before the spend", n)
	}
	if out.Status != "planned" {
		t.Errorf("status = %q, want %q — a caller must be able to tell a priced run from a reviewed one", out.Status, "planned")
	}
	if len(out.Findings) != 0 {
		t.Errorf("a dry run reported %d finding(s); it reviewed nothing, so it can have found nothing", len(out.Findings))
	}
	if out.Shape == nil {
		t.Fatal("no shape on a dry run — the disclosure IS the output")
	}
	s := *out.Shape
	if len(s.Seats) != 3 {
		t.Fatalf("shape names %d seat(s), want 3", len(s.Seats))
	}
	for i, want := range []string{"seat-a", "seat-b", "seat-c"} {
		if s.Seats[i].Adapter != want {
			t.Errorf("seat %d adapter = %q, want %q (requested order is preserved)", i+1, s.Seats[i].Adapter, want)
		}
		if s.Seats[i].Model == "" {
			t.Errorf("seat %d discloses no model — the shape must say WHAT would be called, not just how many times", i+1)
		}
	}
	// The host lane is `execution: host` in the panel profile, so adjudication is free and the
	// floor is exactly the seat count. A run that cannot state this cheaply cannot be budgeted.
	if !s.DeterministicHost {
		t.Error("host adjudication is deterministic in this profile but the shape does not say so")
	}
	if s.MinModelCalls != 3 {
		t.Errorf("MinModelCalls = %d, want 3 (one round per seat, free adjudication)", s.MinModelCalls)
	}
	if s.MaxModelCalls < s.MinModelCalls {
		t.Errorf("ceiling %d is below floor %d", s.MaxModelCalls, s.MinModelCalls)
	}
	if s.Writes {
		t.Error("report mode does not write, but the shape says it would")
	}
}

// TestDryRun_WritesTheShapeArtifactAndTheResolvedPlan: a dry run leaves the same audit record a real
// run's pre-spend phase does, plus the shape. That record is the point — "what would this do" has to
// survive the terminal it was printed in.
func TestDryRun_WritesTheShapeArtifactAndTheResolvedPlan(t *testing.T) {
	m, _ := countingPanel(t)
	ws, _ := makeWorkspace(t)

	out, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel", DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, name := range []string{"run-shape.json", "resolved-plan.json", "doctor.json", "run-state.json"} {
		if _, serr := os.Stat(filepath.Join(out.RunDir, name)); serr != nil {
			t.Errorf("%s missing from a dry run's record: %v", name, serr)
		}
	}
	b, rerr := os.ReadFile(filepath.Join(out.RunDir, "run-shape.json"))
	if rerr != nil {
		t.Fatalf("run-shape.json: %v", rerr)
	}
	var got review.RunShape
	if uerr := json.Unmarshal(b, &got); uerr != nil {
		t.Fatalf("run-shape.json is not a RunShape: %v", uerr)
	}
	if len(got.Seats) != 3 || got.MinModelCalls != out.Shape.MinModelCalls {
		t.Errorf("the artifact and the returned shape disagree: %+v vs %+v", got, *out.Shape)
	}
	// run-state records the same status, so a reader of the directory alone is not misled into
	// thinking an empty findings set means a clean tree.
	st, serr := os.ReadFile(filepath.Join(out.RunDir, "run-state.json"))
	if serr != nil {
		t.Fatalf("run-state.json: %v", serr)
	}
	var state map[string]any
	if uerr := json.Unmarshal(st, &state); uerr != nil {
		t.Fatalf("run-state.json: %v", uerr)
	}
	if state["status"] != "planned" {
		t.Errorf("run-state status = %v, want \"planned\"", state["status"])
	}
}

// TestDryRun_PricesTheModeItWasGiven: `--apply --dry-run` prices an APPLY run — it does not quietly
// become a report run. The mode is the expensive variable (apply can iterate), so a dry run that
// dropped it would answer a question nobody asked.
func TestDryRun_PricesTheModeItWasGiven(t *testing.T) {
	m, calls := countingPanel(t)
	ws, _ := makeWorkspace(t)

	out, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeApply, Surface: "cli", Profile: "panel", DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("an apply dry run made %d model call(s), want 0", n)
	}
	if out.Shape == nil {
		t.Fatal("no shape")
	}
	if out.Shape.Mode != review.ModeApply {
		t.Errorf("shape mode = %q, want apply — the dry run must price what was asked for", out.Shape.Mode)
	}
	if !out.Shape.Writes {
		t.Error("apply mode writes; the shape must say so even though this dry run wrote nothing")
	}
	// Only apply can iterate, so only apply multiplies the cycle ceiling.
	if out.Shape.OuterCycles < 2 {
		t.Errorf("apply OuterCycles = %d, want the configured cap (>1) — a converging apply is the case where the ceiling matters", out.Shape.OuterCycles)
	}
	report, rerr := m.shapeOf(Request{Workspace: ws}, review.RunPlan{Mode: review.ModeReport}, nil, 3)
	if rerr != nil {
		t.Fatalf("shapeOf(report): %v", rerr)
	}
	if report.OuterCycles != 1 {
		t.Errorf("report OuterCycles = %d, want 1 — report commits nothing, so there is nothing to converge", report.OuterCycles)
	}
}

// TestDryRun_StillRefusesEveryKnowableConfigError: the stop is AFTER preflight, not instead of it. A
// dry run that reported a tidy shape for a panel whose binary is missing would be worse than no dry
// run: it would certify a run that cannot start.
func TestDryRun_StillRefusesEveryKnowableConfigError(t *testing.T) {
	m, calls := countingPanel(t)
	ws, _ := makeWorkspace(t)
	// A budget below the seat count would silently drop a seat — a config error the real run
	// refuses before spending, and therefore one a dry run must refuse too.
	two := 2
	m.Cfg.Review.MaxPanelRounds = &two

	_, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel", DryRun: true,
	})
	if err == nil {
		t.Fatal("a dry run accepted a panel budget below its seat count — the free pass is supposed to be the point")
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("the refusal cost %d model call(s), want 0", n)
	}
}

// TestDryRun_UnavailableAdapterIsReportedForFree: the other half of the same promise, on the check
// that costs a real run the most to discover late.
func TestDryRun_UnavailableAdapterIsReportedForFree(t *testing.T) {
	m, _ := countingPanel(t)
	ws, _ := makeWorkspace(t)
	m.Adapters["seat-b"] = unavailableAdapter{name: "seat-b"}

	_, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel", DryRun: true,
	})
	if err == nil {
		t.Fatal("a dry run accepted a panel whose second seat names an unavailable adapter")
	}
}

// TestDryRun_RunStateValidatesAgainstItsPublishedSchema: "planned" is a new value in an enum that
// `additionalProperties: false` schema publishes. A status the schema does not list makes the
// schema a false description of what this repo writes — for the one run shape whose whole purpose
// is to be read by a machine before it commits money.
func TestDryRun_RunStateValidatesAgainstItsPublishedSchema(t *testing.T) {
	root := repoRootFromPackage(t)
	sch, cerr := jsonschema.CompileFile(filepath.Join(root, "docs", "schema", "run-state.schema.json"))
	if cerr != nil {
		t.Fatalf("compile run-state.schema.json: %v", cerr)
	}
	m, _ := countingPanel(t)
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel", DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	b, rerr := os.ReadFile(filepath.Join(out.RunDir, "run-state.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if verr := sch.ValidateJSON(b); verr != nil {
		t.Fatalf("a dry run's run-state.json does not validate against its published schema: %v\n%s", verr, b)
	}
}

// TestDryRun_RefusesAnInadmissibleWorkspace: the stop must not be reached at all for a workspace
// the copy would reject. Measured 2026-08-11 — a dry run pointed at a directory under a protected
// path printed a full panel and a 2..10 model-call range for a root that the very next step refused with
// workspace_excluded_ancestor. A shape for a run that cannot start is worse than no shape: it is a
// confident answer to "could this run?" that is wrong.
func TestDryRun_RefusesAnInadmissibleWorkspace(t *testing.T) {
	m, calls := countingPanel(t)
	// A root INSIDE a protected family. Exclusion is judged on components relative to the root, so
	// naming one as the root is what would strip its protection — hence the refusal.
	//
	// `.vscode`, deliberately, and NOT `.aimesh`: run artifacts are readable without a flag now, so
	// `.aimesh` is no longer a refusal and would make this test assert nothing.
	ws := filepath.Join(t.TempDir(), ".vscode", "temp", "sample")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel", DryRun: true,
	})
	if err == nil {
		t.Fatal("a dry run reported a shape for a workspace the copy would refuse")
	}
	if out.Shape != nil {
		t.Error("a refused dry run still produced a shape — nothing about this run is plannable")
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("the refusal cost %d model call(s), want 0", n)
	}
}

// TestDryRun_PricesWhatTheCallsWouldCarry is the second half of the same disclosure, and it is
// written as the defect that produced it: two workspaces with an IDENTICAL panel, identical caps
// and therefore an identical call range, whose runs would look at completely different things.
// Measured 2026-08-11, `--dry-run .` and `--dry-run internal/review/manager/run` printed
// byte-identical disclosures — a cost estimate that could not tell a monorepo from one package.
//
// The call counts are asserted EQUAL on purpose: it is what makes the payload the only thing
// carrying the difference, and a future change that let the counts diverge here would make the
// test pass for the wrong reason.
func TestDryRun_PricesWhatTheCallsWouldCarry(t *testing.T) {
	m, calls := countingPanel(t)
	big := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go"} {
		if err := os.WriteFile(filepath.Join(big, name), []byte("package p // "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	small := filepath.Join(big, "sub")
	if err := os.MkdirAll(small, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(small, "only.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	shapeFor := func(ws string) review.RunShape {
		t.Helper()
		out, err := m.RunContext(context.Background(), Request{
			Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel", DryRun: true,
		})
		if err != nil {
			t.Fatalf("dry run %s: %v", ws, err)
		}
		if out.Shape == nil {
			t.Fatalf("dry run %s: no shape", ws)
		}
		return *out.Shape
	}
	whole, part := shapeFor(big), shapeFor(small)

	if whole.MinModelCalls != part.MinModelCalls || whole.MaxModelCalls != part.MaxModelCalls {
		t.Fatalf("the two runs no longer cost the same (%d..%d vs %d..%d) — this test's premise is that only the payload distinguishes them",
			whole.MinModelCalls, whole.MaxModelCalls, part.MinModelCalls, part.MaxModelCalls)
	}
	if whole.Payload.Files != 5 {
		t.Errorf("whole workspace: files = %d, want 5", whole.Payload.Files)
	}
	if part.Payload.Files != 1 {
		t.Errorf("subdirectory: files = %d, want 1", part.Payload.Files)
	}
	if whole.Payload.Bytes <= part.Payload.Bytes {
		t.Errorf("bytes: whole %d, part %d — the disclosure still cannot tell them apart", whole.Payload.Bytes, part.Payload.Bytes)
	}
	if len(part.Payload.Paths) != 1 || part.Payload.Paths[0] != "only.go" {
		t.Errorf("paths = %v, want [only.go] — a count says how much, only the list says whether the file you care about is in there", part.Payload.Paths)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("pricing the payload cost %d model call(s), want 0 — it reads the tree and copies nothing", n)
	}
}

// TestDryRun_ShapeArtifactCarriesThePayload: run-shape.json is the machine-readable half of the
// disclosure, and it is the SAME object the projection publishes as `shape` (validated against the
// published schema by TestCLIDryRun_ProjectionValidatesAgainstItsPublishedSchema). What this pins
// is that the artifact on disk carries the payload at all — an agent that reads the run directory
// rather than stdout must not get the cheaper half of the answer.
func TestDryRun_ShapeArtifactCarriesThePayload(t *testing.T) {
	m, _ := countingPanel(t)
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel", DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	b, rerr := os.ReadFile(filepath.Join(out.RunDir, "run-shape.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var got struct {
		Payload *review.ShapePayload `json:"payload"`
	}
	if uerr := json.Unmarshal(b, &got); uerr != nil {
		t.Fatal(uerr)
	}
	if got.Payload == nil {
		t.Fatalf("run-shape.json has no payload:\n%s", b)
	}
	if got.Payload.Files != 1 || len(got.Payload.Paths) != 1 || got.Payload.Paths[0] != "main.go" {
		t.Errorf("payload = %+v, want the workspace's single main.go", *got.Payload)
	}
}

type unavailableAdapter struct{ name string }

func (u unavailableAdapter) Name() string                      { return u.name }
func (u unavailableAdapter) Available() (bool, string)         { return false, "not installed (test)" }
func (u unavailableAdapter) Evidence() review.IdentityEvidence { return review.EvidenceInvocationTag }
func (u unavailableAdapter) Invoke(context.Context, model.Call) (model.Result, error) {
	panic("an unavailable adapter must never be invoked")
}
