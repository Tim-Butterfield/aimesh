package acp_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp/testhost"
)

// fakeExplorer is a deterministic in-process Explorer: it emits one progress event, optionally blocks on
// `block` (for the single-flight test), and returns a canned success (or `err`). It never spawns anything.
type fakeExplorer struct {
	block chan struct{}
	err   error
	// gotRaw captures the last RawTask the server handed it (so a test can assert criteria-via-_meta).
	gotRaw *schema.RawTask
	// gotPlan captures the last PANEL the server handed it (so a test can assert profile/count selection).
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

func testPlan(t *testing.T) roster.Plan {
	t.Helper()
	plan, err := roster.Roster{
		Explorers: []roster.Explorer{{Adapter: "fake", Model: "m1", Effort: "high"}, {Adapter: "fake", Model: "m2", Effort: "low"}},
		Collator:  roster.Collator{Adapter: "fake", Model: "mc"},
	}.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan
}

// serve wires the server to a pair of in-memory pipes and returns a driving Client + a stop func.
func serve(t *testing.T, exp acp.Explorer) (*testhost.Client, func()) {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	t.Setenv("AIMESH_HOME", t.TempDir())
	sr, cw := io.Pipe() // server reads sr; client writes cw
	cr, sw := io.Pipe() // client reads cr; server writes sw
	srv := &acp.Server{Explorer: exp, Plan: testPlan(t), Framing: acp.FramingNewline}
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
	return map[string]any{"exploremesh": map[string]any{"criteria": criteria}}
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
		"_meta":     map[string]any{"exploremesh": map[string]any{"criteria": []string{"c1"}, "purpose": "the exact purpose"}},
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
		"_meta": map[string]any{"exploremesh": map[string]any{"criteria": []string{"c1"}, "mode": "map"}},
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
		"_meta": map[string]any{"exploremesh": map[string]any{"criteria": []string{"c1"}, "mode": "bogus"}},
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
	t.Setenv("AIMESH_HOME", t.TempDir())
	block := make(chan struct{})
	exp := &fakeExplorer{block: block}
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	srv := &acp.Server{Explorer: exp, Plan: testPlan(t), Framing: acp.FramingNewline}
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

// --- panel selection via _meta (profile + count, design §7) ---

// testProfiles is the profile SET a profile-aware agent binds to: `base` (2 explorers) as the DEFAULT and
// `wide` (3 explorers in a deliberate PREFERENCE order, so a count subset is recognizable).
func testProfiles() profile.Set {
	return profile.Set{
		SchemaVersion:  profile.CurrentSchemaVersion,
		DefaultProfile: "base",
		Profiles: map[string]profile.Profile{
			"base": {
				Explorers: []roster.Explorer{
					{Adapter: "fake", Model: "base-a", Effort: "high"},
					{Adapter: "fake", Model: "base-b", Effort: "low"},
				},
				Collator: roster.Collator{Adapter: "fake", Model: "base-c"},
			},
			"wide": {
				Explorers: []roster.Explorer{
					{Adapter: "fake", Model: "z-first", Effort: "high"},
					{Adapter: "fake", Model: "a-second", Effort: "low"},
					{Adapter: "fake", Model: "m-third"},
				},
				Collator: roster.Collator{Adapter: "fake", Model: "wide-c"},
			},
		},
	}
}

// serveWithProfiles wires a server bound to a profile SET (what `exploremesh acp` does without --roster):
// Plan is the DEFAULT profile's full panel and Profiles is everything a prompt may select from.
func serveWithProfiles(t *testing.T, exp acp.Explorer) (*testhost.Client, func()) {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	t.Setenv("AIMESH_HOME", t.TempDir())
	set := testProfiles()
	def, err := set.Default()
	if err != nil {
		t.Fatalf("default profile: %v", err)
	}
	plan, err := def.Roster().Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	srv := &acp.Server{Explorer: exp, Plan: plan, Profiles: set, Framing: acp.FramingNewline}
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
// under test (profile / count).
func promptMeta(extra map[string]any) map[string]any {
	em := map[string]any{"criteria": []string{"c1"}}
	for k, v := range extra {
		em[k] = v
	}
	return map[string]any{"exploremesh": em}
}

// echoed returns the PromptResponse's `_meta.exploremesh` map.
func echoed(t *testing.T, resp *testhost.Response) map[string]any {
	t.Helper()
	meta, _ := resp.Result["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	if em == nil {
		t.Fatal("no _meta.exploremesh in PromptResponse")
	}
	return em
}

// TestServer_ProfileSelection: an absent profile binds to the SET'S DEFAULT; a named profile selects that
// profile's panel; both are echoed back with the selected/configured explorer counts.
func TestServer_ProfileSelection(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serveWithProfiles(t, exp)
	defer stop()
	sid := newSession(t, client)

	// (a) no profile named → the set's default (`base`), echoed by name.
	resp, err := client.Call("session/prompt", map[string]any{"sessionId": sid, "prompt": "explore", "_meta": promptMeta(nil)})
	if err != nil || resp.Error != nil {
		t.Fatalf("prompt: err=%v rpc=%v", err, resp.Error)
	}
	em := echoed(t, resp)
	if em["profile"] != "base" {
		t.Errorf("echoed profile = %v, want the default `base`", em["profile"])
	}
	if em["explorersSelected"] != float64(2) || em["explorersConfigured"] != float64(2) {
		t.Errorf("echoed counts = %v/%v, want 2/2", em["explorersSelected"], em["explorersConfigured"])
	}
	if exp.gotPlan == nil || len(exp.gotPlan.Explorers) != 2 || exp.gotPlan.Collator.Model != "base-c" {
		t.Errorf("server ran the wrong panel: %+v", exp.gotPlan)
	}

	// (b) an explicit profile selects that panel (all 3 explorers + its own collator).
	resp2, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": promptMeta(map[string]any{"profile": "wide"}),
	})
	if err != nil || resp2.Error != nil {
		t.Fatalf("prompt: err=%v rpc=%v", err, resp2.Error)
	}
	em2 := echoed(t, resp2)
	if em2["profile"] != "wide" || em2["explorersSelected"] != float64(3) {
		t.Errorf("echoed panel = %v (%v of %v), want wide 3 of 3", em2["profile"], em2["explorersSelected"], em2["explorersConfigured"])
	}
	if exp.gotPlan == nil || len(exp.gotPlan.Explorers) != 3 || exp.gotPlan.Collator.Model != "wide-c" {
		t.Errorf("server ran the wrong panel: %+v", exp.gotPlan)
	}
}

// TestServer_UnknownProfile_InvalidParams: an unknown profile is invalid-params (-32602) NAMING the
// configured profiles — never a silent fallback to the default panel.
func TestServer_UnknownProfile_InvalidParams(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serveWithProfiles(t, exp)
	defer stop()
	sid := newSession(t, client)

	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": promptMeta(map[string]any{"profile": "nope"}),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("an unknown profile must be invalid-params (-32602), got %v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "nope") || !strings.Contains(resp.Error.Message, "base, wide") {
		t.Errorf("the error should name the bad profile + list the configured ones: %q", resp.Error.Message)
	}
	if exp.gotPlan != nil {
		t.Error("a rejected profile must never reach the pipeline")
	}
}

// TestServer_CountSelectsTopN: `count` takes the top-N by PREFERENCE order out of the selected profile,
// returns the plan in canonical attribution order, and echoes the selected/configured counts. The string
// "all" is the explicit every-explorer form.
func TestServer_CountSelectsTopN(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serveWithProfiles(t, exp)
	defer stop()
	sid := newSession(t, client)

	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": promptMeta(map[string]any{"profile": "wide", "count": 2}),
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("prompt: err=%v rpc=%v", err, resp.Error)
	}
	em := echoed(t, resp)
	if em["explorersSelected"] != float64(2) || em["explorersConfigured"] != float64(3) {
		t.Errorf("echoed counts = %v/%v, want 2/3", em["explorersSelected"], em["explorersConfigured"])
	}
	if exp.gotPlan == nil || len(exp.gotPlan.Explorers) != 2 {
		t.Fatalf("server ran %+v, want a 2-explorer subset", exp.gotPlan)
	}
	// The top 2 by preference (z-first, a-second) — returned in canonical attribution order.
	if exp.gotPlan.Explorers[0].Model != "a-second" || exp.gotPlan.Explorers[1].Model != "z-first" {
		t.Errorf("subset = %+v, want the top 2 by preference in canonical order", exp.gotPlan.Explorers)
	}

	resp2, _ := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": promptMeta(map[string]any{"profile": "wide", "count": "all"}),
	})
	if resp2.Error != nil {
		t.Fatalf("count \"all\" rejected: %v", resp2.Error)
	}
	if len(exp.gotPlan.Explorers) != 3 {
		t.Errorf("count \"all\" ran %d explorers, want 3", len(exp.gotPlan.Explorers))
	}
}

// TestServer_InvalidCount_InvalidParams: out-of-range, <2, non-integer, unrecognized-string and non-scalar
// counts are all invalid-params (-32602) — the count is NEVER clamped (requested = executed, design §7).
func TestServer_InvalidCount_InvalidParams(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serveWithProfiles(t, exp)
	defer stop()
	sid := newSession(t, client)

	for _, tc := range []struct {
		name  string
		count any
	}{
		{"above the profile size", 9},
		{"below the 2-explorer minimum", 1},
		{"not a whole number", 2.5},
		{"an unrecognized string", "most"},
		{"not a scalar", []any{2}},
	} {
		exp.gotPlan = nil
		resp, err := client.Call("session/prompt", map[string]any{
			"sessionId": sid, "prompt": "explore", "_meta": promptMeta(map[string]any{"profile": "wide", "count": tc.count}),
		})
		if err != nil {
			t.Fatalf("%s: call: %v", tc.name, err)
		}
		if resp.Error == nil || resp.Error.Code != -32602 {
			t.Errorf("count %s must be invalid-params (-32602), got %v", tc.name, resp.Error)
		}
		if exp.gotPlan != nil {
			t.Errorf("count %s must be rejected before the pipeline runs", tc.name)
		}
	}
	// An out-of-range count says so explicitly rather than silently clamping.
	resp, _ := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": promptMeta(map[string]any{"profile": "wide", "count": 9}),
	})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "never clamped") {
		t.Errorf("the out-of-range error should say the count is not clamped: %v", resp.Error)
	}
}

// TestServer_RawRoster_HasNoNamedProfiles: with an explicit --roster the agent binds to a single ANONYMOUS
// roster — naming ANY profile is invalid-params, a prompt naming neither runs the configured Plan unchanged
// (echoing an empty profile name), and `count` still subsets that one roster.
func TestServer_RawRoster_HasNoNamedProfiles(t *testing.T) {
	exp := &fakeExplorer{}
	client, stop := serve(t, exp) // no Profiles wired → the --roster posture
	defer stop()
	sid := newSession(t, client)

	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": promptMeta(map[string]any{"profile": "wide"}),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("naming a profile against a raw roster must be invalid-params (-32602), got %v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "no named profiles") {
		t.Errorf("the error should explain the raw-roster posture: %q", resp.Error.Message)
	}

	resp2, _ := client.Call("session/prompt", map[string]any{"sessionId": sid, "prompt": "explore", "_meta": promptMeta(nil)})
	if resp2.Error != nil {
		t.Fatalf("plain prompt rejected: %v", resp2.Error)
	}
	em := echoed(t, resp2)
	if em["profile"] != "" || em["explorersSelected"] != float64(2) || em["explorersConfigured"] != float64(2) {
		t.Errorf("raw-roster echo = %v (%v of %v), want an empty profile + 2 of 2", em["profile"], em["explorersSelected"], em["explorersConfigured"])
	}
	if exp.gotPlan == nil || len(exp.gotPlan.Explorers) != 2 {
		t.Errorf("server ran the wrong panel: %+v", exp.gotPlan)
	}

	// `count` still applies (a raw roster IS the single anonymous profile) and still fails out-of-range.
	resp3, _ := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": promptMeta(map[string]any{"count": 3}),
	})
	if resp3.Error == nil || resp3.Error.Code != -32602 {
		t.Errorf("an out-of-range count against a raw roster must be invalid-params, got %v", resp3.Error)
	}
	resp4, _ := client.Call("session/prompt", map[string]any{
		"sessionId": sid, "prompt": "explore", "_meta": promptMeta(map[string]any{"count": 2}),
	})
	if resp4.Error != nil {
		t.Fatalf("an in-range count against a raw roster was rejected: %v", resp4.Error)
	}
	if len(exp.gotPlan.Explorers) != 2 {
		t.Errorf("count 2 ran %d explorers, want 2", len(exp.gotPlan.Explorers))
	}
}
