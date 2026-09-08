package run

// SCOPE SELECTION — reviewing part of a tree instead of all of it.
//
// Until this existed there was no file-level scope at all: the collector took a root and collected
// it, so "review what I just changed" meant pointing at a smaller directory or nothing.
//
// THE SHAPE, and it is the point of the design rather than an implementation detail. The primitive is
// A SET OF FILES. Everything else here is a BASELINE — a way of producing that set — and the baselines
// available differ by what the tree can offer:
//
//	explicit paths / globs      any tree
//	changed since a timestamp   any tree          (modification time)
//	changed since a VCS ref     a repository only (git/hg)
//
// That ordering is deliberate. Making a git ref the organising idea — `--diff` as the feature, with
// everything else bolted beside it — would give half the usage a first-class answer and the other
// half a documented gap. NOT ALL USAGE IS REPO-FOCUSED: aimesh supports `folder init` as a peer of
// `repo init`, and a plain folder needs a real "what I just changed", not a consolation prize. So the
// VCS baselines are ONE RESOLVER AMONG SEVERAL, and the capability a user gets is uniform even though
// the mechanisms behind it are not.
//
// WHAT SCOPE DOES NOT DO. It narrows what reviewers are SHOWN. It does not narrow containment (the
// copy is still the copy), does not widen anything, and cannot reach outside the workspace root — a
// selector that resolves to nothing is a refusal, never a silent fallback to everything. That last
// rule is the one that matters: a mistyped glob quietly reviewing the entire tree would be the
// opposite of what the user asked for, at full cost.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// Stable MACHINE reason codes for scope selection.
const (
	// ReasonScopeEmpty — the selector resolved to no file. It is a REFUSAL: a run that fell back to
	// the whole tree would review everything at full cost while the user believed they had narrowed
	// it, and a run that proceeded with nothing would report "no findings" about nothing.
	ReasonScopeEmpty = "scope_selected_nothing"
	// ReasonScopeUnavailable — a baseline was requested that this tree cannot answer (a VCS ref in a
	// plain folder). Refused rather than silently ignored, which would review the whole tree.
	ReasonScopeUnavailable = "scope_baseline_unavailable"
	// ReasonScopeInvalid — the selector itself is malformed (an unparseable duration, a path that
	// escapes the root).
	ReasonScopeInvalid = "scope_invalid"
)

// vcsScopeTimeout bounds the diff call. A status that hangs must not hang the review.
const vcsScopeTimeout = 30 * time.Second

// Scope is how a run narrows what it reviews. The zero value reviews everything, which is what every
// run did before this existed.
type Scope struct {
	// Paths are workspace-relative paths or globs, as the operator gave them. A directory selects
	// everything under it; `**` is not special beyond what path.Match does, so a glob that means to
	// cross directories should name the directory.
	Paths []string
	// ChangedSince selects files modified since a time: a Go duration ("2h") or an RFC3339 stamp.
	// It works in ANY tree, which is what makes "what I just changed" answerable in a plain folder.
	ChangedSince string
	// VCSRef selects files changed against a version-control baseline: "diff" (working tree vs
	// HEAD), "staged", or any ref/commit. Available in a repository ONLY, and refused elsewhere
	// rather than ignored.
	VCSRef string
}

// Empty reports whether this scope narrows anything.
func (s Scope) Empty() bool {
	return len(s.Paths) == 0 && strings.TrimSpace(s.ChangedSince) == "" && strings.TrimSpace(s.VCSRef) == ""
}

// Resolve turns the selector into the SET OF WORKSPACE-RELATIVE PATHS this run may show a reviewer.
//
// The baselines UNION rather than intersect. Naming two of them means "review what either selects",
// which is the reading that matches how a person says it — "the files I touched, plus this one I am
// worried about" — and the intersection reading has no natural phrasing at all.
func (s Scope) Resolve(ctx context.Context, root string) (map[string]bool, error) {
	if s.Empty() {
		return nil, nil // nil means "no narrowing"; an EMPTY map would mean "show nothing"
	}
	out := map[string]bool{}
	if len(s.Paths) > 0 {
		matched, err := matchPaths(root, s.Paths)
		if err != nil {
			return nil, err
		}
		for p := range matched {
			out[p] = true
		}
	}
	if since := strings.TrimSpace(s.ChangedSince); since != "" {
		cutoff, err := parseSince(since)
		if err != nil {
			return nil, err
		}
		matched, err := modifiedSince(root, cutoff)
		if err != nil {
			return nil, err
		}
		for p := range matched {
			out[p] = true
		}
	}
	if ref := strings.TrimSpace(s.VCSRef); ref != "" {
		matched, err := changedAgainstVCS(ctx, root, ref)
		if err != nil {
			return nil, err
		}
		for p := range matched {
			out[p] = true
		}
	}
	if len(out) == 0 {
		return nil, fault.New(fault.Usage, fmt.Sprintf(
			"the scope you asked for selected NO files in %s. Nothing was reviewed and nothing was spent — a run that fell back to the whole tree here would review everything at full cost while you believed you had narrowed it. Check the paths, the time window, or the ref",
			root)).WithHalt("A").WithReason(ReasonScopeEmpty)
	}
	return out, nil
}

// matchPaths resolves explicit paths and globs against the root. A path naming a DIRECTORY selects
// everything beneath it, which is what someone typing a directory means.
func matchPaths(root string, patterns []string) (map[string]bool, error) {
	all, err := walkRelative(root)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, raw := range patterns {
		pat := strings.TrimSpace(filepath.ToSlash(raw))
		if pat == "" {
			continue
		}
		if !workspace.SafeRel(strings.TrimSuffix(pat, "/")) && !strings.ContainsAny(pat, "*?[") {
			return nil, fault.New(fault.Usage, fmt.Sprintf(
				"scope path %q is not a relative path inside the workspace", raw)).
				WithHalt("A").WithReason(ReasonScopeInvalid)
		}
		for _, rel := range all {
			if rel == pat || strings.HasPrefix(rel, strings.TrimSuffix(pat, "/")+"/") {
				out[rel] = true
				continue
			}
			if ok, _ := path.Match(pat, rel); ok {
				out[rel] = true
			}
		}
	}
	return out, nil
}

// walkRelative lists every regular file under root as a slash-separated relative path, skipping the
// paths workspace.IsExcluded already keeps out of a review.
func walkRelative(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is the collector's caveat to report, not scope's
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if workspace.IsExcluded(rel) {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// parseSince accepts a Go duration ("2h", "90m") or an RFC3339 timestamp.
func parseSince(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fault.New(fault.Usage,
				"a scope window must be positive — \""+s+"\" selects nothing").
				WithHalt("A").WithReason(ReasonScopeInvalid)
		}
		return time.Now().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fault.New(fault.Usage, fmt.Sprintf(
		"scope window %q is neither a duration (\"2h\", \"90m\") nor an RFC3339 timestamp", s)).
		WithHalt("A").WithReason(ReasonScopeInvalid)
}

// modifiedSince selects files whose modification time is at or after the cutoff.
//
// IT IS THE BASELINE THAT WORKS ANYWHERE, and that is why it exists beside the VCS one rather than
// as a fallback nobody documents: a plain folder has no committed baseline, and mtime is the honest
// answer to "what did I just change" there. It is coarser than a diff — a touched-but-unchanged file
// is selected — and coarse in the SAFE direction, since the cost is reviewing a file that did not
// need it rather than missing one that did.
func modifiedSince(root string, cutoff time.Time) (map[string]bool, error) {
	all, err := walkRelative(root)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, rel := range all {
		fi, serr := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if serr != nil {
			continue
		}
		if !fi.ModTime().Before(cutoff) {
			out[rel] = true
		}
	}
	return out, nil
}

// changedAgainstVCS selects files changed against a version-control baseline.
//
// A TREE WITH NO VCS REFUSES rather than falling back. Silently ignoring the selector would review
// the WHOLE tree — the opposite of what was asked, at full cost — and silently selecting nothing
// would report "no findings" about nothing. The refusal names the baselines that DO work there, so
// the answer is a redirection rather than a dead end.
func changedAgainstVCS(ctx context.Context, root, ref string) (map[string]bool, error) {
	if _, ok := localstate.FindRoot(root); !ok {
		return nil, fault.New(fault.Usage, fmt.Sprintf(
			"a version-control baseline (%q) was requested for %s, which is not inside a repository. Use --path or --changed-since instead — both work in any directory, which is the point: not all usage is repo-focused",
			ref, root)).WithHalt("A").WithReason(ReasonScopeUnavailable)
	}
	cctx, cancel := context.WithTimeout(ctx, vcsScopeTimeout)
	defer cancel()

	var args []string
	switch ref {
	case "diff":
		args = []string{"diff", "--name-only", "HEAD"}
	case "staged":
		args = []string{"diff", "--name-only", "--cached"}
	default:
		args = []string{"diff", "--name-only", ref}
	}
	c := exec.CommandContext(cctx, "git", args...)
	c.Dir, c.Env = root, model.CleanGitEnv()
	body, err := c.Output()
	if err != nil {
		return nil, fault.Wrap(fault.Usage, fmt.Sprintf(
			"the version-control baseline %q could not be resolved in %s", ref, root), err).
			WithHalt("A").WithReason(ReasonScopeUnavailable)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		rel := filepath.ToSlash(strings.TrimSpace(line))
		if rel == "" || workspace.IsExcluded(rel) {
			continue
		}
		// A file the diff names but the tree no longer has (a deletion) is not reviewable, and
		// selecting it would make the scope look larger than what a reviewer was shown.
		if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); serr != nil {
			continue
		}
		out[rel] = true
	}
	return out, nil
}

// scopeSnippets narrows a collected set to the selected paths. A nil selection means no narrowing,
// which is what every run did before scope existed — so an unscoped run is byte-identical.
//
// It filters what a reviewer is SHOWN and nothing else. The containment copy is unchanged, the
// withheld caveats are unchanged, and a file left out here was never a containment refusal — which is
// why it is not appended to the withheld list: `withheld` means "a rule kept this out", and "you did
// not ask for it" is a different fact that the scope summary reports instead.
func scopeSnippets(snippets []workspace.Snippet, selected map[string]bool) []workspace.Snippet {
	if selected == nil {
		return snippets
	}
	out := make([]workspace.Snippet, 0, len(snippets))
	for _, s := range snippets {
		if selected[filepath.ToSlash(s.Path)] {
			out = append(out, s)
		}
	}
	return out
}

// ScopeSummary describes what a selector did, for the run record and every surface.
type ScopeSummary struct {
	// Selected is how many files the scope admitted.
	Selected int `json:"selected"`
	// Available is how many the tree holds, so the ratio is visible rather than inferred.
	Available int `json:"available"`
	// Paths, ChangedSince and VCSRef echo the selector as given.
	Paths        []string `json:"paths,omitempty"`
	ChangedSince string   `json:"changedSince,omitempty"`
	VCSRef       string   `json:"vcsRef,omitempty"`
	// Files is the selected set, sorted — what a reviewer was actually shown.
	Files []string `json:"files"`
	// Note states what scope did NOT do, since a narrowed review is silent about everything it was
	// not shown and that silence must not read as approval.
	Note string `json:"note"`
}

// ScopeNote is that fixed sentence.
const ScopeNote = "This review was NARROWED: reviewers saw only the files listed here. It says nothing whatever about the rest of the tree — a finding's absence elsewhere means nobody looked, not that there is nothing there. Containment is unchanged: the copy still holds what it always did, and scope only decides what was shown."

// summarizeScope builds the record. `selected` is nil when nothing narrowed the run, in which case
// there is no summary — an unnarrowed review needs no disclaimer about what it did not see.
func summarizeScope(s Scope, selected map[string]bool, available int) *ScopeSummary {
	if selected == nil {
		return nil
	}
	files := make([]string, 0, len(selected))
	for f := range selected {
		files = append(files, f)
	}
	sort.Strings(files)
	return &ScopeSummary{
		Selected: len(files), Available: available,
		Paths: s.Paths, ChangedSince: s.ChangedSince, VCSRef: s.VCSRef,
		Files: files, Note: ScopeNote,
	}
}
