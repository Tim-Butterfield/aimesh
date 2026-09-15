package mcp_test

import (
	"strings"
	"testing"
)

// TestRemediate_ApplyWithoutAllowWritesNamesTheLaunchFlag.
//
// A write is protected by two gates: the server must be LAUNCHED with --allow-writes, and every apply
// must additionally pass allowWrite. Those failures need opposite responses — one is a message to hand
// a human, the other the model fixes itself — so the launch refusal names the flag and points at what the
// caller can still do: ask for the diff.
func TestRemediate_ApplyWithoutAllowWritesNamesTheLaunchFlag(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv))
	res := c.tool(t, "review_remediate", map[string]any{"fromRun": "x", "workspace": ws, "output": "apply", "allowWrite": true})
	if res.rpc == nil {
		t.Fatalf("an apply on a server launched without --allow-writes must be refused, got %+v", res)
	}
	msg := res.rpc.Message
	if !strings.Contains(msg, "--allow-writes") {
		t.Errorf("the refusal does not name the launch flag that would grant it: %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "unknown tool") {
		t.Errorf("the refusal reads as a nonexistent tool rather than an ungranted write: %q", msg)
	}
	if !strings.Contains(msg, `"patch"`) {
		t.Errorf("the refusal must point at output=patch, which this server still supplies: %q", msg)
	}
	if runs, rem := rv.counts(); runs != 0 || rem != 0 {
		t.Fatalf("nothing may be spent: runs=%d remediations=%d", runs, rem)
	}
}

// TestRemediate_PatchNeedsNoWriteGrant: the diff is available on every server, and asking for it needs
// neither --allow-writes nor allowWrite, because a patch changes no project content.
func TestRemediate_PatchNeedsNoWriteGrant(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv))
	runID := reportRun(t, c, ws)
	res := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "workspace": ws, "output": "patch"})
	if res.rpc != nil || res.isError {
		t.Fatalf("a patch must be available without any write grant: %+v %+v", res.rpc, res.structured)
	}
	rv.mu.Lock()
	mode := rv.lastRemediate.Mode
	rv.mu.Unlock()
	if mode != "patch" {
		t.Fatalf("remediation mode = %q, want patch", mode)
	}
}
