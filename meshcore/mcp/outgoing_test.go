package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// These tests cover the direction this transport could not speak at all: SERVER→CLIENT requests, and the
// one method that needs them — `roots/list`.
//
// Before this existed the server could only push notifications. It accepted the client's
// `notifications/roots/list_changed` and could do nothing with it, and the intersection rule that both
// governs and fails closed had nothing to intersect WITH: the launch-time roots were always the effective
// set. Everything below asserts the plumbing, not the policy; the policy (startup ∩ client, never a union)
// is asserted at the reviewmesh surface, which is the server that takes paths.

// rootsClient is a driving client that ANSWERS the server's requests. The client in server_test.go
// deliberately discards any frame whose id it did not mint, which is exactly what made a server→client
// request untestable there.
//
// It reads in its OWN goroutine, continuously, which is not a detail: a client that only reads while it
// is waiting for its own response cannot answer a request that arrives at any other moment — and over a
// synchronous pipe it would deadlock the server mid-write. A real client's read loop never stops, and the
// property under test is only meaningful against one that behaves the same way.
type rootsClient struct {
	f mcp.Framer

	mu       sync.Mutex
	id       int
	waiters  map[int]chan *response
	asked    []string // every method the SERVER asked this client for, in order
	askedIDs []string // the ids it used
	answer   func(method string) (any, *rpcErr)
}

func (c *rootsClient) send(method string, id any, params any) {
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

func (c *rootsClient) notify(method string, params any) { c.send(method, nil, params) }

// readLoop dispatches every inbound frame forever: a server→client REQUEST is answered, a response is
// handed to its waiter, a notification is dropped.
func (c *rootsClient) readLoop() {
	for {
		raw, err := c.f.ReadMessage()
		if err != nil {
			return
		}
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(raw, &probe) != nil {
			continue
		}
		switch {
		case probe.Method != "" && len(probe.ID) > 0:
			// A frame with BOTH an id and a method is a REQUEST from the server.
			var sid string
			_ = json.Unmarshal(probe.ID, &sid)
			c.mu.Lock()
			c.asked = append(c.asked, probe.Method)
			c.askedIDs = append(c.askedIDs, sid)
			ans := c.answer
			c.mu.Unlock()
			if ans == nil {
				continue // the never-answering client
			}
			result, rerr := ans(probe.Method)
			out := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(probe.ID)}
			if rerr != nil {
				out["error"] = map[string]any{"code": rerr.Code, "message": rerr.Message}
			} else {
				out["result"] = result
			}
			b, _ := json.Marshal(out)
			// Written off the read loop. Over a synchronous pipe, answering INLINE would stop this
			// client reading while the server is itself mid-write, and the two would stall against
			// each other — a harness artifact, but one a real client avoids the same way.
			//
			// This makes the client a CONCURRENT writer: the test body sends its own frames (the
			// `ping` that follows each notify) on the same Framer at the same moment. That is exactly
			// what a real client does, and it is safe because Framer.WriteMessage is atomic — it was
			// NOT, and the resulting interleave (`{ping}{answer}\n\n` read as one unparseable line)
			// is what made this file's roots test fail at random. See
			// TestFramer_WriteMessageIsAtomicUnderConcurrentWriters in meshcore/acp.
			go func() { _ = c.f.WriteMessage(b) }()
		case len(probe.ID) == 0:
			// a notification
		default:
			var got int
			var resp response
			if json.Unmarshal(raw, &resp) != nil || json.Unmarshal(probe.ID, &got) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.waiters[got]
			delete(c.waiters, got)
			c.mu.Unlock()
			if ch != nil {
				ch <- &resp
			}
		}
	}
}

func (c *rootsClient) call(t *testing.T, method string, params any) *response {
	t.Helper()
	ch := make(chan *response, 1)
	c.mu.Lock()
	c.id++
	id := c.id
	if c.waiters == nil {
		c.waiters = map[int]chan *response{}
	}
	c.waiters[id] = ch
	c.mu.Unlock()
	c.send(method, id, params)
	select {
	case resp := <-ch:
		return resp
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no response", method)
		return nil
	}
}

func (c *rootsClient) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

func (c *rootsClient) seenIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.askedIDs...)
}

// serveRoots wires a server to pipes and returns an answering client. `caps` is the client's declared
// capability object, verbatim, so a test can declare `roots` or deliberately not.
func serveRoots(t *testing.T, s *mcp.Server, caps map[string]any, answer func(string) (any, *rpcErr)) *rootsClient {
	t.Helper()
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	done := make(chan struct{})
	go func() { _ = s.Serve(sr, sw); close(done) }()
	t.Cleanup(func() {
		_ = cw.Close()
		<-done
		_ = sw.Close()
	})
	c := &rootsClient{f: mcp.NewFramer(mcp.FramingNewline, cr, cw), answer: answer}
	go c.readLoop()
	resp := c.call(t, "initialize", map[string]any{
		"protocolVersion": mcp.LatestProtocolVersion,
		"clientInfo":      map[string]any{"name": "roots-client", "version": "1"},
		"capabilities":    caps,
	})
	if resp.Error != nil {
		t.Fatalf("initialize: %+v", resp.Error)
	}
	return c
}

func fileURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// waitFor polls until cond holds or the deadline passes. The roots round trip is asynchronous by
// construction (the read loop must keep reading, because the answer arrives through it), so a test that
// asserted immediately after `notifications/initialized` would be asserting a race.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRootsList_IsIssuedAfterInitializedWhenTheClientDeclaresTheCapability(t *testing.T) {
	var mu sync.Mutex
	var got [][]mcp.Root
	s := newServer(func(s *mcp.Server) {
		s.OnRoots = func(rs []mcp.Root) {
			mu.Lock()
			got = append(got, rs)
			mu.Unlock()
		}
	})
	dir := t.TempDir()
	c := serveRoots(t, s, map[string]any{"roots": map[string]any{"listChanged": true}},
		func(method string) (any, *rpcErr) {
			return map[string]any{"roots": []any{map[string]any{"uri": fileURI(dir), "name": "ws"}}}, nil
		})
	c.notify("notifications/initialized", nil)
	// Drive the read loop so the server's request reaches this client and its answer gets written.
	c.call(t, "ping", nil)

	waitFor(t, "OnRoots after initialize", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	})
	mu.Lock()
	first := got[0]
	mu.Unlock()
	if len(first) != 1 || first[0].Name != "ws" {
		t.Fatalf("OnRoots got %+v, want the one root the client declared", first)
	}
	p, ok := first[0].Path()
	if !ok || p != dir {
		t.Fatalf("Root.Path() = %q,%v; want the client's own directory %q", p, ok, dir)
	}
	if seen := c.seen(); len(seen) == 0 || seen[0] != "roots/list" {
		t.Fatalf("the server asked the client for %v, want roots/list first", seen)
	}
}

// The gate is one-directional. A client that declared no `roots` capability is never asked — which is
// what makes "a client that declares nothing changes nothing" a property of the transport rather than a
// convention the application has to remember.
func TestRootsList_IsNotIssuedWhenTheClientDeclaresNoRootsCapability(t *testing.T) {
	var mu sync.Mutex
	called := 0
	s := newServer(func(s *mcp.Server) {
		s.OnRoots = func([]mcp.Root) { mu.Lock(); called++; mu.Unlock() }
	})
	c := serveRoots(t, s, map[string]any{"sampling": map[string]any{}},
		func(string) (any, *rpcErr) { return map[string]any{"roots": []any{}}, nil })
	c.notify("notifications/initialized", nil)
	c.call(t, "ping", nil)
	c.call(t, "tools/list", map[string]any{})
	// A list_changed from a client that never declared the capability must not provoke a fetch either.
	c.notify("notifications/roots/list_changed", nil)
	c.call(t, "ping", nil)
	time.Sleep(100 * time.Millisecond)

	if seen := c.seen(); len(seen) != 0 {
		t.Fatalf("the server asked an undeclaring client for %v; it must ask for nothing", seen)
	}
	mu.Lock()
	defer mu.Unlock()
	if called != 0 {
		t.Fatalf("OnRoots fired %d time(s) for a client that declared no roots capability", called)
	}
}

// `notifications/roots/list_changed` carries no roots. A server that acknowledged it without asking again
// would keep enforcing a set the client has already withdrawn, so the notification must RE-FETCH.
func TestRootsListChanged_RefetchesTheClientRoots(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	var mu sync.Mutex
	var rounds [][]mcp.Root
	next := 0
	s := newServer(func(s *mcp.Server) {
		s.OnRoots = func(rs []mcp.Root) { mu.Lock(); rounds = append(rounds, rs); mu.Unlock() }
	})
	c := serveRoots(t, s, map[string]any{"roots": map[string]any{"listChanged": true}},
		func(string) (any, *rpcErr) {
			mu.Lock()
			n := next
			next++
			mu.Unlock()
			dir := a
			if n > 0 {
				dir = b
			}
			return map[string]any{"roots": []any{map[string]any{"uri": fileURI(dir)}}}, nil
		})
	c.notify("notifications/initialized", nil)
	c.call(t, "ping", nil)
	waitFor(t, "the first roots/list", func() bool { mu.Lock(); defer mu.Unlock(); return len(rounds) == 1 })

	c.notify("notifications/roots/list_changed", nil)
	c.call(t, "ping", nil)
	waitFor(t, "the re-fetch after list_changed", func() bool { mu.Lock(); defer mu.Unlock(); return len(rounds) == 2 })

	mu.Lock()
	defer mu.Unlock()
	p0, _ := rounds[0][0].Path()
	p1, _ := rounds[1][0].Path()
	if p0 != a || p1 != b {
		t.Fatalf("rounds resolved to %q then %q; want %q then %q — list_changed must RE-ASK, not replay", p0, p1, a, b)
	}
	if seen := c.seen(); len(seen) != 2 {
		t.Fatalf("the server issued %v; want exactly two roots/list requests", seen)
	}
}

// A client that never answers must cost the server a deadline, not a session. This is the failure mode
// that makes server→client requests dangerous: the answer arrives through the very read loop that would
// be blocked if the request were issued synchronously.
func TestServerRequest_TimesOutWithoutWedgingTheSession(t *testing.T) {
	var mu sync.Mutex
	called := 0
	s := newServer(func(s *mcp.Server) {
		s.RequestTimeout = 150 * time.Millisecond
		s.OnRoots = func([]mcp.Root) { mu.Lock(); called++; mu.Unlock() }
	})
	c := serveRoots(t, s, map[string]any{"roots": map[string]any{}}, nil) // nil answer ⇒ never replies
	c.notify("notifications/initialized", nil)

	// The session keeps working while the request is outstanding, and after it expires.
	if resp := c.call(t, "tools/list", map[string]any{}); resp.Error != nil {
		t.Fatalf("the session must keep serving while a server→client request is outstanding: %+v", resp.Error)
	}
	waitFor(t, "the server to have asked", func() bool { return len(c.seen()) > 0 })
	time.Sleep(300 * time.Millisecond)
	if resp := c.call(t, "tools/list", map[string]any{}); resp.Error != nil {
		t.Fatalf("the session must survive an unanswered server→client request: %+v", resp.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if called != 0 {
		t.Fatalf("OnRoots fired %d time(s) for a client that never answered — an unanswered roots/list must change NOTHING", called)
	}
}

// Outgoing ids are NAMESPACED. A bare counter would make the server's `1` indistinguishable from a
// well-behaved client's `1`, and the two tables would cross.
func TestServerRequest_IDsAreNamespacedAwayFromTheClients(t *testing.T) {
	s := newServer(func(s *mcp.Server) { s.OnRoots = func([]mcp.Root) {} })
	c := serveRoots(t, s, map[string]any{"roots": map[string]any{}},
		func(string) (any, *rpcErr) { return map[string]any{"roots": []any{}}, nil })
	c.notify("notifications/initialized", nil)
	c.call(t, "ping", nil)
	waitFor(t, "the server to have asked", func() bool { return len(c.seenIDs()) > 0 })
	for _, id := range c.seenIDs() {
		if !strings.HasPrefix(id, "srv:") {
			t.Fatalf("server-issued request id %q is not namespaced; it can collide with a client id", id)
		}
	}
}

// A JSON-RPC error response to a server→client request is the CLIENT refusing, which is a different fact
// from the client never answering — and neither may be reported as roots.
func TestServerRequest_ClientErrorIsSurfacedAndChangesNothing(t *testing.T) {
	var mu sync.Mutex
	called := 0
	s := newServer(func(s *mcp.Server) {
		s.OnRoots = func([]mcp.Root) { mu.Lock(); called++; mu.Unlock() }
	})
	c := serveRoots(t, s, map[string]any{"roots": map[string]any{}},
		func(string) (any, *rpcErr) {
			return nil, &rpcErr{Code: mcp.CodeMethodNotFound, Message: "no roots here"}
		})
	c.notify("notifications/initialized", nil)
	c.call(t, "ping", nil)
	waitFor(t, "the server to have asked", func() bool { return len(c.seen()) > 0 })
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if called != 0 {
		t.Fatalf("OnRoots fired %d time(s) after the client REFUSED roots/list", called)
	}
}

// Request outside a live session is an error, not a panic and not a silent no-op.
func TestServerRequest_WithoutASessionIsAnError(t *testing.T) {
	s := newServer()
	if _, err := s.Request(context.Background(), "roots/list", nil); !errors.Is(err, mcp.ErrNoSession) {
		t.Fatalf("Request with no session = %v, want ErrNoSession", err)
	}
}

func TestRootPath(t *testing.T) {
	cases := []struct {
		uri  string
		want string
		ok   bool
	}{
		{"file:///Users/x/proj", "/Users/x/proj", true},
		{"file://localhost/Users/x/proj", "/Users/x/proj", true},
		{"file:///C:/proj", "C:/proj", true},
		{"file://otherhost/Users/x", "", false},
		{"https://example.com/x", "", false},
		{"", "", false},
		{"file://", "", false},
	}
	for _, tc := range cases {
		got, ok := mcp.RootPath(tc.uri)
		want := tc.want
		if want != "" {
			want = filepath.FromSlash(want)
		}
		if ok != tc.ok || got != want {
			t.Errorf("RootPath(%q) = (%q, %v), want (%q, %v)", tc.uri, got, ok, want, tc.ok)
		}
	}
}
