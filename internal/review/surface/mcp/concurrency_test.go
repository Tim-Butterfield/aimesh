package mcp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// "Never apply the same accepted set twice" is a property of a LOCK, not of a check that happened
// to run earlier. This file races the write primitive against itself.

// delayedReviewer is a fakeReviewer whose remediation takes long enough that a second caller is
// certainly still inside the window where a check-then-act guard has already looked and not yet
// reserved. The delay is what makes the race deterministic rather than lucky.
type delayedReviewer struct {
	fakeReviewer
	delay time.Duration
}

func (s *delayedReviewer) Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error) {
	time.Sleep(s.delay)
	return s.fakeReviewer.Remediate(ctx, r)
}

// raceToolCalls issues n tools/call requests back to back WITHOUT waiting for any of them, then
// collects all n responses. The server dispatches tools/call concurrently, so the handlers really
// do overlap.
func raceToolCalls(t *testing.T, c *client, n int, name string, args func(i int) map[string]any) []toolResult {
	t.Helper()
	ids := make([]int, 0, n)
	for i := 0; i < n; i++ {
		c.id++
		ids = append(ids, c.id)
		c.send("tools/call", c.id, map[string]any{"name": name, "arguments": args(i)})
	}
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := make([]toolResult, 0, n)
	for len(want) > 0 {
		raw, err := c.f.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var resp response
		if json.Unmarshal(raw, &resp) != nil || len(resp.ID) == 0 {
			continue // a notification
		}
		var got int
		if json.Unmarshal(resp.ID, &got) != nil || !want[got] {
			continue
		}
		delete(want, got)
		res := toolResult{}
		if resp.Error != nil {
			res.rpc = resp.Error
		} else {
			res.isError, _ = resp.Result["isError"].(bool)
			res.structured, _ = resp.Result["structuredContent"].(map[string]any)
		}
		out = append(out, res)
	}
	return out
}

// TestRemediate_ConcurrentSameSourceRunAppliesOnce is the race the source-run guard exists to lose
// gracefully. Eight calls name the same accepted set at the same instant, each with its OWN
// idempotency key so the key guard cannot be what saves them. Exactly one write window may open;
// the other seven must attach to it.
//
// A second application is not a duplicated convenience. It is a write nobody asked for, against a
// tree whose base hashes no longer describe what the first write left behind.
func TestRemediate_ConcurrentSameSourceRunAppliesOnce(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &delayedReviewer{delay: 250 * time.Millisecond}
	s := newServer(t, rv, func(s *mcp.Server) {
		s.Roots, s.AllowRemediate = []string{ws}, true
		// Nothing here bounds admission — there is no server-wide governor — so all eight calls reach
		// the source-run guard. That is exactly what this test needs: a cap would have hidden part of
		// a broken guard behind "run refused" instead of exposing it as a second write.
	})
	c := serve(t, s)
	runID := reportRun(t, c, ws)

	const callers = 8
	results := raceToolCalls(t, c, callers, "review_remediate", func(i int) map[string]any {
		return map[string]any{
			"fromRun": runID, "output": "apply", "allowWrite": true,
			"idempotencyKey": string(rune('a'+i)) + "-key",
		}
	})
	if len(results) != callers {
		t.Fatalf("got %d responses, want %d", len(results), callers)
	}
	if _, rem := rv.counts(); rem != 1 {
		t.Fatalf("%d remediations ran for one accepted set — the decision set was applied more than once", rem)
	}
	// Every caller is answered, and every answer describes the SAME run: a loser attaches to the
	// winner rather than being told "refused" or handed a receipt of its own.
	seen := map[string]int{}
	for _, r := range results {
		if r.rpc != nil {
			t.Fatalf("a concurrent caller got a protocol error: %+v", r.rpc)
		}
		id, _ := r.structured["runId"].(string)
		if id == "" {
			t.Fatalf("a concurrent caller got no runId: %+v", r.structured)
		}
		seen[id]++
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent callers were spread over %d runs (%v); they must all attach to the winner", len(seen), seen)
	}
	for _, r := range results {
		receipt, _ := r.structured["receipt"].(map[string]any)
		if receipt == nil {
			t.Fatalf("an attached caller got no receipt: %+v", r.structured)
		}
		if committed, _ := receipt["committed"].(bool); !committed {
			t.Fatalf("the shared receipt must report the winner's commit: %+v", receipt)
		}
	}
}

// TestRemediate_WorkspaceIdentityRidesTheDecisionSet pins the surface half of the identity binding:
// the reviewed root's identity is captured by the REPORT run and carried to the write path. Without
// it the write path has only a path string, which is not a repository.
func TestRemediate_WorkspaceIdentityRidesTheDecisionSet(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "output": "patch", "allowWrite": true})
	if res.isError || res.rpc != nil {
		t.Fatalf("remediate failed: %+v %+v", res.structured, res.rpc)
	}
	rv.mu.Lock()
	got := rv.lastRemediate
	rv.mu.Unlock()
	if !got.WorkspaceIdentity.Bound() {
		t.Fatal("the remediation request carries no workspace identity, so the write path could only trust a path string")
	}
	if got.Mode != review.ModePatch {
		t.Fatalf("mode = %q, want patch", got.Mode)
	}
}
