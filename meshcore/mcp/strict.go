package mcp

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
)

// This file is the OPT-IN send-time check that a result actually satisfies the schema its own tool
// advertised.
//
// Both servers validate their payloads against their declared `outputSchema` in the BUILD — a table test
// walks every result builder at every state and judges it against the literal schema. That check exists
// because the drift it catches was real: three payloads violated the very `oneOf` their tool declared (a
// cancelled call returned the running shape with `state` overwritten, a still-running job omitted a
// required governance field, and one tool's terminal payload was declared under another's schema).
// Build-time validation is the right default — it costs a caller nothing and it fails CI, not a call.
//
// What it cannot do is speak for a payload the tests do not construct: a field whose value depends on a
// provider's output, a branch reached only by a rare halt. So this is the runtime half, and it is OFF by
// default because it is not free (see the cost note on Server.StrictSchema).
//
// On a violation the call FAILS LOUDLY rather than sending a non-conforming payload. The refusal names the
// tool and quotes the validator, so the offending branch is identifiable from the wire alone; it carries
// NO structuredContent, because the only structured payload available is the one just judged invalid and
// attaching it would ship exactly what the check refused.

// StrictSchemaEnv turns the check on without a command line.
//
// The flag alone would not be enough. An MCP server is almost never launched by the person debugging it —
// it is launched by a HOST from a config file, and the common hosts let an operator set `env` for a
// server entry but not extra argv. A diagnostic switch the people who most need it cannot reach is a
// switch in name only, so the env var is not a convenience alias: it is the form that actually works
// where the failure is observed.
const StrictSchemaEnv = "AIMESH_MCP_STRICT_SCHEMA"

// StrictSchemaFromEnv reports whether StrictSchemaEnv asks for the check. It accepts the shapes an
// operator actually types; anything else (including the empty value) is off.
func StrictSchemaFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(StrictSchemaEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// schemaCache compiles each tool's declared outputSchema once. Compiling per call would make the flag's
// cost a function of schema size on every request rather than of payload size.
type schemaCache struct {
	mu sync.Mutex
	m  map[string]*compiled
}

type compiled struct {
	schema *jsonschema.Schema
	err    error
}

// outputSchemaFor returns the compiled outputSchema for a tool, or (nil, nil) when the tool declares none.
func (s *Server) outputSchemaFor(name string) (*jsonschema.Schema, error) {
	var raw []byte
	for _, t := range s.tools {
		if t.Name == name {
			raw = t.OutputSchema
			break
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	s.schemas.mu.Lock()
	defer s.schemas.mu.Unlock()
	if s.schemas.m == nil {
		s.schemas.m = map[string]*compiled{}
	}
	if c, ok := s.schemas.m[name]; ok {
		return c.schema, c.err
	}
	sch, err := jsonschema.Compile(raw)
	s.schemas.m[name] = &compiled{schema: sch, err: err}
	return sch, err
}

// enforceOutputSchema validates a result's structuredContent against the tool's declared outputSchema when
// StrictSchema is on. It returns the result to SEND — either the original, or a loud internal-error result
// naming the offending tool.
func (s *Server) enforceOutputSchema(tool string, res *CallToolResult) *CallToolResult {
	if !s.StrictSchema || res == nil || res.StructuredContent == nil {
		return res
	}
	sch, err := s.outputSchemaFor(tool)
	if err != nil {
		return ErrorResult(fmt.Sprintf(
			"internal error: tool %q declares an outputSchema this server cannot compile (%v). Strict schema validation is on (--strict-schema), so the result is refused rather than sent unchecked.",
			tool, err), nil)
	}
	if sch == nil {
		return res
	}
	if verr := sch.ValidateGo(res.StructuredContent); verr != nil {
		s.Diagnosticf("mcp: strict-schema violation in %s: %v", tool, verr)
		return ErrorResult(fmt.Sprintf(
			"internal error: tool %q produced a structuredContent payload that does not satisfy its own declared outputSchema, and strict schema validation is on (--strict-schema), so it was NOT sent. This is a server bug, not a bad request — the same call succeeds with the check off, returning a payload a strict client may reject. Validator: %v",
			tool, verr), nil)
	}
	return res
}
