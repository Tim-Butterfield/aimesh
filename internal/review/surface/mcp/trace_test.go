package mcp_test

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	mcp "github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// The SURFACE WIRING for the OpenTelemetry `_meta` trace context: what this server hands the manager
// is what the client put in `_meta`, verbatim, and nothing when the client put nothing there.
//
// This is a separate assertion from the manager-side one (`internal/manager/review/trace_test.go`,
// which proves the value reaches `run-state.json`). The two halves fail differently: a surface that
// never lifts the value off the env leaves a correct writer with nothing to write, and no test of
// the writer can see that. exploremesh's `TestSubprocess_ModernTraceContextReachesTheRunRecord`
// covers both halves at once over the real binary; reviewmesh's manager is faked here, so the two
// halves are pinned separately.
//
// It also pins the era boundary, which is the property most likely to be broken by accident: the
// three keys are a `2026-07-28` `_meta` convention, the legacy env never fills them, and a legacy
// request must therefore reach the manager with no trace at all rather than with an empty block that
// a reader would take for a real (and empty) correlation identity.
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
		// The legacy era's env is filled from session state and never parses these keys. A legacy
		// client that sends them anyway is not refused — the keys are simply not part of that
		// revision's `_meta` contract — and nothing reaches the run record.
		ws := workspaceFixture(t)
		rv := &tracingReviewer{}
		s := newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{ws} })
		c := serve(t, s)
		res, _ := c.call(t, "tools/call", map[string]any{
			"name":      "review_report",
			"arguments": map[string]any{"workspace": ws},
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

// tracingReviewer is the canned reviewer with the last request it was handed retained.
type tracingReviewer struct {
	fakeReviewer
}

func (r *tracingReviewer) lastRequest() reviewRequestSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return reviewRequestSnapshot{Trace: r.lastRun.Trace}
}

type reviewRequestSnapshot struct{ Trace *review.Trace }

// modernTraceServer wires a server whose manager records what it is handed, plus a raw client that
// can speak the modern era in-process.
func modernTraceServer(t *testing.T) (*tracingReviewer, string, *modernRawClient) {
	t.Helper()
	ws := workspaceFixture(t)
	rv := &tracingReviewer{}
	s := newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{ws} })

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

// modernCall sends one modern `tools/call` with the required `_meta` protocol fields plus whatever
// extra keys the caller names, and returns the first response frame carrying the request's id.
func (c *modernRawClient) modernCall(t *testing.T, id int, tool string, args, extraMeta map[string]any) *rawResponse {
	t.Helper()
	meta := map[string]any{
		proto.MetaKeyProtocolVersion:    proto.ProtocolVersion20260728,
		proto.MetaKeyClientCapabilities: map[string]any{},
	}
	for k, v := range extraMeta {
		meta[k] = v
	}
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args, "_meta": meta},
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
