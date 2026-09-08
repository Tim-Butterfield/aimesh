package jsonschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A validator that is itself unchecked would just move the trust problem one level down. These
// tests cover the keywords the repo's schemas actually lean on, and — more importantly — the ones
// whose verdicts differ: `oneOf` must match EXACTLY one (that exclusivity is the whole reason the
// MCP result schema is shaped the way it is), `additionalProperties: false` must refuse, and an
// unknown keyword must be ignored rather than fabricate a failure.

func mustCompile(t *testing.T, s string) *Schema {
	t.Helper()
	sch, err := Compile([]byte(s))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return sch
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name     string
		schema   string
		instance string
		wantErr  string // "" = must validate
	}{
		{"type ok", `{"type":"object"}`, `{}`, ""},
		{"type mismatch", `{"type":"object"}`, `[]`, "type is array"},
		{"type union accepts null", `{"type":["string","null"]}`, `null`, ""},
		{"integer rejects a fraction", `{"type":"integer"}`, `1.5`, "want integer"},
		{"required", `{"required":["a"]}`, `{"b":1}`, `missing required property "a"`},
		{"additionalProperties false", `{"properties":{"a":{}},"additionalProperties":false}`, `{"a":1,"b":2}`, `"b" is not permitted`},
		{"additionalProperties schema", `{"additionalProperties":{"type":"string"}}`, `{"a":1}`, "type is integer"},
		{"const", `{"const":"running"}`, `"halted"`, "does not equal the required const"},
		{"enum", `{"enum":["a","b"]}`, `"c"`, "is not one of"},
		{"minItems", `{"minItems":2}`, `[1]`, "fewer than the minimum"},
		{"maxItems", `{"maxItems":1}`, `[1,2]`, "more than the maximum"},
		{"items", `{"items":{"type":"string"}}`, `["a",2]`, "[1]: type is integer"},
		{"uniqueItems refuses a repeat", `{"uniqueItems":true}`, `["a","b","a"]`, "uniqueItems is true"},
		{"uniqueItems accepts distinct", `{"uniqueItems":true}`, `["a","b"]`, ""},
		{"uniqueItems false permits repeats", `{"uniqueItems":false}`, `["a","a"]`, ""},
		{"pattern", `{"pattern":"^a"}`, `"b"`, "does not match pattern"},
		{"minimum", `{"minimum":2}`, `1`, "below the minimum"},
		{"propertyNames", `{"propertyNames":{"pattern":"^[a-z]+$"}}`, `{"A":1}`, "does not match pattern"},
		{"not", `{"not":{"required":["a"]}}`, `{"a":1}`, "forbids it"},
		{"allOf", `{"allOf":[{"required":["a"]},{"required":["b"]}]}`, `{"a":1}`, "allOf[1]"},
		{"anyOf matches one", `{"anyOf":[{"type":"string"},{"type":"integer"}]}`, `4`, ""},
		{"anyOf matches none", `{"anyOf":[{"type":"string"},{"type":"integer"}]}`, `true`, "matches no anyOf branch"},
		{"anyOf may match both", `{"anyOf":[{"required":["a"]},{"required":["b"]}]}`, `{"a":1,"b":2}`, ""},
		{"oneOf matches exactly one", `{"oneOf":[{"const":"x"},{"const":"y"}]}`, `"x"`, ""},
		{"oneOf matches none", `{"oneOf":[{"const":"x"},{"const":"y"}]}`, `"z"`, "matches NO oneOf branch"},
		{"oneOf matches two", `{"oneOf":[{"required":["a"]},{"required":["b"]}]}`, `{"a":1,"b":2}`, "matches 2 oneOf branches"},
		{"$ref to $defs", `{"$defs":{"s":{"type":"string"}},"properties":{"a":{"$ref":"#/$defs/s"}}}`, `{"a":1}`, "a: type is integer"},
		{"$ref siblings still apply", `{"$defs":{"o":{"type":"object"}},"$ref":"#/$defs/o","required":["a"]}`, `{}`, "missing required property"},
		{"unknown keywords are annotations", `{"type":"string","format":"date-time","title":"t","examples":["x"]}`, `"anything"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mustCompile(t, tc.schema).ValidateJSON([]byte(tc.instance))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected the instance to validate, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected a failure containing %q, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// A cross-FILE $ref is how docs/schema/ reuses a shape (effective-config.schema.json refers to
// config.schema.json), so it has to resolve against the referring file's own directory.
func TestCompileFile_ResolvesSiblingRefs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("base.schema.json", `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`)
	write("derived.schema.json", `{"$ref":"base.schema.json","required":["a"]}`)

	s, err := CompileFile(filepath.Join(dir, "derived.schema.json"))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := s.ValidateJSON([]byte(`{"a":"ok"}`)); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
	if err := s.ValidateJSON([]byte(`{}`)); err == nil {
		t.Fatal("the sibling `required` must apply alongside the $ref")
	}
	if err := s.ValidateJSON([]byte(`{"a":"ok","b":1}`)); err == nil {
		t.Fatal("the referenced file's additionalProperties:false must apply")
	}
	// Compiled from bytes there is no directory, so a file $ref is an error rather than a silent pass.
	inline := mustCompile(t, `{"$ref":"base.schema.json"}`)
	if err := inline.ValidateJSON([]byte(`{}`)); err == nil || !strings.Contains(err.Error(), "no directory") {
		t.Fatalf("an unresolvable file $ref must fail loudly, got %v", err)
	}
}

// ValidateGo marshals through encoding/json first, so a payload built from native Go values is
// judged on the bytes that would actually go on the wire.
func TestValidateGo(t *testing.T) {
	s := mustCompile(t, `{"type":"object","required":["n"],"properties":{"n":{"type":"integer"}}}`)
	if err := s.ValidateGo(map[string]any{"n": 3}); err != nil {
		t.Fatalf("a Go int must be judged as a JSON integer: %v", err)
	}
	if err := s.ValidateGo(map[string]any{"n": "3"}); err == nil {
		t.Fatal("a string must not pass an integer property")
	}
}
