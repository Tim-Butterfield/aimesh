package schema

// This file holds ExtractJSONObject, the repair pass applied to model output before it is decoded. It
// only extracts: it strips one code fence and surrounding prose to leave exactly one top-level object,
// and records each repair. It never coerces types or decodes permissively.

import (
	"bytes"
	"errors"
)

// Repair labels are the transformations ExtractJSONObject may apply, each reported only when applied.
// Trimming surrounding whitespace is not a repair, since it never changes the value.
const (
	RepairStrippedCodeFence    = "stripped-code-fence"
	RepairTrimmedLeadingProse  = "trimmed-leading-prose"
	RepairTrimmedTrailingProse = "trimmed-trailing-prose"
)

// ExtractJSONObject returns the single top-level JSON object in raw, a sub-slice of the input, and the
// repairs applied in order. An input that is already one object, possibly padded with whitespace, comes
// back unchanged with no repairs. No object, or more than one, is an error. The result is not validated;
// the caller decodes and schema-checks it.
func ExtractJSONObject(raw []byte) (obj []byte, repairs []string, err error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return nil, nil, errors.New("no JSON object found (empty response)")
	}
	// Unwrap a single markdown code fence before searching for the object.
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
		// exactly one object
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

// stripCodeFence unwraps one markdown code fence around the trimmed input b and returns the inner content
// and true. It returns b and false unless b opens with ``` and an optional plain info string on its own
// line, ends with ```, and contains no other fence.
func stripCodeFence(b []byte) ([]byte, bool) {
	if !bytes.HasPrefix(b, []byte(fence)) {
		return b, false
	}
	nl := bytes.IndexByte(b, '\n')
	if nl < 0 {
		return b, false // a single-line ``` — not a wrapping block
	}
	// The rest of the opening line is the info string; JSON punctuation there means it is content.
	if info := b[len(fence):nl]; bytes.ContainsAny(info, "{}[]\"`") {
		return b, false
	}
	inner := b[nl+1:]
	if !bytes.HasSuffix(inner, []byte(fence)) {
		return b, false
	}
	inner = inner[:len(inner)-len(fence)]
	if bytes.Contains(inner, []byte(fence)) {
		return b, false // more than one block is ambiguous
	}
	return inner, true
}

// topLevelObjects returns the [start,end) spans of every top-level JSON object in b, in order. Brackets
// inside string literals are ignored. An unterminated string or bracket, or an unmatched close, is an
// error.
func topLevelObjects(b []byte) ([][2]int, error) {
	var spans [][2]int
	depth := 0
	inString := false
	escaped := false
	objStart := -1 // start index of the current top-level run when it opened with '{', else -1
	for i := range b {
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
