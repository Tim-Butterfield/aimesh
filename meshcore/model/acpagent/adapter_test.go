package acpagent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/acp"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// --- fake ACP server (helper process) ---
//
// The integration tests re-exec THIS test binary with a sentinel arg ("fakeacp") so it acts as a
// minimal ACP agent over stdio — no real CLI, network, or token spend. A normal test run invokes
// TestHelperACPServer with no sentinel and it returns immediately (a no-op).

func TestHelperACPServer(t *testing.T) {
	args := os.Args
	idx := -1
	for i, a := range args {
		if a == "fakeacp" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return // ordinary test run — not the child; do nothing
	}
	model, answer, mode := "", "{}", "ok"
	if idx+1 < len(args) {
		model = args[idx+1]
	}
	if idx+2 < len(args) {
		answer = args[idx+2]
	}
	if idx+3 < len(args) {
		mode = args[idx+3]
	}
	runFakeACPServer(model, answer, mode)
	os.Exit(0)
}

func runFakeACPServer(model, answer, mode string) {
	fr := acp.NewFramer(acp.FramingNewline, os.Stdin, os.Stdout)
	reply := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		_ = fr.WriteMessage(b)
	}
	replyErr := func(id json.RawMessage, msg string) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": msg}})
		_ = fr.WriteMessage(b)
	}
	notify := func(method string, params any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		_ = fr.WriteMessage(b)
	}
	for {
		raw, err := fr.ReadMessage()
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Method {
		case "initialize":
			if mode == "hang_initialize" {
				time.Sleep(10 * time.Second) // never respond within the test's short startup deadline
				return
			}
			reply(m.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}})
		case "session/new":
			if mode == "hang_session" {
				time.Sleep(10 * time.Second)
				return
			}
			if mode == "session_error" {
				replyErr(m.ID, "account not supported")
				continue
			}
			models := map[string]any{}
			if model != "" {
				models = map[string]any{
					"currentModelId":  model,
					"availableModels": []any{map[string]any{"modelId": model, "name": model}},
				}
			}
			reply(m.ID, map[string]any{
				"sessionId": "s1",
				"models":    models,
				"modes": map[string]any{
					"currentModeId":  "agent",
					"availableModes": []any{map[string]any{"id": "agent"}, map[string]any{"id": "ask"}, map[string]any{"id": "plan"}},
				},
			})
		case "session/set_mode", "session/set_model":
			reply(m.ID, map[string]any{})
		case "session/prompt":
			if mode == "slow_prompt" {
				// Respond AFTER the (short) startup deadline would have fired — proving the watchdog
				// was disarmed at session/new and does not kill a healthy in-flight prompt.
				time.Sleep(400 * time.Millisecond)
			}
			// A thought chunk (must be ignored) then the answer as a message chunk.
			notify("session/update", map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "thinking…"}}})
			notify("session/update", map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": answer}}})
			reply(m.ID, map[string]any{"stopReason": "end_turn"})
		default:
			if len(m.ID) > 0 {
				replyErr(m.ID, "unknown method")
			}
		}
	}
}

// fakeAdapter builds an Adapter that launches this test binary as the fake ACP server.
func fakeAdapter(model, answer, mode string) *Adapter {
	return &Adapter{
		Recipe: Recipe{
			Name:    "fake-acp",
			Detect:  "unused",
			ACPArgs: []string{"-test.run=TestHelperACPServer", "--", "fakeacp", model, answer, mode},
		},
		Path:    os.Args[0],
		Timeout: 30 * time.Second,
	}
}

func TestInvoke_VerifiedIdentity(t *testing.T) {
	a := fakeAdapter("composer-2.5[fast=true]", `Searching the codebase… {"findings":[]}`, "ok")
	res, err := a.Invoke(context.Background(), model.Call{ModelArg: core.ModelArg("composer-2.5"), Prompt: "review"})
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if res.ActualModel != "composer-2.5" {
		t.Errorf("ActualModel = %q, want composer-2.5 (base slug of the selected model)", res.ActualModel)
	}
	if res.Evidence != core.EvidenceCLIStatus {
		t.Errorf("Evidence = %q, want cli_status", res.Evidence)
	}
	if string(res.Payload) != `{"findings":[]}` {
		t.Errorf("Payload = %q, want the extracted JSON object", res.Payload)
	}
}

func TestInvoke_NoModel_Unknown(t *testing.T) {
	a := fakeAdapter("", `{"findings":[]}`, "ok") // no currentModelId → no availableModels
	res, err := a.Invoke(context.Background(), model.Call{ModelArg: core.ModelArg("default"), Prompt: "review"})
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if res.ActualModel != "" {
		t.Errorf("ActualModel = %q, want empty (agent reported no model)", res.ActualModel)
	}
	if res.Evidence != core.EvidenceNone {
		t.Errorf("Evidence = %q, want none (self-downgraded)", res.Evidence)
	}
}

func TestInvoke_SessionError(t *testing.T) {
	a := fakeAdapter("composer-2.5", `{}`, "session_error")
	_, err := a.Invoke(context.Background(), model.Call{ModelArg: core.ModelArg("composer-2.5"), Prompt: "x"})
	if err == nil {
		t.Fatal("expected an error when session/new fails (account-gated), got nil")
	}
}

func TestProbe_OK(t *testing.T) {
	a := fakeAdapter("composer-2.5", `{}`, "ok")
	r := a.Probe(context.Background())
	if !r.OK || r.Stage != "session_new" {
		t.Fatalf("Probe = %+v, want OK session_new", r)
	}
	if !strings.Contains(r.Detail, "composer-2.5") {
		t.Errorf("probe detail should name the model, got %q", r.Detail)
	}
}

func TestProbe_SessionRefused(t *testing.T) {
	a := fakeAdapter("m", `{}`, "session_error")
	if r := a.Probe(context.Background()); r.OK {
		t.Fatalf("Probe should fail when session/new is refused, got %+v", r)
	}
}

// TestInvoke_StartupHang_FailsFast: a stalled handshake must fail at the SHORT startup deadline, not
// hang for the full (30s) per-call Timeout — the anti-no-op check.
func TestInvoke_StartupHang_FailsFast(t *testing.T) {
	for _, mode := range []string{"hang_initialize", "hang_session"} {
		t.Run(mode, func(t *testing.T) {
			a := fakeAdapter("m", `{}`, mode)
			a.StartupTimeout = 200 * time.Millisecond
			a.Timeout = 30 * time.Second
			start := time.Now()
			_, err := a.Invoke(context.Background(), model.Call{ModelArg: core.ModelArg("m"), Prompt: "x"})
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("a stalled handshake must fail")
			}
			if elapsed > 5*time.Second {
				t.Errorf("startup watchdog did not fire fast: took %s (want ~200ms, not the 30s Timeout)", elapsed)
			}
		})
	}
}

// TestInvoke_SlowPromptNotKilled: the handshake completes instantly, then the prompt takes 400ms while
// the startup deadline is only 200ms. The watchdog must be DISARMED after session/new, so a healthy
// slow prompt is NOT killed — the anti-kills-healthy-sessions check (fable-5's key anti-regression).
func TestInvoke_SlowPromptNotKilled(t *testing.T) {
	a := fakeAdapter("m", `{"ok":true}`, "slow_prompt")
	a.StartupTimeout = 200 * time.Millisecond
	res, err := a.Invoke(context.Background(), model.Call{ModelArg: core.ModelArg("m"), Prompt: "x"})
	if err != nil {
		t.Fatalf("slow-but-healthy prompt must succeed, got: %v", err)
	}
	if string(res.Payload) != `{"ok":true}` {
		t.Errorf("expected the aggregated answer, got Payload=%q Stdout=%q", res.Payload, res.Stdout)
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"prose before\n```json\n{\"a\":1}\n```", `{"a":1}`},
		{`Searching… {"a":{"b":2},"c":"}"}`, `{"a":{"b":2},"c":"}"}`}, // nested + brace in string
		{`no json here`, ``},
		{`text {"x":"a\"b"} tail`, `{"x":"a\"b"}`}, // escaped quote in string
	}
	for _, c := range cases {
		if got := string(extractJSONObject(c.in)); got != c.want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPickReadOnlyMode(t *testing.T) {
	if got := pickReadOnlyMode([]acpMode{{"agent"}, {"ask"}, {"plan"}}); got != "ask" {
		t.Errorf("prefer ask, got %q", got)
	}
	if got := pickReadOnlyMode([]acpMode{{"agent"}, {"plan"}}); got != "plan" {
		t.Errorf("fall back to plan, got %q", got)
	}
	if got := pickReadOnlyMode([]acpMode{{"agent"}, {"bypass"}}); got != "" {
		t.Errorf("no read-only mode → empty, got %q", got)
	}
}

func TestPickModel(t *testing.T) {
	models := []acpModel{{"default[]", "Auto"}, {"composer-2.5[fast=true]", "composer-2.5"}, {"gpt-5.6-sol[reasoning=medium]", "gpt"}}
	if got := pickModel(models, "composer-2.5"); got != "composer-2.5[fast=true]" {
		t.Errorf("base-slug match failed, got %q", got)
	}
	if got := pickModel(models, "gpt-5.6-sol"); got != "gpt-5.6-sol[reasoning=medium]" {
		t.Errorf("prefix match failed, got %q", got)
	}
	if got := pickModel(models, "nonexistent"); got != "" {
		t.Errorf("no match → empty, got %q", got)
	}
}

func TestStripParams(t *testing.T) {
	for in, want := range map[string]string{
		"composer-2.5[fast=true]": "composer-2.5",
		"gemini-3.1-pro (high)":   "gemini-3.1-pro",
		"plain":                   "plain",
		"":                        "",
	} {
		if got := stripParams(in); got != want {
			t.Errorf("stripParams(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegistry_BuildsFromInstances(t *testing.T) {
	instances := map[string]Instance{
		"acp-claude": {Name: "acp-claude", Detect: "claude", Path: "/opt/claude", Args: []string{"--acp"}},
		"acp-gemini": {Name: "acp-gemini", Args: []string{"--acp"}}, // no Detect → falls back to name
	}
	reg := Registry(instances, 0)
	if len(reg) != len(instances) {
		t.Fatalf("Registry size %d != instance count %d", len(reg), len(instances))
	}
	for name := range instances {
		if _, ok := reg[name]; !ok {
			t.Errorf("Registry missing adapter %q", name)
		}
	}
	// Detect falls back to the instance name when unset (used for PATH lookup).
	if got := reg["acp-gemini"].(*Adapter).DetectName(); got != "acp-gemini" {
		t.Errorf("empty Detect should fall back to name, got %q", got)
	}
	if got := reg["acp-claude"].(*Adapter).DetectName(); got != "claude" {
		t.Errorf("Detect = %q, want claude", got)
	}
}

func TestRegistry_Empty(t *testing.T) {
	if reg := Registry(nil, 0); len(reg) != 0 {
		t.Errorf("nil instances → empty registry, got %d", len(reg))
	}
}

var _ interface {
	Evidence() core.IdentityEvidence
} = (*Adapter)(nil)
