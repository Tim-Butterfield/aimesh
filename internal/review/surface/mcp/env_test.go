package mcp_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// Every path this server judges, and every root it gives the write path, comes from the resolver
// captured when the request arrived. A roots/list_changed during a long remediation must not change
// the roots its write is recorded against. The property is checked on the fromRun write, the only
// write this surface has.

// gatedReviewer blocks the phase under test until released, giving the client a window to change
// its roots.
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
		s.Ceiling, s.AllowWrites = []string{parent}, true
	})
	// The client declares the server's own root, so nothing is narrowed initially.
	c := serveRooted(t, s, true, true, []string{parent})
	settleRoots(t, c, 1)

	// The report that produces the handle completes before any write exists.
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
			"fromRun": runID, "workspace": ws, "output": "patch", "allowWrite": true, "waitSeconds": 30,
		})
	}()

	// With the write window open, the client moves to a disjoint tree, emptying the server's effective
	// root set.
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
	// A new call after the narrowing is judged by the narrowed set.
	if res := c.tool(t, "review_report", map[string]any{"workspace": ws}); !res.isError {
		t.Fatal("a call arriving AFTER the client narrowed to a disjoint tree must be refused")
	}
}
