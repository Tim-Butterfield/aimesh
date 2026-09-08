package mcp_test

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
)

// THE DECLARED-KEYWORD ALLOWLIST, over every schema this server publishes.
//
// The risk it closes is specific and is ours, not the protocol's. A tool's inputSchema/outputSchema
// may use any 2020-12 keyword. meshcore/jsonschema treats a keyword it does not implement as an
// ANNOTATION — which is correct for an UNKNOWN keyword and wrong for a KNOWN-and-unimplemented one.
// Declare `patternProperties` here and `--strict-schema` would pass a payload a compliant client
// rejects, while the build stayed green: a silent degradation with a green CI, which is the exact
// shape this repo refuses everywhere else.
//
// So: any keyword outside the validator's declared vocabulary fails the build. It protects OUR
// schemas from drifting outside OUR validator; it establishes nothing about general 2020-12 support.
//
// AGAINST THE OLD CODE THIS DOES NOT COMPILE, because nothing published the vocabulary to check
// against — which was the defect.
func TestPublishedToolSchemas_UseOnlyTheValidatorsDeclaredVocabulary(t *testing.T) {
	// AllowRemediate so that review_remediate — the one tool that writes, and the one with the
	// largest schema — is registered and therefore checked.
	s := newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
		s.Roots, s.AllowRemediate = []string{t.TempDir()}, true
		s.PolicyCeiling = review.ModeApply
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

// Every published schema must also compile, which — now that compiling bounds a schema — is the
// assertion that none of them carries a non-local `$ref`, a `$ref` cycle, or nesting past the
// compile-time bounds. On the strict-schema path a compile failure is a failed CALL, so this is the
// check that keeps that failure out of production rather than finding it there.
func TestPublishedToolSchemas_CompileWithinTheBounds(t *testing.T) {
	s := newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
		s.Roots, s.AllowRemediate = []string{t.TempDir()}, true
		s.PolicyCeiling = review.ModeApply
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

// `tools/list` order is REGISTRATION order and is stable across calls. The property already held;
// what it lacked was an assertion, and an unasserted property is one refactor from being a former
// property. A client that caches a tool list keyed on its order has no other way to find out.
func TestToolsList_IsDeterministicAndInRegistrationOrder(t *testing.T) {
	s := newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
		s.Roots, s.AllowRemediate = []string{t.TempDir()}, true
		s.PolicyCeiling = review.ModeApply
	})
	c := serve(t, s)

	var registered []string
	for _, tool := range s.Core().Tools() {
		registered = append(registered, tool.Name)
	}
	for attempt := 0; attempt < 3; attempt++ {
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
