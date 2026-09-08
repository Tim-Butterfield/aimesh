package pipeline

// Tests for the DRY RUN (Options.DryRun) and the disclosure it produces.
//
// The load-bearing one is TestDryRun_PricesExactlyWhatTheRunSpends: it runs each mode twice — once dry, once
// for real against a counting adapter — and requires the two numbers to be equal. A cost disclosure asserted
// against a hand-written expectation only proves the test and the code were written by the same person on the
// same afternoon; measured against the run it describes, it fails the moment the pipeline gains a stage the
// shape does not know about, which is the only way this can go wrong quietly.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/model/fake"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// countedPanel is panelOf with EVERY adapter wrapped in the counting adapter, and a func that totals the
// invocations across all of them.
//
// It counts at the adapter boundary rather than at any pipeline stage: that is the boundary a dry run
// promises nothing crosses, and it is the one a caller is billed at. Counting stages instead would let the
// shape and the test agree with each other while both disagreed with the provider's bill.
func countedPanel(t *testing.T, explorers []fake.Scenario, collator fake.Scenario) (Registry, roster.Plan, func() int) {
	t.Helper()
	reg, plan := panelOf(t, explorers, collator)
	var counters []*countingAdapter
	for name, a := range reg {
		c := counted(a)
		reg[name] = c
		counters = append(counters, c)
	}
	return reg, plan, func() int {
		total := 0
		for _, c := range counters {
			total += c.total()
		}
		return total
	}
}

// shapeCase is one registered mode with a task it accepts.
type shapeCase struct {
	name string
	task schema.RawTask
}

// shapeCases covers one mode per TERMINAL CONTRACT and one per governance grade, which between them reach
// every model-call site in the package: map (plain collate), catalog (canonicalizing, single canonicalizer,
// host-composed terminal), challenge (dual + confirmation + a mediated second round), shortlist (the same
// plus a ballot round) and compare (fixed space, narrative-only collate).
func shapeCases() []shapeCase {
	plain := func(m string) schema.RawTask {
		return schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: m}
	}
	return []shapeCase{
		{mode.Map, plain(mode.Map)},
		{mode.Catalog, plain(mode.Catalog)},
		{mode.Challenge, adjudicativeTask(mode.Challenge, theArtifact)},
		{mode.Shortlist, adjudicativeTask(mode.Shortlist, "")},
		{mode.Compare, compareTask()},
	}
}

// TestDryRun_MakesNoModelCall_NotEvenThePreflight is the flag's whole promise, and the reason its stop sits
// where it does. exploremesh's identity pre-flight INVOKES — one real call per governed role — so a dry run
// that ran it would spend, and "resolve everything, spend nothing" would be false for every mode.
func TestDryRun_MakesNoModelCall_NotEvenThePreflight(t *testing.T) {
	for _, tc := range shapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			reg, plan, calls := countedPanel(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
			res, err := RunSpec(context.Background(), reg, plan, tc.task, mustSpec(t, tc.name), Options{DryRun: true}, nil)
			if err != nil {
				t.Fatalf("dry run halted: %v", err)
			}
			if got := calls(); got != 0 {
				t.Errorf("a dry run made %d model call(s); it must make none — the pre-flight is a real call and the stop belongs in front of it", got)
			}
			if res.Shape == nil {
				t.Fatal("a dry run must populate Result.Shape")
			}
			// And nothing that only a real run produces.
			if len(res.Envelopes) != 0 || len(res.Rounds) != 0 || res.Output != nil || len(res.Preflight) != 0 {
				t.Errorf("a dry run produced run state: %d envelope(s), %d round(s), output=%v, %d preflight record(s)",
					len(res.Envelopes), len(res.Rounds), res.Output != nil, len(res.Preflight))
			}
		})
	}
}

// TestDryRun_PricesExactlyWhatTheRunSpends measures the disclosure against the thing it discloses: the dry
// run's ModelCalls must equal the invocations the same panel, task and mode actually make.
//
// It is stated as an EQUALITY rather than an upper bound because an exploration's cost is exact — the round
// count is fixed by the mode contract, and every other multiplier is settled before the run starts. A bound
// would pass while the shape quietly over-priced a mode, which is the failure a reader deciding whether to
// pay would be misled by.
func TestDryRun_PricesExactlyWhatTheRunSpends(t *testing.T) {
	for _, tc := range shapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			spec := mustSpec(t, tc.name)
			seats := []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}

			dryReg, dryPlan, dryCalls := countedPanel(t, seats, fake.Valid)
			dry, derr := RunSpec(context.Background(), dryReg, dryPlan, tc.task, spec, Options{DryRun: true}, nil)
			if derr != nil {
				t.Fatalf("dry run: %v", derr)
			}
			if got := dryCalls(); got != 0 {
				t.Fatalf("dry run spent %d call(s)", got)
			}

			realReg, realPlan, realCalls := countedPanel(t, seats, fake.Valid)
			if _, rerr := RunSpec(context.Background(), realReg, realPlan, tc.task, spec, Options{}, nil); rerr != nil {
				t.Fatalf("real run: %v", rerr)
			}

			priced, spent := dry.Shape.ModelCalls, realCalls()
			if priced != spent {
				t.Errorf("%s: the dry run priced %d model call(s); the real run made %d\nstages:\n%s",
					tc.name, priced, spent, stageLines(*dry.Shape))
			}
			// The total must be the sum of the stages, or the breakdown a reader is shown does not add up to
			// the number they are quoted.
			sum := 0
			for _, c := range dry.Shape.Calls {
				sum += c.Calls
			}
			if sum != priced {
				t.Errorf("%s: stages sum to %d but modelCalls says %d\n%s", tc.name, sum, priced, stageLines(*dry.Shape))
			}
		})
	}
}

// stageLines renders the per-stage breakdown for a failure message: when the total is wrong, the stage that
// is wrong is the thing worth printing.
func stageLines(sh Shape) string {
	var b strings.Builder
	for _, c := range sh.Calls {
		fmt.Fprintf(&b, "  %s", c.Phase)
		if c.Round > 0 {
			fmt.Fprintf(&b, " round %d", c.Round)
		}
		fmt.Fprintf(&b, ": %s × %d\n", c.Role, c.Calls)
	}
	return b.String()
}

// TestDryRun_PayloadIsTheBytesTheRunSends pins the strongest half of the disclosure. A review's dry run can
// only describe the files it WOULD read; an exploration's payload is app-owned and deterministic, so the dry
// run can show the exact prompt — and it is only worth showing if it is the same prompt.
func TestDryRun_PayloadIsTheBytesTheRunSends(t *testing.T) {
	task := schema.RawTask{Purpose: "explore X", Criteria: []string{"c1", "c2"}, Mode: mode.Map}
	spec := mustSpec(t, mode.Map)

	dryReg, dryPlan, _ := countedPanel(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	dry, err := RunSpec(context.Background(), dryReg, dryPlan, task, spec, Options{DryRun: true}, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	realReg, realPlan, _ := countedPanel(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	real, rerr := RunSpec(context.Background(), realReg, realPlan, task, spec, Options{}, nil)
	if rerr != nil {
		t.Fatalf("real run: %v", rerr)
	}

	wantHash, herr := real.Formulation.Payload.Hash()
	if herr != nil {
		t.Fatalf("hash the real payload: %v", herr)
	}
	if dry.Shape.Payload.PayloadHash != wantHash {
		t.Errorf("payload hash: dry run says %s, the run recorded %s", dry.Shape.Payload.PayloadHash, wantHash)
	}
	if dry.Shape.Payload.Prompt != real.Formulation.Payload.FinalPrompt {
		t.Error("the disclosed prompt is not the prompt the run sent")
	}
	if dry.Shape.Payload.PromptBytes != len(real.Formulation.Payload.FinalPrompt) {
		t.Errorf("promptBytes = %d, prompt is %d bytes", dry.Shape.Payload.PromptBytes, len(real.Formulation.Payload.FinalPrompt))
	}
	// Every explorer was sent that payload, so the round the run recorded carries the same hash.
	if len(real.Rounds) == 0 || real.Rounds[0].PayloadHash() != wantHash {
		t.Error("round 1 did not record the payload the shape disclosed")
	}
	if len(dry.Shape.Payload.SchemaFields) == 0 {
		t.Error("the response schema fields are part of what the call carries and must be disclosed")
	}
}

// TestDryRun_RefusesTheConfigErrorItIsThereToCatch: canonicalizer derivation is FREE, so a plan that cannot
// supply two independent identities must fail in the dry run rather than at the pre-flight — which is the
// first thing that spends, and where this error would otherwise surface.
func TestDryRun_RefusesTheConfigErrorItIsThereToCatch(t *testing.T) {
	// Every seat is the collator's own adapter and model, differing only in effort: a legal roster (a
	// different reasoning depth is a genuine vantage, so the triples are unique) that cannot yield a second
	// INDEPENDENT canonicalizer.
	reg := Registry{}
	c := counted(fake.New("solo", "A", fake.Valid))
	reg["solo"] = c
	counters := []*countingAdapter{c}
	exs := []roster.Explorer{
		{Adapter: "solo", Model: "one-model", Effort: "low"},
		{Adapter: "solo", Model: "one-model", Effort: "high"},
	}
	plan, perr := roster.Roster{Explorers: exs, Collator: roster.Collator{Adapter: "solo", Model: "one-model"}}.Plan()
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}

	_, err := RunSpec(context.Background(), reg, plan, adjudicativeTask(mode.Challenge, theArtifact),
		mustSpec(t, mode.Challenge), Options{DryRun: true}, nil)
	if err == nil {
		t.Fatal("a dual-canonicalizer mode on a single-model panel must be refused")
	}
	if !strings.Contains(err.Error(), "SECOND canonicalizer identity") {
		t.Errorf("the refusal should say why a second identity cannot be derived, got: %v", err)
	}
	spent := 0
	for _, c := range counters {
		spent += c.total()
	}
	if spent != 0 {
		t.Errorf("the refusal cost %d model call(s); the whole point is that it is free", spent)
	}
}

// TestDryRun_DisclosesWhatTheCanonicalizerPairIsWorth: a pair sharing one model behind two adapters is
// allowed, so the disclosure is the entire safeguard — and the dry run is the most useful place for it,
// because changing the pair still costs nothing here. After the run the same fact only explains a
// corroboration count the reader already has.
func TestDryRun_DisclosesWhatTheCanonicalizerPairIsWorth(t *testing.T) {
	reg := Registry{
		"fake":       fake.New("fake", "A", fake.Valid),
		"acp-claude": fake.New("acp-claude", "B", fake.Valid),
	}
	plan, perr := roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "acp-claude", Model: "one-model", Effort: "high"}, // the collator's model, other adapter
			{Adapter: "fake", Model: "other-model", Effort: "low"},
		},
		Collator: roster.Collator{Adapter: "fake", Model: "one-model"},
	}.Plan()
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	res, err := RunSpec(context.Background(), reg, plan, adjudicativeTask(mode.Challenge, theArtifact),
		mustSpec(t, mode.Challenge), Options{DryRun: true}, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := res.Shape.CanonicalizerIndependence; got != IndependenceSharedModel {
		t.Errorf("shape independence = %q, want %q — the pair runs one model behind two adapters", got, IndependenceSharedModel)
	}
	if res.Shape.CanonicalizerProvenance != ProvenanceDerived {
		t.Errorf("provenance = %q, want %q", res.Shape.CanonicalizerProvenance, ProvenanceDerived)
	}
}

// TestDryRun_ListsTheStagesThatCostNothing: a canonicalizing mode composes its terminal collation in-process,
// so it makes no collator call at all. That stage is listed with 0 calls rather than omitted — an absent
// stage reads as a forgotten one, and "the host does this for free" is a fact worth being able to see.
func TestDryRun_ListsTheStagesThatCostNothing(t *testing.T) {
	reg, plan, _ := countedPanel(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := RunSpec(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: mode.Catalog},
		mustSpec(t, mode.Catalog), Options{DryRun: true}, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var terminal *ShapeCall
	for i, c := range res.Shape.Calls {
		if c.Phase == schema.PhaseSynthesize {
			terminal = &res.Shape.Calls[i]
		}
	}
	if terminal == nil {
		t.Fatal("the terminal stage is not listed at all")
	}
	if terminal.Calls != 0 || terminal.Role != "host" {
		t.Errorf("a canonicalizing mode's terminal collation is host-composed and free; shape says role=%q calls=%d",
			terminal.Role, terminal.Calls)
	}
	if res.Shape.Policy.Terminal != TerminalCanonicalize {
		t.Errorf("terminal contract = %q, want %q", res.Shape.Policy.Terminal, TerminalCanonicalize)
	}
}
