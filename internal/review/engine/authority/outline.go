package authority

// This file addresses authority documents by section. It is not a Markdown parser: it finds ATX
// headings at the shallowest level a document uses and reports the byte span each one covers, which
// is the half-open range a `ranges` declaration selects.

import (
	"fmt"
	"strconv"
	"strings"
)

// sectionError is the error SectionRange returns when a section cannot be addressed.
type sectionError struct{ msg string }

func (e sectionError) Error() string { return e.msg }

// SectionRange resolves a section name to the byte range that selects it, for `--authority
// path.md#Heading`. Matching is case-insensitive and ignores surrounding whitespace. A name that
// matches more than one section is an error rather than a first match, and a name that matches none
// lists the sections that exist.
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

// sectionTitles lists a document's section names for an error message, at most maxOutlineSections.
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

// maxOutlineSections bounds how many section names an error lists; the number omitted is stated.
const maxOutlineSections = 12

// Section is one top-level span of a document: its heading and the half-open byte range
// [Start, End) that selects it.
type Section struct {
	Title string
	Start int
	End   int
}

// outline returns the document's top-level sections: those at the shallowest heading level present,
// so a document whose headings start at `##` is sectioned by them. It returns nil when the document
// has no ATX headings.
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

// atxHeading recognizes `#` through `######` followed by a space, returning the level and title. A
// closing-hash form (`## Title ##`) is trimmed from the title.
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
