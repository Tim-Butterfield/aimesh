package mcp

import (
	"errors"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// This file is the check that was missing.
//
// Every tool here DECLARES an `outputSchema`, and the run-result one is a strict `oneOf` on `state`
// precisely so that the governance-bearing fields can be `required` on the branch where a result
// exists. But nothing validated an emitted `structuredContent` against the schema its own tool
// advertised — so the declaration and the payload drifted, silently, in two places:
//
//  1. a CANCELLED call returned the *running* shape with `state` overwritten to "cancelled", which
//     satisfied no branch: not `running` (the const said otherwise), not `complete`, and not the halt
//     branch (no `exitCode`/`haltClass`/`reasonCode`);
//  2. `run_result` on a still-RUNNING exploration omitted the panel echo the running branch requires.
//
// The fix is the branches and the payloads; THIS is the part that keeps them together. It walks every
// result builder in the package, at every state each can produce, and validates the payload against the
// literal schema the tool declares. A new state, a new field, or a branch someone loosens to make a
// payload fit now has to survive this table.
//
// It is deliberately paired with a wire-level test (mcp_test.go) that re-reads the schema from
// `tools/list` rather than from these variables: this file proves the payloads match the constants,
// that one proves the constants are what a client is actually handed.
//
// reviewmesh carries the identical pair of tests over the identical four-branch shape. The two servers
// answer the same protocol; they must not disagree about what `state` means.

func conformancePlan(t *testing.T) roster.Plan {
	t.Helper()
	plan, err := roster.Roster{
		Explorers: []roster.Explorer{{Adapter: "fake", Model: "m1"}, {Adapter: "fake", Model: "m2"}},
		Collator:  roster.Collator{Adapter: "fake", Model: "mc"},
	}.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan
}

func conformancePick(t *testing.T) panelPick {
	t.Helper()
	return panelPick{
		source:     "adhoc",
		reqSeats:   []slot{{Adapter: "fake", Model: "m1"}, {Adapter: "fake", Model: "m2", Effort: "deep"}},
		reqCollate: &slot{Adapter: "fake", Model: "mc"},
		plan:       conformancePlan(t),
		selected:   2,
		configured: 2,
	}
}

func conformanceTask() schema.RawTask {
	return schema.RawTask{
		Purpose:      "compare two datastores",
		Criteria:     []string{"latency", "cost"},
		Mode:         mode.Map,
		PriorContext: "an earlier exploration",
	}
}

// conformanceResult is a finished run WITH a governance report, so the populated governance block —
// not only the `countsEmitted: false` degenerate one — is judged against the schema.
func conformanceResult() pipeline.Result {
	return pipeline.Result{
		Mode:           mode.Map,
		Formulation:    schema.Formulation{Source: schema.FormulationFreeMap},
		CollatorStatus: schema.IdentityVerified,
		Envelopes: []schema.Envelope{
			{Identity: schema.ExplorerIdentity{Adapter: "fake", Model: "m1"}, IdentityStatus: schema.IdentityVerified},
			{
				Identity:         schema.ExplorerIdentity{Adapter: "fake", Model: "m2"},
				IdentityStatus:   schema.IdentitySelfReported,
				IdentityEvidence: "self_report",
				IdentityCaveat:   "the model only claimed its own identity",
			},
		},
		Dropped: []pipeline.Dropped{{Explorer: schema.ExplorerIdentity{Adapter: "fake", Model: "m3"}, Reason: "empty response"}},
		Output:  schema.CollatorOutput{SynthesisSummary: "synthesized", Findings: []schema.Finding{{Statement: "f1"}}},
		Governance: &govern.Report{
			Panel: govern.Panel{
				Members:    []schema.ExplorerIdentity{{Adapter: "fake", Model: "m1"}, {Adapter: "fake", Model: "m2"}},
				Selected:   2,
				PolicyHash: "sha256:policy",
			},
			RulesVersion: "v1",
			ClaimsHash:   "sha256:claims",
			Claims: []govern.Claim{
				{Subject: "a", Value: 2, Label: govern.LabelCorroborated},
				{Subject: "b", Value: 1, Label: govern.LabelWithheldBelowQuorum},
			},
		},
	}
}

func conformanceRec(tool, modeName string) *record {
	return &record{ID: "run-deadbeef", Tool: tool, Mode: modeName, done: make(chan struct{}), state: StateRunning}
}

// TestEmittedPayloadsSatisfyTheirDeclaredOutputSchema is the table described above.
func TestEmittedPayloadsSatisfyTheirDeclaredOutputSchema(t *testing.T) {
	result, err := jsonschema.Compile([]byte(runResultSchema))
	if err != nil {
		t.Fatalf("the declared run-result outputSchema does not compile: %v", err)
	}
	status, err := jsonschema.Compile([]byte(runStatusSchema))
	if err != nil {
		t.Fatalf("the declared run_status outputSchema does not compile: %v", err)
	}

	pick := conformancePick(t)
	task := conformanceTask()
	out := conformanceResult()
	halt := fault.New(fault.Adapter, "the adapter exited non-zero").
		WithHalt("adapter").WithReason("adapter_exited_nonzero")

	rec := conformanceRec("explore", mode.Map)

	complete, _ := completeResult(rec, task, pick, out, "20260101T000000-0001")

	// A mode that emits no host-computed counts still has to satisfy the same branch — `countsEmitted:
	// false` is what makes "this mode does not count" expressible instead of an omitted key.
	noCounts := conformanceResult()
	noCounts.Governance = nil
	completeNoCounts, _ := completeResult(rec, task, pick, noCounts, "")

	// A DRY RUN: the call completed, the exploration did not. It rides the `complete` branch (the call did
	// finish) carrying `dryRun` + `shape`, so the branch's required governance/identity keys must still be
	// satisfiable from a Result that holds no envelopes — which is the part worth pinning, because the
	// obvious way to add a shape is to add a fifth branch that nothing else in the family has.
	dry := conformanceResult()
	dry.Governance, dry.Envelopes, dry.Output = nil, nil, nil
	dry.Shape = &pipeline.Shape{
		Mode:      mode.Map,
		Explorers: []schema.ExplorerIdentity{{Adapter: "fake", Model: "m1"}, {Adapter: "fake", Model: "m2"}},
		Collator:  schema.ExplorerIdentity{Adapter: "fake", Model: "mc"},
		Rounds:    1,
		Policy:    pipeline.ShapePolicy{Terminal: pipeline.TerminalCollate},
		Calls: []pipeline.ShapeCall{
			{Phase: schema.PhasePreflight, Role: "governed roles", Calls: 1, Detail: "the first spend"},
			{Phase: schema.PhaseExplore, Round: 1, Role: "explorer", Calls: 2, Detail: "the blind round"},
			{Phase: schema.PhaseSynthesize, Role: "collator", Calls: 1, Detail: "the terminal collation"},
		},
		ModelCalls: 4,
		Payload:    pipeline.ShapePayload{Prompt: "explore X", PromptBytes: 9, PayloadHash: "sha256:payload", SchemaFields: []string{"claims"}},
	}
	dryRun, _ := completeResult(rec, task, pick, dry, "")

	halted, _ := haltResult(rec, pick, task, out, halt, "20260101T000000-0002", false)
	cancelledAfterSpend, _ := haltResult(rec, pick, task, out, halt, "", true)

	// A refusal carrying NO run id — the branch with no panel to echo. Nothing on the run-starting
	// path produces one any more (the admission bounds that did are gone), so this is a synthetic
	// payload asserting that the SHAPE still validates if a future refusal takes it.
	refusal, _ := refusalResult("", fault.New(fault.Policy, "run refused before it was admitted").
		WithHalt("policy").WithReason("run_refused"))
	unknown, _ := refusalResult("run-nope", fault.New(fault.Usage, "unknown or expired runId").
		WithHalt("usage").WithReason("unknown_run_id"))
	// An error no layer classified. It must still carry a reasonCode (E2) AND still validate.
	unclassified, _ := refusalResult("run-x", errors.New("something went wrong nobody typed"))

	running, _ := runningResult(rec, pick, 25)
	// The inline-wait cancellation, built exactly as startRun builds it.
	cancelledInline, _ := runningResult(rec, pick, 25)
	cancelledInline["state"] = StateCancelled
	cancelledInline["haltClass"], cancelledInline["reasonCode"] = "cancelled", "run_cancelled"

	// The payload `payloadFor` synthesizes for a run that has not finished. This is defect (2): it used
	// to omit the panel echo that the running branch requires.
	srv := &Server{}
	registryRec := conformanceRec("explore", mode.Compare)
	registryRec.attachPick(pick)
	runningFromRegistry := structuredOf(t, srv.payloadFor(registryRec))

	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"exploration complete", complete},
		{"exploration complete, a mode that emits no counts", completeNoCounts},
		{"dry run: the call completed, the exploration did not", dryRun},
		{"exploration halted", halted},
		{"exploration cancelled after the spend", cancelledAfterSpend},
		{"pre-spend refusal (admission)", refusal},
		{"unknown runId", unknown},
		{"a halt no layer classified", unclassified},
		{"exploration still running", running},
		{"exploration cancelled during the inline wait", cancelledInline},
		{"exploration still running, fetched from the registry", runningFromRegistry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := result.ValidateGo(tc.payload); err != nil {
				t.Fatalf("this payload does not satisfy the outputSchema its own tool declares: %v", err)
			}
		})
	}

	// run_status has its own (deliberately looser) schema; the payload it builds is judged against it.
	t.Run("explore_run_status", func(t *testing.T) {
		for _, p := range []map[string]any{
			{"runId": rec.ID, "state": StateRunning, "tool": rec.Tool, "mode": rec.Mode, "elapsedSeconds": 1.5, "panel": pick.echo(nil)},
			{"runId": rec.ID, "state": StateCancelled, "tool": rec.Tool, "mode": rec.Mode, "elapsedSeconds": 2.0, "captured": true},
		} {
			if err := status.ValidateGo(p); err != nil {
				t.Fatalf("run_status emitted a payload its declared schema rejects: %v", err)
			}
		}
	})
}

// TestTheDriftTheSchemaNowCatches reconstructs the two payloads as they were emitted BEFORE this fix
// and asserts the declared schema rejects each one. Without it the table above proves only that the
// current payloads pass — which a schema loose enough to accept anything would also achieve. These two
// are the regression itself, kept executable.
func TestTheDriftTheSchemaNowCatches(t *testing.T) {
	result, err := jsonschema.Compile([]byte(runResultSchema))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rec := conformanceRec("explore", mode.Map)

	// (1) A cancelled call MIS-BUILT as the RUNNING shape with `state` overwritten. No
	// branch accepts it — `running` says the state must be "running", and the cancelled branch wants
	// the taxonomy this payload never carried.
	oldCancelled, _ := runningResult(rec, conformancePick(t), 25)
	oldCancelled["state"] = StateCancelled
	if err := result.ValidateGo(oldCancelled); err == nil {
		t.Fatal("the old cancelled payload (running shape, state overwritten) validates — the schema has no teeth")
	}

	// (2) `run_result` on a still-running exploration, without the panel echo.
	noPanel := map[string]any{"runId": rec.ID, "state": StateRunning, "tool": rec.Tool, "mode": rec.Mode}
	if err := result.ValidateGo(noPanel); err == nil {
		t.Fatal("a running payload with no panel echo validates — the governance field is not actually required")
	}

	// (3) The halted/cancelled branches are `const`, not a shared enum: a payload may satisfy exactly
	// one. A halt shape wearing state "running" must be refused rather than matching two branches.
	confused, _ := haltResult(rec, conformancePick(t), conformanceTask(), conformanceResult(),
		fault.New(fault.Adapter, "boom").WithHalt("adapter").WithReason("adapter_exited_nonzero"), "", false)
	confused["state"] = "finished"
	if err := result.ValidateGo(confused); err == nil {
		t.Fatal("a payload with an undeclared state validates — the state consts are not doing their job")
	}
}

// TestReasonCodeIsNeverEmpty pins the machine-readability of `reasonCode` — the field the instructions,
// the tool descriptions and the docs all tell a caller to BRANCH ON. A field that is a stable machine
// code most of the time and "" the rest of the time forces every consumer to special-case it, and an
// empty string reads as "no error" to anything that tests truthiness.
//
// The guarantee is two-layered, and the point of this test is that it holds at BOTH layers:
//
//   - meshcore's fault.ReasonOf already returns a code-shaped fallback for any non-nil error (the
//     per-exit-code table: `internal_error` for something nobody classified). So the two servers agree
//     on an unclassified halt today, and neither can emit "" from a real error;
//   - the package-local reasonOf adds the last guard, for the one input meshcore answers "" to. It is
//     character-for-character reviewmesh's, so the two surfaces cannot drift apart here later.
func TestReasonCodeIsNeverEmpty(t *testing.T) {
	rec := conformanceRec("explore", mode.Map)
	plain := errors.New("an error that no layer classified")

	halted, _ := haltResult(rec, conformancePick(t), conformanceTask(), conformanceResult(), plain, "", false)
	if got, _ := halted["reasonCode"].(string); got == "" {
		t.Fatal("an unclassified halt emitted an EMPTY reasonCode — the field a caller is told to branch on must always carry a code")
	}
	refused, _ := refusalResult("run-x", plain)
	if got, _ := refused["reasonCode"].(string); got == "" {
		t.Fatal("an unclassified refusal emitted an EMPTY reasonCode")
	}
	// Both servers land on meshcore's stable per-code fallback for an unclassified error. Pinning the
	// literal is what stops one app quietly answering something else.
	if halted["reasonCode"] != "internal_error" || refused["reasonCode"] != "internal_error" {
		t.Fatalf("an unclassified error must yield meshcore's stable fallback, got %v / %v",
			halted["reasonCode"], refused["reasonCode"])
	}

	// The local guard, exercised on the one input meshcore answers "" to.
	if got := reasonOf(nil); got != "unclassified" {
		t.Fatalf("reasonOf(nil) = %q, want %q — the last guard against an empty machine field", got, "unclassified")
	}

	// A TYPED reason is never overwritten by either fallback.
	typed := fault.New(fault.Model, "the adapter reported a different model").
		WithHalt("model").WithReason("model_identity_mismatch")
	got, _ := refusalResult("run-y", typed)
	if got["reasonCode"] != "model_identity_mismatch" {
		t.Fatalf("a typed reason must survive: %v", got["reasonCode"])
	}

	// And a cancelled run says `run_cancelled`, not a fallback: cancellation IS classified.
	cancelled, _ := haltResult(rec, conformancePick(t), conformanceTask(), conformanceResult(), plain, "", true)
	if cancelled["reasonCode"] != "run_cancelled" || cancelled["haltClass"] != "cancelled" {
		t.Fatalf("a cancelled run must be classified as such, got %v/%v", cancelled["haltClass"], cancelled["reasonCode"])
	}
}

// structuredOf pulls the structuredContent off a CallToolResult.
func structuredOf(t *testing.T, res *proto.CallToolResult) map[string]any {
	t.Helper()
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T, want map[string]any", res.StructuredContent)
	}
	return m
}
