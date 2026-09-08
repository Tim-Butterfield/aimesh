package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/model/fake"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// env builds a registry + roster.Plan from (explorerScenario, collatorScenario) specs. Each explorer
// gets its own uniquely-named fake adapter so the roster's uniqueness rule is satisfied.
func env(t *testing.T, explorers []fake.Scenario, collator fake.Scenario) (Registry, roster.Plan) {
	t.Helper()
	reg := Registry{}
	tags := []string{"A", "B", "C", "D"}
	var exs []roster.Explorer
	for i, sc := range explorers {
		name := "explorer-" + tags[i]
		reg[name] = fake.New(name, tags[i], sc)
		exs = append(exs, roster.Explorer{Adapter: name, Model: "model-" + tags[i], Effort: "high"})
	}
	reg["collator"] = fake.New("collator", "C", collator)
	r := roster.Roster{Explorers: exs, Collator: roster.Collator{Adapter: "collator", Model: "collator-model"}}
	plan, err := r.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return reg, plan
}

func run(t *testing.T, reg Registry, plan roster.Plan) (Result, error) {
	t.Helper()
	return Run(context.Background(), reg, plan, schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}}, Options{}, nil)
}

func TestRun_HappyPath_SynthesisAndDisagreement(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("expected success, got halt: %v", err)
	}
	// Map is FORMULATION-FREE (§3): the collator's formulate step is skipped, so the payload is the
	// mode's app-owned prompt + fixed schema and the source records the formulation-free provenance.
	if res.Formulation.Source != schema.FormulationFreeMap {
		t.Errorf("expected formulation-free map source, got %q", res.Formulation.Source)
	}
	if res.Formulation.CollatorAttempted {
		t.Error("formulation-free map must NOT record a collator formulate attempt")
	}
	// The explorers were validated against the fixed app-owned (minimum) schema, and the deterministic
	// app-owned prompt preserves the task purpose verbatim.
	if err := schema.ExpandedSatisfiesMinimum(res.Formulation.Payload.ExpandedSchema); err != nil {
		t.Errorf("formulation-free payload schema is not the fixed app-owned schema: %v", err)
	}
	if !strings.Contains(res.Formulation.Payload.FinalPrompt, "explore X") {
		t.Errorf("app-owned prompt dropped the task purpose:\n%s", res.Formulation.Payload.FinalPrompt)
	}
	out := mapOutput(t, res)
	if out.SynthesisSummary == "" {
		t.Error("empty synthesis summary")
	}
	if len(out.DisagreementRegister) == 0 {
		t.Error("expected a disagreement register")
	}
	if len(res.Envelopes) != 2 {
		t.Errorf("expected 2 envelopes, got %d", len(res.Envelopes))
	}
	// Every explorer received the SAME payload hash (the §6.2 byte-identity invariant).
	if res.Envelopes[0].PayloadHash == "" || res.Envelopes[0].PayloadHash != res.Envelopes[1].PayloadHash {
		t.Errorf("explorers did not share one payload hash: %q vs %q", res.Envelopes[0].PayloadHash, res.Envelopes[1].PayloadHash)
	}
}

func TestRun_ExplorerDropped_StillTwoVerified(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.SchemaInvalid}, fake.Valid)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("run should survive one dropped explorer: %v", err)
	}
	if len(res.Dropped) != 1 {
		t.Errorf("expected 1 dropped explorer, got %d", len(res.Dropped))
	}
	if len(res.Envelopes) != 2 {
		t.Errorf("expected 2 surviving envelopes, got %d", len(res.Envelopes))
	}
	if res.Output == nil {
		t.Error("expected synthesis to proceed with 2 verified")
	}
}

// TestRun_FencedExplorer_Recovered pins the extraction rule: an explorer that wraps VALID JSON in a ```json
// fence (the real 3-provider dogfood halt) is recovered via extraction — kept in the panel with the
// applied repair recorded on its envelope — not dropped on "invalid character '`'".
func TestRun_FencedExplorer_Recovered(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.FencedResponse}, fake.Valid)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("a fenced-but-valid explorer must be recovered, not halt: %v", err)
	}
	if len(res.Dropped) != 0 {
		t.Fatalf("fenced explorer was dropped: %+v", res.Dropped)
	}
	if len(res.Envelopes) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(res.Envelopes))
	}
	var fenced *schema.Envelope
	for i := range res.Envelopes {
		if len(res.Envelopes[i].Repairs) > 0 {
			fenced = &res.Envelopes[i]
		}
	}
	if fenced == nil {
		t.Fatal("no envelope recorded an extraction repair for the fenced explorer")
	}
	if fenced.Repairs[0] != schema.RepairStrippedCodeFence {
		t.Errorf("expected %q repair, got %v", schema.RepairStrippedCodeFence, fenced.Repairs)
	}
}

func TestRun_BelowTwoVerified_Halts(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.SchemaInvalid}, fake.Valid)
	res, err := run(t, reg, plan)
	if err == nil {
		t.Fatal("a one-verified panel must halt")
	}
	if res.Output != nil {
		t.Error("no synthesis should be produced on a sub-panel halt")
	}
}

// TestRun_MapMakesNoCollatorFormulateCall proves the formulation-free Map path never asks the collator
// to formulate: a collator whose formulate() output is malformed (fake.BadFormulate) is completely
// irrelevant, because the collator is only ever called to synthesize (there is no formulate leg at
// all). The run succeeds with the formulation-free source recorded.
func TestRun_MapMakesNoCollatorFormulateCall(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.BadFormulate)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("there is no formulate call — a bad formulate output must be irrelevant, got halt: %v", err)
	}
	if res.Formulation.Source != schema.FormulationFreeMap {
		t.Errorf("expected formulation-free map source, got %q", res.Formulation.Source)
	}
	if res.Formulation.CollatorAttempted {
		t.Error("no run may record a collator formulate attempt")
	}
	if res.Output == nil {
		t.Error("run should complete on the formulation-free map path")
	}
}

// TestRunSpec_NonFormulationFreeMode_Halts pins the fail-closed guard: there is NO collator-formulated
// round-1 leg, so a spec handed to RunSpec that does not declare FormulationFree is refused
// as a mode-contract config halt rather than silently running a path that does not exist.
func TestRunSpec_NonFormulationFreeMode_Halts(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	spec, _ := mode.Lookup(mode.Map)
	spec.FormulationFree = false
	_, err := RunSpec(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}},
		spec, Options{}, nil)
	if err == nil {
		t.Fatal("a non-formulation-free mode must halt")
	}
	if !strings.Contains(err.Error(), "formulation-free") {
		t.Errorf("halt message should name the formulation-free contract, got: %v", err)
	}
}

// TestRun_UnknownMode_Halts: an unknown task mode is a config halt whose message lists the known modes
// (defense in depth — the surfaces reject it first, but the pipeline must not run an unknown mode).
func TestRun_UnknownMode_Halts(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	_, err := Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: "no-such-mode"},
		Options{}, nil)
	if err == nil {
		t.Fatal("an unknown mode must halt")
	}
	if !strings.Contains(err.Error(), "unknown mode") || !strings.Contains(err.Error(), "map") {
		t.Errorf("halt message should name the unknown mode + list known modes, got: %v", err)
	}
}

// TestRun_ExplicitMapMode_SameAsDefault: naming "map" explicitly is identical to omitting the mode.
func TestRun_ExplicitMapMode_SameAsDefault(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: "map"},
		Options{}, nil)
	if err != nil {
		t.Fatalf("explicit map mode should run: %v", err)
	}
	if res.Formulation.Source != schema.FormulationFreeMap {
		t.Errorf("explicit map must be formulation-free, got %q", res.Formulation.Source)
	}
}

// --- Identity is recorded, never acted on (docs/model-identity.md) ---
//
// These four tests are the executable form of the rule. Each drives an identity outcome that USED to
// stop the run — a weak collator, a proven collator mismatch, a proven explorer mismatch, a weak
// explorer — and asserts the run completes with the response used and the identity recorded as a
// caveat. Whether a finding is worth anything is decided by its content; a label about which model
// produced it cannot make a real finding false, and (per the codex echo test) that label cannot be
// trusted to be true in the first place.

func TestRun_CollatorSelfReported_ProceedsWithCaveat(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.SelfReported)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("a self_reported collator must not stop the run: %v", err)
	}
	if res.Output == nil {
		t.Fatal("expected a synthesis from the self_reported collator")
	}
	if res.CollatorStatus != schema.IdentitySelfReported {
		t.Errorf("collator status = %q, want self_reported RECORDED on the result", res.CollatorStatus)
	}
	if res.CollatorCaveat == "" {
		t.Error("a weak collator identity must be surfaced as a caveat")
	}
}

func TestRun_CollatorMismatch_ProceedsWithCaveat(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.IdentityMismatch)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("a proven collator mismatch must not stop the run: %v", err)
	}
	if res.Output == nil {
		t.Fatal("expected a synthesis despite the collator mismatch")
	}
	if res.CollatorStatus != schema.IdentityMismatch {
		t.Errorf("collator status = %q, want mismatch RECORDED on the result", res.CollatorStatus)
	}
	if !strings.Contains(res.CollatorCaveat, "MISMATCH") {
		t.Errorf("a mismatched collator needs a prominent caveat, got %q", res.CollatorCaveat)
	}
}

func TestRun_ExplorerMismatch_ProceedsAndIsCounted(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.IdentityMismatch}, fake.Valid)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("a proven explorer mismatch must not stop the run: %v", err)
	}
	if len(res.Envelopes) != 2 {
		t.Fatalf("expected both explorers to produce envelopes, got %d", len(res.Envelopes))
	}
	var mismatched *schema.Envelope
	for i := range res.Envelopes {
		if res.Envelopes[i].IdentityStatus == schema.IdentityMismatch {
			mismatched = &res.Envelopes[i]
		}
	}
	if mismatched == nil {
		t.Fatal("the mismatching explorer's envelope is missing — it must be kept, labeled")
	}
	if mismatched.IdentityCaveat == "" {
		t.Error("a mismatched explorer must carry an identity caveat")
	}
	// It counts as a respondent: identity is not a category of non-response.
	if got := res.Panel.Respondents(); got != 2 {
		t.Errorf("respondents = %d, want 2 — identity must not shrink the denominator", got)
	}
}

func TestRun_WeakExplorer_IsPrimaryLikeAnyOther(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.SelfReported}, fake.Valid)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("run should proceed with 2 verified + 1 weak: %v", err)
	}
	if len(res.Envelopes) != 3 {
		t.Fatalf("expected 3 envelopes, got %d", len(res.Envelopes))
	}
	if got := res.Panel.Outcome.Eligible; got != 3 {
		t.Errorf("eligible = %d, want 3 — a weak seat is a full member of the primary panel", got)
	}
	// The synthesis prompt is built over the primary panel; the weak seat's alias must be in it.
	if !strings.Contains(res.SynthesizePrompt, "envelope#2") {
		t.Error("the weak-identity seat was withheld from the collator prompt")
	}
}

// TestRun_MaxParallel_BoundsParallelismWithoutDroppingWork exercises the concurrency semaphore. The
// property that matters is that maxParallel is a THROTTLE and not a filter: a panel larger than the
// limit must still come back with every explorer's envelope, because bounding how many CLIs run at
// once must never change who answered.
//
// It runs the same panel twice — throttled and unthrottled — because "every envelope arrived" is
// only evidence about the semaphore if the semaphore actually blocked. n is comfortably larger than
// the limit so it does.
func TestRun_MaxParallel_BoundsParallelismWithoutDroppingWork(t *testing.T) {
	const n = 12
	build := func() (Registry, roster.Plan) {
		reg := Registry{}
		var exs []roster.Explorer
		for i := range n {
			name := fmt.Sprintf("explorer-%d", i)
			reg[name] = fake.New(name, fmt.Sprintf("t%d", i), fake.Valid)
			exs = append(exs, roster.Explorer{Adapter: name, Model: fmt.Sprintf("model-%d", i), Effort: "high"})
		}
		reg["collator"] = fake.New("collator", "C", fake.Valid)
		plan, err := roster.Roster{Explorers: exs, Collator: roster.Collator{Adapter: "collator", Model: "collator-model"}}.Plan()
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		return reg, plan
	}

	for _, tc := range []struct {
		name        string
		maxParallel int
	}{
		{"throttled to 3 at a time", 3},
		{"unset runs the whole panel at once", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, plan := build()
			res, err := Run(context.Background(), reg, plan,
				schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}},
				Options{MaxParallel: tc.maxParallel}, nil)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(res.Envelopes) != n {
				t.Errorf("maxParallel dropped work: expected %d envelopes, got %d", n, len(res.Envelopes))
			}
			if res.Output == nil {
				t.Error("expected synthesis over the whole panel")
			}
		})
	}
}

// explorerFanout is the rule itself: never above the panel, never below 1, and the caller's limit
// only when they set one. A ceiling that applied when unset is what this replaced.
func TestExplorerFanout(t *testing.T) {
	for _, tc := range []struct{ n, maxParallel, want int }{
		{n: 5, maxParallel: 0, want: 5},  // unset: the whole panel at once
		{n: 5, maxParallel: 2, want: 2},  // the caller's limit
		{n: 2, maxParallel: 9, want: 2},  // never more than the panel has
		{n: 0, maxParallel: 0, want: 1},  // degenerate
		{n: 5, maxParallel: -1, want: 5}, // a negative is not a throttle
	} {
		if got := explorerFanout(tc.n, tc.maxParallel); got != tc.want {
			t.Errorf("explorerFanout(%d, %d) = %d, want %d", tc.n, tc.maxParallel, got, tc.want)
		}
	}
}

// TestRun_FlatPath_Deterministic pins the current flat (non-graph) synthesis path: given the
// deterministic fakes, the pipeline output must be byte-identical across runs. It is the regression
// sentinel that later capture/envelope surgery (W3) must not silently perturb.
func TestRun_FlatPath_Deterministic(t *testing.T) {
	marshal := func() []byte {
		reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
		r, err := run(t, reg, plan)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		b, err := json.Marshal(r.Output)
		if err != nil {
			t.Fatalf("marshal output: %v", err)
		}
		return b
	}
	if a, b := marshal(), marshal(); string(a) != string(b) {
		t.Errorf("flat pipeline output is not deterministic across runs:\n%s\nvs\n%s", a, b)
	}
}

func TestEnvelopeID_DeterministicUnique(t *testing.T) {
	id := schema.ExplorerIdentity{Adapter: "a", Model: "m", Effort: "high"}
	base := envelopeID("ph1", id, 0)
	if base != envelopeID("ph1", id, 0) {
		t.Error("same inputs must yield the same ID (determinism)")
	}
	if base == envelopeID("ph1", id, 1) {
		t.Error("different order must yield a different ID")
	}
	if base == envelopeID("ph2", id, 0) {
		t.Error("different payloadHash must yield a different ID")
	}
	if base == envelopeID("ph1", schema.ExplorerIdentity{Adapter: "b", Model: "m", Effort: "high"}, 0) {
		t.Error("different adapter must yield a different ID")
	}
}

// refusingAdapter mimics a real CLI that REFUSES the request: it exits non-zero and writes the decisive
// error to STDERR after a banner, while producing NO stdout — and it returns no Go error (exactly how the
// shell adapter reports a non-zero exit that is not a spawn failure). This is the real codex behavior a
// dogfood hit: `{"status":400,"message":"The 'gpt-5-codex' model is not supported …"}`.
type refusingAdapter struct{}

func (refusingAdapter) Name() string                    { return "refuser" }
func (refusingAdapter) Available() (bool, string)       { return true, "found" }
func (refusingAdapter) Evidence() core.IdentityEvidence { return core.EvidenceCLIStatus }
func (refusingAdapter) Invoke(context.Context, model.Call) (model.Result, error) {
	return model.Result{
		ExitCode: 1,
		Stderr: []byte("OpenAI Codex v0.145.0\n--------\nmodel: gpt-5-codex\n--------\n" +
			`ERROR: {"status":400,"error":{"message":"The 'gpt-5-codex' model is not supported when using Codex with a ChatGPT account."}}` + "\n"),
	}, nil
}

// TestRun_RefusedExplorer_DropReasonNamesExitCodeAndStderr: when a CLI refuses the request there is no
// stdout, so the PARSE reason alone ("no JSON object found (empty response)") is honest but useless — it
// hides the cause. The drop must therefore carry the process failure signal: the exit code AND a bounded
// stderr excerpt (tail-weighted, since a CLI prints its banner first and its error last). The panel still
// completes on the two healthy explorers, so this is a diagnosability guarantee, not a halt.
func TestRun_RefusedExplorer_DropReasonNamesExitCodeAndStderr(t *testing.T) {
	reg := Registry{
		"explorer-A": fake.New("explorer-A", "A", fake.Valid),
		"explorer-B": fake.New("explorer-B", "B", fake.Valid),
		"refuser":    refusingAdapter{},
		"collator":   fake.New("collator", "C", fake.Valid),
	}
	r := roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "explorer-A", Model: "model-A", Effort: "high"},
			{Adapter: "explorer-B", Model: "model-B", Effort: "high"},
			{Adapter: "refuser", Model: "gpt-5-codex", Effort: "high"},
		},
		Collator: roster.Collator{Adapter: "collator", Model: "collator-model"},
	}
	plan, perr := r.Plan()
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("a refused explorer must be a DROP, not a halt (2 healthy explorers remain): %v", err)
	}
	if len(res.Dropped) != 1 {
		t.Fatalf("want exactly the refused explorer dropped, got %d", len(res.Dropped))
	}
	d := res.Dropped[0]
	if d.ExitCode != 1 {
		t.Errorf("drop must record the adapter exit code, got %d", d.ExitCode)
	}
	if !strings.Contains(d.StderrExcerpt, "not supported when using Codex") {
		t.Errorf("drop must record the decisive stderr excerpt, got %q", d.StderrExcerpt)
	}
	// The one-line REASON must name the cause on its own (it is what a halt message / manifest shows).
	if !strings.Contains(d.Reason, "exit code 1") || !strings.Contains(d.Reason, "not supported when using Codex") {
		t.Errorf("drop reason must name the exit code + stderr cause, got %q", d.Reason)
	}
}

// TestStderrExcerpt_TailWeightedAndBounded pins the excerpt policy: keep the LAST non-blank lines (the
// failure), drop blanks, collapse to one line, and clip a runaway stream.
func TestStderrExcerpt_TailWeightedAndBounded(t *testing.T) {
	if got := stderrExcerpt(nil); got != "" {
		t.Errorf("no stderr → empty excerpt, got %q", got)
	}
	got := stderrExcerpt([]byte("banner\n\nline1\n\nline2\nline3\nTHE ERROR\n"))
	if strings.Contains(got, "banner") {
		t.Errorf("excerpt must be tail-weighted (banner dropped), got %q", got)
	}
	if !strings.Contains(got, "THE ERROR") {
		t.Errorf("excerpt must keep the final error line, got %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("excerpt must be a single line, got %q", got)
	}
	long := stderrExcerpt([]byte(strings.Repeat("x", maxStderrExcerpt*2)))
	if len(long) <= maxStderrExcerpt || !strings.HasSuffix(long, "(clipped)") {
		t.Errorf("a runaway stream must be clipped + marked, got %d bytes", len(long))
	}
}

// capAdapter returns a chosen evidence tier and declares a chosen ceiling — to prove classify caps.
type capAdapter struct{ ceiling core.IdentityEvidence }

func (c capAdapter) Name() string                    { return "cap" }
func (c capAdapter) Available() (bool, string)       { return true, "" }
func (c capAdapter) Evidence() core.IdentityEvidence { return c.ceiling }
func (c capAdapter) Invoke(context.Context, model.Call) (model.Result, error) {
	return model.Result{}, nil
}

// TestClassify_CapsEvidence: an adapter claiming a STRONG tier but declaring a self_report ceiling must
// be capped → self_reported, NOT verified (the exploremesh evidence-ceiling fix; uncapped it would
// inflate to verified).
func TestClassify_CapsEvidence(t *testing.T) {
	a := capAdapter{ceiling: core.EvidenceSelfReport}
	r := model.Result{ActualModel: "m", Evidence: core.EvidenceEnvelope} // claims the strongest tier
	status, capped := classify(a, r, "m", "cap")
	if capped != core.EvidenceSelfReport {
		t.Errorf("evidence not capped to the ceiling: got %q, want self_report", capped)
	}
	if status != schema.IdentitySelfReported {
		t.Errorf("capped self_report + model match must be self_reported, got %q (uncapped would be verified)", status)
	}
}

// TestRun_OnEvent_FiresProgress asserts the optional progress hook fires for the phase bookends and
// per-explorer events on a happy run, and that a nil hook (the CLI default) leaves the run working.
func TestRun_OnEvent_FiresProgress(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	var mu sync.Mutex
	var events []audit.EventLine
	onEvent := func(ev audit.EventLine) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	res, err := Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}}, Options{}, onEvent)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Output == nil {
		t.Fatal("no synthesis output")
	}
	seen := map[string]int{}
	for _, ev := range events {
		if ev.Timestamp == "" || ev.EventType == "" {
			t.Errorf("event missing timestamp/type: %+v", ev)
		}
		seen[ev.EventType]++
	}
	for _, want := range []string{"formulate_start", "formulate_done", "explorer_dispatch", "explorer_verify", "synthesize_start", "synthesize_done"} {
		if seen[want] == 0 {
			t.Errorf("expected an OnEvent of type %q, got events %v", want, seen)
		}
	}
	if seen["explorer_dispatch"] != 2 || seen["explorer_verify"] != 2 {
		t.Errorf("expected 2 dispatch + 2 verify events for a 2-explorer panel, got dispatch=%d verify=%d", seen["explorer_dispatch"], seen["explorer_verify"])
	}
}

// TestRun_OnEvent_HaltEmitsEvent asserts a halt (sub-panel) emits a halt event carrying the class.
func TestRun_OnEvent_HaltEmitsEvent(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.SchemaInvalid}, fake.Valid)
	var halts int
	onEvent := func(ev audit.EventLine) {
		if ev.EventType == "halt" {
			halts++
		}
	}
	if _, err := Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}}, Options{}, onEvent); err == nil {
		t.Fatal("a one-verified panel must halt")
	}
	if halts == 0 {
		t.Error("expected a halt OnEvent")
	}
}

// mapOutput type-asserts a Result's terminal output to the Map mode's CollatorOutput (Result.Output is the
// mode.ModeOutput interface). It fails the test when the output is absent or a different mode's type —
// pinning that a Map run yields a CollatorOutput.
func mapOutput(t *testing.T, res Result) schema.CollatorOutput {
	t.Helper()
	if res.Output == nil {
		t.Fatal("no collator output")
	}
	out, ok := res.Output.(schema.CollatorOutput)
	if !ok {
		t.Fatalf("expected a Map CollatorOutput, got %T", res.Output)
	}
	return out
}

// runMode runs the default fake panel under an explicit mode (the collate step is per-mode).
func runMode(t *testing.T, reg Registry, plan roster.Plan, modeName string) (Result, error) {
	t.Helper()
	return Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: modeName},
		Options{}, nil)
}

// TestRun_MapMode_ProducesCollatorOutput is the regression sentinel for Map against the per-mode collate
// contract: a Map run must yield a schema.CollatorOutput — formulation-free source, non-empty summary,
// disagreement register, and the weak-appendix behavior.
func TestRun_MapMode_ProducesCollatorOutput(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := runMode(t, reg, plan, "map")
	if err != nil {
		t.Fatalf("map run: %v", err)
	}
	if res.Formulation.Source != schema.FormulationFreeMap {
		t.Errorf("map must stay formulation-free, got %q", res.Formulation.Source)
	}
	out := mapOutput(t, res) // must be a CollatorOutput, not any other mode's type
	if out.SynthesisSummary == "" || len(out.DisagreementRegister) == 0 {
		t.Errorf("map CollatorOutput regressed: summary=%q disagreements=%d", out.SynthesisSummary, len(out.DisagreementRegister))
	}
}

// TestRun_SynthesizeMode_ProducesSynthesizeOutput exercises the new Synthesize mode end-to-end on fakes:
// the collate step selects/composes over the panel and yields a valid SynthesizeOutput (non-empty
// artifact; provenance + minority populated by fakes that return distinct answers). It also pins that
// Synthesize is formulation-free (no collator formulate call) — the app owns the round-1 contract (§5).
func TestRun_SynthesizeMode_ProducesSynthesizeOutput(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := runMode(t, reg, plan, "synthesize")
	if err != nil {
		t.Fatalf("synthesize run: %v", err)
	}
	if res.Formulation.Source != schema.FormulationFreeMap {
		// FormulationFreeMap is the shared formulation-free source label; synthesize reuses the
		// formulation-free path, so no collator formulate call was made.
		t.Errorf("synthesize must be formulation-free, got %q", res.Formulation.Source)
	}
	if res.Formulation.CollatorAttempted {
		t.Error("synthesize must make NO collator formulate call")
	}
	// The explorers were validated against the app-owned synthesize schema (answer/rationale/…), NOT the
	// Map minimum schema — the payload carries the synthesize explorer schema.
	fields := res.Formulation.Payload.ExpandedSchema.Fields
	if len(fields) == 0 || fields[0].Name != "answer" {
		t.Errorf("synthesize explorer schema is not the app-owned synthesize schema: %+v", fields)
	}
	out, ok := res.Output.(schema.SynthesizeOutput)
	if !ok {
		t.Fatalf("expected a SynthesizeOutput, got %T", res.Output)
	}
	if strings.TrimSpace(out.Artifact) == "" {
		t.Error("synthesize artifact must be non-empty")
	}
	if len(out.ComponentProvenance) == 0 {
		t.Error("expected component provenance populated")
	}
	if len(out.MinorityReport) == 0 {
		t.Error("expected a minority report populated")
	}
	if err := out.Validate(); err != nil {
		t.Errorf("SynthesizeOutput failed its own validation: %v", err)
	}
}

// TestRun_CatalogMode_ProducesCatalogOutput exercises the Catalog mode end-to-end on fakes: the
// formulation-free fan-out enumerates candidates, the DECOUPLED canonicalizer clusters them, the host
// records the append-only ledger + enforces surjectivity, and the collate assembles a CatalogOutput. It
// pins the headline canonicalization behavior: duplicates MERGE (multi-source) and a singleton is CARRIED
// (singleSource:true), surjectivity holds, and a partition revision hash is recorded.
func TestRun_CatalogMode_ProducesCatalogOutput(t *testing.T) {
	// fake explorer A → {Postgres, SQLite}; B → {Postgres, MySQL}. Postgres merges; SQLite/MySQL carry.
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := runMode(t, reg, plan, "catalog")
	if err != nil {
		t.Fatalf("catalog run: %v", err)
	}
	if res.Formulation.Source != schema.FormulationFreeMap {
		t.Errorf("catalog must be formulation-free, got %q", res.Formulation.Source)
	}
	if res.Formulation.CollatorAttempted {
		t.Error("catalog must make NO collator formulate call")
	}
	// The explorers were validated against the app-owned catalog schema (candidates[]).
	if _, ok := fieldNamed(res.Formulation.Payload.ExpandedSchema, "candidates"); !ok {
		t.Errorf("catalog explorer schema is not the app-owned candidates[] schema: %+v", res.Formulation.Payload.ExpandedSchema.Fields)
	}
	// The canonicalization result + append-only ledger are recorded (surjectivity held → non-nil).
	if res.Canonicalization == nil {
		t.Fatal("a catalog run must record the canonicalization result")
	}
	if res.Canonicalization.PartitionRevisionHash == "" {
		t.Error("a partition revision hash must be recorded")
	}
	// One ledger row per raw nomination: 2 explorers × 2 candidates = 4 nominations.
	if got := res.Canonicalization.Ledger.Len(); got != 4 {
		t.Errorf("expected 4 append-only ledger rows (one per nomination), got %d", got)
	}
	if res.CanonicalizerStatus != schema.IdentityVerified {
		t.Errorf("the fake canonicalizer must verify, got %q", res.CanonicalizerStatus)
	}

	out, ok := res.Output.(schema.CatalogOutput)
	if !ok {
		t.Fatalf("expected a CatalogOutput, got %T", res.Output)
	}
	if err := out.Validate(); err != nil {
		t.Errorf("CatalogOutput failed its own validation: %v", err)
	}
	// Every raw nomination lands in exactly one cluster member (surjectivity, via the output view).
	byName := map[string]schema.CatalogCluster{}
	totalMembers := 0
	for _, c := range out.Clusters {
		byName[c.Name] = c
		totalMembers += len(c.Members)
	}
	if totalMembers != 4 {
		t.Errorf("expected 4 total cluster members (one per nomination), got %d", totalMembers)
	}
	// Postgres merged both explorers → 2 members, NOT single-source.
	pg, ok := byName["Postgres"]
	if !ok {
		t.Fatal("Postgres cluster missing (duplicates did not merge)")
	}
	if len(pg.Members) != 2 {
		t.Errorf("Postgres must merge both nominations: got %d members", len(pg.Members))
	}
	for _, m := range pg.Members {
		if m.SingleSource {
			t.Error("a merged (multi-source) member must not be tagged single-source")
		}
	}
	// A carried singleton exists, tagged single-source (minority carry-through).
	carried := false
	for _, name := range []string{"SQLite", "MySQL"} {
		c, ok := byName[name]
		if !ok {
			t.Errorf("singleton %q was DROPPED (minority carry-through violated)", name)
			continue
		}
		if len(c.Members) == 1 && c.Members[0].SingleSource {
			carried = true
		}
	}
	if !carried {
		t.Error("expected at least one carried singleton tagged single-source")
	}
	if len(out.ProposedDimensions) == 0 {
		t.Error("expected proposed dimensions extracted by the canonicalizer")
	}
}

// TestRun_CatalogMode_SurjectivityGate_Halts pins that a canonicalizer that DROPS a nomination halts the
// run at the surjectivity gate — never a silent partial catalog. It swaps in a dropping fake canonicalizer
// via a custom adapter so the pipeline's canon.Canonicalize sees a partition missing a nomination.
func TestRun_CatalogMode_SurjectivityGate_Halts(t *testing.T) {
	reg := Registry{}
	tags := []string{"A", "B"}
	var exs []roster.Explorer
	for _, tag := range tags {
		name := "explorer-" + tag
		reg[name] = fake.New(name, tag, fake.Valid)
		exs = append(exs, roster.Explorer{Adapter: name, Model: "model-" + tag, Effort: "high"})
	}
	// The collator adapter is a DROPPING canonicalizer: for the canonicalize phase it omits the last
	// nomination from every cluster; for other phases it behaves like a valid fake.
	reg["collator"] = droppingCanonicalizer{fake.New("collator", "C", fake.Valid)}
	plan, err := roster.Roster{Explorers: exs, Collator: roster.Collator{Adapter: "collator", Model: "collator-model"}}.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, rerr := runMode(t, reg, plan, "catalog")
	if rerr == nil {
		t.Fatal("a canonicalizer that drops a nomination must halt at the surjectivity gate")
	}
	if !strings.Contains(rerr.Error(), "surjectivity gate") {
		t.Errorf("halt should name the surjectivity gate, got: %v", rerr)
	}
}

// droppingCanonicalizer wraps a fake adapter but, for the canonicalize phase, returns a proposal that
// DROPS the last nomination (removes the highest index from every cluster) — to exercise the pipeline's
// surjectivity-gate halt. All other phases delegate to the wrapped fake.
type droppingCanonicalizer struct{ *fake.Adapter }

func (d droppingCanonicalizer) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	r, err := d.Adapter.Invoke(ctx, c)
	if err != nil || c.Phase != schema.PhaseCanonicalize {
		return r, err
	}
	// Parse the fake's own valid proposal, then drop the highest member index across all clusters.
	var prop struct {
		Clusters []struct {
			CanonicalID   string `json:"canonicalId"`
			Name          string `json:"name"`
			MemberIndices []int  `json:"memberIndices"`
		} `json:"clusters"`
		ProposedDimensions []string `json:"proposedDimensions"`
		CoverageNotes      string   `json:"coverageNotes"`
	}
	_ = json.Unmarshal(r.Stdout, &prop)
	maxIdx := -1
	for _, cl := range prop.Clusters {
		for _, idx := range cl.MemberIndices {
			if idx > maxIdx {
				maxIdx = idx
			}
		}
	}
	for i := range prop.Clusters {
		var kept []int
		for _, idx := range prop.Clusters[i].MemberIndices {
			if idx != maxIdx {
				kept = append(kept, idx)
			}
		}
		prop.Clusters[i].MemberIndices = kept
	}
	b, _ := json.Marshal(prop)
	r.Stdout = b
	return r, nil
}

// fieldNamed reports whether schema s has a field named name (a local test helper).
func fieldNamed(s schema.Schema, name string) (schema.Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return schema.Field{}, false
}

// guard: the fake satisfies the meshcore adapter interface.
var _ model.Adapter = (*fake.Adapter)(nil)
