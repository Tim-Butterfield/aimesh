package fake

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// The fake is the adapter every demo run, every CLI/ACP end-to-end test and the golden-run gate execute
// on, and it is MODE-AWARE without a mode field: it keys on distinctive phrases in the app-owned prompt
// to decide which shape to emit. That coupling is invisible — a new mode (or a reworded prompt) silently
// falls through to the generic `explore()` shape, and the failure surfaces far away as a dropped
// explorer. These tests close that: for EVERY registered mode, the fake's round-1 answer must validate
// against that mode's OWN app-owned explorer schema.

// modeTask is one registered mode's minimal VALID round-1 task — enough to satisfy the mode's own
// ValidateTask (challenge needs an artifact; the fixed-space modes need their declared space) so the
// prompt the fake is handed is the real one a run would send.
func modeTask(name string) schema.RawTask {
	raw := schema.RawTask{Purpose: "choose a datastore", Criteria: []string{"cost", "latency"}, Mode: name}
	switch name {
	case mode.Challenge:
		raw.Artifact = "The shutdown handler frees state without taking a lock."
	case mode.Compare:
		raw.Options = []string{"Postgres", "SQLite", "MySQL"}
		raw.CompareCriteria = []schema.CompareCriterion{
			{Name: "latency", Direction: schema.LowerIsBetter, Role: schema.RoleDimension},
			{Name: "cost", Direction: schema.LowerIsBetter, Role: schema.RoleDimension},
			{Name: "license", Role: schema.RoleFilter},
		}
	case mode.Forecast:
		raw.Target, raw.Unit, raw.Horizon = "monthly active users", "users", "Q4"
	}
	return raw
}

// modeTasks is the per-mode task table. It is keyed by mode name so TestFake_CoversEveryRegisteredMode
// can assert it covers the registry EXACTLY — a ninth mode landing without fake support fails there
// rather than silently degrading every demo/gate run that mode touches.
func modeTasks() map[string]schema.RawTask {
	out := map[string]schema.RawTask{}
	for _, name := range mode.Names() {
		out[name] = modeTask(name)
	}
	return out
}

// exploreOnce invokes the fake for one blind round-1 explorer call in the given mode and returns the
// decoded response object. It routes through the mode's OWN prompt builder — the fake's phrase matching
// is only meaningful against the real prompt.
func exploreOnce(t *testing.T, a *Adapter, spec mode.ModeSpec, raw schema.RawTask, modelName string) map[string]any {
	t.Helper()
	res, err := a.Invoke(context.Background(), model.Call{
		Phase:  schema.PhaseExplore,
		Model:  modelName,
		Effort: "high",
		Prompt: spec.Prompt(raw),
	})
	if err != nil {
		t.Fatalf("mode %q: fake invoke: %v", spec.Name, err)
	}
	var resp map[string]any
	if err := json.Unmarshal(res.Stdout, &resp); err != nil {
		t.Fatalf("mode %q: fake emitted non-JSON output: %v\n%s", spec.Name, err, res.Stdout)
	}
	return resp
}

// TestFake_ValidAgainstEveryModeExplorerSchema is the core per-mode contract: the `valid` scenario's
// round-1 answer satisfies each registered mode's fixed, app-owned explorer schema. A mode whose prompt
// the fake does not recognize falls through to the Map shape, which fails its schema here.
func TestFake_ValidAgainstEveryModeExplorerSchema(t *testing.T) {
	for name, raw := range modeTasks() {
		spec, ok := mode.Lookup(name)
		if !ok {
			t.Fatalf("mode %q is not registered", name)
		}
		t.Run(name, func(t *testing.T) {
			// The task the mode itself declares as sufficient — proof the fixture is a real run's task.
			if err := spec.CheckTask(raw); err != nil {
				t.Fatalf("test fixture task is not valid for mode %q: %v", name, err)
			}
			a := New("fake", "A", Valid)
			resp := exploreOnce(t, a, spec, raw, "fake-a")
			if err := schema.ValidateResponse(spec.ExplorerSchema(), resp); err != nil {
				b, _ := json.Marshal(resp)
				t.Errorf("mode %q: fake response does not satisfy the mode's explorer schema: %v\n%s", name, err, b)
			}
		})
	}
}

// TestFake_PanelDisagreesPerMode: a panel of fakes must produce genuinely DIFFERENT answers, or every
// count, frontier and outlier the host computes is vacuous. Each explorer's answer stays schema-valid.
func TestFake_PanelDisagreesPerMode(t *testing.T) {
	// The two personas a panel gets: the construction tag (most modes) and the model suffix (the
	// fixed-space modes, which share ONE registry entry per adapter NAME and so key on the per-call model).
	for name, raw := range modeTasks() {
		spec, _ := mode.Lookup(name)
		t.Run(name, func(t *testing.T) {
			seen := map[string]bool{}
			for _, p := range []struct{ tag, model string }{{"A", "fake-a"}, {"B", "fake-b"}} {
				resp := exploreOnce(t, New("fake", p.tag, Valid), spec, raw, p.model)
				if err := schema.ValidateResponse(spec.ExplorerSchema(), resp); err != nil {
					t.Fatalf("explorer %s: %v", p.tag, err)
				}
				b, _ := json.Marshal(resp)
				seen[string(b)] = true
			}
			if len(seen) != 2 {
				t.Errorf("mode %q: a 2-explorer panel of fakes produced IDENTICAL answers — nothing to synthesize", name)
			}
		})
	}
}

// TestFake_AbstentionValidAgainstEveryModeSchema pins the Abstain scenario's stated contract: a
// DELIBERATE abstention is a schema-VALID response in EVERY mode carrying the reserved marker (§1) —
// that is what makes it a recorded position rather than a dropped explorer, and what keeps it out of the
// denominator by the marker rather than by an empty field.
func TestFake_AbstentionValidAgainstEveryModeSchema(t *testing.T) {
	for name, raw := range modeTasks() {
		spec, _ := mode.Lookup(name)
		t.Run(name, func(t *testing.T) {
			resp := exploreOnce(t, New("fake", "A", Abstain), spec, raw, "fake-a")
			if err := schema.ValidateResponse(spec.ExplorerSchema(), resp); err != nil {
				t.Errorf("mode %q: an abstention must be schema-valid: %v", name, err)
			}
			if !schema.IsAbstention(resp) {
				t.Errorf("mode %q: the abstention is not marked with the reserved %q field", name, schema.AbstentionField)
			}
		})
	}
}

// TestFake_CoversEveryRegisteredMode fails when a mode is registered without a fake fixture, so the
// per-mode coverage above can never silently shrink. It also pins the current mode count, which is the
// number of explorer schemas the single fake adapter has to satisfy.
func TestFake_CoversEveryRegisteredMode(t *testing.T) {
	registered := mode.Names()
	covered := make([]string, 0, len(registered))
	for name := range modeTasks() {
		covered = append(covered, name)
	}
	slices.Sort(covered)
	if !slices.Equal(covered, registered) {
		t.Fatalf("fake mode coverage drifted from the registry:\n covered:    %v\n registered: %v", covered, registered)
	}
	if len(registered) != 8 {
		t.Errorf("registered modes = %d, want 8 — update this test WITH the fake fixture for the new mode", len(registered))
	}
}

// TestFake_SchemaInvalidScenarioStillFailsValidation guards the negative half: the drop-path scenario must
// really be rejected by the same validator, or the pipeline's dropped-explorer coverage tests prove nothing.
func TestFake_SchemaInvalidScenarioStillFailsValidation(t *testing.T) {
	spec, _ := mode.Lookup(mode.Map)
	resp := exploreOnce(t, New("fake", "A", SchemaInvalid), spec, modeTask(mode.Map), "fake-a")
	if err := schema.ValidateResponse(spec.ExplorerSchema(), resp); err == nil {
		t.Error("the schema_invalid scenario must FAIL validation (it is the dropped-explorer fixture)")
	}
}
