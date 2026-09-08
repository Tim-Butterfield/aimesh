package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the CONFORMANCE HARNESS the MCP design review asked for: the whole protocol tail, walked
// end to end by the OFFICIAL MCP Go SDK client rather than by a client written alongside the server.
//
// The transcript it walks, in order:
//
//	initialize → notifications/initialized (the SDK's Connect does both, and REFUSES a server whose echoed
//	protocolVersion it does not support — so a successful Connect is itself an assertion)
//	→ tools/list, including a paged tools/list with a cursor
//	→ tools/call success
//	→ tools/call returning isError
//	→ a protocol error (unknown tool)
//	→ tools/call WITH a progressToken, and WITHOUT one
//	→ cancellation of an in-flight call
//	→ logging/setLevel + notifications/message
//	→ ping
//
// The dependency is test-only. `go build ./...` pulls none of it; the shipped binary's module graph is
// unchanged.

// waitFor polls cond until it holds, and fails the test if it never does.
//
// A returned CallTool does NOT mean that call's notifications have been handled: the result travels
// the same connection as the progress and logging notifications, but the handlers run on the
// session's own goroutine, so reading the collected slices the instant CallTool returns is a race.
// It is a race this suite happened to win on an unloaded macOS host and lost the first time it ran
// on Windows — the assertion was never platform-specific, only the timing was.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Errorf("timed out waiting for %s", what)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestConformance_FullTranscriptAgainstTheOfficialSDKClient(t *testing.T) {
	var (
		mu       sync.Mutex
		progress []float64
		tokens   []any
		logs     []string
	)
	opts := &sdk.ClientOptions{
		ProgressNotificationHandler: func(ctx context.Context, req *sdk.ProgressNotificationClientRequest) {
			mu.Lock()
			progress = append(progress, req.Params.Progress)
			tokens = append(tokens, req.Params.ProgressToken)
			mu.Unlock()
		},
		LoggingMessageHandler: func(ctx context.Context, req *sdk.LoggingMessageRequest) {
			mu.Lock()
			logs = append(logs, string(req.Params.Level))
			mu.Unlock()
		},
	}
	exp := &fakeExplorer{}
	session := connect(t, newServer(t, exp), opts)
	ctx := context.Background()

	// --- lifecycle: a successful Connect already proves version negotiation + echo, since the SDK client
	// disconnects on a protocolVersion it does not support.
	init := session.InitializeResult()
	if init.ProtocolVersion != proto.LatestProtocolVersion {
		t.Errorf("negotiated protocolVersion = %q, want the client's own %q echoed", init.ProtocolVersion, proto.LatestProtocolVersion)
	}

	// --- ping
	if err := session.Ping(ctx, nil); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// --- tools/list
	list, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(list.Tools) != 6 {
		t.Fatalf("tools = %d, want the 6 declared", len(list.Tools))
	}

	// --- tools/call: success
	ok, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "explore", Arguments: exploreArgs(nil)})
	if err != nil {
		t.Fatalf("tools/call explore: %v", err)
	}
	if ok.IsError {
		t.Fatalf("explore returned isError: %s", textOf(ok))
	}
	if ok.StructuredContent == nil {
		t.Error("a successful result must carry structuredContent")
	}
	if len(ok.Content) == 0 {
		t.Error("a successful result must carry a human rendering too")
	}

	// --- tools/call: isError (a domain halt is NOT a protocol error)
	bad, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "explore_run_result", Arguments: map[string]any{"runId": "nope"}})
	if err != nil {
		t.Fatalf("a domain refusal must not be a protocol error: %v", err)
	}
	if !bad.IsError {
		t.Error("an unknown run must set isError")
	}

	// --- a protocol error: unknown tool
	if _, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "not_a_tool"}); err == nil {
		t.Error("an unknown tool must be a JSON-RPC error")
	}

	// --- progress WITHOUT a token: nothing may be emitted (a token is not optional decoration; without
	// one a progress notification is unroutable, and inventing one is worse than staying quiet).
	mu.Lock()
	progress, tokens = nil, nil
	mu.Unlock()
	if _, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "explore", Arguments: exploreArgs(nil)}); err != nil {
		t.Fatalf("explore: %v", err)
	}
	// A negative has nothing to wait FOR, so this one settles instead: without a pause it would pass
	// simply by reading the slice before a notification the server did send could arrive.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	gotWithout := len(progress)
	mu.Unlock()
	if gotWithout != 0 {
		t.Errorf("%d progress notification(s) emitted without a caller-supplied token", gotWithout)
	}

	// --- progress WITH a token: echoed verbatim and monotonic.
	params := &sdk.CallToolParams{Name: "explore", Arguments: exploreArgs(nil)}
	params.Meta = sdk.Meta{"progressToken": "tok-explore"}
	if _, err := session.CallTool(ctx, params); err != nil {
		t.Fatalf("explore with progress: %v", err)
	}
	waitFor(t, "the terminal progress notification", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(progress) > 0 && progress[len(progress)-1] == 1
	})
	mu.Lock()
	gotProgress := append([]float64(nil), progress...)
	gotTokens := append([]any(nil), tokens...)
	mu.Unlock()
	if len(gotProgress) < 2 {
		t.Fatalf("progress notifications = %v, want several phase boundaries", gotProgress)
	}
	for i := 1; i < len(gotProgress); i++ {
		if gotProgress[i] <= gotProgress[i-1] {
			t.Errorf("progress must be monotonic, got %v", gotProgress)
			break
		}
	}
	if gotProgress[len(gotProgress)-1] != 1 {
		t.Errorf("the final progress must be 1 (the run completed), got %v", gotProgress)
	}
	for _, tok := range gotTokens {
		if tok != "tok-explore" {
			t.Errorf("progressToken = %v, want the caller's own token echoed", tok)
		}
	}

	// --- logging/setLevel + notifications/message
	if err := session.SetLoggingLevel(ctx, &sdk.SetLoggingLevelParams{Level: "debug"}); err != nil {
		t.Fatalf("logging/setLevel: %v", err)
	}
	mu.Lock()
	logs = nil
	mu.Unlock()
	if _, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "explore", Arguments: exploreArgs(nil)}); err != nil {
		t.Fatalf("explore: %v", err)
	}
	waitFor(t, "notifications/message after logging/setLevel", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(logs) > 0
	})
	if err := session.SetLoggingLevel(ctx, &sdk.SetLoggingLevelParams{Level: "not-a-level"}); err == nil {
		t.Error("an unknown logging level must be refused")
	}
}

// Cancellation gets its own server: the cancelled call receives NO response (a SHOULD NOT on 2025-06-18,
// a MUST NOT on 2026-07-28 stdio), so the proof that anything happened has to come from somewhere else — here, from the idempotency key, which returns the
// very run that was cancelled.
func TestConformance_CancellationStopsTheRunAndCommitsNothing(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	exp := &fakeExplorer{block: block}
	session := connect(t, newServer(t, exp))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "explore",
			Arguments: exploreArgs(map[string]any{"idempotencyKey": "cancel-me", "waitSeconds": 60})})
		done <- err
	}()
	// Give the call time to reach the server and start the run, then cancel it. The SDK turns a cancelled
	// context into `notifications/cancelled` on the wire.
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled call must not return a result")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled call never returned")
	}

	// The run is reachable through its idempotency key, and it is CANCELLED — not quietly still running,
	// and not completed. Re-issuing the same key returns it rather than spending again.
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "explore",
			Arguments: exploreArgs(map[string]any{"idempotencyKey": "cancel-me", "waitSeconds": 1})})
		if err != nil {
			t.Fatalf("re-issue with the same key: %v", err)
		}
		out := structured(t, res)
		if out["state"] == "cancelled" {
			if out["reasonCode"] != "run_cancelled" {
				t.Errorf("reasonCode = %v, want run_cancelled", out["reasonCode"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cancelled run never settled: %v", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := exp.callCount(); n != 1 {
		t.Errorf("explorer ran %d times; a cancelled call re-issued with the same key must never re-spend", n)
	}
}

// tools/list paging: the exploremesh server ships a handful of tools and returns them in one page, but a
// cursor must be HONORED rather than ignored — including a foreign one, which is -32602 per spec.
func TestConformance_ToolsListCursorIsHonored(t *testing.T) {
	s := newServer(t, &fakeExplorer{})
	s.Core().PageSize = 3 // exercise real paging
	session := connect(t, s)
	ctx := context.Background()

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		res, err := session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatalf("tools/list(cursor=%q): %v", cursor, err)
		}
		pages++
		for _, tool := range res.Tools {
			if seen[tool.Name] {
				t.Errorf("tool %q appeared on two pages", tool.Name)
			}
			seen[tool.Name] = true
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
		if pages > 10 {
			t.Fatal("tools/list did not terminate")
		}
	}
	if len(seen) != 6 || pages != 2 {
		t.Errorf("paged listing saw %d tools over %d pages, want 6 over 2", len(seen), pages)
	}
	if _, err := session.ListTools(ctx, &sdk.ListToolsParams{Cursor: "bm90LWEtY3Vyc29y"}); err == nil {
		t.Error("an unrecognized cursor must be refused, not ignored")
	}
}

// STDOUT PURITY, at the surface level: while a run is in flight — with progress AND log notifications
// interleaving with responses — every byte on the protocol stream is a JSON-RPC frame. The server that
// spawns model CLIs gets one chance at this, so it is asserted rather than assumed.
func TestConformance_StdoutIsPureJSONRPCWhileRunning(t *testing.T) {
	exp := &fakeExplorer{}
	s := newServer(t, exp)
	var out lockedBuffer
	sr, cw := io.Pipe()
	served := make(chan struct{})
	go func() { _ = s.Serve(sr, &out); close(served) }()

	send := func(line string) {
		if _, err := cw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + proto.LatestProtocolVersion + `","clientInfo":{"name":"t","version":"1"}}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"debug"}}`)
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"explore","_meta":{"progressToken":9},"arguments":{"purpose":"p","criteria":["c"],"mode":"map"}}}`)

	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(out.String(), `"id":3`) {
		if time.Now().After(deadline) {
			t.Fatalf("no terminal response; stream so far:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cw.Close()
	<-served

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected initialize + setLevel + notifications + result, got:\n%s", out.String())
	}
	sawProgress, sawMessage := false, false
	for i, line := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("protocol stream line %d is not JSON: %q", i, line)
		}
		if v["jsonrpc"] != "2.0" {
			t.Errorf("protocol stream line %d is not JSON-RPC 2.0: %q", i, line)
		}
		switch v["method"] {
		case "notifications/progress":
			sawProgress = true
		case "notifications/message":
			sawMessage = true
		}
	}
	if !sawProgress || !sawMessage {
		t.Errorf("the purity assertion is only meaningful with notifications interleaved (progress=%v message=%v)", sawProgress, sawMessage)
	}
}

// lockedBuffer is a concurrency-safe writer for capturing the protocol stream.
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
