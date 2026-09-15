package mcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// review_doctor and review_list disclose who performs writes and whether the diff is available; a
// host may discard the server's stderr.

func TestDoctor_DisclosesWhoWrites(t *testing.T) {
	ws := workspaceFixture(t)
	for _, tc := range []struct {
		name   string
		allow  bool
		writes string
	}{
		{"launched without --allow-writes, the agent writes", false, "agent"},
		{"launched with --allow-writes, aimesh writes", true, "aimesh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
				s.Ceiling, s.AllowWrites = []string{ws}, tc.allow
			}))
			res := c.tool(t, "review_doctor", map[string]any{})
			if got, _ := res.structured["writes"].(string); got != tc.writes {
				t.Fatalf("writes = %q, want %q", got, tc.writes)
			}
			if got, _ := res.structured["diffAvailable"].(bool); !got {
				t.Fatal("diffAvailable must be true on every server")
			}
			if got, _ := res.structured["rootCeiling"].(float64); int(got) != 1 {
				t.Fatalf("rootCeiling = %v, want 1", res.structured["rootCeiling"])
			}
			// Categorical values only, with no paths.
			blob, _ := json.Marshal(res.structured)
			if strings.Contains(string(blob), ws) {
				t.Fatalf("doctor leaked a ceiling path: %s", blob)
			}
			// The text rendering carries it too, since some clients show a model only that channel.
			if !strings.Contains(res.text, "Writes: "+tc.writes) {
				t.Fatalf("the doctor text must carry the disclosure too, got %q", res.text)
			}

			list := c.tool(t, "review_list", map[string]any{})
			rem, _ := list.structured["remediation"].(map[string]any)
			if got, _ := rem["writes"].(string); got != tc.writes {
				t.Fatalf("review_list remediation.writes = %q, want %q", got, tc.writes)
			}
			if got, _ := rem["diffAvailable"].(bool); !got {
				t.Fatal("review_list remediation.diffAvailable must be true")
			}
		})
	}
}

func TestDoctor_NoCeilingReportsZero(t *testing.T) {
	c := serve(t, newServer(t, &fakeReviewer{}))
	res := c.tool(t, "review_doctor", map[string]any{})
	if got, ok := res.structured["rootCeiling"].(float64); !ok || got != 0 {
		t.Fatalf("rootCeiling = %v, want 0 when no --root was given", res.structured["rootCeiling"])
	}
}
