package acp_test

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	corefake "github.com/Tim-Butterfield/aimesh/meshcore/model/fake"

	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp/testhost"
	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
)

// fakeExplorer is a deterministic in-process Explorer: it emits one progress event, optionally blocks on
// `block` (for the single-flight test), and returns a canned success (or `err`). It never spawns anything.
type fakeExplorer struct {
	block chan struct{}
	err   error
	// gotRaw captures the last RawTask the server handed it (so a test can assert criteria-via-_meta).
	gotRaw *schema.RawTask
	// gotPlan captures the last PANEL the server handed it (so a test can assert the composed panel ran).
	gotPlan *roster.Plan
	// gotOpts captures the last Options — how a test proves a per-turn parameter was PLUMBED to the
	// pipeline rather than merely accepted off the wire.
	gotOpts pipeline.Options
}

func (f *fakeExplorer) Run(ctx context.Context, plan roster.Plan, raw schema.RawTask, opts pipeline.Options, onEvent func(audit.EventLine)) (pipeline.Result, error) {
	f.gotRaw, f.gotPlan, f.gotOpts = &raw, &plan, opts
	// A DRY RUN returns what the pipeline returns for one: a shape and nothing else.
	if opts.DryRun {
		return pipeline.Result{Mode: raw.Mode, Shape: &pipeline.Shape{
			Mode:       raw.Mode,
			Explorers:  []schema.ExplorerIdentity{{Adapter: "fake", Model: "x1"}, {Adapter: "fake", Model: "x2"}},
			Collator:   schema.ExplorerIdentity{Adapter: "fake", Model: "xc"},
			Rounds:     1,
			Policy:     pipeline.ShapePolicy{Terminal: pipeline.TerminalCollate},
			Calls:      []pipeline.ShapeCall{{Phase: schema.PhaseExplore, Round: 1, Role: "explorer", Calls: 2, Detail: "the blind round"}},
			ModelCalls: 2,
			Payload:    pipeline.ShapePayload{Prompt: "explore X", PromptBytes: 9, PayloadHash: "sha256:payload", SchemaFields: []string{"claims"}},
		}}, nil
	}
	if onEvent != nil {
		onEvent(audit.EventLine{Timestamp: "2026-01-01T00:00:00Z", Level: "info", EventType: "formulate_start", Message: "collator formulating"})
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return pipeline.Result{}, ctx.Err()
		}
	}
	if f.err != nil {
		return pipeline.Result{}, f.err
	}
	return pipeline.Result{
		Formulation:    schema.Formulation{Source: schema.FormulationFreeMap},
		CollatorStatus: schema.IdentityVerified,
		Envelopes:      []schema.Envelope{{}, {}},
		Output:         schema.CollatorOutput{SynthesisSummary: "synthesized", Findings: []schema.Finding{{Statement: "f1"}}},
	}, nil
}

// testPanel is the panel a test prompt composes when the panel is not what the test is about: two fake
// explorers and a fake collator.
func testPanel() map[string]any {
	return map[string]any{
		"explorers": []any{
			map[string]any{"adapter": "fake", "model": "m1", "effort": "high"},
			map[string]any{"adapter": "fake", "model": "m2", "effort": "low"},
		},
		"collator": map[string]any{"adapter": "fake", "model": "mc"},
	}
}

// withPanel returns em with testPanel added when it names no panel. A "panel" key set to nil is removed,
// so a test can send a prompt with no panel at all.
func withPanel(em map[string]any) map[string]any {
	out := make(map[string]any, len(em)+1)
	maps.Copy(out, em)
	if p, has := out["panel"]; !has {
		out["panel"] = testPanel()
	} else if p == nil {
		delete(out, "panel")
	}
	return out
}

// launchSet builds the adapter set an agent launched with `--adapter <name>` for each name would hold.
// The internal fake is unlocked for the test; any other built-in adapter is launched at an executable the
// test owns, so its availability is real and needs no CLI on the machine running the tests.
func launchSet(t *testing.T, names ...string) launchflags.Set {
	t.Helper()
	t.Setenv(corefake.EnvVar, "1")
	var adapters []launchflags.Adapter
	for _, n := range names {
		a := launchflags.Adapter{Name: n, Source: launchflags.SourceFlag}
		if n != launchflags.FakeAdapter {
			p := filepath.Join(t.TempDir(), n)
			if runtime.GOOS == "windows" {
				p += ".exe"
			}
			if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			a.Path = p
		}
		adapters = append(adapters, a)
	}
	return launchflags.NewSet(adapters...)
}

// serve wires a server launched with the fake adapter to a pair of in-memory pipes and returns a driving
// Client + a stop func.
func serve(t *testing.T, exp acp.Explorer) (*testhost.Client, func()) {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	sr, cw := io.Pipe() // server reads sr; client writes cw
	cr, sw := io.Pipe() // client reads cr; server writes sw
	srv := &acp.Server{Explorer: exp, Adapters: launchSet(t, "fake"), Framing: acp.FramingNewline}
	done := make(chan struct{})
	go func() { _ = srv.Serve(sr, sw); close(done) }()
	client := testhost.NewClient(acp.FramingNewline, cr, cw)
	stop := func() {
		_ = client.Notify("exit", nil)
		_ = cw.Close()
		<-done
		_ = sw.Close()
	}
	return client, stop
}

func metaCriteria(criteria ...string) map[string]any {
	return map[string]any{"exploremesh": map[string]any{"panel": testPanel(), "criteria": criteria}}
}

func TestServer_HappyPath_PromptResponseAndProgress(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serve(t, exp)
	defer stop()

	init, err := client.Call("initialize", map[string]any{"protocolVersion": 1})
	if err != nil || init.Error != nil {
		t.Fatalf("initialize: err=%v rpc=%v", err, init.Error)
	}
	if pv, _ := init.Result["protocolVersion"].(float64); int(pv) != 1 {
		t.Errorf("protocolVersion = %v, want 1", init.Result["protocolVersion"])
	}

	sn, err := client.Call("session/new", map[string]any{"cwd": "/tmp/x"})
	if err != nil || sn.Error != nil {
		t.Fatalf("session/new: err=%v rpc=%v", err, sn.Error)
	}
	sid, _ := sn.Result["sessionId"].(string)
	if sid == "" {
		t.Fatal("no sessionId")
	}

	resp, notes, err := client.CallCollecting("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "explore whether X holds"}},
		"_meta":     metaCriteria("must be correct", "must be fast"),
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("session/prompt: err=%v rpc=%v", err, resp.Error)
	}
	if sr, _ := resp.Result["stopReason"].(string); sr != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", sr)
	}
	// The applied task is echoed back in _meta.exploremesh.
	meta, _ := resp.Result["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	if em == nil {
		t.Fatal("no _meta.exploremesh in PromptResponse")
	}
	if em["purpose"] != "explore whether X holds" {
		t.Errorf("echoed purpose = %v, want the prompt text", em["purpose"])
	}
	crit, _ := em["criteria"].([]any)
	if len(crit) != 2 || crit[0] != "must be correct" {
		t.Errorf("echoed criteria = %v, want the 2 supplied", em["criteria"])
	}
	// The server actually built a RawTask with prompt→purpose and _meta→criteria.
	if exp.gotRaw == nil || exp.gotRaw.Purpose != "explore whether X holds" || len(exp.gotRaw.Criteria) != 2 {
		t.Errorf("server built RawTask = %+v", exp.gotRaw)
	}
	// A progress notification was streamed as session/update with a structured _meta.exploremesh.
	sawProgress := false
	for _, n := range notes {
		if n.Method == "session/update" {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Error("expected at least one session/update progress notification")
	}
}

func TestServer_PurposeOverrideFromMeta(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serve(t, exp)
	defer stop()
	client.Call("initialize", map[string]any{"protocolVersion": 1})
	sn, _ := client.Call("session/new", map[string]any{})
	sid, _ := sn.Result["sessionId"].(string)
	_, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    "the human prompt",
		"_meta":     map[string]any{"exploremesh": map[string]any{"panel": testPanel(), "criteria": []string{"c1"}, "purpose": "the exact purpose"}},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if exp.gotRaw == nil || exp.gotRaw.Purpose != "the exact purpose" {
		t.Errorf("_meta.purpose should override the prompt text, got %+v", exp.gotRaw)
	}
}

// TestServer_Mode covers _meta.exploremesh.mode: it defaults to map when omitted (echoed back), a known
// mode is applied + echoed, and an unknown mode is invalid-params listing the known modes.
func TestServer_Mode(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serve(t, exp)
	defer stop()
	client.Call("initialize", map[string]any{"protocolVersion": 1})
	sn, _ := client.Call("session/new", map[string]any{})
	sid, _ := sn.Result["sessionId"].(string)

	// (a) omitted mode → default map, echoed in _meta.exploremesh.mode.
	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": metaCriteria("c1"),
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("prompt: err=%v rpc=%v", err, resp.Error)
	}
	meta, _ := resp.Result["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	if em == nil || em["mode"] != "map" {
		t.Errorf("omitted mode should echo the default map, got %v", em["mode"])
	}
	if exp.gotRaw == nil || exp.gotRaw.Mode != "" {
		t.Errorf("omitted mode should leave RawTask.Mode empty (pipeline defaults), got %q", exp.gotRaw.Mode)
	}

	// (b) explicit known mode is applied + echoed.
	resp2, _ := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore",
		"_meta": map[string]any{"exploremesh": map[string]any{"panel": testPanel(), "criteria": []string{"c1"}, "mode": "map"}},
	})
	if resp2.Error != nil {
		t.Fatalf("known mode rejected: %v", resp2.Error)
	}
	if exp.gotRaw == nil || exp.gotRaw.Mode != "map" {
		t.Errorf("explicit mode should be carried on RawTask, got %q", exp.gotRaw.Mode)
	}

	// (c) unknown mode → invalid-params (-32602) listing known modes.
	resp3, _ := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore",
		"_meta": map[string]any{"exploremesh": map[string]any{"panel": testPanel(), "criteria": []string{"c1"}, "mode": "bogus"}},
	})
	if resp3.Error == nil || resp3.Error.Code != -32602 {
		t.Fatalf("unknown mode must be invalid-params (-32602), got %v", resp3.Error)
	}
}

func TestServer_MissingCriteria_InvalidParams(t *testing.T) {
	client, stop := serve(t, &fakeExplorer{})
	defer stop()
	client.Call("initialize", map[string]any{"protocolVersion": 1})
	sn, _ := client.Call("session/new", map[string]any{})
	sid, _ := sn.Result["sessionId"].(string)

	// (a) No _meta at all.
	resp, err := client.Call("session/prompt", map[string]any{"sessionId": sid, "prompt": "explore"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("absent criteria must be invalid-params (-32602), got %v", resp.Error)
	}
	// (b) All-whitespace criteria.
	resp2, _ := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": metaCriteria("  ", "\t"),
	})
	if resp2.Error == nil || resp2.Error.Code != -32602 {
		t.Fatalf("all-whitespace criteria must be invalid-params (-32602), got %v", resp2.Error)
	}
}

func TestServer_PromptWithoutSessionNew_InvalidParams(t *testing.T) {
	client, stop := serve(t, &fakeExplorer{})
	defer stop()
	client.Call("initialize", map[string]any{"protocolVersion": 1})
	resp, err := client.Call("session/prompt", map[string]any{"sessionId": "s-9999", "prompt": "x", "_meta": metaCriteria("c1")})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("unknown sessionId must be invalid-params (-32602), got %v", resp.Error)
	}
}

func TestServer_Halt_MapsToExploreHaltError(t *testing.T) {
	exp := &fakeExplorer{err: fault.New(fault.Config, "primary synthesis requires >=2 verified explorer responses")}
	client, stop := serve(t, exp)
	defer stop()
	client.Call("initialize", map[string]any{"protocolVersion": 1})
	sn, _ := client.Call("session/new", map[string]any{})
	sid, _ := sn.Result["sessionId"].(string)
	resp, err := client.Call("session/prompt", map[string]any{"sessionId": sid, "prompt": "x", "_meta": metaCriteria("c1")})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Fatalf("a halt must map to codeExploreHalt (-32000), got %v", resp.Error)
	}
	if resp.Error.Data == nil || resp.Error.Data["haltClass"] == nil || resp.Error.Data["failure"] == nil {
		t.Errorf("halt error data must carry haltClass + failure, got %v", resp.Error.Data)
	}
}

func TestServer_ProtocolVersionNegotiation(t *testing.T) {
	client, stop := serve(t, &fakeExplorer{})
	defer stop()
	// A missing / non-integer protocolVersion is a deterministic protocol error.
	resp, _ := client.Call("initialize", map[string]any{"protocolVersion": "0.1"})
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("a string protocolVersion must be invalid-params, got %v", resp.Error)
	}
}

// TestServer_ResumeMethodNotFoundWithoutStore: with no session store wired, session/resume is honestly
// method-not-found (the capability is not advertised).
func TestServer_ResumeMethodNotFoundWithoutStore(t *testing.T) {
	client, stop := serve(t, &fakeExplorer{})
	defer stop()
	client.Call("initialize", map[string]any{"protocolVersion": 1})
	resp, _ := client.Call("session/resume", map[string]any{"sessionId": "s-0001"})
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("session/resume without a store must be method-not-found (-32601), got %v", resp.Error)
	}
}

// TestServer_SingleFlight drives two concurrent prompts on one session (raw framing so both requests are
// in flight at once): the second must be rejected "session busy" (codeInvalidRequest) while the first runs.
func TestServer_SingleFlight(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir())
	block := make(chan struct{})
	exp := &fakeExplorer{block: block}
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	srv := &acp.Server{Explorer: exp, Adapters: launchSet(t, "fake"), Framing: acp.FramingNewline}
	done := make(chan struct{})
	go func() { _ = srv.Serve(sr, sw); close(done) }()
	fr := acp.NewFramer(acp.FramingNewline, cr, cw)

	send := func(id int, method string, params any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err := fr.WriteMessage(b); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	readID := func(id int) map[string]any {
		for {
			raw, err := fr.ReadMessage()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var m map[string]any
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			if g, ok := m["id"].(float64); ok && int(g) == id {
				return m
			}
		}
	}

	send(1, "initialize", map[string]any{"protocolVersion": 1})
	readID(1)
	send(2, "session/new", map[string]any{})
	sid := readID(2)["result"].(map[string]any)["sessionId"].(string)

	pp := map[string]any{"sessionId": sid, "prompt": "explore", "_meta": metaCriteria("c1")}
	send(3, "session/prompt", pp) // blocks in the fake
	send(4, "session/prompt", pp) // must be rejected busy

	r4 := readID(4)
	if r4["error"] == nil {
		t.Fatalf("concurrent prompt must be rejected, got %v", r4)
	}
	if code, _ := r4["error"].(map[string]any)["code"].(float64); int(code) != -32600 {
		t.Errorf("session-busy must be codeInvalidRequest (-32600), got %v", r4["error"])
	}

	close(block) // let prompt 3 finish
	r3 := readID(3)
	if r3["error"] != nil {
		t.Fatalf("the first prompt should succeed, got %v", r3["error"])
	}

	send(9, "exit", nil)
	_ = cw.Close()
	<-done
	_ = sw.Close()
}

// --- the required panel ---

// newSession runs initialize + session/new and returns the session id.
func newSession(t *testing.T, client *testhost.Client) string {
	t.Helper()
	if _, err := client.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sn, err := client.Call("session/new", map[string]any{})
	if err != nil || sn.Error != nil {
		t.Fatalf("session/new: err=%v rpc=%v", err, sn.Error)
	}
	sid, _ := sn.Result["sessionId"].(string)
	if sid == "" {
		t.Fatal("no sessionId")
	}
	return sid
}

// promptMeta builds a `_meta` carrying the REQUIRED criteria plus whatever extra exploremesh keys are
// under test. It adds no panel: a test that needs one names it.
func promptMeta(extra map[string]any) map[string]any {
	em := map[string]any{"criteria": []string{"c1"}}
	maps.Copy(em, extra)
	return map[string]any{"exploremesh": em}
}

// TestServer_PanelIsRequired: a prompt that names no panel is invalid-params whose message shows the
// corrected shape, and nothing reaches the pipeline.
func TestServer_PanelIsRequired(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serve(t, exp)
	defer stop()
	sid := newSession(t, client)

	resp, err := client.Call("session/prompt", map[string]any{"sessionId": sid, "prompt": "explore", "_meta": promptMeta(nil)})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("a prompt with no panel must be invalid-params (-32602), got %v", resp.Error)
	}
	for _, want := range []string{"_meta.exploremesh.panel", `"explorers"`, `"collator"`} {
		if !strings.Contains(resp.Error.Message, want) {
			t.Errorf("the refusal must show the corrected shape (%q): %q", want, resp.Error.Message)
		}
	}
	if exp.gotPlan != nil {
		t.Error("a prompt with no panel must never reach the pipeline")
	}
}

// TestServer_ProfileAndCountAreRefused: `profile` and `count` are not exploremesh prompt fields, and an
// unknown key in the exploremesh namespace is refused rather than silently dropped.
func TestServer_ProfileAndCountAreRefused(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serve(t, exp)
	defer stop()
	sid := newSession(t, client)

	for _, key := range []string{"profile", "count"} {
		resp, err := client.Call("session/prompt", map[string]any{
			"sessionId": sid, "prompt": "explore",
			"_meta": promptMeta(map[string]any{"panel": testPanel(), key: "x"}),
		})
		if err != nil {
			t.Fatalf("%s: call: %v", key, err)
		}
		if resp.Error == nil || resp.Error.Code != -32602 || !strings.Contains(resp.Error.Message, key) {
			t.Errorf("a %q key must be refused as invalid params naming it, got %v", key, resp.Error)
		}
	}
	if exp.gotPlan != nil {
		t.Error("a refused prompt must never reach the pipeline")
	}
}

// TestServer_VerifyReadinessReachesThePipeline: the per-turn readiness probe is plumbed to the pipeline,
// not merely accepted off the wire.
func TestServer_VerifyReadinessReachesThePipeline(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serve(t, exp)
	defer stop()
	sid := newSession(t, client)

	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore",
		"_meta": promptMeta(map[string]any{"panel": testPanel(), "verifyReadiness": true}),
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("prompt: err=%v rpc=%v", err, resp.Error)
	}
	if !exp.gotOpts.VerifyReadiness {
		t.Fatal("verifyReadiness was accepted on the wire but not passed to the pipeline")
	}
	if exp.gotPlan == nil || len(exp.gotPlan.Explorers) != 2 || exp.gotPlan.Collator.Model != "mc" {
		t.Errorf("the pipeline ran %+v, want the composed panel", exp.gotPlan)
	}
}
