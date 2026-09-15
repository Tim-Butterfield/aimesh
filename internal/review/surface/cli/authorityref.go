package cli

// This file parses --authority selectors, which name part of a document on the command line:
//
//	--authority spec.md#The Relevant Part     the named top-level section
//	--authority spec.md:1012-3033             a byte range, start inclusive, end exclusive
//
// Both forms resolve at the flag boundary to the same ranges declaration --authority-manifest takes,
// so the run record reports concrete byte ranges rather than a heading that could resolve
// differently later. The selectors are CLI-only; ACP and MCP callers use the structured manifest.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
)

// byteRangeSuffix matches a trailing :START-END. It is anchored at the end and requires digits, so a
// Windows drive colon never matches.
var byteRangeSuffix = regexp.MustCompile(`:(\d+)-(\d+)$`)

// authorityRef is one `--authority` value after its selector has been split off.
type authorityRef struct {
	Path string
	// Section is a heading to resolve; Start and End are an explicit range. At most one form is set.
	Section    string
	Start, End int
	HasRange   bool
}

// parseAuthorityRef splits a --authority value into its path and optional selector. A value without
// a selector names the whole document.
func parseAuthorityRef(raw string) (authorityRef, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return authorityRef{}, fmt.Errorf("empty --authority value")
	}
	// Split on '#' first: it cannot occur in the range syntax, so a heading containing a colon survives.
	if path, section, ok := strings.Cut(v, "#"); ok {
		if strings.TrimSpace(section) == "" {
			return authorityRef{}, fmt.Errorf("--authority %q names no section after '#'", raw)
		}
		if strings.TrimSpace(path) == "" {
			return authorityRef{}, fmt.Errorf("--authority %q names no document before '#'", raw)
		}
		return authorityRef{Path: strings.TrimSpace(path), Section: strings.TrimSpace(section)}, nil
	}
	if m := byteRangeSuffix.FindStringSubmatch(v); m != nil {
		start, _ := strconv.Atoi(m[1])
		end, _ := strconv.Atoi(m[2])
		if end <= start {
			return authorityRef{}, fmt.Errorf("--authority %q: the range end must be greater than the start (start is inclusive, end exclusive)", raw)
		}
		return authorityRef{Path: strings.TrimSpace(v[:len(v)-len(m[0])]), Start: start, End: end, HasRange: true}, nil
	}
	return authorityRef{Path: v}, nil
}

// resolveAuthorityRef turns a parsed reference into the declaration the core consumes.
//
// A selector makes the inclusion partial, declared as ranges, so the manifest records complete:false
// and the prompt marks omitted spans. A heading is resolved by reading the file here, so the offsets
// are fixed and recorded at declaration time.
func resolveAuthorityRef(ref authorityRef, pin string) (review.AuthorityDoc, error) {
	name := filepath.Base(filepath.Clean(ref.Path))
	doc := review.AuthorityDoc{
		Name: name, Path: ref.Path,
		Completeness: review.CompletenessRequireFull,
		ExpectedHash: pin,
	}
	switch {
	case ref.Section != "":
		body, err := os.ReadFile(ref.Path)
		if err != nil {
			return review.AuthorityDoc{}, fmt.Errorf("--authority %s#%s: %w", ref.Path, ref.Section, err)
		}
		start, end, serr := authority.SectionRange(string(body), ref.Section)
		if serr != nil {
			return review.AuthorityDoc{}, fmt.Errorf("--authority %s: %w", ref.Path, serr)
		}
		doc.Completeness = review.CompletenessRanges
		doc.Ranges = []review.AuthorityRange{{Start: start, End: end}}
	case ref.HasRange:
		doc.Completeness = review.CompletenessRanges
		doc.Ranges = []review.AuthorityRange{{Start: ref.Start, End: ref.End}}
	}
	return doc, nil
}
