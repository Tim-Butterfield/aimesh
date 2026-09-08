package jsonschema

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// This file is the COMPILE-TIME half of the validator: what a schema is allowed to be before anything
// is validated against it.
//
// Two obligations meet here.
//
//  1. THE PROTOCOL ONE. A schema `$ref` that resolves to a network URI must never be dereferenced
//     automatically. Until now that held only by accident — Compile takes bytes and has no directory,
//     so `$ref: "https://…"` failed because we tried to open it as a FILE. "It happens to fail" is not
//     a compliance story: the refusal is now explicit, named, and stated at compile time, and it says
//     why. We implement no opt-in fetch mode and will not: a schema is a declaration, and a
//     declaration that reaches the network is a fetch nobody authorized.
//
//  2. THE LOCAL ONE. A self-referential `$defs` chain would recurse without limit. Today the exposure
//     is nil — the only schemas compiled here are literals in this repo's own source — but an
//     unbounded validator on a RUNTIME path (meshcore/mcp's --strict-schema / AIMESH_MCP_STRICT_SCHEMA)
//     is a hole whether or not anything currently reaches it. The bounds are enforced at COMPILE time,
//     which happens once per tool and is already cached, so a malformed schema becomes a startup
//     failure on our own declaration rather than a per-call cost. The specification grades this a
//     SHOULD; we grade it a MUST under our own fail-closed reading, and the difference is recorded
//     rather than blurred.

// The compile-time bounds.
//
// Both numbers are chosen with an order of magnitude of headroom over the largest schema this repo
// publishes, and TestPublishedSchemasAreWellInsideTheBounds asserts that headroom rather than leaving
// it as folklore — if a schema ever grows toward a bound, the build says so before the bound bites.
// They bound the COMPILE walk, so they are a property of the declaration, not of any payload.
const (
	// MaxDepth bounds how deeply schema objects nest, counting a `$ref` hop as one level. The
	// deepest schema in this repo is well under a third of this.
	MaxDepth = 64
	// MaxSubschemas bounds how many schema nodes one compile visits in total. It is a WORK bound as
	// much as a size bound: a `$ref` graph with many diamonds is walked once per reference, so this
	// is what stops a small document from describing a large walk.
	MaxSubschemas = 2048
)

// The compile-time refusals. They are sentinel errors so a caller can branch on the KIND of refusal
// — a non-local `$ref` is a compliance refusal and a depth overrun is a resource refusal, and a test
// that cannot tell them apart is a test that passes for the wrong reason.
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

// implementedKeywords is this validator's VOCABULARY: the keywords it actually acts on. A keyword
// outside this set and outside inertKeywords is not "unknown" in the specification's sense — it is
// KNOWN-AND-UNIMPLEMENTED, which is the dangerous case, because treating it as an annotation means a
// payload our own strict check passes may be one a compliant client rejects.
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

// inertKeywords are the keywords this validator ignores AND that cannot change a verdict, so ignoring
// them is exactly what the specification says to do with an annotation.
//
// `$id` is the one entry that is not inert in general: in 2020-12 it re-bases `$ref` resolution. It is
// listed here because of a property of THIS repo's schemas that a reader should not have to rederive:
// every `$id` we publish is the file's own canonical URL in the same directory as every file it
// refers to, so base-URI resolution and this validator's same-directory resolution name the same
// document. If that ever stops being true, `$id` belongs out of this list and in a real
// implementation.
var inertKeywords = map[string]bool{
	"$schema": true, "$id": true, "$comment": true,
	"title": true, "description": true, "default": true, "examples": true,
	"format": true, "deprecated": true, "readOnly": true, "writeOnly": true,
}

// Vocabulary reports what this validator implements and what it deliberately ignores, both sorted.
// It exists so the package doc's claim and the code cannot drift: a test asserts they agree, and the
// allowlist check over published schemas is computed from the same two sets.
func Vocabulary() (implemented, ignored []string) {
	return sortedKeys(implementedKeywords), sortedKeys(inertKeywords)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UnsupportedKeywords walks a schema document and reports every keyword it declares that this
// validator neither implements nor knowingly ignores — sorted, deduplicated.
//
// It is the guard against a specific silent degradation, and the case is worth stating exactly. A
// tool's inputSchema/outputSchema may use ANY 2020-12 keyword. This validator treats a keyword it does
// not implement as an annotation — right for an UNKNOWN keyword, wrong for a KNOWN-and-unimplemented
// one. Declare `patternProperties` or `dependentRequired` and our own strict check would pass a
// payload a compliant client rejects, while the build stayed green. A build that fails instead is the
// same move as the drift fix that created this package: convert "silently ignored" into "fails CI".
//
// It protects OUR schemas from drifting outside OUR vocabulary. It does not establish general 2020-12
// support and must never be cited as though it did.
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

// Measure reports the compile walk's observed depth and node count, so a test can assert headroom
// against MaxDepth / MaxSubschemas instead of trusting that the bounds are generous.
func Measure(schema []byte) (depth, subschemas int, err error) {
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
	// stack is the `$ref` targets on the CURRENT resolution path, keyed doc+pointer. A cycle is a
	// ref back onto this path; two independent references to the same $defs entry are a diamond,
	// not a cycle, and are walked twice (which the node budget bounds).
	stack   map[string]bool
	unknown map[string]bool
	collect bool
}

func (w *walker) walk(node any, doc, path string, depth int) error {
	m, ok := node.(map[string]any)
	if !ok {
		// A boolean schema, or a fragment this validator will complain about at validation time.
		// The compile walk deliberately adds no NEW rejections beyond the four bounded ones: its job
		// is to bound the declaration, not to re-grade it.
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
	// Descend ONLY through positions this validator itself treats as schemas. A keyword it does not
	// implement is reported (above) rather than followed: following it would mean asserting a
	// structure we do not otherwise honour.
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
		// UNRESOLVABLE IS NOT THE SAME AS FORBIDDEN. A sibling-file `$ref` in a schema compiled from
		// bytes has no directory to resolve against; that is refused LOUDLY at validation, where the
		// existing error already says so. Refusing it here too would turn a schema that has always
		// compiled into one that does not, for a case the walk is not the authority on.
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
