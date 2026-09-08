package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"

	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/mcp"
)

// The exploremesh MCP surface is driven in these tests by the OFFICIAL MCP Go SDK client
// (github.com/modelcontextprotocol/go-sdk), as a test-only dependency. That is deliberate: a hand-rolled
// server checked only by a hand-rolled client proves that two pieces of the same author's understanding
// agree, which is not the property anyone needs. The SDK is never imported by non-test code — `go build`
// pulls none of it — so the runtime posture stays dep-light while the conformance claim is real.

// --- fixtures ---

// fakeExplorer is a deterministic in-process Explorer. It never spawns anything: it emits the pipeline's
// real phase events, optionally blocks (for the job-shape and cancellation tests), and returns a canned
// result or halt.
type fakeExplorer struct {
	block chan struct{}
	err   error
	weak  bool // mark one explorer's identity as self-reported
	// memory, when set, is the cross-iteration disclosure the pipeline would have produced for a run the
	// operator enabled memory for.
	mu      sync.Mutex
	gotPlan roster.Plan
	gotTask schema.RawTask
	gotOpts pipeline.Options
	calls   int
}

func (f *fakeExplorer) Run(ctx context.Context, plan roster.Plan, raw schema.RawTask, opts pipeline.Options, onEvent func(audit.EventLine)) (pipeline.Result, error) {
	f.mu.Lock()
	f.gotPlan, f.gotTask, f.gotOpts, f.calls = plan, raw, opts, f.calls+1
	f.mu.Unlock()
	// A DRY RUN returns what the pipeline returns for one: a shape and nothing else. Standing in for the
	// pipeline here is what lets this test prove the OPTION reached it — the pipeline's own tests prove the
	// shape is right.
	if opts.DryRun {
		return pipeline.Result{Mode: raw.Mode, Shape: &pipeline.Shape{
			Mode:       raw.Mode,
			Explorers:  []schema.ExplorerIdentity{{Adapter: "fake", Model: "m1"}, {Adapter: "fake", Model: "m2"}},
			Collator:   schema.ExplorerIdentity{Adapter: "fake", Model: "mc"},
			Rounds:     1,
			Policy:     pipeline.ShapePolicy{Terminal: pipeline.TerminalCollate},
			Calls:      []pipeline.ShapeCall{{Phase: schema.PhaseExplore, Round: 1, Role: "explorer", Calls: 2, Detail: "the blind round"}},
			ModelCalls: 2,
			Payload:    pipeline.ShapePayload{Prompt: "explore X", PromptBytes: 9, PayloadHash: "sha256:payload", SchemaFields: []string{"claims"}},
		}}, nil
	}
	if onEvent != nil {
		for _, ev := range []string{"panel_frozen", "formulate_start", "formulate_done", "synthesize_start", "synthesize_done"} {
			onEvent(audit.EventLine{Timestamp: "2026-01-01T00:00:00Z", Level: "info", EventType: ev, Message: ev + " reached"})
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return pipeline.Result{}, ctx.Err()
		}
	}
	if f.err != nil {
		return pipeline.Result{Mode: raw.Mode, Dropped: []pipeline.Dropped{{Explorer: schema.ExplorerIdentity{Adapter: "fake", Model: "m1"}, Reason: "empty response"}}}, f.err
	}
	envs := []schema.Envelope{
		{Identity: schema.ExplorerIdentity{Adapter: "fake", Model: "m1"}, IdentityStatus: schema.IdentityVerified},
		{Identity: schema.ExplorerIdentity{Adapter: "fake", Model: "m2"}, IdentityStatus: schema.IdentityVerified},
	}
	if f.weak {
		envs[1].IdentityStatus = schema.IdentitySelfReported
		envs[1].IdentityEvidence = "self_report"
		envs[1].IdentityCaveat = "the model only claimed its own identity"
	}
	return pipeline.Result{
		Mode:           raw.Mode,
		Formulation:    schema.Formulation{Source: schema.FormulationFreeMap},
		CollatorStatus: schema.IdentityVerified,
		Envelopes:      envs,
		Output:         schema.CollatorOutput{SynthesisSummary: "synthesized", Findings: []schema.Finding{{Statement: "f1"}}},
	}, nil
}

func (f *fakeExplorer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeExplorer) plan() roster.Plan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotPlan
}

// opts is the Options the server actually handed the pipeline — how a test proves a per-call parameter was
// PLUMBED rather than merely accepted by the schema.
func (f *fakeExplorer) opts() pipeline.Options {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotOpts
}

// fakeConfig supplies the `list`/`doctor` projections. The readiness detail deliberately CONTAINS an
// absolute path, because meshcore composes those details and the surface's job is to strip them.
type fakeConfig struct{ set profile.Set }

func (f fakeConfig) ProfileSet() profile.Set { return f.set }

func (f fakeConfig) Adapters() []mcp.AdapterFact {
	return []mcp.AdapterFact{
		{Name: "fake", DisplayName: "Fake", Kind: "fake", Configured: true},
		{Name: "claude-code", DisplayName: "Claude Code", Kind: "shell", Configured: true, IdentityEvidenceCapability: "envelope"},
	}
}

func (f fakeConfig) Readiness() (bool, []mcp.ReadinessCheck) {
	return true, []mcp.ReadinessCheck{
		{Name: "roster: explorers >= 2", OK: true, Detail: "2 explorers"},
		{Name: "adapter: claude-code", OK: true, Detail: "required — found at /usr/local/bin/claude (version 1.2.3)"},
	}
}

func testPlan(t *testing.T) roster.Plan {
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

func testProfiles(t *testing.T) profile.Set {
	t.Helper()
	return profile.Set{
		SchemaVersion:  1,
		DefaultProfile: "default",
		Profiles: map[string]profile.Profile{
			"default": {
				Explorers: []roster.Explorer{{Adapter: "fake", Model: "m1"}, {Adapter: "fake", Model: "m2"}, {Adapter: "fake", Model: "m3"}},
				Collator:  roster.Collator{Adapter: "fake", Model: "mc"},
			},
			"pair": {
				Explorers: []roster.Explorer{{Adapter: "fake", Model: "p1"}, {Adapter: "fake", Model: "p2"}},
				Collator:  roster.Collator{Adapter: "fake", Model: "mc"},
				// One profile names its canonicalizers explicitly, so the projections have both provenances
				// to report and `derived` never gets to be the only shape a test ever sees.
				Canonicalizers: []roster.Explorer{{Adapter: "fake", Model: "canon-p1"}, {Adapter: "fake", Model: "canon-p2"}},
			},
		},
	}
}

// newServer builds a hermetic MCP server; tune applies per-test configuration before it serves.
func newServer(t *testing.T, exp mcp.Explorer, tune ...func(*mcp.Server)) *mcp.Server {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	t.Setenv("AIMESH_HOME", t.TempDir())
	s := &mcp.Server{
		Explorer: exp,
		Plan:     testPlan(t),
		Profiles: testProfiles(t),
		Adapters: []string{"claude-code", "fake"},
		Config:   fakeConfig{set: testProfiles(t)},
		// Capture is ON in production; tests disable it unless they are testing capture, so no test
		// writes a run directory into the repo.
		DisableCapture: true,
		Diagnostics:    io.Discard,
	}
	for _, f := range tune {
		f(s)
	}
	return s
}

// connect wires the server to in-memory pipes and returns an initialized OFFICIAL-SDK client session.
func connect(t *testing.T, s *mcp.Server, opts ...*sdk.ClientOptions) *sdk.ClientSession {
	t.Helper()
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	served := make(chan struct{})
	go func() { _ = s.Serve(sr, sw); close(served) }()

	var o *sdk.ClientOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "conformance-client", Version: "1"}, o)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	session, err := client.Connect(ctx, &sdk.IOTransport{Reader: cr, Writer: cw}, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		_ = cw.Close()
		<-served
		_ = sw.Close()
		cancel()
	})
	return session
}

func call(t *testing.T, s *sdk.ClientSession, tool string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

// structured decodes a result's structuredContent.
func structured(t *testing.T, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode structuredContent: %v", err)
	}
	return out
}

func textOf(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func exploreArgs(extra map[string]any) map[string]any {
	args := map[string]any{"purpose": "choose a datastore", "criteria": []string{"cost", "latency"}, "mode": "map"}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// --- declared contract ---

func TestToolsList_DeclaresTheContract(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	res, err := s.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if res.NextCursor != "" {
		t.Errorf("a handful of tools must come back in one page, got nextCursor %q", res.NextCursor)
	}
	byName := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}
	want := []string{"explore", "explore_list", "explore_doctor", "explore_run_status", "explore_run_result", "agents_md"}
	for _, n := range want {
		if byName[n] == nil {
			t.Fatalf("tool %q is not declared (got %v)", n, byName)
		}
	}
	// ANNOTATIONS. Only the read-only tools may claim readOnlyHint: the explore tools spawn
	// processes and spend money, and a host uses this hint to decide whether to ask the human first.
	// agents_md returns a document compiled into the binary — it starts nothing and spends nothing.
	readOnly := map[string]bool{"explore_list": true, "explore_doctor": true, "explore_run_status": true, "explore_run_result": true, "agents_md": true}
	for name, tool := range byName {
		if tool.Annotations == nil {
			t.Fatalf("tool %q declares no annotations", name)
		}
		if tool.Annotations.ReadOnlyHint != readOnly[name] {
			t.Errorf("tool %q readOnlyHint = %v, want %v", name, tool.Annotations.ReadOnlyHint, readOnly[name])
		}
		if !readOnly[name] {
			if tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
				t.Errorf("spending tool %q must declare openWorldHint true", name)
			}
			if tool.Annotations.IdempotentHint {
				t.Errorf("spending tool %q must not declare idempotentHint", name)
			}
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no inputSchema", name)
		}
	}
	// The run-starting tools and run_result declare an outputSchema in which the governance block, the
	// identity caveats and the panel echo are REQUIRED — that is the invariant a summary must never drop,
	// made client-side checkable instead of promised in prose.
	for _, name := range []string{"explore", "explore_run_result"} {
		schemaJSON, err := json.Marshal(byName[name].OutputSchema)
		if err != nil || byName[name].OutputSchema == nil {
			t.Fatalf("tool %q has no outputSchema", name)
		}
		var decoded struct {
			OneOf []struct {
				Required []string `json:"required"`
			} `json:"oneOf"`
		}
		if err := json.Unmarshal(schemaJSON, &decoded); err != nil {
			t.Fatalf("tool %q outputSchema: %v", name, err)
		}
		found := false
		for _, branch := range decoded.OneOf {
			set := map[string]bool{}
			for _, r := range branch.Required {
				set[r] = true
			}
			if set["governance"] && set["identityCaveats"] && set["panel"] {
				found = true
			}
		}
		if !found {
			t.Errorf("tool %q outputSchema has no branch requiring governance + identityCaveats + panel", name)
		}
	}
}

// TestToolsList_TitlesAgreeWithTheirAnnotations holds a tool's TITLE to the same standard as its
// hints. A title is what a human reads in a permission prompt while the host decides on the strength
// of the hint, so a disagreement between the two is exactly the mis-signal these annotations exist to
// prevent — reviewmesh shipped a tool titled "Review (read-only)" whose readOnlyHint was correctly
// false. Nothing in exploremesh conflicts today; this is what keeps it that way.
func TestToolsList_TitlesAgreeWithTheirAnnotations(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	res, err := s.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Annotations == nil {
			t.Fatalf("tool %q declares no annotations", tool.Name)
		}
		ro := tool.Annotations.ReadOnlyHint
		title := strings.ToLower(tool.Annotations.Title)
		desc := strings.ToLower(tool.Description)
		if !ro && strings.Contains(title, "read-only") {
			t.Errorf("%s: title %q claims read-only while readOnlyHint is false", tool.Name, tool.Annotations.Title)
		}
		if ro && (strings.Contains(title, "writes") || strings.Contains(title, "spends") || strings.Contains(title, "run a ")) {
			t.Errorf("%s: title %q claims to run, write or spend while readOnlyHint is true", tool.Name, tool.Annotations.Title)
		}
		// The DESCRIPTION is the other thing a client model reads before choosing. A read-only tool must
		// say so and must never advertise a spend; a spending tool must say SPENDS MONEY out loud.
		if ro && strings.Contains(desc, "spends money") {
			t.Errorf("%s: description advertises a spend while readOnlyHint is true", tool.Name)
		}
		if !ro && !strings.Contains(desc, "spends money") {
			t.Errorf("%s: readOnlyHint is false, so the description must say SPENDS MONEY", tool.Name)
		}
		if !ro && strings.Contains(desc, "read-only") {
			t.Errorf("%s: description claims read-only while readOnlyHint is false", tool.Name)
		}
	}
}

func TestInitialize_CarriesTheCrossToolContract(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	init := s.InitializeResult()
	if init == nil {
		t.Fatal("no initialize result")
	}
	if init.ServerInfo == nil || init.ServerInfo.Name != "exploremesh" {
		t.Errorf("serverInfo = %+v", init.ServerInfo)
	}
	for _, must := range []string{"HOST-COMPUTED", "governance block", "idempotencyKey", "never changes configuration"} {
		if !strings.Contains(init.Instructions, must) {
			t.Errorf("initialize.instructions must state %q; got:\n%s", must, init.Instructions)
		}
	}
	if init.Capabilities == nil || init.Capabilities.Tools == nil || init.Capabilities.Logging == nil {
		t.Errorf("capabilities = %+v, want tools + logging declared", init.Capabilities)
	}
}

// --- a successful run ---

func TestExplore_ReturnsGovernanceIdentityAndPanelEcho(t *testing.T) {
	exp := &fakeExplorer{weak: true}
	s := connect(t, newServer(t, exp))
	res := call(t, s, "explore", exploreArgs(nil))
	if res.IsError {
		t.Fatalf("unexpected isError: %s", textOf(res))
	}
	out := structured(t, res)
	if out["state"] != "complete" {
		t.Fatalf("state = %v, want complete", out["state"])
	}
	if out["mode"] != "map" {
		t.Errorf("mode = %v, want map", out["mode"])
	}
	if out["runId"] == "" || out["runId"] == nil {
		t.Error("no runId")
	}
	// The three fields the design refuses to let a summary drop.
	gov, _ := out["governance"].(map[string]any)
	if gov == nil {
		t.Fatal("no governance block")
	}
	if _, ok := gov["countsEmitted"]; !ok {
		t.Errorf("governance must state countsEmitted even for a mode that emits no counts: %v", gov)
	}
	caveats, ok := out["identityCaveats"].([]any)
	if !ok {
		t.Fatalf("identityCaveats missing or not an array: %v", out["identityCaveats"])
	}
	if len(caveats) != 1 {
		t.Fatalf("identityCaveats = %v, want the one self-reported seat", caveats)
	}
	first, _ := caveats[0].(map[string]any)
	if first["identityStatus"] != "self_reported" || first["role"] != "explorer" {
		t.Errorf("caveat = %v", first)
	}
	pan, _ := out["panel"].(map[string]any)
	if pan == nil || pan["requested"] == nil || pan["executed"] == nil {
		t.Fatalf("panel echo = %v, want both requested and executed", out["panel"])
	}
	// The text channel carries the same governance facts, because some clients show the model only text.
	text := textOf(res)
	for _, must := range []string{"Governance", "Identity caveats", "structuredContent"} {
		if !strings.Contains(text, must) {
			t.Errorf("human rendering must mention %q; got:\n%s", must, text)
		}
	}
	if exp.callCount() != 1 {
		t.Errorf("explorer ran %d times, want 1", exp.callCount())
	}
}

// TestExplore_DryRunReachesThePipelineAndAnswersWithTheShape is the MCP half of the surface-parity
// obligation: the CLI's --dry-run, MCP's `dryRun` and ACP's `_meta.exploremesh.dryRun` are one capability.
//
// It asserts the OPTION reached the pipeline rather than being accepted and dropped — the failure mode a
// spending tool must not have, because a caller who asked to spend nothing and was quietly given a real run
// finds out from their bill.
func TestExplore_DryRunReachesThePipelineAndAnswersWithTheShape(t *testing.T) {
	exp := &fakeExplorer{}
	s := connect(t, newServer(t, exp))
	res := call(t, s, "explore", exploreArgs(map[string]any{"dryRun": true}))
	if res.IsError {
		t.Fatalf("unexpected isError: %s", textOf(res))
	}
	if !exp.opts().DryRun {
		t.Fatal("dryRun was accepted on the wire but not passed to the pipeline — the run would have spent")
	}
	out := structured(t, res)
	if out["dryRun"] != true {
		t.Errorf("dryRun = %v, want true — a caller must not have to infer it from an empty result", out["dryRun"])
	}
	sh, _ := out["shape"].(map[string]any)
	if sh == nil {
		t.Fatalf("no shape block: %v", out)
	}
	// One number, not a range: the difference from reviewmesh's shape, and the thing a caller reports.
	if sh["modelCalls"] == nil || sh["payload"] == nil || sh["calls"] == nil {
		t.Errorf("the shape must carry modelCalls, the per-stage calls and the payload: %v", sh)
	}
	text := textOf(res)
	for _, must := range []string{"Dry run", "nothing was spent", "pre-flight was NOT run"} {
		if !strings.Contains(text, must) {
			t.Errorf("the text channel must say %q (some clients show the model only text); got:\n%s", must, text)
		}
	}
}

// --- domain halts ride isError, never a JSON-RPC error ---

func TestExplore_HaltRidesIsErrorWithTheTaxonomy(t *testing.T) {
	halt := fault.New(fault.Adapter, "adapter claude-code exited non-zero").WithHalt("A").WithReason("adapter_exited_nonzero")
	s := connect(t, newServer(t, &fakeExplorer{err: halt}))
	res := call(t, s, "explore", exploreArgs(nil))
	if !res.IsError {
		t.Fatalf("a halt must set isError; got %s", textOf(res))
	}
	out := structured(t, res)
	if out["haltClass"] != "A" || out["reasonCode"] != "adapter_exited_nonzero" {
		t.Errorf("taxonomy = %v", out)
	}
	if code, _ := out["exitCode"].(float64); int(code) != int(fault.Adapter) {
		t.Errorf("exitCode = %v, want %d", out["exitCode"], fault.Adapter)
	}
	if out["state"] != "halted" {
		t.Errorf("state = %v, want halted", out["state"])
	}
	failure, _ := out["failure"].(map[string]any)
	if failure == nil || failure["dropped"] == nil {
		t.Errorf("failure breakdown = %v, want the dropped seats", out["failure"])
	}
	// The human channel names the machine fields, so a text-only client still gets them.
	if !strings.Contains(textOf(res), "reasonCode=adapter_exited_nonzero") {
		t.Errorf("human rendering lost the taxonomy: %s", textOf(res))
	}
}

// --- teaching errors for malformed requests (the ONE thing that rides a protocol error) ---

func TestMalformedRequests_AreProtocolErrorsWithTeachingMessages(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"missing criteria", "explore", map[string]any{"purpose": "x", "criteria": []string{}, "mode": "map"}, "criteria"},
		{"blank purpose", "explore", map[string]any{"purpose": "  ", "criteria": []string{"c"}, "mode": "map"}, "purpose"},
		{"unknown mode", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "nope"}, "unknown mode"},
		{"genuinely unknown field", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "map", "nonesuch": "a"}, "unknown field"},

		// Required-for-this-mode.
		{"challenge without artifact", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "challenge", "artifact": ""}, "artifact"},
		{"compare with one option", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "compare", "options": []string{"a"}, "comparisonAxes": []any{map[string]any{"name": "cost", "direction": "lower_is_better"}}}, "at least 2"},
		{"compare with duplicate options", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "compare", "options": []string{"a", "a"}, "comparisonAxes": []any{map[string]any{"name": "cost", "direction": "lower_is_better"}}}, "duplicate"},
		{"compare without axes", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "compare", "options": []string{"a", "b"}}, "comparisonAxes"},
		{"forecast without unit", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "forecast", "target": "t", "unit": "", "horizon": "h"}, "unit"},

		// Belongs-to-another-mode. These are the cases the four separate tools used to catch for free
		// via their per-tool decode; with one tool they must be refused EXPLICITLY, because a silently
		// dropped parameter reads to the caller as one that was honoured.
		{"artifact on a map run", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "map", "artifact": "a"}, "does not belong to mode"},
		{"target on a challenge run", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "challenge", "artifact": "a", "target": "t"}, "does not belong to mode"},
		{"options on a forecast run", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "forecast", "target": "t", "unit": "u", "horizon": "h", "options": []string{"a", "b"}}, "does not belong to mode"},
		{"conditioningEvent outside forecast", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "map", "conditioningEvent": "e"}, "does not belong to mode"},
		// The refusal points at the mode that DOES take it, so the caller can correct in one step.
		{"refusal names the owning mode", "explore", map[string]any{"purpose": "x", "criteria": []string{"c"}, "mode": "map", "artifact": "a"}, `mode "challenge"`},

		{"waitSeconds out of range", "explore", exploreArgs(map[string]any{"waitSeconds": 9999}), "waitSeconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err == nil {
				t.Fatalf("want a protocol error, got result isError=%v: %v", res.IsError, res.StructuredContent)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error must name %q; got %v", tc.want, err)
			}
		})
	}
}

func TestUnknownTool_IsAProtocolError(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "explore_everything"}); err == nil {
		t.Fatal("unknown tool must be a protocol error")
	}
}

// --- panel: select or compose, fail-closed ---

func TestPanel_ProfileSelectionAndCount(t *testing.T) {
	exp := &fakeExplorer{}
	s := connect(t, newServer(t, exp))
	res := call(t, s, "explore", exploreArgs(map[string]any{"panel": map[string]any{"profile": "pair"}}))
	if res.IsError {
		t.Fatalf("isError: %s", textOf(res))
	}
	out := structured(t, res)
	pan, _ := out["panel"].(map[string]any)
	req, _ := pan["requested"].(map[string]any)
	if req["source"] != "profile" || req["profile"] != "pair" {
		t.Errorf("requested = %v", req)
	}
	if got := exp.plan().Explorers; len(got) != 2 || got[0].Model != "p1" {
		t.Errorf("executed panel = %v, want the `pair` profile", got)
	}
	// A count is never clamped: over-range fails rather than quietly running fewer seats.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "explore",
		Arguments: exploreArgs(map[string]any{"panel": map[string]any{"profile": "pair", "count": 9}})}); err == nil {
		t.Error("an out-of-range count must fail, never be clamped")
	}
}

func TestPanel_AdHocCompositionIsFailClosedAgainstTheConfiguredSet(t *testing.T) {
	exp := &fakeExplorer{}
	s := connect(t, newServer(t, exp))
	ok := call(t, s, "explore", exploreArgs(map[string]any{"panel": map[string]any{
		"explorers": []any{
			map[string]any{"adapter": "fake", "model": "x1"},
			map[string]any{"adapter": "fake", "model": "x2"},
		},
		"collator": map[string]any{"adapter": "fake", "model": "xc"},
	}}))
	if ok.IsError {
		t.Fatalf("ad-hoc composition failed: %s", textOf(ok))
	}
	out := structured(t, ok)
	pan, _ := out["panel"].(map[string]any)
	req, _ := pan["requested"].(map[string]any)
	if req["source"] != "adhoc" {
		t.Errorf("requested.source = %v, want adhoc", req["source"])
	}
	if got := exp.plan().Collator.Model; got != "xc" {
		t.Errorf("executed collator = %q, want the composed one", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// COMPOSE, NEVER CONFIGURE: an adapter outside the startup-bound set is refused, and the refusal
	// names the configured set so the corrected call follows from the error.
	_, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "explore", Arguments: exploreArgs(map[string]any{"panel": map[string]any{
		"explorers": []any{
			map[string]any{"adapter": "totally-new-cli", "model": "x1"},
			map[string]any{"adapter": "fake", "model": "x2"},
		},
		"collator": map[string]any{"adapter": "fake", "model": "xc"},
	}})})
	if err == nil {
		t.Fatal("an unconfigured adapter must be refused")
	}
	if !strings.Contains(err.Error(), "not configured") || !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("refusal must name the configured set; got %v", err)
	}
	// The two panel forms are mutually exclusive.
	_, err = s.CallTool(ctx, &sdk.CallToolParams{Name: "explore", Arguments: exploreArgs(map[string]any{"panel": map[string]any{
		"profile":   "pair",
		"explorers": []any{map[string]any{"adapter": "fake", "model": "x1"}, map[string]any{"adapter": "fake", "model": "x2"}},
		"collator":  map[string]any{"adapter": "fake", "model": "xc"},
	}})})
	if err == nil || !strings.Contains(err.Error(), "EITHER") {
		t.Errorf("profile + ad-hoc together must be refused; got %v", err)
	}
	// The fan-out cap is a spend control: over-cap fails, it is never clamped.
	seats := make([]any, 0, 17)
	for i := 0; i < 17; i++ {
		seats = append(seats, map[string]any{"adapter": "fake", "model": "m" + string(rune('a'+i))})
	}
	_, err = s.CallTool(ctx, &sdk.CallToolParams{Name: "explore", Arguments: exploreArgs(map[string]any{"panel": map[string]any{
		"explorers": seats, "collator": map[string]any{"adapter": "fake", "model": "xc"},
	}})})
	if err == nil || !strings.Contains(err.Error(), "fan-out cap") {
		t.Errorf("an over-cap panel must be refused; got %v", err)
	}
}

// --- job shape ---

func TestJobShape_LongRunReturnsARunIdAndIsFetchedLater(t *testing.T) {
	block := make(chan struct{})
	exp := &fakeExplorer{block: block}
	s := connect(t, newServer(t, exp))
	res := call(t, s, "explore", exploreArgs(map[string]any{"waitSeconds": 1}))
	out := structured(t, res)
	if out["state"] != "running" {
		t.Fatalf("state = %v, want running (the run outlived the inline budget)", out["state"])
	}
	runID, _ := out["runId"].(string)
	if runID == "" {
		t.Fatal("a running result must carry a runId")
	}
	if out["panel"] == nil {
		t.Error("a running result must still say which panel is executing")
	}
	if !strings.Contains(textOf(res), "explore_run_status") {
		t.Errorf("the human rendering must tell the caller how to continue: %s", textOf(res))
	}

	status := call(t, s, "explore_run_status", map[string]any{"runId": runID})
	so := structured(t, status)
	if so["state"] != "running" {
		t.Errorf("run_status = %v, want running", so["state"])
	}

	close(block)
	// Poll until it lands, then fetch the full result.
	deadline := time.Now().Add(10 * time.Second)
	for {
		so = structured(t, call(t, s, "explore_run_status", map[string]any{"runId": runID}))
		if so["state"] == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never completed: %v", so)
		}
		time.Sleep(10 * time.Millisecond)
	}
	final := call(t, s, "explore_run_result", map[string]any{"runId": runID})
	fo := structured(t, final)
	if fo["state"] != "complete" || fo["governance"] == nil || fo["identityCaveats"] == nil {
		t.Errorf("run_result must return the full governed payload, got %v", fo)
	}
	if fo["runId"] != runID {
		t.Errorf("run_result runId = %v, want %v", fo["runId"], runID)
	}
	// Re-reading is free and never re-runs.
	call(t, s, "explore_run_result", map[string]any{"runId": runID})
	if exp.callCount() != 1 {
		t.Errorf("explorer ran %d times; fetching a result must never re-run it", exp.callCount())
	}
}

func TestRunStatus_UnknownRunIsAnIsErrorRefusal(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	res := call(t, s, "explore_run_status", map[string]any{"runId": "no-such-run"})
	if !res.IsError {
		t.Fatal("an unknown runId must come back as isError")
	}
	out := structured(t, res)
	if out["reasonCode"] != "unknown_run_id" {
		t.Errorf("reasonCode = %v, want unknown_run_id", out["reasonCode"])
	}
}

// --- admission governor ---

func TestGovernor_IdempotencyKeyNeverRespends(t *testing.T) {
	exp := &fakeExplorer{}
	s := connect(t, newServer(t, exp))
	first := structured(t, call(t, s, "explore", exploreArgs(map[string]any{"idempotencyKey": "k-1"})))
	second := structured(t, call(t, s, "explore", exploreArgs(map[string]any{"idempotencyKey": "k-1"})))
	if first["runId"] != second["runId"] {
		t.Errorf("a repeated idempotency key must return the SAME run: %v vs %v", first["runId"], second["runId"])
	}
	if exp.callCount() != 1 {
		t.Errorf("explorer ran %d times; a duplicate key must never re-spend", exp.callCount())
	}
}

// There is NO admission governor: a run in flight does not stop the next one being admitted. This
// asserts the absence, so re-adding a server-wide cap fails the suite rather than passing silently.
//
// The bound that replaced it is per-invocation (`maxParallel`), and it governs the seats WITHIN one
// run rather than admission to the server — see TestExploreArgs_MaxParallelIsRefusedBelowOne here
// and TestRun_MaxParallel_BoundsParallelismWithoutDroppingWork in the pipeline package.
func TestGovernor_ThereIsNoAdmissionCap(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	exp := &fakeExplorer{block: block}
	s := connect(t, newServer(t, exp))
	// Two runs in flight at once, neither of which can finish while `block` is open.
	for i := range 2 {
		out := structured(t, call(t, s, "explore", exploreArgs(map[string]any{"waitSeconds": 1})))
		if out["state"] != "running" {
			t.Fatalf("run %d: state = %v, want running — no admission bound may refuse it", i, out["state"])
		}
	}
}

// maxParallel is refused rather than clamped: a caller who says "two at a time" because that is what
// their machine can host must not silently get the whole panel.
func TestExploreArgs_MaxParallelIsRefusedBelowOne(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	if _, err := s.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "explore", Arguments: exploreArgs(map[string]any{"maxParallel": 0}),
	}); err == nil {
		t.Fatal("maxParallel: 0 must be refused")
	}
}

// There is NO lifetime run cap, and that is a decision rather than an omission: a cumulative bound is
// cleared by restarting the process, so it never was the spend ceiling its name implied. This asserts
// the absence — a server that has already run is still willing to run again.
func TestGovernor_ThereIsNoLifetimeRunCap(t *testing.T) {
	exp := &fakeExplorer{}
	s := connect(t, newServer(t, exp))
	if out := structured(t, call(t, s, "explore", exploreArgs(nil))); out["state"] != "complete" {
		t.Fatalf("first run: %v", out)
	}
	if out := structured(t, call(t, s, "explore", exploreArgs(nil))); out["state"] != "complete" {
		t.Fatalf("a second run must not be refused by any lifetime bound: %v", out)
	}
}

func TestGovernor_OversizedArgumentsAreRefusedBeforeAnySpend(t *testing.T) {
	exp := &fakeExplorer{}
	s := connect(t, newServer(t, exp))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "explore", Arguments: map[string]any{
		"purpose": "x", "criteria": []string{"c"}, "mode": "challenge", "artifact": strings.Repeat("A", (1<<20)+64),
	}})
	if err == nil {
		t.Fatal("an oversized artifact must be refused")
	}
	if exp.callCount() != 0 {
		t.Errorf("explorer ran %d times; an oversized request must be refused BEFORE any spend", exp.callCount())
	}
}

// --- sanitized read-only tools ---

func TestList_IsSanitizedAndReportsTheLimits(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	res := call(t, s, "explore_list", map[string]any{})
	if res.IsError {
		t.Fatalf("list: %s", textOf(res))
	}
	out := structured(t, res)
	// No adapter row may carry a path/args field, or a value that looks like one. The projection type
	// has no such field at all — this asserts that the type has not quietly grown one.
	adapters, _ := out["adapters"].([]any)
	if len(adapters) == 0 {
		t.Fatal("list reported no adapters")
	}
	for _, a := range adapters {
		row, _ := a.(map[string]any)
		for k, v := range row {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "path") || strings.Contains(lk, "arg") || strings.Contains(lk, "env") {
				t.Errorf("list adapter row carries the environment-describing field %q — a tool result is inference input for a third party", k)
			}
			if sv, ok := v.(string); ok && (strings.Contains(sv, "/") || strings.Contains(sv, `\`)) {
				t.Errorf("list adapter field %q looks like a path: %q", k, sv)
			}
		}
	}
	if out["limits"] == nil || out["modes"] == nil || out["profiles"] == nil {
		t.Errorf("list payload = %v", out)
	}
	limits, _ := out["limits"].(map[string]any)
	if limits["maxPanelExplorers"] == nil || limits["maxWaitSeconds"] == nil {
		t.Errorf("limits = %v, want the limits in force", limits)
	}
}

func TestDoctor_StripsPathsFromCheckDetails(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	res := call(t, s, "explore_doctor", map[string]any{})
	out := structured(t, res)
	checks, _ := out["checks"].([]any)
	if len(checks) == 0 {
		t.Fatal("doctor reported no checks")
	}
	redacted := false
	for _, c := range checks {
		row, _ := c.(map[string]any)
		detail, _ := row["detail"].(string)
		if strings.Contains(detail, "/usr/local/bin/claude") {
			t.Errorf("doctor leaked a binary path: %q", detail)
		}
		if strings.Contains(detail, "<path>") {
			redacted = true
		}
	}
	if !redacted {
		// The redaction has to be VISIBLE: a silently-deleted path reads like a check that never mentioned
		// one, and the reader cannot tell the difference.
		t.Errorf("no check detail shows the <path> marker; checks = %v", checks)
	}
	if strings.Contains(textOf(res), "/usr/local/bin") {
		t.Errorf("doctor leaked a binary path in the human rendering: %s", textOf(res))
	}
}

// --- the declared outputSchema, checked against what actually goes on the wire ---

// declaredOutputSchemas compiles each tool's `outputSchema` AS THE CLIENT RECEIVES IT. Reading it back
// off the wire is the point: the in-package conformance table (schemaconformance_test.go) proves the
// payloads match the schema constants, and this proves the constants are what a client is handed.
func declaredOutputSchemas(t *testing.T, s *sdk.ClientSession) map[string]*jsonschema.Schema {
	t.Helper()
	res, err := s.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := map[string]*jsonschema.Schema{}
	for _, tool := range res.Tools {
		if tool.OutputSchema == nil {
			t.Fatalf("tool %q declares no outputSchema — a structured result nothing describes is not machine-readable", tool.Name)
		}
		b, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatalf("tool %q: %v", tool.Name, err)
		}
		compiled, cerr := jsonschema.Compile(b)
		if cerr != nil {
			t.Fatalf("tool %q: its declared outputSchema does not compile: %v", tool.Name, cerr)
		}
		out[tool.Name] = compiled
	}
	return out
}

// TestWire_EveryStructuredContentValidatesAgainstTheDeclaredOutputSchema drives real calls and judges
// every `structuredContent` against the schema its own tool advertised. The two payloads most likely
// to drift from it — a cancelled call and `run_result` on a still-running run — are covered here,
// because nothing else checks them. reviewmesh carries the identical test; the two servers are held
// to the same bar.
func TestWire_EveryStructuredContentValidatesAgainstTheDeclaredOutputSchema(t *testing.T) {
	check := func(t *testing.T, schemas map[string]*jsonschema.Schema, tool string, payload map[string]any) {
		t.Helper()
		if payload == nil {
			t.Fatalf("%s: no structuredContent — half of every client sees only this channel", tool)
		}
		if err := schemas[tool].Validate(payload); err != nil {
			t.Fatalf("%s emitted a payload its own declared outputSchema rejects: %v", tool, err)
		}
	}

	// (a) the read-only tools, a completed exploration, and the run it leaves behind.
	s := connect(t, newServer(t, &fakeExplorer{weak: true}))
	schemas := declaredOutputSchemas(t, s)
	done := structured(t, call(t, s, "explore", exploreArgs(nil)))
	runID, _ := done["runId"].(string)
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"explore_list", "explore_list", map[string]any{}},
		{"explore_doctor", "explore_doctor", map[string]any{}},
		{"a completed exploration", "explore", exploreArgs(nil)},
		{"a completed comparison", "explore", map[string]any{"mode": "compare",
			"purpose": "pick one", "criteria": []string{"cost"},
			"options":        []string{"a", "b"},
			"comparisonAxes": []any{map[string]any{"name": "cost", "direction": "lower_is_better"}},
		}},
		{"run_status on a finished run", "explore_run_status", map[string]any{"runId": runID}},
		{"run_result on a finished run", "explore_run_result", map[string]any{"runId": runID}},
		{"run_result on an unknown run", "explore_run_result", map[string]any{"runId": "run-nope"}},
		{"run_status on an unknown run", "explore_run_status", map[string]any{"runId": "run-nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check(t, schemas, tc.tool, structured(t, call(t, s, tc.tool, tc.args)))
		})
	}

	// (b) a HALT: the taxonomy branch, on a server whose explorer fails.
	t.Run("a halted exploration", func(t *testing.T) {
		halt := fault.New(fault.Adapter, "adapter exited non-zero").WithHalt("A").WithReason("adapter_exited_nonzero")
		hs := connect(t, newServer(t, &fakeExplorer{err: halt}))
		check(t, declaredOutputSchemas(t, hs), "explore", structured(t, call(t, hs, "explore", exploreArgs(nil))))
	})

	// (c) There is deliberately NO admission-refusal case here. It used to be driven by the
	// concurrency cap, and before that the lifetime cap; both are gone, and with them the last way
	// this surface could refuse an `explore` call BEFORE the run exists. Every remaining refusal on
	// the run-starting path is either a protocol error (no structuredContent to validate) or a halt
	// that already has a run id — covered by (a) and (b). Reinstating a synthetic case here would
	// assert a payload shape nothing produces, which is worse coverage than none.

	// (d) a run that is genuinely STILL RUNNING: the `running` reply, and run_result mid-flight — the
	// payload that must not drop the panel echo its own schema marks required.
	t.Run("a run that is still running", func(t *testing.T) {
		block := make(chan struct{})
		defer close(block)
		js := connect(t, newServer(t, &fakeExplorer{block: block}))
		jschemas := declaredOutputSchemas(t, js)
		running := structured(t, call(t, js, "explore", exploreArgs(map[string]any{"waitSeconds": 1})))
		if running["state"] != "running" {
			t.Fatalf("state = %v, want running (the inline budget was 1s)", running["state"])
		}
		check(t, jschemas, "explore", running)

		id, _ := running["runId"].(string)
		midflight := structured(t, call(t, js, "explore_run_result", map[string]any{"runId": id}))
		if _, ok := midflight["panel"]; !ok {
			t.Fatalf("run_result on a still-running exploration dropped the panel echo: %v", midflight)
		}
		check(t, jschemas, "explore_run_result", midflight)
		check(t, jschemas, "explore_run_status", structured(t, call(t, js, "explore_run_status", map[string]any{"runId": id})))
	})
}

// --- run capture ---

func TestCapture_WritesARunDirectoryByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", dir)
	s := connect(t, newServer(t, &fakeExplorer{}, func(s *mcp.Server) { s.DisableCapture = false }))
	out := structured(t, call(t, s, "explore", exploreArgs(nil)))
	if captured, _ := out["captured"].(bool); !captured {
		t.Fatalf("an MCP run must be captured by default: %v", out)
	}
	runID, _ := out["runId"].(string)
	// The wire run id IS the on-disk record's name — a run id that did not name its own audit record
	// would make the record unfindable from the only handle the caller has.
	manifest := dir + "/" + runID + "/manifest.json"
	b, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("no run record at the run id (%s): %v", manifest, err)
	}
	if !strings.Contains(string(b), `"status": "complete"`) {
		t.Errorf("manifest = %s", b)
	}
}
