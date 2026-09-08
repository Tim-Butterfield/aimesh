package mcp

import (
	"encoding/json"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the check that was missing.
//
// Every tool here DECLARES an `outputSchema`, and the review one is a strict `oneOf` on `state`
// precisely so that the governance-bearing fields can be `required` on the branch where a result
// exists. But nothing validated an emitted `structuredContent` against the schema its own tool
// advertised — so the declaration and the payload drifted, silently, in three places at once:
//
//  1. a CANCELLED call returned the *running* shape with `state` overwritten to "cancelled",
//     which satisfied no branch: not `running` (the const said otherwise), not `complete`, and not
//     the halt branch (no `exitCode`/`haltClass`/`reasonCode`);
//  2. `run_result` on a still-RUNNING review omitted the panel echo;
//  3. `run_result` for a REMEDIATION returned the receipt payload while declaring the report
//     schema — a shape the declaration said could not occur.
//
// The fix is the branches and the payloads; THIS is the part that keeps them together. It walks
// every result builder in the package, at every state each can produce, and validates the payload
// against the literal schema the tool declares. A new state, a new field, or a branch someone
// loosens to make a payload fit now has to survive this table.
//
// It is deliberately paired with a wire-level test (mcp_test.go) that re-reads the schema from
// `tools/list` rather than from these variables: this file proves the payloads match the constants,
// that one proves the constants are what a client is actually handed.

func conformanceOutcome() review.RunOutcome {
	return review.RunOutcome{
		Status: "single_pass", Mode: review.ModeReport,
		RunID: "20260101T000000-0001", RunDir: "/private/tmp/artifacts/20260101T000000-0001",
		Findings: []review.Finding{{
			ID: "F1", Title: "sample returns a magic number", Kind: review.KindRisk,
			Severity: review.SeverityMedium, File: "sample.go", Source: "reviewer",
		}},
		Decisions: []review.Decision{{
			FindingID: "F1", Valid: true, State: review.StateReportedValid,
			SupportingSeats: []review.SeatRef{
				{SeatID: "reviewer", Adapter: "fake", Model: "m1", IdentityTier: review.SeatIdentityVerified},
			},
			AgreementCount: 1, DissentingSeats: []string{"reviewer-2"},
		}},
		Panel: []review.SeatStatus{
			{SeatID: "reviewer", Index: 1, Adapter: "fake", Model: "m1", Status: "completed", Rounds: 1, Findings: 1, IdentityTier: "verified"},
			{SeatID: "reviewer-2", Index: 2, Adapter: "fake", Model: "m2", Status: "halted", Rounds: 0, Findings: 0, ReasonCode: "adapter_exited_nonzero"},
		},
		IdentityCaveats: []review.IdentityCaveat{{
			Role: "reviewer-2", Adapter: "fake", RequestedModel: "m2", Status: review.VerifSelfReported,
		}},
		Withheld: []review.WithheldFile{{Path: "vendor/x.go", Reason: "workspace_hardlink_denied", Stage: "copy"}},
		Authority: []review.AuthorityInclusion{{
			Name: "spec.md", Source: review.AuthoritySourcePath, FullHash: "sha256:aa", EmbeddedHash: "sha256:aa",
			BytesEmbedded: 10, BytesTotal: 10, Complete: true,
		}},
		ShownFiles: []string{"sample.go"},
	}
}

func conformanceView(err error) runview.View {
	return runview.Build(runview.Input{
		Outcome: conformanceOutcome(), RequestedMode: review.ModeReport, Err: err,
		ExitCode: int(fault.CodeOf(err)), ReasonCode: fault.ReasonOf(err), Signal: fault.SignalOf(err),
	})
}

func conformancePick() panelPick {
	return panelPick{
		source:    "adhoc",
		reviewers: []review.SeatSpec{{Adapter: "fake", Model: "m1"}, {Adapter: "fake", Model: "m2", Effort: "deep"}},
		roles: map[review.Role]review.SeatSpec{
			review.RoleAuthorRemediator: {Adapter: "fake", Model: "m1"},
		},
	}
}

func conformanceRecord(tool, mode string) *record {
	return &record{ID: "run-deadbeef", Tool: tool, Mode: mode, done: make(chan struct{}), state: StateRunning}
}

func conformanceReceipt(status string, committed bool) run.RemediateOutcome {
	return run.RemediateOutcome{
		RunID: "20260101T000000-0002", SourceRunID: "run-source", Mode: review.ModeApply,
		Cancelled: status == "cancelled",
		Receipt: run.Receipt{
			SchemaVersion: 1, RunID: "20260101T000000-0002", SourceRunID: "run-source",
			Mode: string(review.ModeApply), Status: status, BaseHashesVerified: 1,
			Intended: []run.IntendedHunk{{FindingID: "F1", File: "sample.go", Bytes: 12, Origin: "marker"}},
			Applied:  []run.AppliedFinding{{FindingID: "F1", File: "sample.go", State: "applied"}},
			Files:    []string{"sample.go"}, Committed: committed,
			PatchArtifact: "patches/changes.patch", PatchSHA256: "sha256:deadbeef",
		},
		Withheld: []review.WithheldFile{{Path: "vendor/x.go", Reason: "workspace_hardlink_denied", Stage: "copy"}},
	}
}

// conformancePartialRefusal is a COMMITTED write that refused one finding for a protected path —
// the shape §13.4.3 specifies. It rides `isError: true` on a `state: "complete"` payload, which
// is the combination most likely to be broken by a future edit that assumes the two move
// together.
func conformancePartialRefusal() run.RemediateOutcome {
	out := conformanceReceipt("complete", true)
	out.Refusals = []review.ApplyRefusal{{
		Fingerprint: "sha1:c0ffee", File: ".env",
		Reason: review.ApplyRefusalProtectedPath, FindingID: "F2",
	}}
	out.Receipt.NotApplied = []run.AppliedFinding{{
		FindingID: "F2", File: ".env", State: string(review.StateReportedValid),
		Reason: review.ApplyRefusalProtectedPath,
	}}
	return out
}

// TestEmittedPayloadsSatisfyTheirDeclaredOutputSchema is the table described above.
func TestEmittedPayloadsSatisfyTheirDeclaredOutputSchema(t *testing.T) {
	schemas := map[string]*jsonschema.Schema{}
	for tool, literal := range map[string]string{
		toolReport:    reportResultSchema,
		toolRemediate: remediateResultSchema,
		toolRunResult: runResultSchema,
		toolRunStatus: runStatusSchema,
	} {
		s, err := jsonschema.Compile([]byte(literal))
		if err != nil {
			t.Fatalf("%s: its declared outputSchema does not compile: %v", tool, err)
		}
		schemas[tool] = s
	}

	halt := fault.New(fault.Model, "adapter reported a different model").WithHalt("E").WithReason("model_identity_mismatch")
	cancelErr := fault.Wrap(fault.Policy, "remediation cancelled before apply", fault.New(fault.Policy, "cancelled")).
		WithHalt("F").WithReason("remediate_cancelled")

	// A review record that finished, so payloadFor returns the stored payload rather than the
	// running shape — the path `run_result` takes for a completed run.
	reportRec := conformanceRecord(toolReport, string(review.ModeReport))
	remediateRec := conformanceRecord(toolRemediate, string(review.ModeApply))

	completePayload, _, _ := completeResult(reportRec, conformancePick(), conformanceView(nil), false, true)
	inlinePayload, _, _ := completeResult(reportRec, conformancePick(), conformanceView(nil), true, false)
	haltPayload, _ := haltResult(reportRec, conformancePick(), conformanceView(halt), halt, false)
	haltCancelledPayload, _ := haltResult(reportRec, conformancePick(), conformanceView(halt), halt, true)
	// A refusal carrying NO run id — the branch with no panel to echo. This one is still reachable:
	// an authority document that cannot be resolved is refused before the run exists.
	refusal, _ := refusalResult("", fault.New(fault.Policy, "authority document cannot be resolved").
		WithHalt("policy").WithReason("authority_unreadable"))
	unknown, _ := refusalResult("run-nope", fault.New(fault.Usage, "unknown or expired runId").
		WithHalt("usage").WithReason("unknown_run_id"))

	reportRunning, _ := runningResult(reportRec, conformancePick(), 25)
	reportCancelled, _ := runningResult(reportRec, conformancePick(), 25)
	reportCancelled["state"] = StateCancelled
	reportCancelled["haltClass"], reportCancelled["reasonCode"] = "cancelled", "run_cancelled"

	remediateRunning, _ := runningResult(remediateRec, panelPick{}, 25)
	remediateCancelledInline, _ := runningResult(remediateRec, panelPick{}, 25)
	remediateCancelledInline["state"] = StateCancelled
	remediateCancelledInline["haltClass"], remediateCancelledInline["reasonCode"] = "cancelled", "run_cancelled"

	receiptComplete, _, _ := remediateResult(remediateRec, conformanceReceipt("complete", true), nil, nil)
	receiptRefused, _, refusedIsErr := remediateResult(remediateRec, conformancePartialRefusal(), nil, nil)
	if !refusedIsErr {
		t.Fatal("a partial refusal must ride isError; the payload below would then be declaring a clean success")
	}
	// run_status's projection of the same run — the poller's view, which has its own schema.
	refusedStatus := map[string]any{
		"runId": remediateRec.ID, "state": StateComplete, "tool": toolRemediate,
		"mode": remediateRec.Mode, "elapsedSeconds": 1.5,
		"outcome": receiptRefused["outcome"], "refusedCount": 1,
	}
	receiptPatch, _, _ := remediateResult(remediateRec, conformanceReceipt("complete", false), nil, nil)
	receiptHalted, _, _ := remediateResult(remediateRec, conformanceReceipt("halted", false), halt, nil)
	receiptCancelled, _, _ := remediateResult(remediateRec, conformanceReceipt("cancelled", false), cancelErr, nil)
	// The patch receipt WITH its resource link — the shape a patch-mode remediation now actually
	// returns. It carries `receipt.patchResource`, so the declaration has to allow it.
	receiptLinked, _, _ := remediateResult(remediateRec, conformanceReceipt("complete", false), nil,
		&proto.Resource{URI: proto.ResourceURI(remediateRec.ID, patchArtifactName), Name: remediateRec.ID + "/" + patchArtifactName, MimeType: "text/x-diff"})

	// The payload `payloadFor` synthesizes for a run that has not finished. This is defect (2): it
	// must not omit the panel echo that the running branch requires.
	srv := &Server{}
	reportRec.attachPick(conformancePick())
	runningFromRegistry := structuredOf(t, srv.payloadFor(reportRec))
	remediateRunningFromRegistry := structuredOf(t, srv.payloadFor(remediateRec))

	cases := []struct {
		name    string
		tools   []string
		payload map[string]any
	}{
		{"review complete", []string{toolReport, toolRunResult}, completePayload},
		{"review complete (inline workspace)", []string{toolReport, toolRunResult}, inlinePayload},
		{"review halted", []string{toolReport, toolRunResult}, haltPayload},
		{"review cancelled after the spend", []string{toolReport, toolRunResult}, haltCancelledPayload},
		{"pre-spend refusal (admission)", []string{toolReport, toolRemediate, toolRunResult}, refusal},
		{"unknown runId", []string{toolRunResult, toolRunStatus, toolRemediate}, unknown},
		{"review still running", []string{toolReport, toolRunResult}, reportRunning},
		{"review cancelled during the inline wait", []string{toolReport, toolRunResult}, reportCancelled},
		{"review still running, fetched from the registry", []string{toolRunResult}, runningFromRegistry},
		{"remediation still running", []string{toolRemediate, toolRunResult}, remediateRunning},
		{"remediation cancelled during the inline wait", []string{toolRemediate, toolRunResult}, remediateCancelledInline},
		{"remediation still running, fetched from the registry", []string{toolRunResult}, remediateRunningFromRegistry},
		{"remediation receipt (apply, committed)", []string{toolRemediate, toolRunResult}, receiptComplete},
		{"remediation receipt (apply, partial refusal)", []string{toolRemediate, toolRunResult}, receiptRefused},
		{"run_status for a partially-refused remediation", []string{toolRunStatus}, refusedStatus},
		{"remediation receipt (patch, nothing committed)", []string{toolRemediate, toolRunResult}, receiptPatch},
		{"remediation receipt (patch, with a resource link)", []string{toolRemediate, toolRunResult}, receiptLinked},
		{"remediation halted", []string{toolRemediate, toolRunResult}, receiptHalted},
		{"remediation cancelled in the write window", []string{toolRemediate, toolRunResult}, receiptCancelled},
	}
	for _, tc := range cases {
		for _, tool := range tc.tools {
			t.Run(tc.name+" → "+tool, func(t *testing.T) {
				if err := schemas[tool].ValidateGo(tc.payload); err != nil {
					t.Fatalf("this payload does not satisfy the outputSchema %s declares: %v", tool, err)
				}
			})
		}
	}
}

// TestTheDriftTheSchemaNowCatches reconstructs the three payloads as they were emitted BEFORE this
// fix and asserts the declared schema rejects each one. Without it the table above proves only that
// the current payloads pass — which a schema loose enough to accept anything would also achieve.
// These three are the regression itself, kept executable.
func TestTheDriftTheSchemaNowCatches(t *testing.T) {
	report, err := jsonschema.Compile([]byte(reportResultSchema))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rec := conformanceRecord(toolReport, string(review.ModeReport))

	// (1) A cancelled call MIS-BUILT as the RUNNING shape with `state` overwritten.
	// No branch accepts it — `running` says the state must be "running", and the halt branch wants
	// the taxonomy this payload never carried.
	oldCancelled, _ := runningResult(rec, conformancePick(), 25)
	oldCancelled["state"] = StateCancelled
	if err := report.Validate(mustRoundTrip(t, oldCancelled)); err == nil {
		t.Fatal("the old cancelled payload (running shape, state overwritten) validates — the schema has no teeth")
	}

	// (2) `run_result` on a still-running review, without the panel echo.
	noPanel := map[string]any{"runId": rec.ID, "state": StateRunning, "tool": rec.Tool, "mode": rec.Mode}
	if err := report.Validate(mustRoundTrip(t, noPanel)); err == nil {
		t.Fatal("a running payload with no panel echo validates — the governance field is not actually required")
	}

	// (3) A remediation receipt judged against the REPORT schema, which is what `run_result`
	// declared before it got its own union.
	receipt, _, _ := remediateResult(conformanceRecord(toolRemediate, string(review.ModeApply)),
		conformanceReceipt("complete", true), nil, nil)
	if err := report.Validate(mustRoundTrip(t, receipt)); err == nil {
		t.Fatal("a remediation receipt validates against the REVIEW schema — the two shapes are not actually distinguished")
	}
}

func mustRoundTrip(t *testing.T, v map[string]any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
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

// authorityMaxItemsFromSchema reads the bound out of the DECLARED input schema of review_report —
// the JSON a client is handed — rather than out of the Go constant, so the assertion covers the
// wiring and not just the identifier.
func authorityMaxItemsFromSchema(t *testing.T) int {
	t.Helper()
	var doc struct {
		OneOf []struct {
			Properties struct {
				Authority struct {
					MaxItems *int `json:"maxItems"`
				} `json:"authority"`
			} `json:"properties"`
		} `json:"oneOf"`
	}
	if err := json.Unmarshal([]byte(reportInputSchema), &doc); err != nil {
		t.Fatalf("review_report's declared input schema does not parse: %v", err)
	}
	if len(doc.OneOf) == 0 || doc.OneOf[0].Properties.Authority.MaxItems == nil {
		t.Fatalf("review_report's input schema declares no `authority.maxItems` — the bound a caller learns from the declaration has gone missing")
	}
	return *doc.OneOf[0].Properties.Authority.MaxItems
}

// TestAuthoritySchemaBoundMatchesEngineCap holds the `authority` declaration to the engine's actual
// cap. The schema advertised `maxItems: 16` while authority.MaxDocs was 8: fail-closed (the engine
// refused), but the declaration over-promised, so a caller who trusted it met a refusal the schema
// had said would not come. The constant is now derived; this asserts the derivation reaches the
// emitted JSON rather than only the Go identifier.
func TestAuthoritySchemaBoundMatchesEngineCap(t *testing.T) {
	if MaxAuthorityDocs != authority.MaxDocs {
		t.Fatalf("MaxAuthorityDocs = %d, but the engine enforces %d", MaxAuthorityDocs, authority.MaxDocs)
	}
	got := authorityMaxItemsFromSchema(t)
	if got != authority.MaxDocs {
		t.Fatalf("the `authority` input schema advertises maxItems %d, but the engine enforces %d — a caller who trusts the declaration meets a refusal it promised would not come", got, authority.MaxDocs)
	}
}
