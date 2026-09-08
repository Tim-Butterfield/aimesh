package run

// NON-EXECUTING CITATION GROUNDING — the mechanically checkable half of "is this finding about real
// code?", answered by reading files and running nothing.
//
// THE GOVERNING INVARIANT, which settles every question this file could otherwise raise:
//
//	Grounding is a LABEL on a finding. It never invalidates one, never drops one, never reorders
//	one, and never changes a severity or a decision state. Nothing in the engine reads it back.
//
// That is the rule task #41 established for model identity, restated for a different signal, and it
// is worth stating twice because this is exactly where it would be tempting to break: a finding whose
// cited file does not exist LOOKS like a finding worth suppressing. It is not. A reviewer that named
// the wrong file may still have found a real defect a line away, in a file it half-remembered, or in
// a file the collector's scope never handed it. The host knows one thing — the citation did not
// resolve — and reporting that is the whole of what it is entitled to say.
//
// WHAT IS CHECKED, and nothing beyond it:
//
//   - THE FILE. Does the cited path exist under the reviewed root, read through the same bounded
//     reader every other read in this package uses?
//   - THE LINES. When the location names a line or a range, is the file actually that long?
//   - THE SYMBOL. When the location names a single identifier-shaped token, does that token appear
//     in the file at all? A plain substring match, deliberately: it is language-agnostic, it costs
//     nothing, and a parser would be a promise about languages this tool does not make.
//
// WHAT IS NOT CHECKED, and why saying so matters: nothing here verifies that the code at that line
// does what the finding says it does. Grounding is a floor, not a corroboration. `grounded` means the
// finding points somewhere real — it does not mean the finding is right, and a reader who reads it
// that way has been misled by a label that was supposed to reduce misplaced confidence.
//
// WHY IT IS NOT OPT-IN. It executes nothing, spawns nothing, and reads only files this run already
// read to build the reviewer prompts, so it has no threat surface and no meaningful cost. A flag
// would be a knob whose only function is to let someone turn off a fact.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/rootfile"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
)

// maxGroundedFileBytes bounds a file this pass will read. A finding citing a 200 MB artifact is not a
// reason to load it: past this size the pass reports `unchecked` rather than either reading it or
// pretending the citation failed.
const maxGroundedFileBytes = 4 << 20

// symbolPattern is what the pass is willing to treat as a SYMBOL: one identifier-shaped token, no
// whitespace. Anything wordier is a prose description of a section, and searching a file for a
// sentence would produce `symbol_absent` on findings whose location was perfectly good English.
//
// The conservative direction here is deliberate and it is the important design choice in this file:
// the pass would rather say "I did not check this" than accuse a correct finding of pointing nowhere.
// An unchecked location costs a reader nothing; a false `symbol_absent` costs them a real finding.
var symbolPattern = regexp.MustCompile(`^[\p{L}_$][\p{L}\p{N}_$.:<>\-]{0,127}$`)

// lineRefPattern extracts the line numbers from a location that names any. It matches the shapes
// reviewers actually emit — `42`, `42-58`, `L42`, `lines 42–58`, `42:10` — by pulling the integers out
// rather than by insisting on one spelling, because the field is model-authored free text and a
// grammar it must match is a grammar it will fail.
var lineRefPattern = regexp.MustCompile(`\d+`)

// Grounding statuses. They are STABLE MACHINE CODES: a surface renders them, a driver branches on
// them, and both are entitled to expect the set not to shift underneath.
const (
	// GroundedOK — the cited file exists and the location, if this pass understood it, resolved.
	GroundedOK = "grounded"
	// GroundedNoCitation — the finding names no file. Not a failure: plenty of legitimate findings are
	// about the change as a whole. It is reported so that "no citation" and "a citation that failed"
	// can never be read as the same thing.
	GroundedNoCitation = "no_citation"
	// GroundedFileMissing — the cited path is not under the reviewed root. This is the failure the
	// pass exists to catch.
	GroundedFileMissing = "file_missing"
	// GroundedLineOutOfRange — the file exists and is shorter than the line the finding cites.
	GroundedLineOutOfRange = "line_out_of_range"
	// GroundedSymbolAbsent — the file exists and the named identifier does not appear anywhere in it.
	GroundedSymbolAbsent = "symbol_absent"
	// GroundedLocationUnchecked — the file exists; the location was not in a shape this pass is willing
	// to check. The FILE half grounded; the location half was not examined and says nothing either way.
	GroundedLocationUnchecked = "location_unchecked"
	// GroundedUnreadable — the path exists but could not be read (a directory, a permission error, a
	// file past the size bound). Distinct from file_missing, because "I could not look" and "it is not
	// there" are different facts and only one of them is about the finding.
	GroundedUnreadable = "unreadable"
)

// GroundingNote is the fixed sentence carried on every summary. A grounded citation is a floor and never a corroboration.
const GroundingNote = "Grounding checks only that a finding points at something real: the file exists, the cited lines exist, the named symbol appears. It does NOT check that the code there does what the finding says. A grounded finding may still be wrong, and an ungrounded one may still be right — nothing here drops, downgrades or reorders a finding."

// groundFindings labels every finding in the FINAL adjudication with what this run could verify about
// its citation, and returns the run-level tally.
//
// It writes `Decision.Grounding` and nothing else. The loop is index-aligned with Findings exactly as
// annotatePrior's is, and it stops at the shorter of the two rather than assuming they match — a
// mismatch there is a bug elsewhere, and a panic in a labelling pass would take down a completed
// review over an annotation.
func groundFindings(workspaceRoot string, adj *adjudication.Result) *review.GroundingSummary {
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		// Nothing to check against. Silence is correct here: a caller with no workspace (the ACP
		// validation probe) has no tree in which a citation could be resolved, and an empty report
		// would assert a check that never ran.
		return nil
	}
	rep := &review.GroundingSummary{Root: root, ByStatus: map[string]int{}, Note: GroundingNote}
	// One read per distinct FILE, however many findings cite it. A busy file is one fact.
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
	// A LINE reference wins over a symbol reading when the location contains digits at all: `parse42`
	// is a symbol, but `L42` and `42-58` are lines, and the cheap discriminator is whether stripping
	// the digits and separators leaves anything identifier-shaped behind.
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

// readForGrounding reads one file through the bounded, root-confined reader. A missing file is
// reported as lines = -1 rather than as an error, because "not there" is the ANSWER this pass is
// looking for and an error would confuse it with "I could not look".
func readForGrounding(root, rel string) *groundedFile {
	b, err := rootfile.ReadUnder(root, rel)
	if err != nil {
		// ReadUnder refuses an escaping path and fails on a missing one alike. Both mean the same
		// thing to a reader of the finding — the citation does not name a file of this tree — and the
		// refusal reason is already recorded by the reader itself.
		return &groundedFile{lines: -1}
	}
	if len(b) > maxGroundedFileBytes {
		return &groundedFile{err: true}
	}
	body := string(b)
	// A file with no trailing newline still has a last line; a file that ends with one does not have
	// an extra empty line after it. Counting separators and adding one gets both right, and an empty
	// file is one (empty) line, which is what a citation to "line 1" of it would find.
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

// looksLikeSymbol reports whether the location is one identifier-shaped token. See symbolPattern for
// why this is deliberately narrow.
func looksLikeSymbol(loc string) bool {
	return symbolPattern.MatchString(loc)
}

// GroundingRows projects a run's grounding annotations in finding order — the ONE projection every
// surface builds from, so the label a CLI reader sees, the one an ACP host receives and the one an
// MCP client is handed cannot describe the same run differently.
func GroundingRows(out review.RunOutcome) []review.CitationGrounding {
	var rows []review.CitationGrounding
	for _, d := range out.Decisions {
		if d.Grounding != nil {
			rows = append(rows, *d.Grounding)
		}
	}
	return rows
}
