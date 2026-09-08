package mcp_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// D6's CONDITION: the waiver's state must appear in `doctor` and the readiness projection, not only
// as a stderr line at launch. A host launches its servers from a config file, and the stdio transport
// says in as many words that the client "MAY capture, forward, or ignore the server's stderr output"
// — so a security-relevant setting visible only there is disclosed in name only.
//
// AGAINST THE OLD CODE these do not compile: Server had no RootSource and no AllowInferredRoot, and
// there was no readiness entry to name. That is stated rather than dressed up as a behavioral
// failure. Kept in their own file so the wire-behaviour tests beside them (writerule_test.go) stay
// compilable against the old tree and can fail on behaviour instead.

func TestDoctor_DisclosesRootProvenanceAndTheWaiver(t *testing.T) {
	ws := workspaceFixture(t)
	for _, tc := range []struct {
		name       string
		source     acp.RootSource
		waived     bool
		wantWaived bool
		wantDetail string
	}{
		{"an explicit root needs no waiver", acp.RootsExplicit, false, false, "EXPLICIT"},
		{"an inferred root, unwaived, says what it will refuse", acp.RootsInferredCwd, false, false, "INFERRED"},
		{"an inferred root, waived, says so", acp.RootsInferredCwd, true, true, "--allow-inferred-root is SET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
				s.Roots, s.RootSource, s.AllowInferredRoot = []string{ws}, tc.source, tc.waived
			}))
			res := c.tool(t, "review_doctor", map[string]any{})
			if got, _ := res.structured["inferredRootWaived"].(bool); got != tc.wantWaived {
				t.Fatalf("inferredRootWaived = %v, want %v", got, tc.wantWaived)
			}
			if got, _ := res.structured["rootCount"].(float64); int(got) != 1 {
				t.Fatalf("rootCount = %v, want 1", res.structured["rootCount"])
			}
			if got, _ := res.structured["rootNarrowing"].(string); got != "startup-only" {
				t.Fatalf("rootNarrowing = %q, want startup-only", got)
			}
			// The readiness ENTRY is what makes this flow to every consumer of the projection rather
			// than to this one tool's payload — that is D6's condition.
			var detail string
			checks, _ := res.structured["checks"].([]any)
			for _, raw := range checks {
				row, _ := raw.(map[string]any)
				if row["name"] == mcp.RootConfinementCheck {
					detail, _ = row["detail"].(string)
				}
			}
			if detail == "" {
				t.Fatalf("no %q readiness entry in %v", mcp.RootConfinementCheck, res.structured["checks"])
			}
			if !strings.Contains(detail, tc.wantDetail) {
				t.Fatalf("the readiness detail must say %q; got %q", tc.wantDetail, detail)
			}
			// CATEGORICAL, WITH NO PATHS. A waiver check that leaked the waived directory would break
			// the very sanitization rule it exists to disclose.
			blob, _ := json.Marshal(res.structured)
			if strings.Contains(string(blob), ws) {
				t.Fatalf("doctor leaked a trusted-root path: %s", blob)
			}
			// And the human rendering carries it too: some clients show a model only that channel.
			if !strings.Contains(res.text, "inferredRootWaived") {
				t.Fatalf("the doctor text must carry the disclosure too, got %q", res.text)
			}
		})
	}
}

func TestDoctor_ReportsClientNarrowing(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	mkdirs(t, project)
	s := newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
		s.Roots, s.RootSource = []string{parent}, acp.RootsExplicit
	})
	c := serveRooted(t, s, true, true, []string{project})
	settleRoots(t, c, 1)

	res := c.tool(t, "review_doctor", map[string]any{})
	if got, _ := res.structured["rootNarrowing"].(string); got != "startup+client" {
		t.Fatalf("rootNarrowing = %q, want startup+client once a client has declared roots", got)
	}
}
