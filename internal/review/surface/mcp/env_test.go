package mcp_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// This file pins the reviewmesh half of the per-request protocol context: every path this server
// judges, and every root it hands the write path, comes from the REQUEST's own resolver.
//
// The trap it guards: a remediation that read `s.trusted().Roots()` from inside its own goroutine,
// minutes after the request arrived, would let a `roots/list_changed` in that window silently replace
// the roots the write window is then recorded against, so the receipt would describe a confinement
// the caller never asked for. The env captures the resolver once, when the call arrives, and the
// write is recorded against that.
//
// The property is proved on the `fromRun` write, which is the only write this surface has: D5 removes
// the FULL-CYCLE `review_remediate {workspace}` form (a write whose response is cancelled would leave
// the caller holding no handle to the run it created).

// gatedReviewer is the fake Manager with a gate: the phase under test blocks until the test releases
// it, which is the window in which the client changes its roots.
type gatedReviewer struct {
	fakeReviewer
	entered chan struct{}
	release chan struct{}
}

func (g *gatedReviewer) Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error) {
	close(g.entered)
	<-g.release
	return g.fakeReviewer.Remediate(ctx, r)
}

func TestPerRequestTrust_TheWriteIsRecordedAgainstTheRootsTheCallArrivedWith(t *testing.T) {
	parent := t.TempDir()
	ws, elsewhere := filepath.Join(parent, "project"), t.TempDir()
	mkdirs(t, ws)

	rv := &gatedReviewer{entered: make(chan struct{}), release: make(chan struct{})}
	s := newServer(t, rv, func(s *mcp.Server) {
		s.Roots, s.AllowRemediate = []string{parent}, true
	})
	// The client declares the server's own root, so nothing is narrowed to begin with.
	c := serveRooted(t, s, true, true, []string{parent})
	settleRoots(t, c, 1)

	// Turn one: the report that produces the durable handle. It completes before any write exists,
	// which is the whole point of the two-step rule.
	rep := c.tool(t, "review_report", map[string]any{"workspace": ws})
	if rep.rpc != nil {
		t.Fatalf("review_report: %+v", rep.rpc)
	}
	runID, _ := rep.structured["runId"].(string)
	if runID == "" {
		t.Fatalf("review_report returned no runId: %+v", rep.structured)
	}

	done := make(chan toolResult, 1)
	go func() {
		done <- c.tool(t, "review_remediate", map[string]any{
			"fromRun": runID, "output": "patch", "allowWrite": true, "waitSeconds": 30,
		})
	}()

	// The write window is open. The client moves its workspace to a DISJOINT tree, which empties the
	// server's effective root set — the process-global resolver now refuses everything.
	<-rv.entered
	c.setRoots([]string{elsewhere})
	c.notify("notifications/roots/list_changed", nil)
	settleRoots(t, c, 2)
	close(rv.release)

	if res := <-done; res.rpc != nil {
		t.Fatalf("review_remediate: %+v", res.rpc)
	}
	rv.mu.Lock()
	got := append([]string(nil), rv.lastRemediate.TrustedRoots...)
	rv.mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("the write window was recorded against %v — it must carry the roots the REQUEST arrived with, not whatever the process-global resolver held when the write happened to run", got)
	}
	// And a NEW call, which arrives after the narrowing, is judged by the narrowed set: per-request
	// means per-request in both directions.
	if res := c.tool(t, "review_report", map[string]any{"workspace": ws}); !res.isError {
		t.Fatal("a call arriving AFTER the client narrowed to a disjoint tree must be refused")
	}
}
