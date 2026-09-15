package run

// Citation grounding checks, without executing anything, whether each finding points at something
// real: the cited file exists under the reviewed root, the cited lines exist, and a named identifier
// appears in the file (a plain substring match, so it is language-agnostic). It never checks that the
// code does what the finding says.
//
// Grounding is only a label. It never invalidates, drops or reorders a finding or changes its
// severity or state, because a reviewer that cited the wrong file may still have found a real defect.
// It reads only files the run already read, so it always runs.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/rootfile"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
)

// maxGroundedFileBytes bounds a file this pass reads; a larger file is reported unreadable.
const maxGroundedFileBytes = 4 << 20

// symbolPattern matches a single identifier-shaped token. Wordier locations are left unchecked rather
// than risking a false symbol_absent on a correct finding.
var symbolPattern = regexp.MustCompile(`^[\p{L}_$][\p{L}\p{N}_$.:<>\-]{0,127}$`)

// lineRefPattern extracts the integers from a location, so forms such as 42, 42-58, L42 and 42:10 all
// yield line numbers.
var lineRefPattern = regexp.MustCompile(`\d+`)

// Grounding statuses.
const (
	// GroundedOK — the cited file exists and the location, if checked, resolved.
	GroundedOK = "grounded"
	// GroundedNoCitation — the finding names no file, which is not a failure.
	GroundedNoCitation = "no_citation"
	// GroundedFileMissing — the cited path is not under the reviewed root.
	GroundedFileMissing = "file_missing"
	// GroundedLineOutOfRange — the file exists and is shorter than the line the finding cites.
	GroundedLineOutOfRange = "line_out_of_range"
	// GroundedSymbolAbsent — the file exists and the named identifier does not appear anywhere in it.
	GroundedSymbolAbsent = "symbol_absent"
	// GroundedLocationUnchecked — the file exists, but the location is not a line reference or a
	// single symbol, so it was not checked.
	GroundedLocationUnchecked = "location_unchecked"
	// GroundedUnreadable — the path could not be read (a directory, a permission error, or past the
	// size bound); distinct from file_missing.
	GroundedUnreadable = "unreadable"
)

// GroundingNote is the fixed sentence carried on every summary.
const GroundingNote = "Grounding checks only that a finding points at something real: the file exists, the cited lines exist, the named symbol appears. It does NOT check that the code there does what the finding says. A grounded finding may still be wrong, and an ungrounded one may still be right — nothing here drops, downgrades or reorders a finding."

// groundFindings labels each finding in the final adjudication with its citation grounding and returns
// the run-level tally. It writes only Decision.Grounding, and stops at the shorter of Findings and
// Decisions rather than panicking on a mismatch.
func groundFindings(workspaceRoot string, adj *adjudication.Result) *review.GroundingSummary {
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		// With no workspace there is nothing to check against.
		return nil
	}
	rep := &review.GroundingSummary{Root: root, ByStatus: map[string]int{}, Note: GroundingNote}
	// Each distinct file is read once.
	files := map[string]*groundedFile{}
	for i := range adj.Findings {
		if i >= len(adj.Decisions) {
			break
		}
		g := groundOne(root, adj.Findings[i], files)
		adj.Decisions[i].Grounding = &g
		rep.ByStatus[g.Status]++
		switch g.Status {
		case GroundedNoCitation:
			rep.NoCitation++
		case GroundedOK, GroundedLocationUnchecked:
			rep.Checked++
			rep.Grounded++
		default:
			rep.Checked++
			rep.Unresolved++
		}
	}
	return rep
}

// groundedFile is one file read once: its contents' line count and whether it could be read at all.
type groundedFile struct {
	lines int
	body  string
	err   bool
}

// groundOne evaluates a single finding's citation.
func groundOne(root string, f review.Finding, cache map[string]*groundedFile) review.CitationGrounding {
	file := strings.TrimSpace(f.File)
	loc := strings.TrimSpace(f.Location)
	if file == "" {
		return review.CitationGrounding{
			Status: GroundedNoCitation,
			Detail: "the finding names no file, so there is no citation to resolve",
		}
	}
	g := review.CitationGrounding{File: file, Location: loc}
	gf, ok := cache[file]
	if !ok {
		gf = readForGrounding(root, file)
		cache[file] = gf
	}
	switch {
	case gf.err:
		g.Status = GroundedUnreadable
		g.Detail = "the path could not be read in the reviewed tree — it may be a directory, unreadable, or past the size this check will load"
		return g
	case gf.lines < 0:
		g.Status = GroundedFileMissing
		g.Detail = "no such file under the reviewed root"
		return g
	}
	g.LineCount = gf.lines
	if loc == "" {
		g.Status = GroundedOK
		g.Detail = "the cited file exists; the finding names no location within it"
		return g
	}
	// A location with digits is a line reference unless the whole location is identifier-shaped:
	// `parse42` is a symbol, while `L42` and `42-58` are lines.
	if lines := lineNumbers(loc); len(lines) > 0 && !looksLikeSymbol(loc) {
		high := lines[len(lines)-1]
		if high > gf.lines {
			g.Status = GroundedLineOutOfRange
			g.Detail = "the cited line " + strconv.Itoa(high) + " is past the end of the file, which has " + strconv.Itoa(gf.lines) + " line(s)"
			return g
		}
		g.Status = GroundedOK
		g.Detail = "the cited file exists and is long enough to contain the cited line(s)"
		return g
	}
	if looksLikeSymbol(loc) {
		if !strings.Contains(gf.body, loc) {
			g.Status = GroundedSymbolAbsent
			g.Detail = "the named symbol does not appear anywhere in the cited file"
			return g
		}
		g.Status = GroundedOK
		g.Detail = "the cited file exists and contains the named symbol"
		return g
	}
	g.Status = GroundedLocationUnchecked
	g.Detail = "the cited file exists; the location is not a line reference or a single symbol, so it was not checked"
	return g
}

// readForGrounding reads one file through rootfile. A missing or escaping path yields lines -1, which
// is distinct from an unreadable file.
func readForGrounding(root, rel string) *groundedFile {
	b, err := rootfile.ReadUnder(root, rel)
	if err != nil {
		// Missing and escaping paths both mean the citation names no file in this tree.
		return &groundedFile{lines: -1}
	}
	if len(b) > maxGroundedFileBytes {
		return &groundedFile{err: true}
	}
	body := string(b)
	// Newlines plus one counts a last line without a trailing newline and treats an empty file as one
	// line.
	return &groundedFile{lines: strings.Count(body, "\n") + 1, body: body}
}

// lineNumbers pulls the integers out of a location, in order.
func lineNumbers(loc string) []int {
	var out []int
	for _, m := range lineRefPattern.FindAllString(loc, 8) {
		n, err := strconv.Atoi(m)
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// looksLikeSymbol reports whether the location is one identifier-shaped token (see symbolPattern).
func looksLikeSymbol(loc string) bool {
	return symbolPattern.MatchString(loc)
}

// GroundingRows returns a run's grounding labels in finding order. Every surface builds from it, so
// they describe a run identically.
func GroundingRows(out review.RunOutcome) []review.CitationGrounding {
	var rows []review.CitationGrounding
	for _, d := range out.Decisions {
		if d.Grounding != nil {
			rows = append(rows, *d.Grounding)
		}
	}
	return rows
}
