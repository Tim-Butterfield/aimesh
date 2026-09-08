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

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is reviewmesh's CONFORMANCE HARNESS, and it is deliberately the twin of
// exploremesh/internal/surface/mcp/conformance_test.go: the whole protocol tail, walked end to end by
// the OFFICIAL MCP Go SDK client rather than by a client written alongside the server.
//
// The pair exists because the asymmetry ran the wrong way. exploremesh had this coverage and
// reviewmesh did not — and reviewmesh is the server with the WRITE tool, whose tool list, annotations
// and cancellation semantics are the ones a host reads before it decides whether to ask a human. The
// shared transport (meshcore/mcp) being proved once elsewhere is not the same claim as "a real client
// can drive THIS server's tools", which is what these tests assert.
//
// The transcript it walks, in order:
//
//	initialize → notifications/initialized (the SDK's Connect does both, and REFUSES a server whose
//	echoed protocolVersion it does not support — so a successful Connect is itself an assertion)
//	→ tools/list, including a paged tools/list with a cursor
//	→ tools/call success
//	→ tools/call returning isError
//	→ a protocol error (unknown tool)
//	→ tools/call WITH a progressToken, and WITHOUT one
//	→ cancellation of an in-flight run
//	→ logging/setLevel + notifications/message
//	→ ping
//
// The dependency is test-only. `go build ./...` pulls none of it; the shipped binary's module graph is
// unchanged (verify with `GOWORK=off go list -deps ./cmd/...`, which must contain no
// `modelcontextprotocol` entry).

// eventingReviewer is a deterministic Manager stand-in that EMITS the pipeline's real phase events, so
// the progress and logging halves of the protocol have something to carry. It optionally blocks, for
// the cancellation transcript.
type eventingReviewer struct {
	fakeReviewer
	block chan struct{}
}

func (e *eventingReviewer) RunContext(ctx context.Context, r run.Request) (review.RunOutcome, error) {
	e.mu.Lock()
	e.runs++
	e.lastRun = r
	e.mu.Unlock()
	if r.OnEvent != nil {
		// Four PHASE boundaries from the fixed ladder (mcp.go's phaseFraction), in ascending order:
		// the progress stream is only meaningful if it moves.
		for _, ev := range []string{"run_started", "panel_started", "panel_completed", "host_adjudication_completed"} {
			r.OnEvent(audit.EventLine{Timestamp: "2026-01-01T00:00:00Z", Level: "info", EventType: ev, Message: ev + " reached"})
		}
	}
	if e.block != nil {
		select {
		case <-e.block:
		case <-ctx.Done():
			return review.RunOutcome{Status: "halted", Mode: r.Mode}, ctx.Err()
		}
	}
	return cannedOutcome(r.Mode), nil
}

// connect wires the server to in-memory pipes and returns an initialized OFFICIAL-SDK client session.
// It is `serve`'s counterpart: same server, a client nobody here wrote.
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

// structured decodes a result's structuredContent.
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

// TestConformance_FromRunFromDiskIsWireVisibleToAnSDKClient. This change altered the refusal surface
// of the ONE tool that writes: a `fromRun` naming a run the in-memory registry no longer holds used
// to answer `unknown_run_id` and now resolves from the run's own directory and APPLIES. A host
// decides whether to ask a human on the strength of `isError` and reads the outcome out of
// `structuredContent`, so the change is asserted here through the OFFICIAL SDK client — the same
// three things a real host reads, read by a client nobody in this repo wrote.
//
// Both halves are asserted, because a write tool that started succeeding is only half the claim: the
// handle that must STILL be refused is refused over the same transport, as a domain refusal carrying
// one reason code rather than as a protocol error.
func TestConformance_FromRunFromDiskIsWireVisibleToAnSDKClient(t *testing.T) {
	f := newFromRunFixture(t)
	sourceRun := f.report(t, f.server(t))

	// A server that never saw that run — a restarted one, in every respect that matters here.
	session := connect(t, newServer(t, f.mgr, func(s *mcp.Server) {
		s.Roots, s.AllowRemediate = []string{f.ws}, true
		s.PolicyCeiling = review.ModeApply
	}))
	ctx := context.Background()

	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "review_remediate", Arguments: map[string]any{
		"fromRun": sourceRun, "output": "apply", "allowWrite": true,
	}})
	if err != nil {
		t.Fatalf("a from-disk apply must not be a protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("a handle the registry no longer holds must still apply: %s", textOf(res))
	}
	st := structured(t, res)
	receipt, _ := st["receipt"].(map[string]any)
	if committed, _ := receipt["committed"].(bool); !committed {
		t.Fatalf("the receipt an SDK client reads must report the commit: %+v", st)
	}
	if !f.applied(t) {
		t.Fatalf("nothing was written to the live workspace; file = %q", f.body(t))
	}

	// The fail-closed half, over the same transport.
	bad, berr := session.CallTool(ctx, &sdk.CallToolParams{Name: "review_remediate", Arguments: map[string]any{
		"fromRun": t.TempDir(), "output": "apply", "allowWrite": true,
	}})
	if berr != nil {
		t.Fatalf("an unresolvable handle is a domain refusal, not a protocol error: %v", berr)
	}
	if !bad.IsError {
		t.Fatal("a handle this agent did not produce must set isError")
	}
	if code, _ := structured(t, bad)["reasonCode"].(string); code != "unknown_run_id" {
		t.Errorf("reasonCode = %q, want unknown_run_id", code)
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
	ws := workspaceFixture(t)
	rv := &eventingReviewer{}
	session := connect(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }), opts)
	ctx := context.Background()

	// --- lifecycle: a successful Connect already proves version negotiation + echo, since the SDK client
	// disconnects on a protocolVersion it does not support.
	init := session.InitializeResult()
	if init.ProtocolVersion != proto.LatestProtocolVersion {
		t.Errorf("negotiated protocolVersion = %q, want the client's own %q echoed", init.ProtocolVersion, proto.LatestProtocolVersion)
	}
	if init.ServerInfo == nil || init.ServerInfo.Name != mcp.ServerName {
		t.Errorf("serverInfo = %+v, want name %q", init.ServerInfo, mcp.ServerName)
	}

	// --- ping
	if err := session.Ping(ctx, nil); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// --- tools/list. Seven tools, because the write primitive is granted on this server; a host that
	// cannot enumerate them cannot present them.
	list, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(list.Tools) != 7 {
		t.Fatalf("tools = %d, want the 7 declared (review_report, review_remediate, list, doctor, agents_md, run_status, run_result)", len(list.Tools))
	}
	// The one annotation an SDK client is most likely to act on, read back through the SDK's own types:
	// the write tool is destructive and never read-only.
	for _, tool := range list.Tools {
		if tool.Name != "review_remediate" {
			continue
		}
		if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Errorf("review_remediate annotations = %+v, want destructiveHint true as an SDK client reads it", tool.Annotations)
		}
		if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
			t.Error("review_remediate must never read back as readOnlyHint through the official SDK")
		}
	}

	// --- tools/call: success
	ok, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "review_report", Arguments: map[string]any{"workspace": ws}})
	if err != nil {
		t.Fatalf("tools/call review_report: %v", err)
	}
	if ok.IsError {
		t.Fatalf("review_report returned isError: %s", textOf(ok))
	}
	if ok.StructuredContent == nil {
		t.Error("a successful result must carry structuredContent")
	}
	if len(ok.Content) == 0 {
		t.Error("a successful result must carry a human rendering too")
	}

	// --- tools/call: isError (a domain halt is NOT a protocol error)
	bad, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "review_run_result", Arguments: map[string]any{"runId": "nope"}})
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
	if _, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "review_report", Arguments: map[string]any{"workspace": ws}}); err != nil {
		t.Fatalf("review_report: %v", err)
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
	params := &sdk.CallToolParams{Name: "review_report", Arguments: map[string]any{"workspace": ws}}
	params.Meta = sdk.Meta{"progressToken": "tok-review"}
	if _, err := session.CallTool(ctx, params); err != nil {
		t.Fatalf("review_report with progress: %v", err)
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
		if tok != "tok-review" {
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
	if _, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "review_report", Arguments: map[string]any{"workspace": ws}}); err != nil {
		t.Fatalf("review_report: %v", err)
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
// a MUST NOT on 2026-07-28 stdio), so the proof that anything happened has to come from somewhere else — here, from the idempotency key, which
// returns the very run that was cancelled. On the server with the write tool this is the load-bearing
// one: a cancelled run must settle as CANCELLED, not linger as running and not report complete.
func TestConformance_CancellationStopsTheRunAndCommitsNothing(t *testing.T) {
	ws := workspaceFixture(t)
	block := make(chan struct{})
	defer close(block)
	rv := &eventingReviewer{block: block}
	session := connect(t, newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{ws} }))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "review_report",
			Arguments: map[string]any{"workspace": ws, "idempotencyKey": "cancel-me", "waitSeconds": 60}})
		done <- err
	}()
	// Give the call time to reach the server and start the run, then cancel it. The SDK turns a
	// cancelled context into `notifications/cancelled` on the wire.
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

	// The run is reachable through its idempotency key, and it is CANCELLED — not quietly still
	// running, and not completed. Re-issuing the same key returns it rather than spending again.
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "review_report",
			Arguments: map[string]any{"workspace": ws, "idempotencyKey": "cancel-me", "waitSeconds": 1}})
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
	if runs, rem := rv.counts(); runs != 1 || rem != 0 {
		t.Errorf("reviewer ran %d time(s) / %d remediation(s); a cancelled call re-issued with the same key must never re-spend and must never write", runs, rem)
	}
}

// tools/list paging: the reviewmesh server ships a handful of tools and returns them in one page, but
// a cursor must be HONORED rather than ignored — including a foreign one, which is -32602 per spec.
func TestConformance_ToolsListCursorIsHonored(t *testing.T) {
	ws := workspaceFixture(t)
	s := newServer(t, &eventingReviewer{}, func(sv *mcp.Server) { sv.Roots, sv.AllowRemediate = []string{ws}, true })
	s.Core().PageSize = 2 // exercise real paging
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
	if len(seen) != 7 || pages != 4 {
		t.Errorf("paged listing saw %d tools over %d pages, want 7 over 4", len(seen), pages)
	}
	if _, err := session.ListTools(ctx, &sdk.ListToolsParams{Cursor: "bm90LWEtY3Vyc29y"}); err == nil {
		t.Error("an unrecognized cursor must be refused, not ignored")
	}
}

// STDOUT PURITY, at the surface level: while a run is in flight — with progress AND log notifications
// interleaving with responses — every byte on the protocol stream is a JSON-RPC frame. The server that
// spawns model CLIs gets one chance at this, so it is asserted rather than assumed. The real-binary
// version of the same claim is in subprocess_test.go.
func TestConformance_StdoutIsPureJSONRPCWhileRunning(t *testing.T) {
	ws := workspaceFixture(t)
	s := newServer(t, &eventingReviewer{}, func(sv *mcp.Server) { sv.Roots = []string{ws} })
	var out lockedBuffer
	sr, cw := io.Pipe()
	served := make(chan struct{})
	go func() { _ = s.Serve(sr, &out); close(served) }()

	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := cw.Write(append(b, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": proto.LatestProtocolVersion,
		"clientInfo":      map[string]any{"name": "t", "version": "1"},
	}})
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "logging/setLevel", "params": map[string]any{"level": "debug"}})
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
		"name": "review_report", "_meta": map[string]any{"progressToken": 9},
		"arguments": map[string]any{"workspace": ws},
	}})

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
