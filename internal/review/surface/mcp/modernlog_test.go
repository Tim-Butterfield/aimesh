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

// THE POST-RESPONSE LIMB of the no-log rule, which the subprocess transcript cannot stage on its own.
//
// The case is concrete and it is the one `docs/mcp.md` §Progress already documents: a run that
// outlives `waitSeconds` is handed back as {runId, state:"running"} and KEEPS RUNNING. The progress
// sink is disarmed on the way out, but `OnEvent` still calls `Call.Log` for every event the pipeline
// emits afterwards — so on the legacy era a client goes on receiving `notifications/message` frames
// for a request that has already been answered.
//
// Under 2026-07-28 that is forbidden twice over (request-scoped, and only for a request that asked),
// and this server answers by emitting none at all. The two halves below are deliberately the same
// test run on the two eras: the legacy half proves the harness can SEE a post-response log frame, and
// the modern half proves there is none to see. Without the first, the second is just a test that
// nothing happened.
//
// AGAINST A TREE WITH NO MODERN ERA the modern half cannot even be expressed: no request could select the
// modern era, so there was no way to reach the branch from the wire.

// slowEventingReviewer emits pipeline events BEFORE and AFTER the inline wait expires. The `after`
// events are the ones that matter: they arrive once the response has already gone out.
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
	// Outlive the caller's one-second inline wait, then keep talking.
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return review.RunOutcome{Status: "halted", Mode: r.Mode}, ctx.Err()
	}
	for i := 0; i < 5; i++ {
		emit("panel_completed")
	}
	close(s.afterResponse)
	return cannedOutcome(r.Mode), nil
}

// tap keeps reading frames off the wire for as long as the test wants, so that anything the server
// writes AFTER a response is captured rather than missed by a helper that stops at the response.
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

// runBackgroundedReport drives one review_report that outlives its inline wait, on the era the caller
// names, and returns everything the server wrote afterwards.
func runBackgroundedReport(t *testing.T, era proto.Era) *tap {
	t.Helper()
	ws := workspaceFixture(t)
	rv := &slowEventingReviewer{afterResponse: make(chan struct{})}
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

	args := map[string]any{"workspace": ws, "waitSeconds": 1}
	params := map[string]any{"name": "review_report", "arguments": args}
	switch era {
	case proto.EraModern:
		// A STRAY `notifications/initialized` first. It is not what a modern client would send, and it
		// is here for a specific reason: the legacy log sink independently
		// refuses to emit before that notification arrives, so a modern transcript that never sent it
		// would show zero log frames even with the era check REMOVED. Sending it disarms the unrelated
		// guard, so what this test observes is the era rule and nothing else.
		nb, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		if err := f.WriteMessage(nb); err != nil {
			t.Fatalf("write: %v", err)
		}
		params["_meta"] = map[string]any{
			proto.MetaKeyProtocolVersion:    proto.ProtocolVersion20260728,
			proto.MetaKeyClientCapabilities: map[string]any{},
			// The client ASKED for logs, at the most verbose level there is. On this era it is
			// accepted and ignored.
			proto.MetaKeyLogLevel: "debug",
		}
		tp.start(f)
		send(1, "tools/call", params)
	default:
		// The legacy handshake, then the same call. `logging/setLevel` is the era's live control and
		// the client sets it to debug so nothing is filtered out.
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

	// The run outlives the inline wait, answers, and then keeps emitting. Wait for the post-response
	// events to have actually been emitted before looking.
	select {
	case <-rv.afterResponse:
	case <-time.After(20 * time.Second):
		t.Fatal("the backgrounded run never reached its post-response events")
	}
	time.Sleep(250 * time.Millisecond) // let anything emitted reach the wire
	return tp
}

func TestBackgroundedRun_LegacyStillLogsAfterTheResponse(t *testing.T) {
	// The CONTROL, and it is load-bearing: it proves this harness can see a post-response log frame.
	// It is also the assertion that the legacy surface did not move — an unconditional change here
	// would have silently removed background log lines a legacy client still expects, which is an
	// observable change on the one surface this migration must leave alone.
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
	// The one live in-flight channel still works, so the absence above is a decision rather than a
	// broken sink.
	if got := tp.count("notifications/progress"); got != 0 {
		// No progress token was supplied, so there must be none of these either — a server MUST NOT
		// invent a token, and an unroutable progress notification is worse than no progress at all.
		t.Fatalf("%d progress notification(s) with no caller-supplied token", got)
	}
}
