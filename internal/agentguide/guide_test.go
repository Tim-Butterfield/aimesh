package agentguide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_EmbeddedByDefault(t *testing.T) {
	t.Setenv(EnvVar, "")
	guide, source, path, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if source != SourceEmbedded || path != "" {
		t.Errorf("source = %q path = %q, want embedded with no path", source, path)
	}
	if !strings.HasPrefix(guide, "# AGENTS.md") {
		t.Errorf("the embedded guide does not look like the guide: %q", guide[:min(60, len(guide))])
	}
}

// The guide is load-bearing, not decoration: an agent that reads it and still gets containment or
// identity wrong means the document failed. Pin the rules that are expensive to omit.
func TestEmbeddedGuide_CarriesTheExpensiveRules(t *testing.T) {
	t.Setenv(EnvVar, "")
	guide, _, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{
		"Identity is recorded, never acted on",
		"containment copy",
		"denylist",
		"Nothing is silently substituted",
		"aimesh review run",
		"aimesh explore run",
	} {
		if !strings.Contains(guide, must) {
			t.Errorf("the guide omits %q", must)
		}
	}
	// It must not advertise the removed workbench.
	for _, gone := range []string{"aimesh review ui", "aimesh explore ui", "workbench"} {
		if strings.Contains(guide, gone) {
			t.Errorf("the guide still mentions %q, which no longer exists", gone)
		}
	}
}

func TestLoad_OverrideIsServedAndIdentified(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom.md")
	if err := os.WriteFile(custom, []byte("# CUSTOM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, custom)

	guide, source, path, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if guide != "# CUSTOM\n" {
		t.Errorf("the override must be served verbatim, got %q", guide)
	}
	if source != SourceOverride || path != custom {
		t.Errorf("source = %q path = %q, want override at %q", source, path, custom)
	}
}

// An unreadable override is an ERROR. Falling back would hand the agent a document the operator did
// not choose while reporting success.
func TestLoad_UnreadableOverrideErrorsAndNamesTheRemedy(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.md")
	t.Setenv(EnvVar, missing)

	guide, _, path, err := Load()
	if err == nil {
		t.Fatal("an unreadable override must not succeed")
	}
	if guide != "" {
		t.Errorf("nothing should be served, got %q", guide)
	}
	if path != missing {
		t.Errorf("the offending path should be reported, got %q", path)
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "Unset "+EnvVar) {
		t.Errorf("the error must name the path AND the remedy: %v", err)
	}
}

// The MCP payload must MATCH the declared OutputSchema. A schema and a struct that disagree produce
// a result no strict client accepts, and nothing else in the build would catch it: the schema is a
// string constant and the payload is a Go struct, so only a test relates them.
func TestToolPayload_MatchesTheDeclaredSchema(t *testing.T) {
	t.Setenv(EnvVar, "")
	text, payload, err := ToolPayload()
	if err != nil {
		t.Fatal(err)
	}
	if text != payload.Guide {
		t.Error("the text block and the structured guide must be the same document")
	}
	if payload.Bytes != len(text) {
		t.Errorf("bytes = %d, want %d", payload.Bytes, len(text))
	}

	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}

	var schema struct {
		Required   []string                  `json:"required"`
		Properties map[string]map[string]any `json:"properties"`
		Additional bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal([]byte(OutputSchema), &schema); err != nil {
		t.Fatalf("OutputSchema is not valid JSON: %v", err)
	}
	for _, r := range schema.Required {
		if _, ok := got[r]; !ok {
			t.Errorf("schema requires %q but the payload omits it", r)
		}
	}
	for k := range got {
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("payload carries %q, which the schema does not declare (additionalProperties is false)", k)
		}
	}
	// `path` is omitempty and absent for the embedded guide — the schema must NOT require it.
	for _, r := range schema.Required {
		if r == "path" {
			t.Error("`path` cannot be required: it is absent whenever the guide is the embedded one")
		}
	}
}

func TestInputSchema_IsValidAndTakesNoArguments(t *testing.T) {
	var in struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Additional bool           `json:"additionalProperties"`
	}
	if err := json.Unmarshal([]byte(InputSchema), &in); err != nil {
		t.Fatalf("InputSchema is not valid JSON: %v", err)
	}
	if in.Type != "object" || len(in.Properties) != 0 || in.Additional {
		t.Errorf("the tool takes no arguments; schema = %s", InputSchema)
	}
}
