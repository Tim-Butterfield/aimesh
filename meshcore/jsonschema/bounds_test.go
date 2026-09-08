package jsonschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AGAINST THE OLD CODE EVERY TEST IN THIS FILE FAILS.
//
// A `$ref: "https://…"` compiled fine and failed later, if it was ever reached, with a file-not-found
// — the protocol's "MUST NOT automatically dereference a network URI" held by accident rather than by
// refusal. A `$ref` naming `../../etc/x.json` was joined onto the schema's directory and opened. A
// self-referential `$defs` chain recursed until the stack ran out. And nothing anywhere reported which
// keywords this validator does and does not implement, so a published schema could adopt one it
// silently ignores.

// --- (a) the non-local $ref refusal ---

func TestCompile_RefusesNonLocalRefsByName(t *testing.T) {
	cases := []struct {
		name, ref, want string
	}{
		{"https", "https://example.test/s.json#/$defs/x", "URI scheme"},
		{"http", "http://example.test/s.json", "URI scheme"},
		{"file", "file:///etc/passwd", "URI scheme"},
		{"urn", "urn:example:schema", "URI scheme"},
		{"network-path", "//example.test/s.json", "network-path reference"},
		{"absolute path", "/etc/schemas/s.json", "names a path"},
		{"parent traversal", "../../etc/s.json", "names a path"},
		{"subdirectory", "sub/s.json", "names a path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := fmt.Sprintf(`{"properties":{"a":{"$ref":%q}}}`, tc.ref)
			_, err := Compile([]byte(doc))
			if err == nil {
				t.Fatalf("$ref %q compiled — a non-local reference must be refused at compile time, not discovered when something happens to reach it", tc.ref)
			}
			if !errors.Is(err, ErrNonLocalRef) {
				t.Fatalf("error %v is not ErrNonLocalRef — a caller must be able to tell a compliance refusal from a resource one", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say why (want %q)", err, tc.want)
			}
		})
	}
}

// The refusal is repeated at the point of USE, so it survives a code path that reaches resolve
// without going through Compile.
func TestResolve_RefusesANonLocalRefEvenIfOneReachesIt(t *testing.T) {
	s := &Schema{name: "x", docs: map[string]any{"x": map[string]any{}}}
	if _, _, err := s.resolve("https://example.test/s.json", "x"); !errors.Is(err, ErrNonLocalRef) {
		t.Fatalf("resolve accepted a network URI: %v", err)
	}
}

// The two forms that ARE local keep working — the refusal must not cost the reuse it exists beside.
func TestCompile_LocalRefsStillResolve(t *testing.T) {
	if _, err := Compile([]byte(`{"$defs":{"s":{"type":"string"}},"properties":{"a":{"$ref":"#/$defs/s"}}}`)); err != nil {
		t.Fatalf("an in-document pointer must still compile: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "base.schema.json"), []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "derived.schema.json"), []byte(`{"$ref":"base.schema.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CompileFile(filepath.Join(dir, "derived.schema.json")); err != nil {
		t.Fatalf("a same-directory sibling ref must still compile: %v", err)
	}
}

// --- (b) the compile-time bounds ---

func TestCompile_RefusesARefCycle(t *testing.T) {
	// a → b → a. Before the cycle guard this recursed until the stack ran out, which on a runtime
	// path is a crash rather than a refusal.
	_, err := Compile([]byte(`{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`))
	if !errors.Is(err, ErrRefCycle) {
		t.Fatalf("a $ref cycle compiled (err = %v) — an unbounded validator on a runtime path is a hole whether or not anything currently reaches it", err)
	}
}

// A self-reference is the one-node case of the same defect, and the common one: a recursive tree
// shape written without noticing that this validator cannot express it.
func TestCompile_RefusesASelfReference(t *testing.T) {
	if _, err := Compile([]byte(`{"$defs":{"node":{"properties":{"child":{"$ref":"#/$defs/node"}}}},"$ref":"#/$defs/node"}`)); !errors.Is(err, ErrRefCycle) {
		t.Fatalf("a self-referential $defs compiled (err = %v)", err)
	}
}

func TestCompile_RefusesExcessiveNesting(t *testing.T) {
	doc := `{"type":"object"}`
	for i := 0; i < MaxDepth+5; i++ {
		doc = `{"properties":{"a":` + doc + `}}`
	}
	if _, err := Compile([]byte(doc)); !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("nesting past MaxDepth compiled (err = %v)", err)
	}
}

func TestCompile_RefusesTooManySubschemas(t *testing.T) {
	// Wide rather than deep: many siblings, so this trips the node budget and not the depth bound.
	props := make([]string, 0, MaxSubschemas+16)
	for i := 0; i < MaxSubschemas+16; i++ {
		props = append(props, fmt.Sprintf(`"p%d":{"type":"string"}`, i))
	}
	doc := `{"properties":{` + strings.Join(props, ",") + `}}`
	if _, err := Compile([]byte(doc)); !errors.Is(err, ErrMaxSubschemas) {
		t.Fatalf("a schema past MaxSubschemas compiled (err = %v)", err)
	}
}

// A schema at the bound must still compile: a bound that fired early would be a different bug with
// the same symptom.
func TestCompile_AcceptsASchemaJustInsideTheBounds(t *testing.T) {
	doc := `{"type":"object"}`
	for i := 0; i < MaxDepth-2; i++ {
		doc = `{"properties":{"a":` + doc + `}}`
	}
	if _, err := Compile([]byte(doc)); err != nil {
		t.Fatalf("a schema inside the bounds was refused: %v", err)
	}
}

// --- (c)/(d) the vocabulary ---

func TestUnsupportedKeywords_NamesKnownButUnimplementedKeywords(t *testing.T) {
	got, err := UnsupportedKeywords([]byte(`{
		"type": "object",
		"title": "fine",
		"patternProperties": {"^x": {"type": "string"}},
		"properties": {"a": {"type": "string", "dependentRequired": {"a": ["b"]}}}
	}`))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := map[string]bool{"patternProperties": true, "dependentRequired": true}
	if len(got) != len(want) {
		t.Fatalf("unsupported = %v, want exactly %v", got, sortedKeys(want))
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected %q in %v", k, got)
		}
	}
}

func TestUnsupportedKeywords_AcceptsTheDeclaredVocabulary(t *testing.T) {
	implemented, ignored := Vocabulary()
	// A schema declaring every keyword we claim to handle must report nothing. This is the half that
	// catches a keyword dropped from the implementation but left in the doc comment.
	m := map[string]any{}
	for _, k := range append(append([]string{}, implemented...), ignored...) {
		switch k {
		case "$ref":
			m[k] = "#/$defs/x"
		case "$defs":
			m[k] = map[string]any{"x": map[string]any{"type": "string"}}
		case "properties", "propertyNames", "additionalProperties", "not", "items":
			m[k] = map[string]any{}
		case "allOf", "anyOf", "oneOf", "prefixItems", "required", "enum", "examples":
			m[k] = []any{}
		default:
			m[k] = "x"
		}
	}
	b, _ := json.Marshal(m)
	got, err := UnsupportedKeywords(b)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the declared vocabulary reported %v as unsupported", got)
	}
}

// The package doc and Vocabulary() must agree. A doc comment nobody checks is how a validator comes
// to claim support it does not have — which is the exact failure this package exists to remove, so it
// does not get to make it about itself.
func TestPackageDoc_ListsExactlyTheDeclaredVocabulary(t *testing.T) {
	src, err := os.ReadFile("jsonschema.go")
	if err != nil {
		t.Fatalf("read package doc: %v", err)
	}
	head, _, ok := strings.Cut(string(src), "\npackage jsonschema")
	if !ok {
		t.Fatal("could not isolate the package doc")
	}
	implemented, ignored := Vocabulary()
	for _, k := range append(append([]string{}, implemented...), ignored...) {
		if !strings.Contains(head, k) {
			t.Errorf("the package doc does not list %q — the declaration and the code have drifted", k)
		}
	}
	// And nothing this validator does NOT implement may appear in the doc's lists as though it did.
	for _, k := range []string{"patternProperties", "dependentRequired", "dependentSchemas", "unevaluatedProperties", "unevaluatedItems", "multipleOf", "contains", "if", "then", "else"} {
		if strings.Contains(head, `//	`+k) || strings.Contains(head, ", "+k+",") {
			t.Errorf("the package doc lists %q, which is not implemented", k)
		}
	}
}

// The bounds are supposed to have an order of magnitude of headroom over anything this repo writes.
// Asserting it keeps the numbers honest: if a schema grows toward a bound, this fails first, with the
// measurement, rather than the bound firing in a build nobody expected it to.
func TestPublishedSchemasAreWellInsideTheBounds(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "schema")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("docs/schema not reachable from here: %v", err)
	}
	worstDepth, worstNodes, worstName := 0, 0, ""
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".schema.json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		d, n, merr := Measure(b)
		if merr != nil {
			t.Fatalf("%s: %v", e.Name(), merr)
		}
		if d > worstDepth {
			worstDepth, worstName = d, e.Name()
		}
		if n > worstNodes {
			worstNodes = n
		}
	}
	if worstDepth*2 > MaxDepth {
		t.Errorf("deepest published schema is %d levels (%s), more than half of MaxDepth=%d — the headroom this bound was chosen for is gone", worstDepth, worstName, MaxDepth)
	}
	if worstNodes*2 > MaxSubschemas {
		t.Errorf("largest published schema is %d nodes, more than half of MaxSubschemas=%d", worstNodes, MaxSubschemas)
	}
	t.Logf("deepest published schema: %d levels (%s); largest: %d nodes (bounds %d / %d)", worstDepth, worstName, worstNodes, MaxDepth, MaxSubschemas)
}

// Every schema this repo publishes under docs/schema/ must compile — which, now that compiling bounds
// a schema, is also the assertion that none of them carries a non-local `$ref`. Two of them did.
func TestPublishedSchemaFilesCompile(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "schema")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("docs/schema not reachable from here: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".schema.json") {
			continue
		}
		if _, cerr := CompileFile(filepath.Join(dir, e.Name())); cerr != nil {
			t.Errorf("%s: %v", e.Name(), cerr)
		}
	}
}

// And the same allowlist, applied to the published FILES rather than to the tool schemas the apps
// declare (which their own packages check, because only they can enumerate their tools).
func TestPublishedSchemaFilesUseOnlyTheDeclaredVocabulary(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "schema")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("docs/schema not reachable from here: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".schema.json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		unsupported, serr := UnsupportedKeywords(b)
		if serr != nil {
			t.Fatalf("%s: %v", e.Name(), serr)
		}
		if len(unsupported) > 0 {
			t.Errorf("%s declares %v, which this validator does not implement — a declared-but-unimplemented keyword is silently ignored here and enforced by a compliant client, so the two disagree about the same document", e.Name(), unsupported)
		}
	}
}
