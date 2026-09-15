package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// A run that outlives waitSeconds is answered with {runId, state: "running"} and keeps running (see
// docs/mcp.md). On the legacy era its later events still reach the client as notifications/message;
// on 2026-07-28 the server sends none. The two tests run the same scenario on each era: the legacy one
// proves the harness can see a post-response log frame, and the modern one that none is sent.

// slowEventingReviewer emits events before and after the inline wait expires; the later ones arrive
// after the response.
type slowEventingReviewer struct {
	fakeReviewer
	afterResponse chan struct{} // closed once the post-response events have been emitted
}

func (s *slowEventingReviewer) RunContext(ctx context.Context, r run.Request) (review.RunOutcome, error) {
	s.mu.Lock()
	s.runs++
	s.lastRun = r
	s.mu.Unlock()
	emit := func(kind string) {
		if r.OnEvent == nil {
			return
		}
		r.OnEvent(audit.EventLine{
			Timestamp: "2026-01-01T00:00:00Z", Level: "error", EventType: kind, Message: kind + " happened",
		})
	}
	emit("run_started")
	// Outlive the one-second inline wait, then keep emitting.
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return review.RunOutcome{Status: "halted", Mode: r.Mode}, ctx.Err()
	}
	for range 5 {
		emit("panel_completed")
	}
	close(s.afterResponse)
	return cannedOutcome(r.Mode), nil
}

// tap keeps reading frames for as long as the test runs, capturing anything written after a response.
type tap struct {
	mu    sync.Mutex
	notes []string
}

func (tp *tap) start(f proto.Framer) {
	go func() {
		for {
			raw, err := f.ReadMessage()
			if err != nil {
				return
			}
			tp.mu.Lock()
			tp.notes = append(tp.notes, string(raw))
			tp.mu.Unlock()
		}
	}()
}

func (tp *tap) count(method string) int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	n := 0
	for _, line := range tp.notes {
		var fr struct {
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(line), &fr) == nil && fr.Method == method {
			n++
		}
	}
	return n
}

// runBackgroundedReport runs one review_report on era that outlives its inline wait and returns what
// the server wrote afterwards.
func runBackgroundedReport(t *testing.T, era proto.Era) *tap {
	t.Helper()
	ws := workspaceFixture(t)
	rv := &slowEventingReviewer{afterResponse: make(chan struct{})}
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
	f := proto.NewFramer(proto.FramingNewline, cr, cw)
	tp := &tap{}

	send := func(id int, method string, params map[string]any) {
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := f.WriteMessage(b); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	args := map[string]any{"workspace": ws, "waitSeconds": 1, "panel": defaultPanel()}
	params := map[string]any{"name": "review_report", "arguments": args}
	switch era {
	case proto.EraModern:
		// Send a stray notifications/initialized first. The legacy log sink will not emit before it, so
		// without it the modern transcript would show no log frames even if the era check were removed.
		nb, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		if err := f.WriteMessage(nb); err != nil {
			t.Fatalf("write: %v", err)
		}
		params["_meta"] = map[string]any{
			proto.MetaKeyProtocolVersion:    proto.ProtocolVersion20260728,
			proto.MetaKeyClientCapabilities: map[string]any{},
			// The client requests debug logs; on this era the request is accepted and ignored.
			proto.MetaKeyLogLevel: "debug",
		}
		tp.start(f)
		send(1, "tools/call", params)
	default:
		// The legacy handshake, with logging set to debug so nothing is filtered.
		send(1, "initialize", map[string]any{
			"protocolVersion": proto.LatestProtocolVersion,
			"clientInfo":      map[string]any{"name": "log-test", "version": "1"},
		})
		if _, err := f.ReadMessage(); err != nil {
			t.Fatalf("initialize: %v", err)
		}
		nb, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		if err := f.WriteMessage(nb); err != nil {
			t.Fatalf("write: %v", err)
		}
		send(2, "logging/setLevel", map[string]any{"level": "debug"})
		if _, err := f.ReadMessage(); err != nil {
			t.Fatalf("logging/setLevel: %v", err)
		}
		tp.start(f)
		send(3, "tools/call", params)
	}

	// Wait until the post-response events have been emitted.
	select {
	case <-rv.afterResponse:
	case <-time.After(20 * time.Second):
		t.Fatal("the backgrounded run never reached its post-response events")
	}
	time.Sleep(250 * time.Millisecond) // let anything emitted reach the wire
	return tp
}

func TestBackgroundedRun_LegacyStillLogsAfterTheResponse(t *testing.T) {
	// The control: the harness can see a post-response log frame, and legacy clients still receive them.
	tp := runBackgroundedReport(t, proto.EraLegacy)
	if got := tp.count("notifications/message"); got == 0 {
		t.Fatal("the legacy era stopped emitting notifications/message for a backgrounded run — that is a wire-visible change to the surface this migration must leave byte-identical, and it would also mean the modern assertion below is blind")
	}
}

func TestBackgroundedRun_ModernEmitsNoLogNotificationsBeforeOrAfterTheResponse(t *testing.T) {
	tp := runBackgroundedReport(t, proto.EraModern)
	if got := tp.count("notifications/message"); got != 0 {
		t.Fatalf("%d notifications/message frame(s) on the modern era, for a request that had already been answered. `server/utilities/logging`: the notification is request-scoped and the server \"MUST NOT deliver it … on any stream other than the one carrying the response to the request that set the log level\" — and this server emits none at all on this revision", got)
	}
	// Progress still works, so the absence of logs is not a broken sink.
	if got := tp.count("notifications/progress"); got != 0 {
		// No progress token was supplied, so no progress may be sent.
		t.Fatalf("%d progress notification(s) with no caller-supplied token", got)
	}
}
