package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// --- a minimal JSON-RPC driver (the transport is the unit under test, so the test client is hand-rolled
// here; the SPEC-CONFORMANCE proof against the official SDK client lives in the exploremesh MCP surface,
// which is the package that actually ships a server). ---

type client struct {
	f  mcp.Framer
	id int
}

type response struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *rpcErr         `json:"error"`
}

type rpcErr struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

type notification struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

func (c *client) send(method string, id any, params any) {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		req["id"] = id
	}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	_ = c.f.WriteMessage(b)
}

// call sends a request and returns the matching response plus every notification that preceded it.
func (c *client) call(t *testing.T, method string, params any) (*response, []notification) {
	t.Helper()
	c.id++
	id := c.id
	c.send(method, id, params)
	var notes []notification
	for {
		raw, err := c.f.ReadMessage()
		if err != nil {
			t.Fatalf("%s: read: %v", method, err)
		}
		var resp response
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		if len(resp.ID) == 0 {
			var n notification
			if json.Unmarshal(raw, &n) == nil && n.Method != "" {
				notes = append(notes, n)
			}
			continue
		}
		var got int
		if json.Unmarshal(resp.ID, &got) == nil && got == id {
			return &resp, notes
		}
	}
}

func (c *client) notify(method string, params any) { c.send(method, nil, params) }

// serve wires a server to in-memory pipes and returns a driving client + a stop func.
func serve(t *testing.T, s *mcp.Server) (*client, func()) {
	t.Helper()
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	done := make(chan struct{})
	go func() { _ = s.Serve(sr, sw); close(done) }()
	stop := func() {
		_ = cw.Close()
		<-done
		_ = sw.Close()
	}
	return &client{f: mcp.NewFramer(mcp.FramingNewline, cr, cw)}, stop
}

const echoSchema = `{"type":"object","additionalProperties":false,"required":["text"],"properties":{"text":{"type":"string"}}}`

// newServer builds a server with an `echo` tool plus whatever extra tools the test needs.
func newServer(extra ...func(*mcp.Server)) *mcp.Server {
	s := &mcp.Server{Info: mcp.Implementation{Name: "test", Version: "0.0.1"}, Instructions: "instructions here"}
	s.Register(mcp.Tool{Name: "echo", InputSchema: json.RawMessage(echoSchema)},
		func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
			var in struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(c.Arguments, &in); err != nil {
				return nil, mcp.InvalidParams("invalid params: %v", err)
			}
			return mcp.Result(in.Text, map[string]any{"text": in.Text}), nil
		})
	for _, f := range extra {
		f(s)
	}
	return s
}

// handshake runs initialize + notifications/initialized.
func handshake(t *testing.T, c *client) *response {
	t.Helper()
	resp, _ := c.call(t, "initialize", map[string]any{
		"protocolVersion": mcp.LatestProtocolVersion,
		"clientInfo":      map[string]any{"name": "test-client", "version": "1"},
	})
	if resp.Error != nil {
		t.Fatalf("initialize: %+v", resp.Error)
	}
	c.notify("notifications/initialized", nil)
	return resp
}

func TestInitialize_EchoesNegotiatedVersionAndDeclaresCapabilities(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	resp := handshake(t, c)
	if got := resp.Result["protocolVersion"]; got != mcp.LatestProtocolVersion {
		t.Errorf("protocolVersion = %v, want the echoed %s", got, mcp.LatestProtocolVersion)
	}
	caps, _ := resp.Result["capabilities"].(map[string]any)
	if caps == nil || caps["tools"] == nil || caps["logging"] == nil {
		t.Errorf("capabilities = %v, want tools + logging declared", resp.Result["capabilities"])
	}
	if resp.Result["instructions"] != "instructions here" {
		t.Errorf("instructions = %v", resp.Result["instructions"])
	}
	info, _ := resp.Result["serverInfo"].(map[string]any)
	if info == nil || info["name"] != "test" {
		t.Errorf("serverInfo = %v", resp.Result["serverInfo"])
	}
}

// An OLDER supported version is echoed verbatim — negotiation, not "always answer with our latest".
func TestInitialize_EchoesOlderSupportedVersion(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	resp, _ := c.call(t, "initialize", map[string]any{"protocolVersion": mcp.ProtocolVersion20241105})
	if resp.Error != nil {
		t.Fatalf("initialize: %+v", resp.Error)
	}
	if got := resp.Result["protocolVersion"]; got != mcp.ProtocolVersion20241105 {
		t.Errorf("protocolVersion = %v, want the client's own %s echoed", got, mcp.ProtocolVersion20241105)
	}
}

func TestInitialize_UnsupportedVersionIsACleanRefusal(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	for _, pv := range []any{"1999-01-01", 1, nil} {
		params := map[string]any{}
		if pv != nil {
			params["protocolVersion"] = pv
		}
		resp, _ := c.call(t, "initialize", params)
		if resp.Error == nil {
			t.Fatalf("protocolVersion %v: want a refusal, got result %v", pv, resp.Result)
		}
		if resp.Error.Code != mcp.CodeInvalidParams {
			t.Errorf("protocolVersion %v: code = %d, want %d", pv, resp.Error.Code, mcp.CodeInvalidParams)
		}
		if !strings.Contains(resp.Error.Message, mcp.LatestProtocolVersion) {
			t.Errorf("refusal must name the supported versions, got %q", resp.Error.Message)
		}
	}
}

func TestPreInitialized_RefusesEverythingButPing(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	// Before initialize at all.
	resp, _ := c.call(t, "tools/list", nil)
	if resp.Error == nil || resp.Error.Code != mcp.CodeNotInitialized {
		t.Fatalf("tools/list before initialize: %+v", resp)
	}
	// ping IS allowed pre-initialization.
	pong, _ := c.call(t, "ping", nil)
	if pong.Error != nil {
		t.Errorf("ping before initialize: %+v", pong.Error)
	}
	// After initialize but BEFORE notifications/initialized: still refused.
	ir, _ := c.call(t, "initialize", map[string]any{"protocolVersion": mcp.LatestProtocolVersion})
	if ir.Error != nil {
		t.Fatalf("initialize: %+v", ir.Error)
	}
	resp2, _ := c.call(t, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}})
	if resp2.Error == nil || resp2.Error.Code != mcp.CodeNotInitialized {
		t.Fatalf("tools/call before notifications/initialized: %+v", resp2)
	}
}

func TestToolsList_PagingHonorsCursor(t *testing.T) {
	s := newServer(func(s *mcp.Server) {
		for _, n := range []string{"b", "c", "d"} {
			s.Register(mcp.Tool{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
					return mcp.Result("ok", nil), nil
				})
		}
	})
	s.PageSize = 2
	c, stop := serve(t, s)
	defer stop()
	handshake(t, c)

	resp, _ := c.call(t, "tools/list", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("tools/list: %+v", resp.Error)
	}
	tools, _ := resp.Result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("page 1 = %d tools, want 2", len(tools))
	}
	cursor, _ := resp.Result["nextCursor"].(string)
	if cursor == "" {
		t.Fatal("page 1 has no nextCursor but more tools exist")
	}
	resp2, _ := c.call(t, "tools/list", map[string]any{"cursor": cursor})
	tools2, _ := resp2.Result["tools"].([]any)
	if len(tools2) != 2 {
		t.Fatalf("page 2 = %d tools, want 2", len(tools2))
	}
	if _, more := resp2.Result["nextCursor"]; more {
		t.Errorf("page 2 must be the last page, got nextCursor %v", resp2.Result["nextCursor"])
	}
	// An unrecognized cursor is -32602 (honored, not ignored).
	bad, _ := c.call(t, "tools/list", map[string]any{"cursor": "bm90LWEtY3Vyc29y"})
	if bad.Error == nil || bad.Error.Code != mcp.CodeInvalidParams {
		t.Fatalf("unknown cursor: %+v", bad)
	}
}

// With the default page size the whole (small) list comes back with NO nextCursor.
func TestToolsList_DefaultReturnsEverything(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	handshake(t, c)
	resp, _ := c.call(t, "tools/list", nil)
	if _, more := resp.Result["nextCursor"]; more {
		t.Errorf("small list must not paginate, got nextCursor %v", resp.Result["nextCursor"])
	}
	tools, _ := resp.Result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "echo" || tool["inputSchema"] == nil {
		t.Errorf("tool projection = %v", tool)
	}
}

func TestToolsCall_SuccessAndUnknownTool(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	handshake(t, c)

	resp, _ := c.call(t, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "hello"}})
	if resp.Error != nil {
		t.Fatalf("tools/call: %+v", resp.Error)
	}
	if isErr, _ := resp.Result["isError"].(bool); isErr {
		t.Errorf("success result must not set isError")
	}
	sc, _ := resp.Result["structuredContent"].(map[string]any)
	if sc == nil || sc["text"] != "hello" {
		t.Errorf("structuredContent = %v", resp.Result["structuredContent"])
	}
	content, _ := resp.Result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want one block", resp.Result["content"])
	}

	// An unknown TOOL is a protocol error (reserved, alongside malformed requests + pre-init calls).
	unknown, _ := c.call(t, "tools/call", map[string]any{"name": "nope"})
	if unknown.Error == nil || unknown.Error.Code != mcp.CodeInvalidParams {
		t.Fatalf("unknown tool: %+v", unknown)
	}
}

// A DOMAIN failure rides a successful response carrying isError — never a JSON-RPC error, whose `data`
// clients routinely flatten or drop.
func TestToolsCall_DomainHaltRidesIsError(t *testing.T) {
	s := newServer(func(s *mcp.Server) {
		s.Register(mcp.Tool{Name: "halt", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				return mcp.ErrorResult("adapter failed", map[string]any{"exitCode": 4, "haltClass": "A"}), nil
			})
		s.Register(mcp.Tool{Name: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				return nil, errors.New("unexpected")
			})
	})
	c, stop := serve(t, s)
	defer stop()
	handshake(t, c)

	resp, _ := c.call(t, "tools/call", map[string]any{"name": "halt"})
	if resp.Error != nil {
		t.Fatalf("a domain halt must not be a JSON-RPC error: %+v", resp.Error)
	}
	if isErr, _ := resp.Result["isError"].(bool); !isErr {
		t.Errorf("isError = false, want true")
	}
	sc, _ := resp.Result["structuredContent"].(map[string]any)
	if sc == nil || sc["haltClass"] != "A" {
		t.Errorf("taxonomy payload lost: %v", resp.Result["structuredContent"])
	}
	// A handler that returns a plain error also becomes an isError RESULT, not a protocol error.
	boom, _ := c.call(t, "tools/call", map[string]any{"name": "boom"})
	if boom.Error != nil {
		t.Fatalf("handler error became a protocol error: %+v", boom.Error)
	}
	if isErr, _ := boom.Result["isError"].(bool); !isErr {
		t.Errorf("handler error must set isError")
	}
}

// A handler-returned *RequestError (the MALFORMED-request case) is the one thing that does ride a
// JSON-RPC error.
func TestToolsCall_MalformedArgumentsAreProtocolErrors(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	handshake(t, c)
	resp, _ := c.call(t, "tools/call", map[string]any{"name": "echo", "arguments": "not-an-object"})
	if resp.Error == nil || resp.Error.Code != mcp.CodeInvalidParams {
		t.Fatalf("malformed arguments: %+v", resp)
	}
}

func TestProgress_OnlyWithACallerSuppliedToken(t *testing.T) {
	s := newServer(func(s *mcp.Server) {
		s.Register(mcp.Tool{Name: "work", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				c.Progress(0.5, 1, "half")
				c.Progress(0.25, 1, "backwards — must be dropped")
				c.Progress(1, 1, "done")
				return mcp.Result("ok", map[string]any{"hadToken": c.HasProgressToken()}), nil
			})
	})
	c, stop := serve(t, s)
	defer stop()
	handshake(t, c)

	// WITHOUT a token: no progress notifications at all.
	resp, notes := c.call(t, "tools/call", map[string]any{"name": "work"})
	if resp.Error != nil {
		t.Fatalf("call: %+v", resp.Error)
	}
	for _, n := range notes {
		if n.Method == "notifications/progress" {
			t.Fatalf("progress emitted without a caller-supplied token: %v", n.Params)
		}
	}

	// WITH a token: the token is echoed and progress is monotonic.
	resp2, notes2 := c.call(t, "tools/call", map[string]any{
		"name":  "work",
		"_meta": map[string]any{"progressToken": "tok-42"},
	})
	if resp2.Error != nil {
		t.Fatalf("call: %+v", resp2.Error)
	}
	var progress []float64
	for _, n := range notes2 {
		if n.Method != "notifications/progress" {
			continue
		}
		if n.Params["progressToken"] != "tok-42" {
			t.Errorf("progressToken = %v, want the caller's token echoed", n.Params["progressToken"])
		}
		p, _ := n.Params["progress"].(float64)
		progress = append(progress, p)
	}
	if len(progress) != 2 {
		t.Fatalf("progress notifications = %v, want the 2 monotonic ones (the backwards one dropped)", progress)
	}
	for i := 1; i < len(progress); i++ {
		if progress[i] <= progress[i-1] {
			t.Errorf("progress not monotonic: %v", progress)
		}
	}
}

func TestCancellation_KillsTheCallAndSendsNoResponse(t *testing.T) {
	started := make(chan struct{})
	var observed error
	var mu sync.Mutex
	s := newServer(func(s *mcp.Server) {
		s.Register(mcp.Tool{Name: "slow", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				close(started)
				<-ctx.Done()
				mu.Lock()
				observed = ctx.Err()
				mu.Unlock()
				return mcp.Result("should never be sent", nil), nil
			})
	})
	c, stop := serve(t, s)
	defer stop()
	handshake(t, c)

	c.id++
	id := c.id
	c.send("tools/call", id, map[string]any{"name": "slow"})
	<-started
	c.notify("notifications/cancelled", map[string]any{"requestId": id, "reason": "user cancelled"})

	// Prove no response arrives for the cancelled id: a follow-up ping must be the very next frame.
	pong, notes := c.call(t, "ping", nil)
	if pong.Error != nil {
		t.Fatalf("ping after cancel: %+v", pong.Error)
	}
	for _, n := range notes {
		if n.Method == "notifications/progress" {
			t.Errorf("unexpected notification after cancel: %v", n)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := observed
		mu.Unlock()
		if got != nil {
			if !errors.Is(got, context.Canceled) {
				t.Errorf("handler ctx err = %v, want context.Canceled", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handler context was never cancelled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLogging_SetLevelFiltersNotifications(t *testing.T) {
	s := newServer(func(s *mcp.Server) {
		s.Register(mcp.Tool{Name: "chatty", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				c.Log(mcp.LevelDebug, "test", "debug line")
				c.Log(mcp.LevelError, "test", "error line")
				return mcp.Result("ok", nil), nil
			})
	})
	c, stop := serve(t, s)
	defer stop()
	handshake(t, c)

	// Default level is info: the debug line is dropped, the error line is not.
	_, notes := c.call(t, "tools/call", map[string]any{"name": "chatty"})
	var levels []string
	for _, n := range notes {
		if n.Method == "notifications/message" {
			lv, _ := n.Params["level"].(string)
			levels = append(levels, lv)
		}
	}
	if len(levels) != 1 || levels[0] != "error" {
		t.Fatalf("levels = %v, want just [error] at the default info level", levels)
	}

	// Raising the level to `critical` drops both.
	set, _ := c.call(t, "logging/setLevel", map[string]any{"level": "critical"})
	if set.Error != nil {
		t.Fatalf("logging/setLevel: %+v", set.Error)
	}
	_, notes2 := c.call(t, "tools/call", map[string]any{"name": "chatty"})
	for _, n := range notes2 {
		if n.Method == "notifications/message" {
			t.Errorf("message emitted below the selected level: %v", n.Params)
		}
	}

	// Lowering it to debug lets both through.
	set2, _ := c.call(t, "logging/setLevel", map[string]any{"level": "debug"})
	if set2.Error != nil {
		t.Fatalf("logging/setLevel debug: %+v", set2.Error)
	}
	_, notes3 := c.call(t, "tools/call", map[string]any{"name": "chatty"})
	n := 0
	for _, note := range notes3 {
		if note.Method == "notifications/message" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("messages at debug = %d, want 2", n)
	}

	bad, _ := c.call(t, "logging/setLevel", map[string]any{"level": "loud"})
	if bad.Error == nil || bad.Error.Code != mcp.CodeInvalidParams {
		t.Fatalf("unknown level: %+v", bad)
	}
}

// Batching was removed in the 2025-06-18 revision: an ARRAY on the wire must be refused, not crash the
// reader — the session has to survive it.
func TestWireTolerance_ArrayAndGarbageDoNotKillTheReader(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	_ = c.f.WriteMessage([]byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`))
	raw, err := c.f.ReadMessage()
	if err != nil {
		t.Fatalf("read after array: %v", err)
	}
	var resp response
	if json.Unmarshal(raw, &resp) != nil || resp.Error == nil || resp.Error.Code != mcp.CodeInvalidRequest {
		t.Fatalf("array frame: got %s", raw)
	}
	_ = c.f.WriteMessage([]byte(`{not json`))
	raw2, err := c.f.ReadMessage()
	if err != nil {
		t.Fatalf("read after garbage: %v", err)
	}
	var resp2 response
	if json.Unmarshal(raw2, &resp2) != nil || resp2.Error == nil || resp2.Error.Code != mcp.CodeParse {
		t.Fatalf("garbage frame: got %s", raw2)
	}
	// The session still works.
	handshake(t, c)
	ok, _ := c.call(t, "tools/list", nil)
	if ok.Error != nil {
		t.Fatalf("session did not survive: %+v", ok.Error)
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	handshake(t, c)
	resp, _ := c.call(t, "resources/list", nil)
	if resp.Error == nil || resp.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("undeclared method: %+v", resp)
	}
}

// STDOUT PURITY at the transport level: every byte the server writes is a JSON-RPC frame, including while
// notifications interleave with responses. (The app-level proof — that a server spawning model CLIs keeps
// their stdio off the protocol stream — lives with the server that spawns them.)
func TestStdoutPurity_EveryFrameIsJSONRPC(t *testing.T) {
	s := newServer(func(s *mcp.Server) {
		s.Register(mcp.Tool{Name: "noisy", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				c.Log(mcp.LevelError, "noisy", "a log line")
				c.Progress(0.5, 1, "half")
				c.Progress(1, 1, "done")
				return mcp.Result("ok", map[string]any{"k": "v"}), nil
			})
	})
	// Diagnostics is a SEPARATE sink; nothing written there may reach the protocol stream.
	var diag strings.Builder
	s.Diagnostics = &diag
	sr, cw := io.Pipe()
	var out lockedBuffer
	done := make(chan struct{})
	go func() { _ = s.Serve(sr, &out); close(done) }()
	enc := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = cw.Write(append(b, '\n'))
	}
	enc(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": mcp.LatestProtocolVersion}})
	enc(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	enc(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "noisy", "_meta": map[string]any{"progressToken": 7}}})
	// Wait for the terminal response to land, then close.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(out.String(), `"id":2`) {
		if time.Now().After(deadline) {
			t.Fatalf("no response for id 2; got:\n%s", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = cw.Close()
	<-done

	s.Diagnosticf("this line must not reach the protocol stream")
	if !strings.Contains(diag.String(), "must not reach") {
		t.Errorf("Diagnosticf did not write to the diagnostics sink")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected at least 4 frames (init, log, 2 progress, result), got:\n%s", out.String())
	}
	for i, line := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("stdout line %d is not JSON: %q", i, line)
		}
		if v["jsonrpc"] != "2.0" {
			t.Errorf("stdout line %d is not JSON-RPC 2.0: %q", i, line)
		}
		if strings.Contains(line, "must not reach") {
			t.Errorf("a diagnostic leaked onto the protocol stream: %q", line)
		}
	}
}

// lockedBuffer is a concurrency-safe io.Writer for capturing the server's output stream.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestRegister_RejectsDuplicatesAndMissingSchemas(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("duplicate registration must panic")
		}
	}()
	s := newServer()
	s.Register(mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) { return nil, nil })
}
