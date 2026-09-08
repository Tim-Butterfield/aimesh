// Package jsonschema is a small, dependency-free JSON Schema (draft 2020-12) validator.
//
// It is primarily a TEST / VERIFICATION utility: the tool the build uses to hold DECLARED schemas to
// what the code actually emits. It is deliberately too small to be trusted as a general-purpose
// validator, and nothing may depend on it to decide whether an INPUT is acceptable — a subset validator
// that silently accepts what it does not implement is not an input boundary.
//
// It has exactly ONE runtime caller, and the exception is narrow and stated: meshcore/mcp's opt-in
// send-time check (Server.StrictSchema), which validates a payload THIS PROCESS JUST BUILT against a
// schema THIS PROCESS declares. Both sides are ours; the check answers "did we emit what we promised",
// never "is this stranger's input safe". It is off by default, so the default import-graph posture is
// unchanged in effect even though the package is now reachable from a shipped binary.
//
// It exists for ONE reason: this repo declares schemas — the MCP tools' `outputSchema`, the files
// under `docs/schema/` — and until it existed nothing checked that the payloads the code actually
// emits satisfy them. A declaration nothing validates drifts silently, and it drifted: MCP
// `structuredContent` payloads matched no branch of the very `oneOf` their tool advertised. The
// point of this package is that the drift is now a FAILING TEST rather than a discovery.
//
// It lives in meshcore because JSON Schema is domain-free and because every application's
// conformance tests must judge payloads by the SAME rules — a validator that differed per app
// would let two surfaces drift apart while both looked green. It was briefly duplicated per app,
// only because the original copy was `internal/` to one application module and an app-to-app
// import fails scripts/boundarycheck.
//
// # THIS IS A SUBSET VALIDATOR. IT IS NOT A CONFORMANT 2020-12 IMPLEMENTATION.
//
// That sentence is the vocabulary declaration, and it is deliberately the first thing here rather
// than a caveat at the end. Nothing in this repo may be documented, tested or advertised in a way
// that implies otherwise.
//
// # Dialect
//
// The only dialect handled is JSON Schema draft 2020-12, and only the subset below. Every schema this
// repo compiles is one this repo wrote, all of them use the default dialect, and no client-supplied
// schema is ever compiled — this package is never an input boundary (see the paragraph above).
// A `$schema` naming any other dialect is IGNORED rather than honoured, which is safe only because of
// that scope; it would not be safe for a validator that consumed strangers' schemas.
//
// # Implemented — the keywords this validator acts on
//
//	$ref (in-document JSON pointers and same-directory file names), $defs,
//	type, const, enum, oneOf, anyOf, allOf, not,
//	properties, required, additionalProperties, propertyNames, minProperties, maxProperties,
//	items, prefixItems, minItems, maxItems, uniqueItems,
//	minLength, maxLength, pattern, minimum, maximum
//
// # Deliberately ignored — annotations that cannot change a verdict
//
//	$schema, $id, $comment, title, description, default, examples,
//	format, deprecated, readOnly, writeOnly
//
// Both lists are exported by Vocabulary(), and a test asserts that this comment and that function
// agree, so the declaration and the code cannot drift.
//
// # Everything else fails the build, and this is the reason
//
// The specification says an UNKNOWN keyword is an annotation, and ignoring one is correct. A
// KNOWN-AND-UNIMPLEMENTED keyword is a different thing wearing the same clothes: declare
// `patternProperties` or `dependentRequired` in a published schema and this validator would ignore
// it, our own strict send-time check would pass a payload a compliant client rejects, and the build
// would stay green. So UnsupportedKeywords walks every schema this repo publishes and fails CI on any
// keyword that is in neither list above. That check protects OUR schemas from drifting outside OUR
// vocabulary; it establishes nothing about general 2020-12 support, and must not be cited as though
// it did.
//
// # Non-goal, stated as a rule rather than an omission
//
// This package NEVER dereferences a `$ref` over the network, and implements no opt-in mode that
// would. A schema is a declaration; a declaration that reaches the network is a fetch nobody
// authorized. A `$ref` carrying a URI scheme, a network-path prefix, an absolute path or any path
// separator is refused at compile time with a named error (see bounds.go), so a `$ref` can neither
// reach the network nor walk out of its own directory. Compile-time depth, subschema and `$ref`-cycle
// bounds are enforced in the same place and for the same reason: on a runtime path, an unbounded
// validator is a hole whether or not anything currently reaches it.
//
// Instances are JSON-DECODED values (`map[string]any`, `[]any`, `string`, `float64`, `bool`, nil).
// Use ValidateJSON, or Validate on a value you round-tripped through encoding/json — a Go `int` is
// not a JSON number and this package will not pretend otherwise.
package jsonschema

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Schema is one compiled schema document, plus the sibling documents its `$ref`s reach.
type Schema struct {
	dir  string // directory sibling file refs resolve against ("" = file refs are refused)
	name string // this document's key in docs
	docs map[string]any
}

// Compile parses a schema literal and BOUNDS it (see bounds.go): a non-local `$ref`, a `$ref` cycle,
// or nesting past the compile-time limits is refused here, once, rather than discovered per call. It
// has no directory, so a `$ref` naming a sibling file still fails at validation, loudly, with the
// error that says why.
func Compile(b []byte) (*Schema, error) {
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("jsonschema: schema is not valid JSON: %w", err)
	}
	const name = "<inline>"
	s := &Schema{name: name, docs: map[string]any{name: doc}}
	if err := s.checkBounds(); err != nil {
		return nil, err
	}
	return s, nil
}

// CompileFile parses the schema at path and bounds it. `$ref`s naming a bare file name resolve
// against that file's own directory, which is how docs/schema/ cross-references itself; a `$ref` that
// names anything else — a URI, an absolute path, a path with a separator — is refused at compile time
// so that a schema can never walk out of its own directory or onto the network.
func CompileFile(path string) (*Schema, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("jsonschema: %w", err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("jsonschema: %s is not valid JSON: %w", path, err)
	}
	name := filepath.Base(path)
	s := &Schema{dir: filepath.Dir(path), name: name, docs: map[string]any{name: doc}}
	if err := s.checkBounds(); err != nil {
		return nil, fmt.Errorf("jsonschema: %s: %w", path, err)
	}
	return s, nil
}

// ValidateJSON validates raw JSON bytes.
func (s *Schema) ValidateJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("jsonschema: instance is not valid JSON: %w", err)
	}
	return s.Validate(v)
}

// ValidateGo marshals v through encoding/json first, so a caller may hand it a payload built from
// native Go values (the `map[string]any` an MCP handler returns) and still be judged on exactly the
// bytes that would go on the wire.
func (s *Schema) ValidateGo(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("jsonschema: instance does not marshal: %w", err)
	}
	return s.ValidateJSON(b)
}

// Validate reports every way the instance fails this schema. A nil error means it validates.
func (s *Schema) Validate(v any) error {
	var errs []string
	s.check(s.docs[s.name], s.name, v, "", &errs)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

// --- the walk ---

func (s *Schema) check(sch any, doc string, inst any, path string, errs *[]string) {
	switch t := sch.(type) {
	case bool:
		if !t {
			fail(errs, path, "schema is `false`: nothing validates here")
		}
		return
	case map[string]any:
		s.checkObject(t, doc, inst, path, errs)
	default:
		fail(errs, path, "schema fragment is neither an object nor a boolean")
	}
}

func (s *Schema) checkObject(m map[string]any, doc string, inst any, path string, errs *[]string) {
	if ref, ok := m["$ref"].(string); ok {
		target, tdoc, err := s.resolve(ref, doc)
		if err != nil {
			fail(errs, path, "%s", err.Error())
		} else {
			s.check(target, tdoc, inst, path, errs)
		}
	}
	s.checkType(m, inst, path, errs)
	s.checkConstEnum(m, inst, path, errs)
	s.checkCombinators(m, doc, inst, path, errs)
	s.checkObjectKeywords(m, doc, inst, path, errs)
	s.checkArrayKeywords(m, doc, inst, path, errs)
	s.checkStringKeywords(m, inst, path, errs)
	s.checkNumberKeywords(m, inst, path, errs)
}

func (s *Schema) checkType(m map[string]any, inst any, path string, errs *[]string) {
	raw, ok := m["type"]
	if !ok {
		return
	}
	var want []string
	switch t := raw.(type) {
	case string:
		want = []string{t}
	case []any:
		for _, v := range t {
			if str, ok := v.(string); ok {
				want = append(want, str)
			}
		}
	}
	if len(want) == 0 {
		return
	}
	for _, w := range want {
		if typeMatches(w, inst) {
			return
		}
	}
	fail(errs, path, "type is %s, want %s", typeOf(inst), strings.Join(want, "|"))
}

func (s *Schema) checkConstEnum(m map[string]any, inst any, path string, errs *[]string) {
	if want, ok := m["const"]; ok && !jsonEqual(want, inst) {
		fail(errs, path, "value %s does not equal the required const %s", render(inst), render(want))
	}
	if raw, ok := m["enum"]; ok {
		if options, ok := raw.([]any); ok {
			matched := false
			for _, o := range options {
				if jsonEqual(o, inst) {
					matched = true
					break
				}
			}
			if !matched {
				fail(errs, path, "value %s is not one of %s", render(inst), render(raw))
			}
		}
	}
}

func (s *Schema) checkCombinators(m map[string]any, doc string, inst any, path string, errs *[]string) {
	if raw, ok := m["allOf"].([]any); ok {
		for i, sub := range raw {
			var subErrs []string
			s.check(sub, doc, inst, path, &subErrs)
			for _, e := range subErrs {
				*errs = append(*errs, fmt.Sprintf("allOf[%d]: %s", i, e))
			}
		}
	}
	if raw, ok := m["anyOf"].([]any); ok {
		var reasons []string
		matched := false
		for i, sub := range raw {
			var subErrs []string
			s.check(sub, doc, inst, path, &subErrs)
			if len(subErrs) == 0 {
				matched = true
				break
			}
			reasons = append(reasons, fmt.Sprintf("[%d] %s", i, strings.Join(subErrs, ", ")))
		}
		if !matched {
			fail(errs, path, "matches no anyOf branch — %s", strings.Join(reasons, " | "))
		}
	}
	if raw, ok := m["oneOf"].([]any); ok {
		var matches []int
		var reasons []string
		for i, sub := range raw {
			var subErrs []string
			s.check(sub, doc, inst, path, &subErrs)
			if len(subErrs) == 0 {
				matches = append(matches, i)
				continue
			}
			reasons = append(reasons, fmt.Sprintf("[%d] %s", i, strings.Join(subErrs, ", ")))
		}
		switch len(matches) {
		case 1: // exactly one: the point of oneOf
		case 0:
			fail(errs, path, "matches NO oneOf branch — %s", strings.Join(reasons, " | "))
		default:
			fail(errs, path, "matches %d oneOf branches (%v); a oneOf must match exactly one", len(matches), matches)
		}
	}
	if sub, ok := m["not"]; ok {
		var subErrs []string
		s.check(sub, doc, inst, path, &subErrs)
		if len(subErrs) == 0 {
			fail(errs, path, "matches the `not` schema, which forbids it")
		}
	}
}

func (s *Schema) checkObjectKeywords(m map[string]any, doc string, inst any, path string, errs *[]string) {
	obj, ok := inst.(map[string]any)
	if !ok {
		return
	}
	if raw, ok := m["required"].([]any); ok {
		for _, r := range raw {
			name, _ := r.(string)
			if _, present := obj[name]; !present {
				fail(errs, path, "missing required property %q", name)
			}
		}
	}
	props, _ := m["properties"].(map[string]any)
	for name, sub := range props {
		if v, present := obj[name]; present {
			s.check(sub, doc, v, join(path, name), errs)
		}
	}
	if pn, ok := m["propertyNames"]; ok {
		for name := range obj {
			s.check(pn, doc, name, join(path, name), errs)
		}
	}
	if ap, ok := m["additionalProperties"]; ok {
		for name, v := range obj {
			if _, declared := props[name]; declared {
				continue
			}
			switch t := ap.(type) {
			case bool:
				if !t {
					fail(errs, path, "property %q is not permitted (additionalProperties: false)", name)
				}
			default:
				s.check(ap, doc, v, join(path, name), errs)
			}
		}
	}
	if n, ok := numberOf(m["minProperties"]); ok && float64(len(obj)) < n {
		fail(errs, path, "has %d properties, fewer than the minimum %v", len(obj), n)
	}
	if n, ok := numberOf(m["maxProperties"]); ok && float64(len(obj)) > n {
		fail(errs, path, "has %d properties, more than the maximum %v", len(obj), n)
	}
}

func (s *Schema) checkArrayKeywords(m map[string]any, doc string, inst any, path string, errs *[]string) {
	arr, ok := inst.([]any)
	if !ok {
		return
	}
	prefix := 0
	if raw, ok := m["prefixItems"].([]any); ok {
		for i, sub := range raw {
			if i < len(arr) {
				s.check(sub, doc, arr[i], fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
		prefix = len(raw)
	}
	if items, ok := m["items"]; ok {
		if tuple, isTuple := items.([]any); isTuple {
			// draft-07 tuple spelling, kept working so an older schema file still validates.
			for i, sub := range tuple {
				if i < len(arr) {
					s.check(sub, doc, arr[i], fmt.Sprintf("%s[%d]", path, i), errs)
				}
			}
		} else {
			for i := prefix; i < len(arr); i++ {
				s.check(items, doc, arr[i], fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
	}
	if n, ok := numberOf(m["minItems"]); ok && float64(len(arr)) < n {
		fail(errs, path, "has %d items, fewer than the minimum %v", len(arr), n)
	}
	if n, ok := numberOf(m["maxItems"]); ok && float64(len(arr)) > n {
		fail(errs, path, "has %d items, more than the maximum %v", len(arr), n)
	}
	// uniqueItems is implemented rather than ignored because a published schema in this repo
	// DECLARES it (exploremesh's `explore_compare.options`, where a repeated option would silently
	// change what the panel was asked to compare). The allowlist test found it: it was declared,
	// ignored here, and enforced by any compliant client — the two disagreeing about the same
	// payload, with the build green. That is the whole case for the allowlist.
	if u, ok := m["uniqueItems"].(bool); ok && u {
		for i := 0; i < len(arr); i++ {
			for j := i + 1; j < len(arr); j++ {
				if jsonEqual(arr[i], arr[j]) {
					fail(errs, path, "items[%d] and items[%d] are both %s, but uniqueItems is true", i, j, render(arr[i]))
				}
			}
		}
	}
}

func (s *Schema) checkStringKeywords(m map[string]any, inst any, path string, errs *[]string) {
	str, ok := inst.(string)
	if !ok {
		return
	}
	runes := len([]rune(str))
	if n, ok := numberOf(m["minLength"]); ok && float64(runes) < n {
		fail(errs, path, "is %d characters, shorter than the minimum %v", runes, n)
	}
	if n, ok := numberOf(m["maxLength"]); ok && float64(runes) > n {
		fail(errs, path, "is %d characters, longer than the maximum %v", runes, n)
	}
	if p, ok := m["pattern"].(string); ok {
		re, err := regexp.Compile(p)
		if err != nil {
			fail(errs, path, "schema pattern %q does not compile: %v", p, err)
		} else if !re.MatchString(str) {
			fail(errs, path, "value %q does not match pattern %q", str, p)
		}
	}
}

func (s *Schema) checkNumberKeywords(m map[string]any, inst any, path string, errs *[]string) {
	f, ok := inst.(float64)
	if !ok {
		return
	}
	if n, ok := numberOf(m["minimum"]); ok && f < n {
		fail(errs, path, "value %v is below the minimum %v", f, n)
	}
	if n, ok := numberOf(m["maximum"]); ok && f > n {
		fail(errs, path, "value %v is above the maximum %v", f, n)
	}
}

// --- $ref resolution ---

// resolve returns the schema a `$ref` names, plus the document it lives in (so a nested `$ref`
// inside a referenced file resolves against THAT file, not the one that pointed at it).
func (s *Schema) resolve(ref, doc string) (any, string, error) {
	// The same refusal the compile walk makes, repeated at the point of use. It is not redundant:
	// compile-time is where a bad schema is REPORTED, and this is where it would otherwise be ACTED
	// ON, and a boundary that exists in only one of those two places is a boundary one refactor away
	// from not existing.
	if err := checkLocalRef(ref); err != nil {
		return nil, "", err
	}
	file, pointer, _ := strings.Cut(ref, "#")
	target := doc
	if file != "" {
		if s.dir == "" {
			return nil, "", fmt.Errorf("$ref %q names a file, but this schema was compiled from bytes with no directory", ref)
		}
		if _, loaded := s.docs[file]; !loaded {
			b, err := os.ReadFile(filepath.Join(s.dir, file))
			if err != nil {
				return nil, "", fmt.Errorf("$ref %q: %v", ref, err)
			}
			var d any
			if err := json.Unmarshal(b, &d); err != nil {
				return nil, "", fmt.Errorf("$ref %q: %s is not valid JSON: %v", ref, file, err)
			}
			s.docs[file] = d
		}
		target = file
	}
	node := s.docs[target]
	if pointer == "" || pointer == "/" {
		return node, target, nil
	}
	for _, tok := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch cur := node.(type) {
		case map[string]any:
			v, ok := cur[tok]
			if !ok {
				return nil, "", fmt.Errorf("$ref %q: no member %q", ref, tok)
			}
			node = v
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(cur) {
				return nil, "", fmt.Errorf("$ref %q: bad array index %q", ref, tok)
			}
			node = cur[i]
		default:
			return nil, "", fmt.Errorf("$ref %q: cannot descend into %q", ref, tok)
		}
	}
	return node, target, nil
}

// --- helpers ---

func typeMatches(want string, inst any) bool {
	switch want {
	case "object":
		_, ok := inst.(map[string]any)
		return ok
	case "array":
		_, ok := inst.([]any)
		return ok
	case "string":
		_, ok := inst.(string)
		return ok
	case "boolean":
		_, ok := inst.(bool)
		return ok
	case "null":
		return inst == nil
	case "number":
		_, ok := inst.(float64)
		return ok
	case "integer":
		f, ok := inst.(float64)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0)
	}
	return true // an unknown type name is an annotation, not a verdict
}

func typeOf(inst any) string {
	switch v := inst.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if v == math.Trunc(v) {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T (not a JSON-decoded value)", inst)
}

func numberOf(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

func render(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(b) > 200 {
		return string(b[:200]) + "…"
	}
	return string(b)
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func fail(errs *[]string, path, format string, args ...any) {
	where := path
	if where == "" {
		where = "(root)"
	}
	*errs = append(*errs, where+": "+fmt.Sprintf(format, args...))
}
