package mcp_test

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
)

// Every published schema uses only keywords meshcore/jsonschema implements. The validator treats an
// unimplemented keyword as an annotation, so a schema using one (patternProperties, say) would let
// --strict-schema pass payloads a compliant client rejects.
func TestPublishedToolSchemas_UseOnlyTheValidatorsDeclaredVocabulary(t *testing.T) {
	// Register every tool, including review_remediate, which has the largest schema.
	s := newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
		s.Ceiling, s.AllowWrites = []string{t.TempDir()}, true
	})
	tools := s.Core().Tools()
	if len(tools) < 6 {
		t.Fatalf("only %d tool(s) registered — this check is worth nothing if it runs over a partial tool set", len(tools))
	}
	for _, tool := range tools {
		for _, sch := range []struct {
			kind string
			raw  []byte
		}{{"inputSchema", tool.InputSchema}, {"outputSchema", tool.OutputSchema}} {
			if len(sch.raw) == 0 {
				continue
			}
			unsupported, err := jsonschema.UnsupportedKeywords(sch.raw)
			if err != nil {
				t.Errorf("%s.%s does not compile: %v", tool.Name, sch.kind, err)
				continue
			}
			if len(unsupported) > 0 {
				t.Errorf("%s.%s declares %v, which meshcore/jsonschema does not implement — it would be silently ignored by our own strict check and ENFORCED by a compliant client, so the two would disagree about the same payload. Implement the keyword in the validator or drop it from the schema; do not leave the build green.",
					tool.Name, sch.kind, unsupported)
			}
		}
	}
}

// Every published schema compiles within the validator's bounds: no non-local $ref, no $ref cycle and
// no excessive nesting. With --strict-schema a compile failure would fail calls.
func TestPublishedToolSchemas_CompileWithinTheBounds(t *testing.T) {
	s := newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
		s.Ceiling, s.AllowWrites = []string{t.TempDir()}, true
	})
	for _, tool := range s.Core().Tools() {
		for _, raw := range [][]byte{tool.InputSchema, tool.OutputSchema} {
			if len(raw) == 0 {
				continue
			}
			if _, err := jsonschema.Compile(raw); err != nil {
				t.Errorf("%s: %v", tool.Name, err)
			}
		}
	}
}

// tools/list returns tools in registration order, the same on every call.
func TestToolsList_IsDeterministicAndInRegistrationOrder(t *testing.T) {
	s := newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
		s.Ceiling, s.AllowWrites = []string{t.TempDir()}, true
	})
	c := serve(t, s)

	var registered []string
	for _, tool := range s.Core().Tools() {
		registered = append(registered, tool.Name)
	}
	for range 3 {
		resp, _ := c.call(t, "tools/list", map[string]any{})
		if resp.Error != nil {
			t.Fatalf("tools/list: %+v", resp.Error)
		}
		var got []string
		rows, _ := resp.Result["tools"].([]any)
		for _, row := range rows {
			m, _ := row.(map[string]any)
			name, _ := m["name"].(string)
			got = append(got, name)
		}
		if len(got) != len(registered) {
			t.Fatalf("tools/list returned %d tools, want %d", len(got), len(registered))
		}
		for i := range got {
			if got[i] != registered[i] {
				t.Fatalf("tools/list[%d] = %q, want %q — the order must be registration order, identically, on every call", i, got[i], registered[i])
			}
		}
	}
}
