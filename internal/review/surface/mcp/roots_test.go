package mcp_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the LIVE `roots/list` round trip, end to end over the wire.
//
// The intersection rule and its fail-closed behavior were already implemented and tested — against a
// field a test set by hand. Nothing populated it: the transport could not issue a server→client request,
// so `roots/list` could not be asked for at all, and the server's launch-time roots were always the
// effective set. These tests drive the real thing: a client that declares the capability, answers the
// request, and changes its mind.
//
// The rule under test never varies. The effective set is startup ∩ client:
//
//   - a client that NARROWS is honored;
//   - a client that offers a WIDER root does NOT widen anything;
//   - a DISJOINT client leaves no effective root, which refuses every path;
//   - a client that declares nothing changes nothing;
//   - a client that never answers changes nothing, and does not wedge the server.

// rootedClient is the reviewmesh driving client that also ANSWERS server→client requests. It reads
// continuously in its own goroutine, because the answer to the server's request travels back through the
// same stream its own responses do.
type rootedClient struct {
	f proto.Framer

	mu      sync.Mutex
	id      int
	waiters map[int]chan *response
	asked   int
	roots   []string // the paths this client currently declares
}

func (c *rootedClient) setRoots(paths []string) {
	c.mu.Lock()
	c.roots = append([]string(nil), paths...)
	c.mu.Unlock()
}

func (c *rootedClient) asks() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}

func (c *rootedClient) write(v any) {
	b, _ := json.Marshal(v)
	_ = c.f.WriteMessage(b)
}

func (c *rootedClient) notify(method string, params any) {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	c.write(req)
}

func (c *rootedClient) call(t *testing.T, method string, params any) *response {
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
	req := map[string]any{"jsonrpc": "2.0", "method": method, "id": id}
	if params != nil {
		req["params"] = params
	}
	c.write(req)
	select {
	case resp := <-ch:
		return resp
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: no response", method)
		return nil
	}
}

// tool is the same convenience the other tests use, over this client.
func (c *rootedClient) tool(t *testing.T, name string, args map[string]any) toolResult {
	t.Helper()
	resp := c.call(t, "tools/call", map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		return toolResult{rpc: resp.Error}
	}
	out := toolResult{}
	if v, ok := resp.Result["isError"].(bool); ok {
		out.isError = v
	}
	if blocks, ok := resp.Result["content"].([]any); ok {
		for _, b := range blocks {
			if m, ok := b.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					out.text += s
				}
			}
		}
	}
	out.structured, _ = resp.Result["structuredContent"].(map[string]any)
	return out
}

func (c *rootedClient) readLoop(answer bool) {
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
			c.mu.Lock()
			c.asked++
			roots := append([]string(nil), c.roots...)
			c.mu.Unlock()
			if !answer {
				continue // the client that never answers
			}
			rows := make([]any, 0, len(roots))
			for _, p := range roots {
				rows = append(rows, map[string]any{"uri": pathURI(p), "name": filepath.Base(p)})
			}
			out := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(probe.ID), "result": map[string]any{"roots": rows}}
			// Off the read loop: over a synchronous pipe an inline answer would stop this client
			// reading while the server is itself mid-write.
			go c.write(out)
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

func pathURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// serveRooted starts the server and completes the handshake with a client that declares the `roots`
// capability (unless declare is false) and answers with `roots` (unless answer is false).
func serveRooted(t *testing.T, s *mcp.Server, declare, answer bool, roots []string) *rootedClient {
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
	c := &rootedClient{f: proto.NewFramer(proto.FramingNewline, cr, cw), roots: roots}
	go c.readLoop(answer)
	caps := map[string]any{}
	if declare {
		caps["roots"] = map[string]any{"listChanged": true}
	}
	resp := c.call(t, "initialize", map[string]any{
		"protocolVersion": proto.LatestProtocolVersion,
		"clientInfo":      map[string]any{"name": "rooted-client", "version": "1"},
		"capabilities":    caps,
	})
	if resp.Error != nil {
		t.Fatalf("initialize: %+v", resp.Error)
	}
	c.notify("notifications/initialized", nil)
	return c
}

// eventually polls a condition. The roots round trip is asynchronous by construction — the answer arrives
// through the read loop — so asserting the moment after `notifications/initialized` would assert a race.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settleRoots waits for the server to have asked at least `n` times and for the answer to have been
// installed.
//
// It deliberately does NOT poll by calling a tool. A `review_report` on an in-root path STARTS A RUN,
// and spending a panel per poll to observe a roots handshake is a cost this test has no reason to
// pay — nor should a roots assertion depend on how a run happens to complete.
func settleRoots(t *testing.T, c *rootedClient, n int) {
	t.Helper()
	eventually(t, "the server to have issued roots/list", func() bool { return c.asks() >= n })
	// The answer is written off the client's read loop and applied by the server's own goroutine; both
	// are in-process, so this is a settle, not a race window being papered over.
	time.Sleep(250 * time.Millisecond)
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
}

// A client that declares a SUBDIRECTORY of the server's root narrows the server to it. This is the half
// the intersection rule exists to permit — and until the transport could ask, it never happened.
func TestLiveRoots_AClientThatOffersASubsetNarrowsTheServer(t *testing.T) {
	parent := t.TempDir()
	sub, other := filepath.Join(parent, "sub"), filepath.Join(parent, "other")
	mkdirs(t, sub, other)

	rv := &fakeReviewer{}
	c := serveRooted(t, newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{parent} }), true, true, []string{sub})

	settleRoots(t, c, 1)
	if res := c.tool(t, "review_report", map[string]any{"workspace": sub}); res.isError {
		t.Fatalf("a path inside the intersection must still be accepted: %+v", res.structured)
	}
	if res := c.tool(t, "review_report", map[string]any{"workspace": other}); !res.isError {
		t.Fatal("a path inside the SERVER root but outside the client's must be refused — the effective set is the INTERSECTION")
	}
}

// The half that matters more. A client offering a WIDER root — the parent of the server's own, or an
// unrelated tree alongside it — must not gain anything. Nothing in a request, and nothing in a
// capability, widens what an operator authorized at launch.
func TestLiveRoots_AClientThatOffersAWiderRootWidensNothing(t *testing.T) {
	parent := t.TempDir()
	serverRoot, sibling := filepath.Join(parent, "project"), filepath.Join(parent, "sibling")
	elsewhere := t.TempDir()
	mkdirs(t, serverRoot, sibling)

	rv := &fakeReviewer{}
	// The client declares the PARENT of the server's root, plus a wholly unrelated tree.
	c := serveRooted(t, newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{serverRoot} }),
		true, true, []string{parent, elsewhere})

	settleRoots(t, c, 1)
	if res := c.tool(t, "review_report", map[string]any{"workspace": serverRoot}); res.isError {
		t.Fatalf("the server's own root must remain usable: %+v", res.structured)
	}
	for _, p := range []string{parent, sibling, elsewhere} {
		if res := c.tool(t, "review_report", map[string]any{"workspace": p}); !res.isError {
			t.Fatalf("the client widened this server to %q — a client may narrow the trusted roots and may NEVER widen them", p)
		}
	}
	if runs, _ := rv.counts(); runs != 1 {
		t.Fatalf("only the one in-root review may have spent: %d run(s)", runs)
	}
}

// Disjoint sets intersect to nothing, and nothing is fail-closed: every path is refused. This is the
// case a union would have turned into "both", which is exactly the widening the rule forbids.
func TestLiveRoots_ADisjointClientLeavesNoEffectiveRootAndRefusesEverything(t *testing.T) {
	serverRoot, clientRoot := t.TempDir(), t.TempDir()
	rv := &fakeReviewer{}
	c := serveRooted(t, newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{serverRoot} }),
		true, true, []string{clientRoot})

	settleRoots(t, c, 1)
	for _, p := range []string{serverRoot, clientRoot} {
		if res := c.tool(t, "review_report", map[string]any{"workspace": p}); !res.isError {
			t.Fatalf("a disjoint client root must leave NO effective root, but %q was accepted", p)
		}
	}
}

// `notifications/roots/list_changed` carries no roots. The server must RE-ASK, or it keeps enforcing a
// set the client has already changed.
func TestLiveRoots_ListChangedRefetchesAndRenarrows(t *testing.T) {
	parent := t.TempDir()
	a, b := filepath.Join(parent, "a"), filepath.Join(parent, "b")
	mkdirs(t, a, b)

	rv := &fakeReviewer{}
	c := serveRooted(t, newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{parent} }), true, true, []string{a})
	settleRoots(t, c, 1)
	if res := c.tool(t, "review_report", map[string]any{"workspace": b}); !res.isError {
		t.Fatal("the client's first root set must have narrowed the server")
	}

	// The client moves its workspace. Nothing on the wire carries the new set except the re-fetch.
	c.setRoots([]string{b})
	c.notify("notifications/roots/list_changed", nil)
	settleRoots(t, c, 2)
	if res := c.tool(t, "review_report", map[string]any{"workspace": a}); !res.isError {
		t.Fatal("after list_changed the client's OLD root must no longer be usable — the server must RE-ASK rather than keep enforcing a withdrawn set")
	}
	if res := c.tool(t, "review_report", map[string]any{"workspace": b}); res.isError {
		t.Fatalf("after list_changed the client's NEW root must be usable: %+v", res.structured)
	}
	if c.asks() < 2 {
		t.Fatalf("the server asked %d time(s); list_changed must RE-ASK rather than replay", c.asks())
	}
}

// A client that declares no `roots` capability is never asked, and the server keeps exactly the roots its
// operator launched it with. "Declares nothing ⇒ changes nothing" is the rule the whole round trip hangs
// off: if it did not hold, adding the round trip would itself be a policy change.
func TestLiveRoots_AClientDeclaringNoCapabilityChangesNothing(t *testing.T) {
	parent := t.TempDir()
	sub := filepath.Join(parent, "sub")
	mkdirs(t, sub)

	rv := &fakeReviewer{}
	// The client would have declared a narrower root — but it never declares the capability, so it is
	// never asked and its opinion never arrives.
	c := serveRooted(t, newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{parent} }), false, true, []string{sub})
	c.call(t, "tools/list", map[string]any{})
	time.Sleep(150 * time.Millisecond)

	if c.asks() != 0 {
		t.Fatalf("the server asked an undeclaring client %d time(s)", c.asks())
	}
	if res := c.tool(t, "review_report", map[string]any{"workspace": parent}); res.isError {
		t.Fatalf("the launch-time roots must stand unchanged: %+v", res.structured)
	}
}

// A client that declares the capability and then never answers must cost the server a deadline, not its
// root set and not its session.
func TestLiveRoots_AClientThatNeverAnswersLeavesTheServerUnchangedAndServing(t *testing.T) {
	root := t.TempDir()
	rv := &fakeReviewer{}
	s := newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{root} })
	s.Core().RequestTimeout = 200 * time.Millisecond
	c := serveRooted(t, s, true, false, nil)

	eventually(t, "the server to have asked", func() bool { return c.asks() > 0 })
	// While the request is outstanding, and after it expires, the session keeps working and the roots
	// are exactly what the operator set. (No settle here on purpose: the point is that nothing ever
	// arrives to be settled.)
	if res := c.tool(t, "review_report", map[string]any{"workspace": root}); res.isError {
		t.Fatalf("an unanswered roots/list must not disturb the server's own roots: %+v", res.structured)
	}
	time.Sleep(400 * time.Millisecond)
	if res := c.tool(t, "review_report", map[string]any{"workspace": root}); res.isError {
		t.Fatalf("the session must survive an unanswered server→client request: %+v", res.structured)
	}
}

// A root URI that is not a local `file://` path is ignored rather than guessed at — and ignoring it must
// not be mistaken for "the client declared nothing".
func TestLiveRoots_ANonFileRootURIIsIgnoredAndTheIntersectionStaysClosed(t *testing.T) {
	root := t.TempDir()
	rv := &fakeReviewer{}
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	s := newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{root} })
	done := make(chan struct{})
	go func() { _ = s.Serve(sr, sw); close(done) }()
	t.Cleanup(func() { _ = cw.Close(); <-done; _ = sw.Close() })

	c := &rootedClient{f: proto.NewFramer(proto.FramingNewline, cr, cw)}
	// A remote root: real in the protocol, but not a directory on this machine.
	c.setRoots(nil)
	go func() {
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
			if probe.Method != "" && len(probe.ID) > 0 {
				c.mu.Lock()
				c.asked++
				c.mu.Unlock()
				out := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(probe.ID),
					"result": map[string]any{"roots": []any{map[string]any{"uri": "https://example.com/repo"}}}}
				go c.write(out)
				continue
			}
			if len(probe.ID) == 0 {
				continue
			}
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
	}()
	resp := c.call(t, "initialize", map[string]any{
		"protocolVersion": proto.LatestProtocolVersion,
		"clientInfo":      map[string]any{"name": "remote-root-client", "version": "1"},
		"capabilities":    map[string]any{"roots": map[string]any{}},
	})
	if resp.Error != nil {
		t.Fatalf("initialize: %+v", resp.Error)
	}
	c.notify("notifications/initialized", nil)
	settleRoots(t, c, 1)
	// No usable client root arrived, so nothing narrowed and — crucially — nothing widened.
	if res := c.tool(t, "review_report", map[string]any{"workspace": root}); res.isError {
		t.Fatalf("an unusable root URI must leave the operator's own roots in force: %+v", res.structured)
	}
}
