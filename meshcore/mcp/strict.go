package mcp

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
)

// This file implements the opt-in send-time check that a result satisfies its tool's declared
// outputSchema. Build-time tests validate every result builder against its schema; this check covers
// payloads the tests do not construct, such as values that depend on provider output. It is off by
// default because of its cost (see Server.StrictSchema). A violation fails the call with an error that
// names the tool and quotes the validator, without structuredContent, since the only payload available
// is the invalid one.

// StrictSchemaEnv enables the check through the environment, which MCP hosts let an operator set for a
// server entry even when they do not allow extra arguments.
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
// StrictSchema is on. It returns the result to send: the original, or an internal-error result naming the
// tool.
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
