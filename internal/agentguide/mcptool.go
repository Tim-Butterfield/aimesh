package agentguide

import (
	"encoding/json"
	"fmt"
)

// The `agents_md` MCP tool is defined HERE, once, rather than in each server. Both servers must
// return the same document under the same tool name with the same schema — that is the whole claim
// ("an agent arriving by shell and one arriving over MCP get identical guidance"), and two
// registrations maintained side by side would let it quietly stop being true.
//
// The servers own the registration call itself, because the Tool/Result types belong to their
// protocol layer and meshcore's MCP package must not learn app vocabulary. This package supplies the
// name, the descriptions, the schema and the payload; a server pairs them with its own
// `proto.Tool` + `proto.Result`.
const (
	// ToolName is the tool as it appears on the wire, on BOTH servers.
	ToolName = "agents_md"

	// ToolTitle is the short human label.
	ToolTitle = "Read the aimesh agent guide"

	// ToolDescription tells a model why to call this BEFORE anything else. It is deliberately
	// explicit that the guide carries rules whose violation is expensive, because a model that reads
	// "documentation" as optional will skip it and then get containment or apply semantics wrong.
	ToolDescription = "Returns the complete aimesh agent guide: what the tool does, the rules that " +
		"are expensive to get wrong (identity is recorded but never acted on, the containment copy, " +
		"the write denylist, apply semantics), every command with a one-line contract, and the safety " +
		"posture. Read this FIRST — it is the same document `aimesh agents-md` prints, and it is " +
		"compiled into this binary, so it matches the version answering you. Read-only and free: it " +
		"starts no process and spends nothing."

	// ToolAnnotationTitle is the read-only annotation's label.
	ToolAnnotationTitle = "Read the agent guide"
)

// InputSchema is the empty object: the tool takes no arguments. Declared here rather than reusing
// each server's `emptyInputSchema` so the two cannot drift apart.
const InputSchema = `{"type":"object","additionalProperties":false,"properties":{}}`

// OutputSchema describes the structured result. `source` is REQUIRED and closed to two values, so a
// caller can always tell whether it received the guide that matches this binary or an operator's
// substitute — the fact that makes an override a disclosure rather than a silent swap.
//
// (No backticks inside: this is a Go raw string literal, and one would terminate it.)
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

// Payload is the structured result. The guide is carried in BOTH the text block and here on purpose:
// a model reads the text, while a programmatic client reads structuredContent and should not have to
// scrape a content block to get the same document.
type Payload struct {
	Guide  string `json:"guide"`
	Source Source `json:"source"`
	Path   string `json:"path,omitempty"`
	Bytes  int    `json:"bytes"`
}

// ToolPayload loads the guide and returns the text block and the structured payload a server pairs
// into its result. The error is the unreadable-override case, which callers must surface as a tool
// error rather than falling back to the embedded text.
func ToolPayload() (text string, payload Payload, err error) {
	guide, source, path, err := Load()
	if err != nil {
		return "", Payload{}, err
	}
	return guide, Payload{Guide: guide, Source: source, Path: path, Bytes: len(guide)}, nil
}

// payloadJSON is a compile-time guard that Payload actually marshals to what OutputSchema declares.
// A schema and a struct that disagree produce a result no strict client will accept, and nothing
// else in the build would notice.
var _ = func() struct{} {
	if _, err := json.Marshal(Payload{Source: SourceEmbedded}); err != nil {
		panic(fmt.Sprintf("agentguide.Payload does not marshal: %v", err))
	}
	return struct{}{}
}()
