package mcp_test

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// review_report's findings carry a host-computed fingerprint for review_remediate's select. An empty
// select is invalid params, never "apply everything".

// review_report discloses each finding's fingerprint.
func TestSelect_MCP_FingerprintIsDisclosedByReviewReport(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))

	res := c.tool(t, "review_report", map[string]any{"workspace": ws})
	if res.rpc != nil {
		t.Fatalf("review_report: %+v", res.rpc)
	}
	findings, _ := res.structured["findings"].([]any)
	if len(findings) == 0 {
		t.Fatalf("no findings in %+v", res.structured)
	}
	row, _ := findings[0].(map[string]any)
	fp, _ := row["fingerprint"].(string)
	if fp == "" {
		t.Fatal("review_report disclosed no `fingerprint` — a caller cannot select on a host-computed value it was never told, and this is the only moment we can tell it")
	}
	if !strings.HasPrefix(fp, "sha1:") {
		t.Errorf("fingerprint = %q, want the host-computed identity", fp)
	}
	// The disclosed selector is not the finding's id.
	if fp == row["id"] {
		t.Fatal("the disclosed selector IS the model-authored id — a model that could relabel findings could then steer which one a follow-up selection names")
	}
	if row["id"] != "F1" {
		t.Errorf("id = %v, want the model-authored label still present (it is what a human reads)", row["id"])
	}
}

// The selection reaches the write path unexamined; this surface resolves no fingerprint.
func TestSelect_MCP_ThreadsToTheWritePathUnexamined(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{
		"fromRun": runID, "workspace": ws, "output": "apply", "allowWrite": true,
		"select": []any{"sha1:aaa", "sha1:bbb"},
	})
	if res.rpc != nil {
		t.Fatalf("review_remediate with select: %+v", res.rpc)
	}
	rv.mu.Lock()
	got := rv.lastRemediate.Select
	rv.mu.Unlock()
	if len(got) != 2 || got[0] != "sha1:aaa" || got[1] != "sha1:bbb" {
		t.Fatalf("RemediateRequest.Select = %v, want the caller's list threaded through verbatim", got)
	}
}

// An empty select is refused before admission, so no run starts for a call that would write nothing.
func TestSelect_MCP_EmptySelectIsRefusedBeforeAnySpend(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	runID := reportRun(t, c, ws)
	_, before := rv.counts()

	res := c.tool(t, "review_remediate", map[string]any{
		"fromRun": runID, "workspace": ws, "output": "apply", "allowWrite": true, "select": []any{},
	})
	if res.rpc == nil {
		t.Fatalf("an empty `select` must be refused, got %+v", res.structured)
	}
	if res.rpc.Code != -32602 {
		t.Errorf("code = %d, want -32602", res.rpc.Code)
	}
	if !strings.Contains(res.rpc.Message, "ZERO findings") {
		t.Errorf("the refusal must say what an empty filter names: %q", res.rpc.Message)
	}
	if !strings.Contains(res.rpc.Message, "fingerprint") {
		t.Errorf("the refusal must name the correction: %q", res.rpc.Message)
	}
	if _, after := rv.counts(); after != before {
		t.Errorf("a refused selection still started a remediation (%d → %d) — the refusal must precede any spend", before, after)
	}
}

// A mistyped selector yields a smaller write, so unmatched selectors are reported and lead the text.
func TestSelect_MCP_UnmatchedSelectorsRideTheResultAndLeadTheText(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{selection: &review.ApplySelection{
		Requested: []string{"sha1:aaa", "sha1:typo"},
		Matched:   []string{"sha1:aaa"},
		Unmatched: []string{"sha1:typo"},
	}}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{
		"fromRun": runID, "workspace": ws, "output": "apply", "allowWrite": true,
		"select": []any{"sha1:aaa", "sha1:typo"},
	})
	if res.rpc != nil {
		t.Fatalf("a PARTIAL match must proceed: %+v", res.rpc)
	}
	sel, _ := res.structured["selection"].(map[string]any)
	if sel == nil {
		t.Fatalf("the result must carry `selection`: %+v", res.structured)
	}
	for _, k := range []string{"requested", "matched", "unmatched"} {
		if _, present := sel[k]; !present {
			t.Errorf("selection.%s is absent — all three lists are always present so \"none unmatched\" is a stated fact, not an absent key", k)
		}
	}
	un, _ := sel["unmatched"].([]any)
	if len(un) != 1 || un[0] != "sha1:typo" {
		t.Fatalf("selection.unmatched = %v, want the selector that named nothing", sel["unmatched"])
	}
	// The text leads with it, since some clients show the model nothing else.
	first, _, _ := strings.Cut(res.text, "\n")
	if !strings.Contains(first, "SELECTOR MATCHED NOTHING") || !strings.Contains(first, "sha1:typo") {
		t.Errorf("the first content line must name the unmatched selector, got %q\nfull text:\n%s", first, res.text)
	}
}

// A remediation without select carries no selection key.
func TestSelect_MCP_AbsentSelectionCarriesNoSelectionKey(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "workspace": ws, "output": "apply", "allowWrite": true})
	if res.rpc != nil {
		t.Fatalf("review_remediate: %+v", res.rpc)
	}
	if _, present := res.structured["selection"]; present {
		t.Errorf("`selection` must be absent when none was supplied: %+v", res.structured["selection"])
	}
	if strings.Contains(res.text, "SELECTOR") || strings.Contains(res.text, "Selective apply") {
		t.Errorf("the text channel must not manufacture a selection:\n%s", res.text)
	}
}

// select is declared in the input schema, including the rule that an empty list is refused.
func TestSelect_MCP_IsDeclaredInTheInputSchema(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	resp, _ := c.call(t, "tools/list", map[string]any{})
	tools, _ := resp.Result["tools"].([]any)
	var schema string
	for _, raw := range tools {
		tm, _ := raw.(map[string]any)
		if tm["name"] != "review_remediate" {
			continue
		}
		b, _ := tm["inputSchema"].(map[string]any)
		props, _ := b["properties"].(map[string]any)
		sel, _ := props["select"].(map[string]any)
		if sel == nil {
			t.Fatalf("review_remediate declares no `select`: %v", props)
		}
		if mi, _ := sel["minItems"].(float64); mi != 1 {
			t.Errorf("select.minItems = %v, want 1 — an empty array is refused, not widened", sel["minItems"])
		}
		schema, _ = sel["description"].(string)
	}
	if schema == "" {
		t.Fatal("review_remediate was not found in tools/list")
	}
	for _, want := range []string{"fingerprint", "NEVER a finding's 'id'", "REFUSED", "NARROW"} {
		if !strings.Contains(schema, want) {
			t.Errorf("the `select` description must state %q — a model reads this and nothing else:\n%s", want, schema)
		}
	}
}
