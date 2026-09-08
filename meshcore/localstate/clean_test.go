package localstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// runsFixture builds a runs root holding named directories with given ages, plus one stray file.
func runsFixture(t *testing.T, ages map[string]time.Duration, now time.Time) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, age := range ages {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run-state.json"), []byte(`{"x":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
		when := now.Add(-age)
		if err := os.Chtimes(dir, when, when); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestClean_NoSelectorRemovesNothing. Retention is a judgement about someone else's data: the run
// directory holds that run's findings, decisions, patch and — after an apply — its undo. A default
// policy would delete on a rule the user never stated.
func TestClean_NoSelectorRemovesNothing(t *testing.T) {
	now := time.Now()
	root := runsFixture(t, map[string]time.Duration{"a": 100 * time.Hour, "b": 200 * time.Hour}, now)
	loc, err := CleanRuns(root, "review", CleanSelector{}, false, now)
	if err != nil {
		t.Fatalf("CleanRuns: %v", err)
	}
	if n, _ := loc.Removed(); n != 0 {
		t.Errorf("removed %d run(s) with no selector — it must remove nothing", n)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("run %q was deleted: %v", name, err)
		}
	}
}

// TestClean_SparesARunInProgress is the one that protects a concurrent review: a directory being
// written right now is indistinguishable from one that finished a second ago, so both are spared.
func TestClean_SparesARunInProgress(t *testing.T) {
	now := time.Now()
	root := runsFixture(t, map[string]time.Duration{
		"live": 5 * time.Second, // inside the grace period
		"old":  100 * time.Hour, // outside it
	}, now)
	loc, err := CleanRuns(root, "review", CleanSelector{All: true}, false, now)
	if err != nil {
		t.Fatalf("CleanRuns: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "live")); err != nil {
		t.Error("a run modified seconds ago was deleted — it may have been mid-write")
	}
	if _, err := os.Stat(filepath.Join(root, "old")); !os.IsNotExist(err) {
		t.Error("--all did not remove a run well outside the grace period")
	}
	var sawSkip bool
	for _, r := range loc.Runs {
		if r.Name == "live" && r.Skipped == SkipInProgress {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Error("the spared run is not reported as possibly in progress — a silent skip is not a report")
	}
}

// TestClean_KeepCountsNewestFirst: --keep 1 retains the most recent, not an arbitrary one.
func TestClean_KeepCountsNewestFirst(t *testing.T) {
	now := time.Now()
	root := runsFixture(t, map[string]time.Duration{
		"newest": 10 * time.Minute,
		"middle": 50 * time.Hour,
		"oldest": 500 * time.Hour,
	}, now)
	if _, err := CleanRuns(root, "review", CleanSelector{Keep: 1}, false, now); err != nil {
		t.Fatalf("CleanRuns: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "newest")); err != nil {
		t.Error("--keep 1 removed the most recent run")
	}
	for _, gone := range []string{"middle", "oldest"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Errorf("--keep 1 retained %q", gone)
		}
	}
}

// TestClean_OlderThanIsAThreshold, and the boundary is strictly greater — a run exactly at the
// threshold is kept, so a repeated command is not quietly age-dependent at the edge.
func TestClean_OlderThanIsAThreshold(t *testing.T) {
	now := time.Now()
	root := runsFixture(t, map[string]time.Duration{
		"young": 10 * time.Hour,
		"old":   100 * time.Hour,
	}, now)
	if _, err := CleanRuns(root, "review", CleanSelector{OlderThan: 48 * time.Hour}, false, now); err != nil {
		t.Fatalf("CleanRuns: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "young")); err != nil {
		t.Error("a run younger than the threshold was removed")
	}
	if _, err := os.Stat(filepath.Join(root, "old")); !os.IsNotExist(err) {
		t.Error("a run older than the threshold survived")
	}
}

// TestClean_DryRunDeletesNothingAndAnswersTheSame is what makes a preview trustworthy: the same code
// path decides, so what it lists is what the real command removes.
func TestClean_DryRunDeletesNothingAndAnswersTheSame(t *testing.T) {
	now := time.Now()
	ages := map[string]time.Duration{"a": 100 * time.Hour, "b": 200 * time.Hour, "c": 10 * time.Second}
	root := runsFixture(t, ages, now)

	preview, err := CleanRuns(root, "review", CleanSelector{All: true}, true, now)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for name := range ages {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("a dry run deleted %q", name)
		}
	}
	// The dry run marks candidates by leaving them unremoved AND unskipped.
	candidates := map[string]bool{}
	for _, r := range preview.Runs {
		if !r.Removed && r.Skipped == "" {
			candidates[r.Name] = true
		}
	}
	real, err := CleanRuns(root, "review", CleanSelector{All: true}, false, now)
	if err != nil {
		t.Fatalf("real run: %v", err)
	}
	removed := map[string]bool{}
	for _, r := range real.Runs {
		if r.Removed {
			removed[r.Name] = true
		}
	}
	if len(candidates) != len(removed) {
		t.Fatalf("preview listed %v, the real run removed %v", candidates, removed)
	}
	for name := range candidates {
		if !removed[name] {
			t.Errorf("preview said %q would go; the real run kept it", name)
		}
	}
}

// TestClean_TouchesOnlyDirectChildren: a file sitting in the runs root is reported, never removed —
// this command deletes run directories, and something else being there is a fact for the user.
func TestClean_TouchesOnlyDirectChildren(t *testing.T) {
	now := time.Now()
	root := runsFixture(t, map[string]time.Duration{"a": 100 * time.Hour}, now)
	stray := filepath.Join(root, "NOTES.txt")
	if err := os.WriteFile(stray, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loc, err := CleanRuns(root, "review", CleanSelector{All: true}, false, now)
	if err != nil {
		t.Fatalf("CleanRuns: %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Error("a non-run file in the runs root was deleted")
	}
	if _, err := os.Stat(root); err != nil {
		t.Error("the runs root itself was deleted")
	}
	var reported bool
	for _, r := range loc.Runs {
		if r.Name == "NOTES.txt" && r.Skipped == SkipNotADirectory {
			reported = true
		}
	}
	if !reported {
		t.Error("the stray entry was neither removed nor reported")
	}
}

// TestClean_MissingLocationIsNotAnError: the temp fallback frequently does not exist, and a command
// that failed on it would be unusable on exactly the machines it matters most for.
func TestClean_MissingLocationIsNotAnError(t *testing.T) {
	loc, err := CleanRuns(filepath.Join(t.TempDir(), "nope", "runs"), "review", CleanSelector{All: true}, false, time.Now())
	if err != nil {
		t.Fatalf("a missing runs directory must not be an error: %v", err)
	}
	if loc.Exists {
		t.Error("a missing location reported Exists = true")
	}
}

// TestCleanTargets_AlwaysIncludesTheTempFallback. Output goes to the project directory when init has
// been run and to temp when it has not, so a clean that knew only the first would leave every
// artifact of every uninitialized run in place — the case where the user can least easily find them.
func TestCleanTargets_AlwaysIncludesTheTempFallback(t *testing.T) {
	targets := CleanTargets("review")
	if len(targets) == 0 {
		t.Fatal("no targets resolved")
	}
	last := targets[len(targets)-1]
	if !filepath.IsAbs(last) {
		t.Errorf("target %q is not absolute", last)
	}
	if filepath.Base(last) != RunsSubdir {
		t.Errorf("target %q does not end in the runs subdirectory", last)
	}
	if !contains(last, os.TempDir()) {
		t.Errorf("the last target %q is not under the OS temp directory — the fallback would go unswept", last)
	}
}

func contains(path, prefix string) bool {
	rel, err := filepath.Rel(prefix, path)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !hasDotDotPrefix(rel)
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}
