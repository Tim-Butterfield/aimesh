package mcp_test

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
)

// The declared-keyword allowlist, over every schema THIS server publishes. Both servers get the same
// check for the same reason: meshcore/jsonschema treats a keyword it does not implement as an
// annotation, which is correct for an UNKNOWN keyword and wrong for a KNOWN-and-unimplemented one —
// so a schema that adopted `patternProperties` would pass our own strict check and be rejected by a
// compliant client, with the build green throughout.
//
// Two servers in one repo that answered this differently would be two contracts a client has to
// learn, which is the same reason their job shape and halt handling already match.
func TestPublishedToolSchemas_UseOnlyTheValidatorsDeclaredVocabulary(t *testing.T) {
	s := newServer(t, &fakeExplorer{})
	tools := s.Core().Tools()
	if len(tools) == 0 {
		t.Fatal("no tools registered — this check is worth nothing over an empty tool set")
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
				t.Errorf("%s.%s declares %v, which meshcore/jsonschema does not implement — it would be silently ignored by our own strict check and ENFORCED by a compliant client. Implement the keyword in the validator or drop it from the schema.",
					tool.Name, sch.kind, unsupported)
			}
		}
	}
}

// Every published schema compiles within the compile-time bounds — no non-local `$ref`, no `$ref`
// cycle, no nesting past the limits. On the strict-schema path a compile failure is a failed CALL.
func TestPublishedToolSchemas_CompileWithinTheBounds(t *testing.T) {
	for _, tool := range newServer(t, &fakeExplorer{}).Core().Tools() {
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
