package cli

// ADDRESSING PART OF AN AUTHORITY DOCUMENT FROM THE COMMAND LINE.
//
// `--authority` declares a whole document. Declaring PART of one has always been possible, but only
// through `--authority-manifest`: a JSON file naming explicit byte offsets. That is the right wire
// format and the wrong thing to ask a person for. Measured, the cost of it was that someone who hit
// the embedding budget copied a section into a temp file with a provenance comment instead — which
// works, and silently changes what the reviewers were told is authoritative.
//
// The budget refusal now prints the offsets (see the authority package's outline), so half of that
// cost is gone. This closes the other half: the offsets no longer have to be carried into a JSON file
// by hand.
//
//	--authority spec.md#The Relevant Part     the named top-level section
//	--authority spec.md:1012-3033             an explicit byte range, start inclusive, end exclusive
//
// THE SUGAR DISSOLVES HERE. Both forms resolve to the SAME `ranges` declaration the manifest takes,
// at the flag boundary, before anything else sees them — so the run record, the inclusion manifest
// and every surface report a concrete byte range rather than a heading that might resolve differently
// on a later read. What was included stays a recorded fact, which is the whole point of the manifest.
//
// It is deliberately CLI-ONLY. On ACP and MCP the caller is a model or a peer process, and it
// declares authority through the structured manifest where ranges are already expressible; a
// convenience whose job is to save a human some typing has no reason to exist there.

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

// byteRangeSuffix matches a trailing `:START-END`. It is anchored at the END of the string and the
// bounds must be digits, so a Windows path keeps working: `C:\spec.md` has a colon that cannot match,
// and `C:\spec.md:10-20` still splits at the right one.
var byteRangeSuffix = regexp.MustCompile(`:(\d+)-(\d+)$`)

// authorityRef is one `--authority` value after its selector has been split off.
type authorityRef struct {
	Path string
	// Section is a heading to resolve; Start/End are an explicit range. At most one form is set.
	Section    string
	Start, End int
	HasRange   bool
}

// parseAuthorityRef splits a `--authority` value into its path and optional selector. A value with
// neither selector is returned unchanged, which is the ordinary whole-document case.
func parseAuthorityRef(raw string) (authorityRef, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return authorityRef{}, fmt.Errorf("empty --authority value")
	}
	// A HEADING WINS OVER A RANGE when both appear, because '#' cannot occur in the range syntax:
	// splitting on '#' first means a heading containing a colon is not mangled.
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

// resolveAuthorityRef turns one parsed reference into the declaration the core consumes.
//
// A SELECTOR MAKES THIS A PARTIAL INCLUSION, so the document is declared `ranges` rather than
// requireFull — that is what makes the manifest record `complete: false` with its own embedded hash,
// and what makes the omitted spans explicit in the prompt. Nothing about partial inclusion becomes
// implicit just because the command line got shorter.
//
// A heading is resolved by READING THE FILE HERE. That costs a read the resolver will repeat, and it
// buys the property that matters: the concrete offsets are fixed at declaration time and recorded,
// rather than a name being re-resolved later against a document that may have moved on.
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
