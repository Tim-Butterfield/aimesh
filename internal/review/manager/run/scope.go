package run

// Scope selection narrows which files reviewers are shown. The primitive is a set of files, produced
// by any combination of baselines:
//
//	explicit paths / globs      any tree
//	changed since a timestamp   any tree          (modification time)
//	changed since a VCS ref     a repository only (git/hg)
//
// Plain folders therefore get the same capability as repositories. Scope never narrows containment or
// reaches outside the workspace root, and a selector that matches nothing is refused rather than
// falling back to the whole tree.

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

// Reason codes for scope selection.
const (
	// ReasonScopeEmpty — the selector matched no file. It is refused: falling back to the whole tree
	// would review everything at full cost.
	ReasonScopeEmpty = "scope_selected_nothing"
	// ReasonScopeUnavailable — a baseline this tree cannot answer, such as a VCS ref in a plain folder.
	ReasonScopeUnavailable = "scope_baseline_unavailable"
	// ReasonScopeInvalid — the selector itself is malformed (an unparseable duration, a path that
	// escapes the root).
	ReasonScopeInvalid = "scope_invalid"
)

// vcsScopeTimeout bounds the diff call. A status that hangs must not hang the review.
const vcsScopeTimeout = 30 * time.Second

// Scope is how a run narrows what it reviews. The zero value reviews everything.
type Scope struct {
	// Paths are workspace-relative paths or globs, as the operator gave them. A directory selects
	// everything under it; `**` is not special beyond what path.Match does, so a glob that means to
	// cross directories should name the directory.
	Paths []string
	// ChangedSince selects files modified since a time: a Go duration ("2h") or an RFC3339 stamp. It
	// works in any tree.
	ChangedSince string
	// VCSRef selects files changed against a version-control baseline: "diff" (working tree vs HEAD),
	// "staged", or any ref or commit. It is refused outside a repository.
	VCSRef string
}

// Empty reports whether this scope narrows anything.
func (s Scope) Empty() bool {
	return len(s.Paths) == 0 && strings.TrimSpace(s.ChangedSince) == "" && strings.TrimSpace(s.VCSRef) == ""
}

// Resolve returns the workspace-relative paths this run may show a reviewer: the union of every
// baseline given. It returns nil for an empty scope and refuses a selector that matches nothing.
func (s Scope) Resolve(ctx context.Context, root string) (map[string]bool, error) {
	if s.Empty() {
		return nil, nil // nil means no narrowing; an empty map would mean show nothing
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

// matchPaths resolves explicit paths and globs against the root; a directory selects everything
// beneath it.
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

// modifiedSince selects files modified at or after the cutoff. It works without version control; a
// touched but unchanged file is also selected, which errs toward reviewing more.
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

// changedAgainstVCS selects files changed against a version-control baseline. A tree without version
// control is refused, with a message naming the baselines that do work there.
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
	for line := range strings.SplitSeq(string(body), "\n") {
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
	// Note states that the review covers only the selected files.
	Note string `json:"note"`
}

// ScopeNote is the fixed Note sentence.
const ScopeNote = "This review was NARROWED: reviewers saw only the files listed here. It says nothing whatever about the rest of the tree — a finding's absence elsewhere means nobody looked, not that there is nothing there. Containment is unchanged: the copy still holds what it always did, and scope only decides what was shown."

// summarizeScope builds the record, or returns nil when nothing narrowed the run.
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
