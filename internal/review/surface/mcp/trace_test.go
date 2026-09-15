package mcp_test

import (
	"encoding/json"
	"io"
	"maps"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	mcp "github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// The server passes the client's _meta trace context to the manager verbatim, and nothing when the
// client sent none. The run package tests that the value reaches run-state.json; this covers the
// surface, whose failure a writer test cannot see.
//
// A legacy request must reach the manager with no trace, since the keys are a 2026-07-28 convention,
// rather than an empty block.
func TestReviewReport_CarriesTheCallersTraceContextToTheManager(t *testing.T) {
	const (
		parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		state  = "rojo=00f067aa0ba902b7"
		bag    = "userId=alice"
	)

	t.Run("a modern request carries it verbatim", func(t *testing.T) {
		rv, ws, c := modernTraceServer(t)
		res := c.modernCall(t, 1, "review_report", map[string]any{"workspace": ws}, map[string]any{
			"traceparent": parent, "tracestate": state, "baggage": bag,
		})
		if res.Error != nil {
			t.Fatalf("review_report: %+v", res.Error)
		}
		got := rv.lastRequest().Trace
		if got == nil {
			t.Fatal("the manager received no trace context; the surface parsed `_meta` and dropped it")
		}
		if got.TraceParent != parent || got.TraceState != state || got.Baggage != bag {
			t.Errorf("trace = %+v, want the caller's own three values carried verbatim", got)
		}
		if got.Measures != review.MeasuresCorrelationOnly {
			t.Errorf("measures = %q, want %q", got.Measures, review.MeasuresCorrelationOnly)
		}
	})

	t.Run("a modern request with none carries none", func(t *testing.T) {
		rv, ws, c := modernTraceServer(t)
		res := c.modernCall(t, 1, "review_report", map[string]any{"workspace": ws}, nil)
		if res.Error != nil {
			t.Fatalf("review_report: %+v", res.Error)
		}
		if got := rv.lastRequest().Trace; got != nil {
			t.Errorf("trace = %+v, want nil — an empty block reads as a real correlation identity that happens to be blank", got)
		}
	})

	t.Run("a legacy request carries none even when it sends the keys", func(t *testing.T) {
		// The legacy env never parses these keys; a legacy client that sends them is not refused, and nothing
		// reaches the run record.
		ws := workspaceFixture(t)
		rv := &tracingReviewer{}
		s := newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} })
		c := serve(t, s)
		res, _ := c.call(t, "tools/call", map[string]any{
			"name":      "review_report",
			"arguments": map[string]any{"workspace": ws, "panel": defaultPanel()},
			"_meta":     map[string]any{"traceparent": parent},
		})
		if res.Error != nil {
			t.Fatalf("review_report: %+v", res.Error)
		}
		if got := rv.lastRequest().Trace; got != nil {
			t.Errorf("a legacy request produced a trace block %+v; the three keys are a 2026-07-28 `_meta` convention and the legacy fill must not invent one", got)
		}
	})
}

// tracingReviewer is the canned reviewer, retaining the last request it received.
type tracingReviewer struct {
	fakeReviewer
}

func (r *tracingReviewer) lastRequest() reviewRequestSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return reviewRequestSnapshot{Trace: r.lastRun.Trace}
}

type reviewRequestSnapshot struct{ Trace *review.Trace }

// modernTraceServer returns a server whose manager records requests, and a raw client that speaks the
// modern era in-process.
func modernTraceServer(t *testing.T) (*tracingReviewer, string, *modernRawClient) {
	t.Helper()
	ws := workspaceFixture(t)
	rv := &tracingReviewer{}
	s := newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} })

	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	done := make(chan struct{})
	go func() { _ = s.Serve(sr, sw); close(done) }()
	t.Cleanup(func() {
		_ = cw.Close()
		<-done
		_ = sw.Close()
	})
	return rv, ws, &modernRawClient{f: proto.NewFramer(proto.FramingNewline, cr, cw)}
}

type modernRawClient struct{ f proto.Framer }

// modernCall sends one modern tools/call with the required _meta fields plus extraMeta, and returns the
// first response frame with id.
func (c *modernRawClient) modernCall(t *testing.T, id int, tool string, args, extraMeta map[string]any) *rawResponse {
	t.Helper()
	meta := map[string]any{
		proto.MetaKeyProtocolVersion:    proto.ProtocolVersion20260728,
		proto.MetaKeyClientCapabilities: map[string]any{},
	}
	maps.Copy(meta, extraMeta)
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": withPanel(tool, args), "_meta": meta},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := c.f.WriteMessage(b); err != nil {
		t.Fatalf("write: %v", err)
	}
	for {
		raw, rerr := c.f.ReadMessage()
		if rerr != nil {
			t.Fatalf("read: %v", rerr)
		}
		var resp rawResponse
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		var got int
		if len(resp.ID) > 0 && json.Unmarshal(resp.ID, &got) == nil && got == id && resp.Method == "" {
			return &resp
		}
	}
}

type rawResponse struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result map[string]any  `json:"result"`
	Error  *rawError       `json:"error"`
}

type rawError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
