package schema

// This file holds ExtractJSONObject: the narrow, enumerated-and-RECORDED repair pass exploremesh runs
// over UNTRUSTED model output before it is unmarshaled (design §6.8 — model output is only ever parsed
// as JSON data, never executed). A real 3-provider dogfood halted because one provider fenced its JSON
// in a ```json … ``` block ("invalid character '`'"). The fix is extraction ONLY: strip a single code
// fence, trim surrounding prose to exactly one balanced top-level object, and RECORD each repair — it
// deliberately does NOT coerce field types or permissively decode (schema type-variance tolerance is
// out of scope). A clean input that is already exactly one object returns byte-identical bytes with an
// empty repair list, so the fake adapter + existing goldens see zero behavior change.

import (
	"bytes"
	"errors"
)

// Repair labels — the closed set of transformations ExtractJSONObject may apply. Each is appended to
// the returned repairs slice ONLY when actually applied, so a caller can audit exactly what was done.
// Surrounding-whitespace trimming is deliberately NOT a repair: it never changes the JSON value, so a
// whitespace-padded clean object still reports an empty repair list.
const (
	RepairStrippedCodeFence    = "stripped-code-fence"
	RepairTrimmedLeadingProse  = "trimmed-leading-prose"
	RepairTrimmedTrailingProse = "trimmed-trailing-prose"
)

// ExtractJSONObject recovers the single top-level JSON object from an untrusted model response, applying
// only the enumerated repairs above and appending a label for each one used. It returns the extracted
// object bytes (a sub-slice of the input for a clean object — byte-identical, zero-copy), the ordered
// repair list, and an error when the input does not contain exactly one unambiguous object.
//
// Guarantees:
//   - A clean input that is already exactly one JSON object (optionally whitespace-padded) returns
//     byte-identical object bytes with an EMPTY repair list.
//   - TWO OR MORE top-level objects → error (ambiguous — it never silently picks one); ZERO → error.
//   - It is extraction only: it does NOT validate the JSON, coerce types, or permissively decode. The
//     caller still json.Unmarshal's (and schema-validates) the returned bytes.
func ExtractJSONObject(raw []byte) (obj []byte, repairs []string, err error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return nil, nil, errors.New("no JSON object found (empty response)")
	}
	// A single wrapping markdown code fence (```json … ``` or ``` … ```) is the exact dogfood failure:
	// unwrap it before searching for the object.
	if inner, ok := stripCodeFence(b); ok {
		b = bytes.TrimSpace(inner)
		repairs = append(repairs, RepairStrippedCodeFence)
		if len(b) == 0 {
			return nil, repairs, errors.New("no JSON object found (empty code fence)")
		}
	}

	spans, serr := topLevelObjects(b)
	if serr != nil {
		return nil, repairs, serr
	}
	switch len(spans) {
	case 0:
		return nil, repairs, errors.New("no JSON object found")
	case 1:
		// single unambiguous object — the only accepted shape
	default:
		return nil, repairs, errors.New("multiple top-level JSON objects (ambiguous)")
	}

	start, end := spans[0][0], spans[0][1]
	// b has no surrounding whitespace, so any bytes outside the object span are non-whitespace prose.
	if start > 0 {
		repairs = append(repairs, RepairTrimmedLeadingProse)
	}
	if end < len(b) {
		repairs = append(repairs, RepairTrimmedTrailingProse)
	}
	return b[start:end], repairs, nil
}

// fence is the CommonMark code-fence delimiter.
const fence = "```"

// stripCodeFence unwraps a SINGLE markdown code fence around b (which the caller has already
// whitespace-trimmed), returning the inner content and true. It is conservative — it declines (returns
// b, false) unless b is unambiguously one wrapping fence: it must open with ``` on its own line
// (optionally followed by a plain info string), close with a trailing ```, and contain no further fence
// delimiter inside (multiple code blocks are ambiguous, so they are left for the object scanner to
// reject). It never treats brace/quote-bearing text as an info string.
func stripCodeFence(b []byte) ([]byte, bool) {
	if !bytes.HasPrefix(b, []byte(fence)) {
		return b, false
	}
	nl := bytes.IndexByte(b, '\n')
	if nl < 0 {
		return b, false // a single-line ``` — not a wrapping block
	}
	// The remainder of the opening line after ``` is the info string (e.g. "json"); reject anything
	// that carries JSON punctuation so we never mistake inline content for a language tag.
	if info := b[len(fence):nl]; bytes.ContainsAny(info, "{}[]\"`") {
		return b, false
	}
	inner := b[nl+1:]
	if !bytes.HasSuffix(inner, []byte(fence)) {
		return b, false
	}
	inner = inner[:len(inner)-len(fence)]
	if bytes.Contains(inner, []byte(fence)) {
		return b, false // a second fence ⇒ more than one block ⇒ ambiguous
	}
	return inner, true
}

// topLevelObjects returns the [start,end) byte spans of every top-level (nesting depth 0) JSON object in
// b, in order. The scanner is JSON-string-aware: braces and brackets inside a string literal (honoring
// backslash escapes) do NOT change depth, so a `}` inside a string can never falsely close an object.
// A structural imbalance (unterminated object/array or string, or an unmatched close) is an error — the
// caller surfaces it as the drop reason rather than guessing.
func topLevelObjects(b []byte) ([][2]int, error) {
	var spans [][2]int
	depth := 0
	inString := false
	escaped := false
	objStart := -1 // start index of the current top-level run when it opened with '{', else -1
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			if depth == 0 {
				if c == '{' {
					objStart = i
				} else {
					objStart = -1 // a top-level array is not an object we extract
				}
			}
			depth++
		case '}', ']':
			if depth == 0 {
				return nil, errors.New("malformed JSON: unbalanced closing bracket")
			}
			depth--
			if depth == 0 && objStart >= 0 {
				spans = append(spans, [2]int{objStart, i + 1})
				objStart = -1
			}
		}
	}
	if inString {
		return nil, errors.New("malformed JSON: unterminated string")
	}
	if depth != 0 {
		return nil, errors.New("malformed JSON: unterminated object")
	}
	return spans, nil
}
