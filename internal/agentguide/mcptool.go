package agentguide

import (
	"encoding/json"
	"fmt"
)

// The agents_md MCP tool is defined once here so every server returns the same document under the
// same name and schema. Each server makes the registration call itself, pairing these values with its
// protocol layer's Tool and Result types.
const (
	// ToolName is the tool's wire name.
	ToolName = "agents_md"

	// ToolTitle is the short human label.
	ToolTitle = "Read the aimesh agent guide"

	// ToolDescription asks a model to read the guide before anything else, and says why.
	ToolDescription = "Returns the complete aimesh agent guide: what the tool does, the rules that " +
		"are expensive to get wrong (identity is recorded but never acted on, the containment copy, " +
		"the write denylist, apply semantics), every command with a one-line contract, and the safety " +
		"posture. Read this FIRST — it is the same document `aimesh agents-md` prints, and it is " +
		"compiled into this binary, so it matches the version answering you. Read-only and free: it " +
		"starts no process and spends nothing."

	// ToolAnnotationTitle is the read-only annotation's label.
	ToolAnnotationTitle = "Read the agent guide"
)

// InputSchema is the empty object schema: the tool takes no arguments.
const InputSchema = `{"type":"object","additionalProperties":false,"properties":{}}`

// OutputSchema describes the structured result. The required source field tells a caller whether it
// received the embedded guide or an operator's override. It is a raw string literal, so it cannot
// contain a backtick.
const OutputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["guide", "source", "bytes"],
  "properties": {
    "guide":  { "type": "string", "description": "The guide, verbatim. Identical to the text block and to what 'aimesh agents-md' prints." },
    "source": { "type": "string", "enum": ["embedded", "override"], "description": "'embedded' = compiled into this binary, so it matches the version serving it. 'override' = a document named by the AGENTS_MD environment variable, usually set in this server's launch config; the version-match guarantee does NOT hold for it." },
    "path":   { "type": "string", "description": "The override's path. Present only when source is 'override'." },
    "bytes":  { "type": "integer", "minimum": 0, "description": "Length of the guide in bytes." }
  }
}`

// Payload is the structured result. It repeats the guide from the text block so a programmatic
// client can read it from structuredContent.
type Payload struct {
	Guide  string `json:"guide"`
	Source Source `json:"source"`
	Path   string `json:"path,omitempty"`
	Bytes  int    `json:"bytes"`
}

// ToolPayload loads the guide and returns the text block and structured payload for a tool result.
// An error means the override could not be read; callers report it as a tool error.
func ToolPayload() (text string, payload Payload, err error) {
	guide, source, path, err := Load()
	if err != nil {
		return "", Payload{}, err
	}
	return guide, Payload{Guide: guide, Source: source, Path: path, Bytes: len(guide)}, nil
}

// Payload must marshal; this check fails at package initialization if it cannot. The test
// TestToolPayload_MatchesTheDeclaredSchema checks it against OutputSchema.
var _ = func() struct{} {
	if _, err := json.Marshal(Payload{Source: SourceEmbedded}); err != nil {
		panic(fmt.Sprintf("agentguide.Payload does not marshal: %v", err))
	}
	return struct{}{}
}()
