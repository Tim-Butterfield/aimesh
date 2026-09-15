package localstate

// Removing prior run artifacts.
//
// Run directories hold prompts that embed verbatim copies of everything the models were shown, so
// cleaning is a privacy operation as well as a disk-space one.
//
// Both locations a run can write to are swept: the project `.aimesh/<component>/runs`, and the temp
// fallback `<os temp>/aimesh/<component>/runs` used when `init` has not been run.
//
// The sweep never touches:
//
//   - anything that is not a direct child of a resolved runs root (no recursion, no walking upward,
//     no globs);
//   - the runs root itself or its siblings, such as configuration and ledgers;
//   - a run modified within the grace period, since a run in progress may still be writing to it.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// CleanGrace is how recently a run directory may have been modified and still be spared, so a run that
// is still writing is not deleted.
const CleanGrace = 2 * time.Minute

// CleanSelector says which prior runs to remove. At most one field is set; a zero selector removes
// nothing and requests an inventory. There is no default retention policy.
type CleanSelector struct {
	// All removes every run outside the grace period.
	All bool
	// Keep retains the N most recent runs and removes the rest. 0 with All unset selects nothing.
	Keep int
	// OlderThan removes runs last modified longer ago than this. Zero means unset.
	OlderThan time.Duration
}

// Selective reports whether the caller actually asked for a removal.
func (s CleanSelector) Selective() bool { return s.All || s.Keep > 0 || s.OlderThan > 0 }

// RunDirInfo is one run directory as the sweep sees it.
type RunDirInfo struct {
	Path     string
	Name     string
	Modified time.Time
	Bytes    int64
	// Removed is true when this sweep deleted it; Skipped names why it was spared, and the two are
	// never both set. A dry run sets neither.
	Removed bool
	Skipped string
}

// CleanLocation is one runs root and what happened in it. It is present even when empty, so a caller
// can report that the location was checked.
type CleanLocation struct {
	Root      string
	Component string
	// Exists is false when the location has no runs directory at all; Runs is then empty.
	Exists bool
	Runs   []RunDirInfo
}

// Removed counts this location's deletions and the bytes they freed.
func (l CleanLocation) Removed() (int, int64) {
	n, bytes := 0, int64(0)
	for _, r := range l.Runs {
		if r.Removed {
			n++
			bytes += r.Bytes
		}
	}
	return n, bytes
}

// Total counts every run present, removed or not.
func (l CleanLocation) Total() (int, int64) {
	bytes := int64(0)
	for _, r := range l.Runs {
		bytes += r.Bytes
	}
	return len(l.Runs), bytes
}

// Skip reasons, as stable machine codes.
const (
	// SkipInProgress — modified within CleanGrace, so possibly still being written.
	SkipInProgress = "possibly_in_progress"
	// SkipRetained — the selector kept it (a --keep slot, or newer than --older-than).
	SkipRetained = "retained_by_selector"
	// SkipNotADirectory — an entry in the runs root that is not a run directory; it is reported, not
	// removed.
	SkipNotADirectory = "not_a_run_directory"
	// SkipRemoveFailed — deletion was attempted and failed (permissions, a file held open).
	SkipRemoveFailed = "remove_failed"
)

// CleanRuns sweeps the run directories of one component in one location. at is the reference time for
// the grace period and OlderThan, so one sweep judges every location against one instant. dryRun
// computes the same result without deleting anything.
func CleanRuns(root, component string, sel CleanSelector, dryRun bool, at time.Time) (CleanLocation, error) {
	loc := CleanLocation{Root: root, Component: component}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return loc, nil // not an error: this location simply has no runs
		}
		return loc, fmt.Errorf("read %s: %w", root, err)
	}
	loc.Exists = true
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if !e.IsDir() {
			loc.Runs = append(loc.Runs, RunDirInfo{Path: p, Name: e.Name(), Skipped: SkipNotADirectory})
			continue
		}
		info, ierr := e.Info()
		mod := time.Time{}
		if ierr == nil {
			mod = info.ModTime()
		}
		loc.Runs = append(loc.Runs, RunDirInfo{Path: p, Name: e.Name(), Modified: mod, Bytes: dirBytes(p)})
	}
	// Newest first: the order --keep counts in.
	sort.SliceStable(loc.Runs, func(i, j int) bool { return loc.Runs[i].Modified.After(loc.Runs[j].Modified) })

	kept := 0
	for i := range loc.Runs {
		r := &loc.Runs[i]
		if r.Skipped != "" {
			continue
		}
		if at.Sub(r.Modified) < CleanGrace {
			r.Skipped = SkipInProgress
			continue
		}
		if !selected(*r, sel, &kept, at) {
			r.Skipped = SkipRetained
			continue
		}
		if dryRun {
			continue
		}
		if err := os.RemoveAll(r.Path); err != nil {
			r.Skipped = SkipRemoveFailed
			continue
		}
		r.Removed = true
	}
	return loc, nil
}

// selected applies the selector to one run. kept is threaded so --keep counts across the whole
// (newest-first) listing rather than per call.
func selected(r RunDirInfo, sel CleanSelector, kept *int, at time.Time) bool {
	switch {
	case sel.All:
		return true
	case sel.Keep > 0:
		if *kept < sel.Keep {
			*kept++
			return false
		}
		return true
	case sel.OlderThan > 0:
		return at.Sub(r.Modified) > sel.OlderThan
	default:
		return false // no selector: an inventory, never a deletion
	}
}

// dirBytes sums a run directory's file sizes. Unreadable entries count as 0, since the size is only
// informational.
func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is skipped, never fatal
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// CleanTargets returns every runs root a component can write to: the project location first, then the
// temp fallback. Both are always returned, so a caller can report each.
func CleanTargets(component string) []string {
	var out []string
	if dir, ok := ComponentDir(component); ok {
		out = append(out, filepath.Join(dir, RunsSubdir))
	}
	out = append(out, filepath.Join(os.TempDir(), HomeDirName[1:], component, RunsSubdir))
	return out
}
