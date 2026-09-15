package jsonschema

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// This file bounds what a schema may be before anything is validated against it:
//
//  1. A `$ref` to a network URI is never dereferenced; it is refused at compile time with a named
//     error, and there is no opt-in fetch mode.
//  2. Nesting depth, total subschemas and `$ref` cycles are bounded at compile time, so a malformed
//     schema fails once at startup instead of recursing without limit on the runtime strict-schema
//     path.

// The compile-time bounds, with an order of magnitude of headroom over the largest published schema;
// TestPublishedSchemasAreWellInsideTheBounds asserts the headroom.
const (
	// MaxDepth bounds how deeply schema objects nest, counting a `$ref` hop as one level.
	MaxDepth = 64
	// MaxSubschemas bounds how many schema nodes one compile visits in total. It bounds work as well as
	// size, since a `$ref` graph with many diamonds is walked once per reference.
	MaxSubschemas = 2048
)

// The compile-time refusals, as sentinel errors so a caller can tell a non-local `$ref` from a resource
// bound.
var (
	// ErrNonLocalRef reports a `$ref` this validator will not resolve because resolving it would
	// mean leaving the document set the caller handed us.
	ErrNonLocalRef = errors.New("jsonschema: $ref is not local")
	// ErrMaxDepth reports a schema nested deeper than MaxDepth.
	ErrMaxDepth = errors.New("jsonschema: schema nesting exceeds the compile-time depth bound")
	// ErrMaxSubschemas reports a schema whose compile walk exceeds MaxSubschemas nodes.
	ErrMaxSubschemas = errors.New("jsonschema: schema exceeds the compile-time subschema bound")
	// ErrRefCycle reports a `$ref` that reaches a schema already on its own resolution path.
	ErrRefCycle = errors.New("jsonschema: $ref cycle")
)

// implementedKeywords are the keywords this validator acts on. A keyword in neither this set nor
// inertKeywords is unimplemented here, and UnsupportedKeywords reports it.
var implementedKeywords = map[string]bool{
	"$ref": true, "$defs": true,
	"type": true, "const": true, "enum": true,
	"oneOf": true, "anyOf": true, "allOf": true, "not": true,
	"properties": true, "required": true, "additionalProperties": true,
	"propertyNames": true, "minProperties": true, "maxProperties": true,
	"items": true, "prefixItems": true, "minItems": true, "maxItems": true, "uniqueItems": true,
	"minLength": true, "maxLength": true, "pattern": true,
	"minimum": true, "maximum": true,
}

// inertKeywords are annotations this validator ignores because they cannot change a verdict.
//
// `$id` normally re-bases `$ref` resolution. It is listed because every `$id` in this repo is the
// file's own URL in the directory of the files it refers to, so both resolutions name the same
// document; if that changes, `$id` needs a real implementation.
var inertKeywords = map[string]bool{
	"$schema": true, "$id": true, "$comment": true,
	"title": true, "description": true, "default": true, "examples": true,
	"format": true, "deprecated": true, "readOnly": true, "writeOnly": true,
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UnsupportedKeywords reports every keyword a schema declares that this validator neither implements
// nor ignores, sorted and deduplicated, so CI fails when a published schema relies on an unimplemented
// keyword such as `patternProperties`. It says nothing about general 2020-12 support.
func UnsupportedKeywords(schema []byte) ([]string, error) {
	s, err := Compile(schema)
	if err != nil {
		return nil, err
	}
	w := &walker{s: s, stack: map[string]bool{}, unknown: map[string]bool{}, collect: true}
	if err := w.walk(s.docs[s.name], s.name, "(root)", 1); err != nil {
		return nil, err
	}
	return sortedKeys(w.unknown), nil
}

// measure reports the compile walk's observed depth and node count, so a test can assert headroom
// against MaxDepth and MaxSubschemas.
func measure(schema []byte) (depth, subschemas int, err error) {
	s, cerr := Compile(schema)
	if cerr != nil {
		return 0, 0, cerr
	}
	w := &walker{s: s, stack: map[string]bool{}, unknown: map[string]bool{}}
	if werr := w.walk(s.docs[s.name], s.name, "(root)", 1); werr != nil {
		return 0, 0, werr
	}
	return w.maxDepth, w.count, nil
}

// checkBounds is the compile-time walk every Compile / CompileFile runs.
func (s *Schema) checkBounds() error {
	w := &walker{s: s, stack: map[string]bool{}, unknown: map[string]bool{}}
	return w.walk(s.docs[s.name], s.name, "(root)", 1)
}

type walker struct {
	s        *Schema
	count    int
	maxDepth int
	// stack is the `$ref` targets on the current resolution path, keyed doc+pointer. A cycle is a
	// ref back onto this path; two independent references to the same $defs entry are a diamond,
	// not a cycle, and are walked twice (which the node budget bounds).
	stack   map[string]bool
	unknown map[string]bool
	collect bool
}

func (w *walker) walk(node any, doc, path string, depth int) error {
	m, ok := node.(map[string]any)
	if !ok {
		// A boolean schema, or a fragment reported at validation time. The compile walk adds no
		// rejections beyond its bounds.
		return nil
	}
	w.count++
	if w.count > MaxSubschemas {
		return fmt.Errorf("%w: more than %d subschemas (at %s)", ErrMaxSubschemas, MaxSubschemas, path)
	}
	if depth > MaxDepth {
		return fmt.Errorf("%w: deeper than %d levels (at %s)", ErrMaxDepth, MaxDepth, path)
	}
	if depth > w.maxDepth {
		w.maxDepth = depth
	}

	if w.collect {
		for k := range m {
			if !implementedKeywords[k] && !inertKeywords[k] {
				w.unknown[k] = true
			}
		}
	}

	if ref, ok := m["$ref"].(string); ok {
		if err := w.followRef(ref, doc, path, depth); err != nil {
			return err
		}
	}
	// Descend only through positions this validator treats as schemas; unimplemented keywords are
	// reported above, not followed.
	for _, key := range []string{"properties", "$defs"} {
		if sub, ok := m[key].(map[string]any); ok {
			for name, v := range sub {
				if err := w.walk(v, doc, path+"."+key+"."+name, depth+1); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if arr, ok := m[key].([]any); ok {
			for i, v := range arr {
				if err := w.walk(v, doc, fmt.Sprintf("%s.%s[%d]", path, key, i), depth+1); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"not", "propertyNames", "additionalProperties"} {
		if v, ok := m[key]; ok {
			if err := w.walk(v, doc, path+"."+key, depth+1); err != nil {
				return err
			}
		}
	}
	if v, ok := m["items"]; ok {
		if tuple, isTuple := v.([]any); isTuple {
			for i, sub := range tuple {
				if err := w.walk(sub, doc, fmt.Sprintf("%s.items[%d]", path, i), depth+1); err != nil {
					return err
				}
			}
		} else if err := w.walk(v, doc, path+".items", depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) followRef(ref, doc, path string, depth int) error {
	if err := checkLocalRef(ref); err != nil {
		return fmt.Errorf("%w (at %s)", err, path)
	}
	target, tdoc, err := w.s.resolve(ref, doc)
	if err != nil {
		// An unresolvable reference, such as a sibling file in a schema compiled from bytes, is
		// reported at validation time rather than refused here.
		return nil
	}
	key := tdoc + "#" + ref
	if w.stack[key] {
		return fmt.Errorf("%w: %q resolves back onto its own path (at %s)", ErrRefCycle, ref, path)
	}
	w.stack[key] = true
	defer delete(w.stack, key)
	return w.walk(target, tdoc, path+"->"+ref, depth+1)
}

// checkLocalRef refuses a `$ref` this validator will not resolve. Local means one of exactly two
// things: a pointer into the document being compiled, or the bare file name of a sibling in the same
// directory. Everything else — a URI scheme, a network-path reference, an absolute path, or any path
// with a separator or a `..` in it — is refused by name, with the reason stated in the error.
func checkLocalRef(ref string) error {
	file, _, _ := strings.Cut(ref, "#")
	if file == "" {
		return nil // an in-document JSON pointer
	}
	if strings.HasPrefix(file, "//") {
		return fmt.Errorf("%w: %q is a network-path reference; this validator resolves only in-document pointers and same-directory file names, and it never fetches", ErrNonLocalRef, ref)
	}
	if scheme, rest, found := strings.Cut(file, ":"); found && isURIScheme(scheme) {
		_ = rest
		return fmt.Errorf("%w: %q carries the URI scheme %q. A schema is a declaration; dereferencing it would be a fetch nobody authorized, and for a network URI the protocol forbids doing so automatically. Refer to the sibling by its bare file name instead", ErrNonLocalRef, ref, scheme)
	}
	if filepath.IsAbs(file) || strings.ContainsAny(file, `/\`) {
		return fmt.Errorf("%w: %q names a path rather than a sibling file; this validator resolves only bare file names in the referring schema's own directory, so a $ref can never walk out of it", ErrNonLocalRef, ref)
	}
	if file == ".." || strings.HasPrefix(file, "..") {
		return fmt.Errorf("%w: %q escapes the referring schema's directory", ErrNonLocalRef, ref)
	}
	return nil
}

// isURIScheme reports whether s is syntactically a URI scheme (RFC 3986: ALPHA *( ALPHA / DIGIT / "+"
// / "-" / "." )). A Windows drive letter matches too, which is intended: `C:\schemas\x.json` is
// exactly as non-local as `https://…`.
func isURIScheme(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}
