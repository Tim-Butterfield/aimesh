package mcp_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// On the wire: the write rule (writes go through fromRun and the workspace that run reviewed), and the
// per-call `roots` argument (extra directories a call reads, always inside the operator's ceiling).

// --- writes go through fromRun, and only through fromRun ---

func TestRemediate_TheFullCycleFormIsRefusedWithATeachingError(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))

	res := c.tool(t, "review_remediate", map[string]any{
		"workspace": ws, "output": "apply", "allowWrite": true,
	})
	if res.rpc == nil {
		t.Fatalf("the one-call review-and-write form must be refused on MCP, got %+v", res)
	}
	// A refusal that only says "no" makes a model guess. This one has to hand back the exact path.
	for _, want := range []string{"review_report", "fromRun", "workspace", "allowWrite"} {
		if !strings.Contains(res.rpc.Message, want) {
			t.Fatalf("the teaching error must name %q; got %q", want, res.rpc.Message)
		}
	}
	if runs, rem := rv.counts(); runs != 0 || rem != 0 {
		t.Fatalf("the refusal must land before any spend: runs=%d remediations=%d", runs, rem)
	}
}

func TestRemediate_TheInputSchemaRequiresFromRunAndWorkspace(t *testing.T) {
	c := serve(t, newServer(t, &fakeReviewer{}))
	rem, ok := toolNames(t, c)["review_remediate"]
	if !ok {
		t.Fatal("review_remediate must be listed")
	}
	blob, err := json.Marshal(rem["inputSchema"])
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(blob, &schema); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fromRun", "workspace", "output"} {
		if !slices.Contains(schema.Required, want) {
			t.Fatalf("review_remediate's input schema must require %q: %s", want, blob)
		}
	}
}

// A workspace is required beside fromRun: it locates the run's record, and it must be the tree the run
// judged.
func TestRemediate_WorkspaceIsRequiredBesideFromRun(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "output": "patch"})
	if res.rpc == nil || !strings.Contains(res.rpc.Message, "`workspace` is required") {
		t.Fatalf("fromRun without a workspace must be a field-addressed refusal, got %+v", res)
	}
	if _, rem := rv.counts(); rem != 0 {
		t.Fatalf("nothing may be written: %d remediation(s)", rem)
	}
}

// A workspace other than the one the source run reviewed is refused before any write.
func TestRemediate_AWorkspaceTheRunDidNotReviewIsRefused(t *testing.T) {
	ws, other := workspaceFixture(t), workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "workspace": other, "output": "patch"})
	if !res.isError {
		t.Fatalf("a workspace the source run did not review must be refused, got %+v", res)
	}
	if code, _ := res.structured["reasonCode"].(string); code != mcp.ReasonSourceRunWorkspaceMismatch {
		t.Fatalf("reasonCode = %q, want %q", code, mcp.ReasonSourceRunWorkspaceMismatch)
	}
	if _, rem := rv.counts(); rem != 0 {
		t.Fatalf("nothing may be written: %d remediation(s)", rem)
	}
}

// --- the per-call `roots` argument, on the wire ---

func TestRootsArgument_AddsDirectoriesTheCallReads(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	temp := filepath.Join(parent, "temp")
	mkdirs(t, project, temp)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv))

	if res := c.tool(t, "review_report", map[string]any{"workspace": temp, "roots": []any{project}}); res.isError || res.rpc != nil {
		t.Fatalf("a call may read its workspace and the roots it declares: %+v %+v", res.structured, res.rpc)
	}
	rv.mu.Lock()
	got := append([]string(nil), rv.lastRun.TrustedRoots...)
	rv.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the run was given roots %v — it must carry the workspace and the extra root the call declared", got)
	}
}

func TestRootsArgument_ARelativeRootIsInvalidParams(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}))
	res := c.tool(t, "review_report", map[string]any{"workspace": ws, "roots": []any{"project"}})
	if res.rpc == nil {
		t.Fatalf("a relative root must be a malformed request, got %+v", res)
	}
}

func TestRootsArgument_CannotReachOutsideTheCeiling(t *testing.T) {
	ws, outside := workspaceFixture(t), t.TempDir()
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} }))

	// THE SECURITY PROPERTY. Naming a directory outside the operator's --root ceiling does not grant it.
	res := c.tool(t, "review_report", map[string]any{"workspace": ws, "roots": []any{outside}})
	if !res.isError {
		t.Fatalf("`roots` reached a directory outside the operator's ceiling: %+v", res.structured)
	}
	if runs, _ := rv.counts(); runs != 0 {
		t.Fatalf("the refusal must land before any spend: %d run(s)", runs)
	}
	blob, _ := json.Marshal(res.structured)
	if strings.Contains(string(blob), outside) || strings.Contains(string(blob), ws) {
		t.Fatalf("the refusal named a filesystem path: %s", blob)
	}
}

func TestRootsArgument_IsAcceptedOnTheWriteToo(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	docs := filepath.Join(parent, "docs")
	mkdirs(t, project, docs)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv))
	runID := reportRun(t, c, project)

	if res := c.tool(t, "review_remediate", map[string]any{
		"fromRun": runID, "workspace": project, "output": "patch", "roots": []any{docs},
	}); res.rpc != nil {
		t.Fatalf("review_remediate: %+v", res.rpc)
	}
	rv.mu.Lock()
	got := append([]string(nil), rv.lastRemediate.TrustedRoots...)
	rv.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the write window was recorded against %v — the write must carry the scope THIS call declared", got)
	}
}
