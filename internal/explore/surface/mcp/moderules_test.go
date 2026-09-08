package mcp

// The per-mode input contract is stated in FOUR places: the generated schema, the tool description,
// the server instructions, and the agent guide. That redundancy is deliberate (conditional schemas
// are unevenly supported, so a model may only ever see the prose) — but redundancy that can drift is
// worse than a single source, because three of the four would still look authoritative.
//
// These are white-box tests (package mcp, not mcp_test) because they check the generator against the
// rules table it generates from.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/agentguide"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
)

// Every registered mode must appear in the tool's enum. A mode the CLI can run but MCP cannot name is
// a surface-parity break, and the only thing that would notice is a user who tried it.
func TestAllModes_CoversTheRegistry(t *testing.T) {
	registered := map[string]bool{}
	for _, n := range mode.Names() {
		registered[n] = true
	}
	declared := map[string]bool{}
	for _, n := range allModes {
		declared[n] = true
		if !registered[n] {
			t.Errorf("allModes declares %q, which is not a registered mode", n)
		}
	}
	for n := range registered {
		if !declared[n] {
			t.Errorf("mode %q is registered but the explore tool does not accept it — the CLI could run it and MCP could not", n)
		}
	}
}

// The generated schema must carry one branch per mode, each requiring what the rules table says and
// forbidding everything else. This is the check that stops the schema and the handler drifting.
func TestGeneratedSchema_MatchesTheRulesTable(t *testing.T) {
	var doc struct {
		OneOf []struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
			Not        struct {
				AnyOf []struct {
					Required []string `json:"required"`
				} `json:"anyOf"`
			} `json:"not"`
		} `json:"oneOf"`
	}
	if err := json.Unmarshal([]byte(exploreInputSchema), &doc); err != nil {
		t.Fatalf("the generated schema is not valid JSON: %v", err)
	}
	if len(doc.OneOf) != len(allModes) {
		t.Fatalf("oneOf has %d branches, want one per mode (%d)", len(doc.OneOf), len(allModes))
	}

	for i, m := range allModes {
		b := doc.OneOf[i]

		// requires: mode + whatever the table says
		want := map[string]bool{"mode": true}
		for _, p := range modeInputRules[m].requires {
			want[p] = true
		}
		got := map[string]bool{}
		for _, r := range b.Required {
			got[r] = true
		}
		for p := range want {
			if !got[p] {
				t.Errorf("mode %q: schema branch does not require %q", m, p)
			}
		}
		for p := range got {
			if !want[p] {
				t.Errorf("mode %q: schema branch requires %q, which the rules table does not", m, p)
			}
		}

		// forbids: every mode-specific prop this mode does not allow
		allowed := allowedFor(m)
		forbidden := map[string]bool{}
		for _, alt := range b.Not.AnyOf {
			for _, r := range alt.Required {
				forbidden[r] = true
			}
		}
		for _, p := range modeSpecificProps {
			switch {
			case allowed[p] && forbidden[p]:
				t.Errorf("mode %q: schema forbids %q, which the mode actually takes", m, p)
			case !allowed[p] && !forbidden[p]:
				t.Errorf("mode %q: schema does not forbid %q — it would be accepted and silently ignored", m, p)
			}
		}
	}
}

// The schema is precise but may be ignored by a client, so the requirement must ALSO be readable as
// prose in the two places a model actually reads: the tool description and the server instructions.
func TestProseStatesEveryModeRequirement(t *testing.T) {
	guide, _, _, err := agentguide.Load()
	if err != nil {
		t.Fatal(err)
	}

	for m, rule := range modeInputRules {
		for _, p := range rule.requires {
			for _, src := range []struct{ name, text string }{
				{"the explore tool description", exploreToolDescription},
				{"the server instructions", instructions},
				{"the agent guide", guide},
			} {
				if !strings.Contains(src.text, p) {
					t.Errorf("%s never mentions %q, required by mode %q — a client that ignores the conditional schema would never learn it", src.name, p, m)
				}
			}
		}
		if !strings.Contains(instructions, m) {
			t.Errorf("the server instructions never mention mode %q", m)
		}
	}
}
