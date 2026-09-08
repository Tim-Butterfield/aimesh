package mcp_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// This file pins the PER-REQUEST PROTOCOL CONTEXT (env.go).
//
// AGAINST THE OLD CODE EVERY TEST HERE FAILS, and not only because the API is new: there was no
// per-request context at all. The progress token lived on the Call, the negotiated version and the log
// level lived on the Server, and a handler that wanted to know which roots governed its paths had to
// reach back into the application's process-global resolver. What is asserted below is that a request
// now CARRIES those facts, which is the precondition for the sessionless revision, where there is no
// session left to read them from.

// envProbe registers a tool that hands the test whatever the handler saw on its Call.
func envProbe(t *testing.T, tune ...func(*mcp.Server)) (*client, func(), func() *mcp.RequestEnv) {
	t.Helper()
	var mu sync.Mutex
	var seen *mcp.RequestEnv
	s := newServer(func(s *mcp.Server) {
		s.Register(mcp.Tool{Name: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				mu.Lock()
				seen = c.Env()
				mu.Unlock()
				return mcp.Result("ok", map[string]any{"ok": true}), nil
			})
		for _, f := range tune {
			f(s)
		}
	})
	c, stop := serve(t, s)
	return c, stop, func() *mcp.RequestEnv {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

// The envelope carries the request's own protocol facts, filled from the session under the legacy era
// — which is precisely why the envelope is invisible on the wire.
func TestRequestEnv_CarriesTheRequestsOwnProtocolContext(t *testing.T) {
	c, stop, seen := envProbe(t)
	defer stop()
	handshake(t, c)

	resp, _ := c.call(t, "tools/call", map[string]any{
		"name": "probe", "arguments": map[string]any{},
		"_meta": map[string]any{"progressToken": "tok-1"},
	})
	if resp.Error != nil {
		t.Fatalf("probe: %+v", resp.Error)
	}
	env := seen()
	if env == nil {
		t.Fatal("the handler received no request env")
	}
	if env.Method != "tools/call" {
		t.Errorf("env.Method = %q, want tools/call", env.Method)
	}
	if env.Era != mcp.EraLegacy {
		t.Errorf("env.Era = %q, want %q — no modern version is negotiable yet", env.Era, mcp.EraLegacy)
	}
	if env.Version != mcp.LatestProtocolVersion {
		t.Errorf("env.Version = %q, want the version negotiated at initialize (%q)", env.Version, mcp.LatestProtocolVersion)
	}
	if string(env.ProgressToken) != `"tok-1"` {
		t.Errorf("env.ProgressToken = %s, want the caller's token echoed verbatim", env.ProgressToken)
	}
	if env.LogLevel != mcp.LevelInfo {
		t.Errorf("env.LogLevel = %q, want the level in force at arrival (%q)", env.LogLevel, mcp.LevelInfo)
	}
}

// A call that volunteered no token must carry none. A server MUST NOT invent one — an unroutable
// progress notification is worse than no progress at all — and the env is now where that is decided.
func TestRequestEnv_NoProgressTokenIsCarriedWhenNoneWasSupplied(t *testing.T) {
	c, stop, seen := envProbe(t)
	defer stop()
	handshake(t, c)

	if _, notes := c.call(t, "tools/call", map[string]any{"name": "probe", "arguments": map[string]any{}}); len(notes) != 0 {
		t.Fatalf("unexpected notifications: %+v", notes)
	}
	if env := seen(); len(env.ProgressToken) != 0 {
		t.Errorf("env.ProgressToken = %s, want none", env.ProgressToken)
	}
	// And a JSON `null` token is the same as no token: it is a value, not a handle.
	c.call(t, "tools/call", map[string]any{
		"name": "probe", "arguments": map[string]any{}, "_meta": map[string]any{"progressToken": nil},
	})
	if env := seen(); len(env.ProgressToken) != 0 {
		t.Errorf("a null progressToken produced %s, want none", env.ProgressToken)
	}
}

// FAIL-CLOSED BY DEFAULT. A server that supplies no TrustContext is a server whose tools take no
// filesystem paths — so the env carries the ZERO resolver, which refuses everything. "No context" must
// never read as "no restrictions", and the provenance must say `none` rather than claim a human named
// roots that were never named.
func TestRequestEnv_TrustFailsClosedWhenTheServerSuppliesNoRoots(t *testing.T) {
	c, stop, seen := envProbe(t)
	defer stop()
	handshake(t, c)
	c.call(t, "tools/call", map[string]any{"name": "probe", "arguments": map[string]any{}})

	env := seen()
	if env.Trust == nil {
		t.Fatal("env.Trust must never be nil — a nil resolver is a resolver a handler will skip")
	}
	if len(env.Roots) != 0 {
		t.Errorf("env.Roots = %v, want none", env.Roots)
	}
	if env.RootSource != mcp.RootsNone {
		t.Errorf("env.RootSource = %q, want %q", env.RootSource, mcp.RootsNone)
	}
	if _, err := env.Trust.ResolveRead(t.TempDir()); err == nil {
		t.Fatal("the zero resolver accepted a path — a server with no roots must refuse every path")
	}
}

// THE POINT OF THE PHASE: the resolver is captured PER REQUEST, so a call is judged against the roots
// in force when it arrived rather than against whatever the process holds when the handler gets round
// to asking. The probe blocks mid-call while the provider's answer changes underneath it; the env it
// is holding must not change with it.
func TestRequestEnv_TrustIsCapturedPerRequestAndDoesNotMoveUnderAnInFlightCall(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	resA, err := scope.New(rootA)
	if err != nil {
		t.Fatalf("scope.New: %v", err)
	}
	resB, err := scope.New(rootB)
	if err != nil {
		t.Fatalf("scope.New: %v", err)
	}

	var mu sync.Mutex
	current := resA
	entered, release := make(chan struct{}), make(chan struct{})
	var envDuring *mcp.RequestEnv

	s := newServer(func(s *mcp.Server) {
		s.TrustFor = func(mcp.Era) mcp.TrustContext {
			mu.Lock()
			defer mu.Unlock()
			return mcp.TrustContext{Roots: current.Roots(), Source: mcp.RootsExplicit, Resolver: current}
		}
		s.Register(mcp.Tool{Name: "slow", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				envDuring = c.Env()
				close(entered)
				<-release
				// Resolve AFTER the provider has moved on: the verdict must come from the env this
				// call captured, not from the provider's current answer.
				_, aErr := c.Env().Trust.ResolveRead(rootA)
				_, bErr := c.Env().Trust.ResolveRead(rootB)
				return mcp.Result("ok", map[string]any{
					"aOK": aErr == nil, "bOK": bErr == nil,
				}), nil
			})
	})
	c, stop := serve(t, s)
	defer stop()
	handshake(t, c)

	done := make(chan *response, 1)
	go func() {
		resp, _ := c.call(t, "tools/call", map[string]any{"name": "slow", "arguments": map[string]any{}})
		done <- resp
	}()
	<-entered
	mu.Lock()
	current = resB // the process-global answer changes while the call is in flight
	mu.Unlock()
	close(release)

	resp := <-done
	if resp.Error != nil {
		t.Fatalf("slow: %+v", resp.Error)
	}
	structured, _ := resp.Result["structuredContent"].(map[string]any)
	if aOK, _ := structured["aOK"].(bool); !aOK {
		t.Error("the in-flight call lost the root it arrived with — the resolver is not per-request")
	}
	if bOK, _ := structured["bOK"].(bool); bOK {
		t.Error("the in-flight call picked up a root that arrived AFTER it did — the resolver is not per-request")
	}
	if envDuring.RootSource != mcp.RootsExplicit {
		t.Errorf("env.RootSource = %q, want the provenance the provider supplied", envDuring.RootSource)
	}
	if len(envDuring.Roots) != 1 {
		t.Errorf("env.Roots = %v, want the one root in force when the request arrived", envDuring.Roots)
	}
}
