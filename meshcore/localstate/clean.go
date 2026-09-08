package localstate

// REMOVING PRIOR RUN ARTIFACTS.
//
// Run output accumulates, and it is not innocuous: each run directory holds the prompts, which embed
// VERBATIM COPIES of everything the models were shown — roughly one copy per seat, plus the same text
// again in a seat's stderr. So this is a disk-space command and a privacy command at once, and the
// second reading is the one that decides its rules.
//
// IT SWEEPS BOTH LOCATIONS. Where a run writes depends on whether `init` has been run in the tree:
// the project `.aimesh/<component>/runs`, else `<os temp>/aimesh/<component>/runs`. A clean that knew
// about only one would leave the other quietly accumulating — and it would be the temp one, which is
// where a user who never ran `init` has every artifact they have ever produced.
//
// WHAT IT WILL NOT TOUCH, enforced structurally rather than promised:
//
//   - Anything that is not a DIRECT CHILD of a resolved runs root. No recursion into a run to delete
//     part of it, no walking upward, no globs.
//   - The runs root itself, and every sibling of it — config, ledgers, containment copies. Only
//     `<root>/runs/*` is ever a candidate.
//   - A run younger than the grace period, because a review in progress is writing into its own run
//     directory right now and this command has no way to ask it whether it has finished.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// CleanGrace is how recently a run directory may have been modified and still be spared. It exists
// because a concurrently running review owns a directory this command can see but cannot ask about:
// the alternative to a grace period is deleting a live run's journal mid-write.
const CleanGrace = 2 * time.Minute

// CleanSelector says WHICH prior runs to remove. Exactly one field is set; a zero selector removes
// nothing and is how a caller asks for an inventory.
//
// There is deliberately no default policy. Retention is a judgement about someone else's data, and a
// command that guessed it would delete review artifacts nobody asked it to.
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

// CleanLocation is one runs root and what happened in it. Present even when empty, so a caller can
// see that a location was CONSIDERED — "nothing in temp" and "temp was never looked at" are different
// facts, and only the first is reassuring.
type CleanLocation struct {
	Root      string
	Component string
	// Exists is false when the location has no runs directory at all; Runs is then empty.
	Exists bool
	Runs   []RunDirInfo
}

// Removed / Freed tally this location's actual deletions.
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

// SkipReasons — stable machine codes.
const (
	// SkipInProgress — modified within CleanGrace. A run being written right now looks exactly like
	// a run that finished a second ago, so both are spared.
	SkipInProgress = "possibly_in_progress"
	// SkipRetained — the selector kept it (a --keep slot, or newer than --older-than).
	SkipRetained = "retained_by_selector"
	// SkipNotADirectory — an entry in the runs root that is not a run directory. It is REPORTED
	// rather than removed: this command deletes run directories, and something else being there is
	// a fact for the user, not a thing to tidy away.
	SkipNotADirectory = "not_a_run_directory"
	// SkipRemoveFailed — deletion was attempted and failed (permissions, a file held open).
	SkipRemoveFailed = "remove_failed"
)

// CleanRuns sweeps the run directories of one component in ONE location.
//
// `at` is the reference time for the grace period and for OlderThan, supplied rather than read so the
// behaviour is testable and so a single sweep judges every location against one instant.
//
// dryRun computes exactly the same answer and deletes nothing — the report a caller prints is
// therefore the same code path whether or not anything was removed, which is what keeps a preview
// honest about what the real run would do.
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
	// NEWEST FIRST — the order --keep counts in, and the order a human reads a listing in.
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

// dirBytes sums a run directory's file sizes. An unreadable entry contributes 0 rather than failing
// the sweep: the size is a courtesy for the report, and refusing to clean because one file could not
// be measured would be the tail wagging the dog.
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

// CleanTargets resolves EVERY runs root a component's output can be written to, in the order a
// reader should see them: the project location first (where an initialized tree writes), then the
// temp fallback (where an uninitialized one does).
//
// Both are always returned, present or not. The caller reports what it found at each, so "your temp
// fallback is empty" is a statement the command actually makes rather than an omission a user has to
// interpret.
func CleanTargets(component string) []string {
	var out []string
	if dir, ok := ComponentDir(component); ok {
		out = append(out, filepath.Join(dir, RunsSubdir))
	}
	out = append(out, filepath.Join(os.TempDir(), HomeDirName[1:], component, RunsSubdir))
	return out
}
