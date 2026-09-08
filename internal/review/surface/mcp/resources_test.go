package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file closes the finding that made MCP `patch` mode a broken output mode: a doubly-gated governed
// write completed, wrote a diff, returned a run-record-relative NAME plus a sha256 — and the caller had
// no mechanism by which to obtain the artifact it had just paid for. `runDir` is deliberately dropped,
// stdout is the JSON-RPC transport, and `resources/*` did not exist.
//
// What is asserted here is the whole round trip: remediate → link → fetch → the same bytes. Plus the two
// properties that must survive it — no host path on the wire, and nothing readable but this run's own
// published artifacts.

const fixturePatch = "--- a/sample.go\n+++ b/sample.go\n@@ -1 +1 @@\n-package sample\n+package sample // fixed\n"

// patchingReviewer writes a REAL patch into a real run directory, so the resource round trip has actual
// bytes to deliver rather than a fixture path that never existed.
type patchingReviewer struct {
	fakeReviewer
	runDir string
	sha    string
}

func newPatchingReviewer(t *testing.T) *patchingReviewer {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "patches"), 0o755); err != nil {
		t.Fatalf("run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "patches", "changes.patch"), []byte(fixturePatch), 0o644); err != nil {
		t.Fatalf("patch fixture: %v", err)
	}
	return &patchingReviewer{runDir: dir, sha: sha256Of(fixturePatch)}
}

// sha256Of is the receipt's own digest form, so the store re-verifies against exactly what the receipt
// publishes rather than against a differently-spelled hash.
func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (p *patchingReviewer) Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error) {
	p.mu.Lock()
	p.remediations++
	p.lastRemediate = r
	p.mu.Unlock()
	return run.RemediateOutcome{
		RunID: "audit-run", RunDir: p.runDir, SourceRunID: r.SourceRunID, Mode: r.Mode,
		Receipt: run.Receipt{
			SchemaVersion: 1, RunID: "audit-run", SourceRunID: r.SourceRunID, Mode: string(r.Mode),
			Status: "complete", BaseHashesVerified: len(r.BaseHashes),
			Intended: []run.IntendedHunk{{FindingID: "F1", File: "sample.go", Bytes: 12, Origin: "marker"}},
			Applied:  []run.AppliedFinding{{FindingID: "F1", File: "sample.go", State: "applied"}},
			Files:    []string{"sample.go"}, Committed: false,
			PatchArtifact: "patches/changes.patch", PatchSHA256: p.sha,
		},
	}, nil
}

// callResult is c.call's result map, for a probe that only cares about the payload.
func callResult(t *testing.T, c *client, method string, params any) map[string]any {
	t.Helper()
	resp, _ := c.call(t, method, params)
	return resp.Result
}

// contentBlocks returns a tools/call result's raw content blocks.
func contentBlocks(t *testing.T, resp *response) []map[string]any {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("tools/call: %+v", resp.Error)
	}
	raw, _ := resp.Result["content"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		if m, ok := b.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// remediateForPatch runs a report then a patch-mode remediation and returns the remediation response.
func remediateForPatch(t *testing.T, c *client, ws string) *response {
	t.Helper()
	rep := c.tool(t, "review_report", map[string]any{"workspace": ws})
	if rep.isError {
		t.Fatalf("review_report: %+v", rep.structured)
	}
	runID, _ := rep.structured["runId"].(string)
	if runID == "" {
		t.Fatalf("review_report returned no runId: %+v", rep.structured)
	}
	resp, _ := c.call(t, "tools/call", map[string]any{
		"name":      "review_remediate",
		"arguments": map[string]any{"fromRun": runID, "output": "patch", "allowWrite": true},
	})
	return resp
}

// The whole point. A patch-mode remediation returns a link the caller can actually resolve, and resolving
// it yields the very bytes the receipt hashed.
func TestPatchIsRetrievableEndToEndOverTheProtocol(t *testing.T) {
	ws := workspaceFixture(t)
	rv := newPatchingReviewer(t)
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))

	resp := remediateForPatch(t, c, ws)
	structured, _ := resp.Result["structuredContent"].(map[string]any)
	receipt, _ := structured["receipt"].(map[string]any)
	uri, _ := receipt["patchResource"].(string)
	if uri == "" {
		t.Fatalf("the receipt carries no resolvable reference to the patch it produced: %+v", receipt)
	}
	if receipt["patchSha256"] != rv.sha {
		t.Fatalf("receipt.patchSha256 = %v, want %v", receipt["patchSha256"], rv.sha)
	}

	// The same URI also rides the result as a resource_link block, for a client that reads content
	// rather than structuredContent.
	var linked string
	for _, b := range contentBlocks(t, resp) {
		if b["type"] == "resource_link" {
			linked, _ = b["uri"].(string)
		}
	}
	if linked != uri {
		t.Fatalf("resource_link block = %q, want the receipt's own %q", linked, uri)
	}

	// FETCH IT. This is the step that did not exist.
	read, _ := c.call(t, "resources/read", map[string]any{"uri": uri})
	if read.Error != nil {
		t.Fatalf("resources/read(%s): %+v", uri, read.Error)
	}
	contents, _ := read.Result["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("resources/read returned %d block(s), want 1", len(contents))
	}
	block, _ := contents[0].(map[string]any)
	if block["text"] != fixturePatch {
		t.Fatalf("the fetched patch is not the artifact the receipt described:\n got %q\nwant %q", block["text"], fixturePatch)
	}
}

// The deliberate decision this preserves: no host filesystem path reaches the wire. It is asserted over
// the RAW frames, because a path leaking through a title or a description would be as much a leak as one
// in the URI.
func TestPatchResourceURICarriesNoHostPath(t *testing.T) {
	ws := workspaceFixture(t)
	rv := newPatchingReviewer(t)
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))

	resp := remediateForPatch(t, c, ws)
	structured, _ := resp.Result["structuredContent"].(map[string]any)
	receipt, _ := structured["receipt"].(map[string]any)
	uri, _ := receipt["patchResource"].(string)

	for _, probe := range []struct {
		what string
		v    any
	}{
		{"the remediation result", resp.Result},
		{"resources/list", callResult(t, c, "resources/list", map[string]any{})},
		{"resources/read", callResult(t, c, "resources/read", map[string]any{"uri": uri})},
	} {
		b, _ := json.Marshal(probe.v)
		if strings.Contains(string(b), rv.runDir) {
			t.Fatalf("%s leaked the run directory %q: %s", probe.what, rv.runDir, b)
		}
		if strings.Contains(string(b), ws) && probe.what != "resources/read" {
			t.Fatalf("%s leaked the workspace path %q: %s", probe.what, ws, b)
		}
	}
	if !strings.HasPrefix(uri, "aimesh://run/") {
		t.Fatalf("patch resource URI = %q, want the opaque run-scoped form", uri)
	}
}

// Only this run's own published artifacts are readable. Everything else is unaddressable, not merely
// checked — the store resolves a registration, never a caller-supplied path.
func TestResourcesRefuseAnythingOutsideTheRunsOwnArtifacts(t *testing.T) {
	ws := workspaceFixture(t)
	rv := newPatchingReviewer(t)
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))
	resp := remediateForPatch(t, c, ws)
	structured, _ := resp.Result["structuredContent"].(map[string]any)
	receipt, _ := structured["receipt"].(map[string]any)
	uri, _ := receipt["patchResource"].(string)
	runID := strings.TrimSuffix(strings.TrimPrefix(uri, "aimesh://run/"), "/patch")

	secret := filepath.Join(ws, "secret.txt")
	if err := os.WriteFile(secret, []byte("NOT-FOR-THE-WIRE"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, bad := range []string{
		"aimesh://run/" + runID + "/secret",
		"aimesh://run/" + runID + "/../../etc/hosts",
		"aimesh://run/some-other-run/patch",
		"file://" + filepath.ToSlash(secret),
		filepath.Join(rv.runDir, "patches", "changes.patch"),
	} {
		read, _ := c.call(t, "resources/read", map[string]any{"uri": bad})
		if read.Error == nil {
			t.Fatalf("resources/read(%q) succeeded: %+v", bad, read.Result)
		}
		b, _ := json.Marshal(read)
		if strings.Contains(string(b), "NOT-FOR-THE-WIRE") {
			t.Fatalf("resources/read(%q) returned content it must never reach", bad)
		}
	}
}

// A run with no patch publishes nothing, and `resources/list` says so honestly rather than advertising a
// link to an artifact that does not exist.
func TestResourcesListIsEmptyBeforeAnythingIsProduced(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Roots = []string{ws} }))
	list, _ := c.call(t, "resources/list", map[string]any{})
	if list.Error != nil {
		t.Fatalf("resources/list: %+v", list.Error)
	}
	rows, _ := list.Result["resources"].([]any)
	if len(rows) != 0 {
		t.Fatalf("resources/list returned %d row(s) before any run produced an artifact: %+v", len(rows), rows)
	}
}

// The `resources` capability is declared, because something backs it. A client that cannot see the
// capability will never call the method, so the declaration is load-bearing rather than decorative.
func TestResourcesCapabilityIsDeclared(t *testing.T) {
	ws := workspaceFixture(t)
	s := newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Roots = []string{ws} })
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	done := make(chan struct{})
	go func() { _ = s.Serve(sr, sw); close(done) }()
	t.Cleanup(func() { _ = cw.Close(); <-done; _ = sw.Close() })
	c := &client{f: proto.NewFramer(proto.FramingNewline, cr, cw)}
	resp, _ := c.call(t, "initialize", map[string]any{
		"protocolVersion": proto.LatestProtocolVersion,
		"clientInfo":      map[string]any{"name": "cap-client", "version": "1"},
	})
	if resp.Error != nil {
		t.Fatalf("initialize: %+v", resp.Error)
	}
	caps, _ := resp.Result["capabilities"].(map[string]any)
	if _, ok := caps["resources"]; !ok {
		t.Fatalf("the resources capability is not declared: %+v", caps)
	}
}
