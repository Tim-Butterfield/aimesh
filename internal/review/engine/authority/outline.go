package authority

// THE OUTLINE A BUDGET REFUSAL CARRIES.
//
// The refusal itself is right and stays exactly as it was: an oversized authority document is never
// silently truncated, because a document a review is JUDGED AGAINST is the last thing that may be
// quietly shortened. What was wrong was the remedy. It said "declare completeness `ranges` with
// explicit ranges, or split the document" — and both of those require the user to compute BYTE
// OFFSETS by hand, in a file that frequently belongs to a different repository. Measured, the person
// who hit this did neither: they copied one section into a temp file with a provenance comment, which
// works and silently changes what the reviewers were told is authoritative.
//
// So the refusal now hands over the numbers it is asking for. The offsets in the message are exactly
// the `ranges` values that would make the next attempt succeed, which turns "go measure your file"
// into a line to paste.
//
// WHAT THIS IS NOT: a markdown parser. It finds ATX headings at the shallowest level the document
// uses and reports the byte span between them. A document with no headings gets no outline and says
// so, rather than an invented structure — the whole value here is that the numbers are real.

import (
	"fmt"
	"strconv"
	"strings"
)

// ErrNoSuchSection is returned by SectionRange when the document has no section by that name. It is
// distinguished from "the document has no sections at all" in the message, because the two need
// different responses: fix the name, versus stop trying to address this document by section.
type sectionError struct{ msg string }

func (e sectionError) Error() string { return e.msg }

// SectionRange resolves a section NAME to the byte range that selects it, for `--authority
// path.md#Heading`.
//
// The match is case-insensitive and ignores surrounding whitespace, because a heading is prose a user
// retypes rather than an identifier they copy. An AMBIGUOUS name — two sections with the same heading
// — is an error rather than a first-match: a document is being declared as the authority a review is
// judged against, and silently picking one of two candidates is exactly the kind of quiet choice that
// must not happen there.
//
// On failure the error NAMES the sections that do exist, so a mistyped heading is one correction
// rather than a guessing game.
func SectionRange(text, section string) (start, end int, err error) {
	want := strings.ToLower(strings.TrimSpace(section))
	secs := outline(text)
	if len(secs) == 0 {
		return 0, 0, sectionError{fmt.Sprintf("cannot address section %q: the document has no headings to address by", section)}
	}
	var found []Section
	for _, s := range secs {
		if strings.ToLower(strings.TrimSpace(s.Title)) == want {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 1:
		return found[0].Start, found[0].End, nil
	case 0:
		return 0, 0, sectionError{fmt.Sprintf("no section %q in the document; its top-level sections are: %s", section, strings.Join(sectionTitles(secs), ", "))}
	default:
		return 0, 0, sectionError{fmt.Sprintf("section %q appears %d times in the document — name it unambiguously, or declare explicit byte ranges in an --authority-manifest", section, len(found))}
	}
}

// sectionTitles lists a document's section names for an error message, bounded like the outline is.
func sectionTitles(secs []Section) []string {
	out := make([]string, 0, len(secs))
	for i, s := range secs {
		if i == maxOutlineSections {
			out = append(out, fmt.Sprintf("… and %d more", len(secs)-i))
			break
		}
		out = append(out, strconv.Quote(s.Title))
	}
	return out
}

// maxOutlineSections bounds how many sections a refusal lists. A 200-section document would bury the
// refusal itself; the count of what was dropped is always stated (never a silent cap).
const maxOutlineSections = 12

// Section is one top-level span of a document: its heading, and the byte range that would select it.
// Start is inclusive and End exclusive, which is the half-open convention the range selector uses, so
// the numbers can be pasted without adjustment.
type Section struct {
	Title string
	Start int
	End   int
}

// Bytes is the section's size.
func (s Section) Bytes() int { return s.End - s.Start }

// outline finds the document's top-level sections. It returns nil when the document has no ATX
// headings at all — there is no structure to report, and inventing one would be worse than silence.
//
// "Top level" is the SHALLOWEST heading level present, not literally `#`: a document whose headings
// all start at `##` is sectioned by its `##`s, which is what its author meant by a section.
func outline(text string) []Section {
	type heading struct {
		level int
		title string
		at    int // byte offset of the heading line's first character
	}
	var headings []heading
	shallowest := 0
	for offset := 0; offset < len(text); {
		end := strings.IndexByte(text[offset:], '\n')
		lineEnd := len(text)
		if end >= 0 {
			lineEnd = offset + end
		}
		line := text[offset:lineEnd]
		if level, title, ok := atxHeading(line); ok {
			headings = append(headings, heading{level: level, title: title, at: offset})
			if shallowest == 0 || level < shallowest {
				shallowest = level
			}
		}
		if end < 0 {
			break
		}
		offset = lineEnd + 1
	}
	if shallowest == 0 {
		return nil
	}
	var top []heading
	for _, h := range headings {
		if h.level == shallowest {
			top = append(top, h)
		}
	}
	if len(top) == 0 {
		return nil
	}
	out := make([]Section, 0, len(top))
	for i, h := range top {
		end := len(text)
		if i+1 < len(top) {
			end = top[i+1].at
		}
		out = append(out, Section{Title: h.title, Start: h.at, End: end})
	}
	return out
}

// atxHeading recognises `#`..`######` followed by a space. A `#` with no space is not a heading (it
// is a comment in most of the formats that share this file extension), and the closing-hash form
// (`## Title ##`) is trimmed so the reported title reads the way the document does.
func atxHeading(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	// More than three leading spaces is an indented code block in every markdown dialect.
	if len(line)-len(trimmed) > 3 {
		return 0, "", false
	}
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(trimmed) || trimmed[level] != ' ' {
		return 0, "", false
	}
	title := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(trimmed[level+1:]), "#"))
	if title == "" {
		return 0, "", false
	}
	return level, title, true
}

// outlineRemedy renders the actionable half of a budget refusal: which sections the document has,
// how big each one is, and the exact byte range that selects it.
//
