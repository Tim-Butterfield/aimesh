package mcp_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// On the wire: the write rule (D5), the narrowing-only `roots` argument, and the waiver's
// disclosure (D6). These are black-box — they drive the real tool surface — because all three are
// wire-visible on the era that is reachable today. Only the era-conditional half of the confinement
// fix needs an in-package test, and it has one.

// --- D5: MCP writes go through fromRun, and only through fromRun ---

func TestRemediate_TheFullCycleFormIsRefusedWithATeachingError(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))

	res := c.tool(t, "review_remediate", map[string]any{
		"workspace": ws, "output": "apply", "allowWrite": true,
	})
	if res.rpc == nil {
		t.Fatalf("the one-call review-and-write form must be refused on MCP, got %+v", res)
	}
	// A refusal that only says "no" makes a model guess. This one has to hand back the exact path.
	for _, want := range []string{"review_report", "fromRun", "allowWrite"} {
		if !strings.Contains(res.rpc.Message, want) {
			t.Fatalf("the teaching error must name %q; got %q", want, res.rpc.Message)
		}
	}
	if _, rem := rv.counts(); rem != 0 {
		t.Fatalf("nothing may be written: %d remediation(s)", rem)
	}
	// And it is refused BEFORE any spend: no review was started either.
	if runs, _ := rv.counts(); runs != 0 {
		t.Fatalf("the refusal must land before any run is started: %d run(s)", runs)
	}
}

func TestRemediate_TheInputSchemaNoLongerOffersAFullCycleBranch(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))
	tools := toolNames(t, c)
	rem, ok := tools["review_remediate"]
	if !ok {
		t.Fatal("review_remediate must be listed once the capability is granted")
	}
	blob, err := json.Marshal(rem["inputSchema"])
	if err != nil {
		t.Fatal(err)
	}
	// The schema is a client's only description of what it may send. Leaving a `oneOf` branch there
	// that the server refuses would be an advertised call that cannot succeed.
	if strings.Contains(string(blob), "oneOf") {
		t.Fatalf("review_remediate's input schema still advertises a second form: %s", blob)
	}
	if !strings.Contains(string(blob), "fromRun") {
		t.Fatalf("review_remediate's input schema must still require fromRun: %s", blob)
	}
}

// TestRemediate_WorkspaceBesideFromRunIsRefused is a PRESERVATION guard: `workspace` beside
// `fromRun` is refused, and the line that enforces it has no other test of its own.
func TestRemediate_WorkspaceBesideFromRunIsRefused(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{
		"fromRun": runID, "workspace": ws, "output": "patch", "allowWrite": true,
	})
	if res.rpc == nil {
		t.Fatalf("naming a workspace beside fromRun must be refused: the workspace is the SOURCE RUN's, and re-naming it would let a write land where the accepted set was never judged; got %+v", res)
	}
	if _, rem := rv.counts(); rem != 0 {
		t.Fatalf("nothing may be written: %d remediation(s)", rem)
	}
}

// --- the narrowing-only `roots` argument, on the wire ---

func TestRootsArgument_NarrowsAndNeverWidens(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	sibling := filepath.Join(parent, "sibling")
	mkdirs(t, project, sibling)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{parent} }))

	// Narrowing to `project` refuses a sibling the call could otherwise have reviewed.
	res := c.tool(t, "review_report", map[string]any{"workspace": sibling, "roots": []any{project}})
	if !res.isError {
		t.Fatalf("a call narrowed to %q must refuse a sibling directory, got %+v", project, res)
	}
	// …and the same narrowing still permits the directory it named.
	if res := c.tool(t, "review_report", map[string]any{"workspace": project, "roots": []any{project}}); res.isError {
		t.Fatalf("the narrowed call must still reach the directory it narrowed to: %+v", res.structured)
	}
	rv.mu.Lock()
	got := append([]string(nil), rv.lastRun.TrustedRoots...)
	rv.mu.Unlock()
	if len(got) != 1 || got[0] == parent {
		t.Fatalf("the run was given roots %v — a narrowed call must hand the NARROWED set to the manager, or the write path would re-widen it", got)
	}
}

func TestRootsArgument_CannotReachOutsideTheOperatorsRoots(t *testing.T) {
	ws, outside := workspaceFixture(t), t.TempDir()
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots = []string{ws} }))

	// THE SECURITY PROPERTY. Naming a directory the operator never granted does not grant it: the
	// intersection drops it, leaves nothing, and the call is refused. The worst a hostile or confused
	// caller can do with this argument is refuse its own call.
	res := c.tool(t, "review_report", map[string]any{"workspace": outside, "roots": []any{outside}})
	if !res.isError {
		t.Fatalf("`roots` granted a directory outside the operator's trusted roots — it must never be a union: %+v", res.structured)
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
	mkdirs(t, project)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{parent}, true }))
	runID := reportRun(t, c, project)

	if res := c.tool(t, "review_remediate", map[string]any{
		"fromRun": runID, "output": "patch", "allowWrite": true, "roots": []any{project},
	}); res.rpc != nil {
		t.Fatalf("review_remediate: %+v", res.rpc)
	}
	rv.mu.Lock()
	got := append([]string(nil), rv.lastRemediate.TrustedRoots...)
	rv.mu.Unlock()
	if len(got) != 1 || got[0] == parent {
		t.Fatalf("the write window was recorded against %v — the call's own narrowing must reach the write, or a receipt describes a confinement the caller did not ask for", got)
	}
}
