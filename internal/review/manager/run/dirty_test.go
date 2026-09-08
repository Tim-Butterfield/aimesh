package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// gitRepo turns a directory into a git repository with one committed file, or skips.
func gitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.test"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "-q", "--no-gpg-sign", "-m", "base"},
	} {
		c := exec.Command("git", args...)
		c.Dir, c.Env = dir, model.CleanGitEnv()
		if out, err := c.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", args, err, out)
		}
	}
}

// TestProbeWorkspaceDirtiness_AnswersOnlyWhenItCan covers the three states, and the third is the one
// that decides the whole design: a tree with no VCS returns UNKNOWN, not clean. Treating "no answer"
// as "clean" would silently drop the protection for exactly the trees that have no other one.
func TestProbeWorkspaceDirtiness_AnswersOnlyWhenItCan(t *testing.T) {
	t.Run("no VCS is unknown, never clean", func(t *testing.T) {
		d := ProbeWorkspaceDirtiness(context.Background(), t.TempDir())
		if d.Known {
			t.Fatalf("a plain folder must be UNKNOWN, got %+v", d)
		}
		if !strings.Contains(d.Summary(), "no version control") {
			t.Errorf("the summary must say why there is no answer: %q", d.Summary())
		}
	})

	t.Run("a clean repository", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRepo(t, dir)
		d := ProbeWorkspaceDirtiness(context.Background(), dir)
		if !d.Known || d.Dirty {
			t.Fatalf("a committed tree must be known-clean, got %+v", d)
		}
	})

	t.Run("a dirty repository names its files", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRepo(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a // edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		d := ProbeWorkspaceDirtiness(context.Background(), dir)
		if !d.Known || !d.Dirty {
			t.Fatalf("an edited tracked file must read as dirty, got %+v", d)
		}
		if len(d.Paths) == 0 || !strings.Contains(strings.Join(d.Paths, " "), "a.go") {
			t.Errorf("the refusal must NAME the files, or an operator cannot recognize the tree: %+v", d.Paths)
		}
	})

	// UNTRACKED FILES ARE NOT DIRTY. Nothing this tool writes touches them and `git checkout -- .`
	// does not remove them, so they cannot be entangled. Counting them would refuse an apply in
	// every repository with a stray build artifact — which is most of them — and a precondition that
	// fires constantly is one an operator learns to override by reflex.
	t.Run("an untracked file is not dirty", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRepo(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "scratch.tmp"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if d := ProbeWorkspaceDirtiness(context.Background(), dir); d.Dirty {
			t.Errorf("an untracked file made the tree read as dirty: %+v", d)
		}
	})
}

// TestApply_ADirtyTreeIsRecordedNotRefused.
//
// This used to be a refusal. It is not any more: working on a tree with uncommitted changes is the
// NORMAL state of the work, not an anomaly, and a gate that fires on the normal case is a flag every
// single invocation has to carry — which is friction that stops people using the tool at all.
//
// The harm the refusal cited was real but overstated. Our edits and the user's do end up in the same
// tree, so the BLUNT undo (`git checkout -- .`) takes both. The precise undo has always existed and
// is produced on this very path: `changes.patch` is a complete reverse-appliable delta of exactly
// what this run wrote. So the fact is worth RECORDING and not worth refusing over.
func TestApply_ADirtyTreeIsRecordedNotRefused(t *testing.T) {
	m, ws := remediateFixture(t, true)
	gitRepo(t, ws)
	// Uncommitted work in a file this run does NOT target — the case the base-hash pins cannot see,
	// and the exact case the old refusal existed for.
	bystander := filepath.Join(ws, "my-unfinished-work.go")
	if err := os.WriteFile(bystander, []byte("package a // mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "add", "-A")
	c.Dir, c.Env = ws, model.CleanGitEnv()
	_ = c.Run()

	req := baseRequest(t, ws, review.ModeApply)
	var events []string
	req.OnEvent = func(ev audit.EventLine) { events = append(events, ev.EventType) }

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("an apply into a dirty tree must proceed: %v", err)
	}
	if !out.Receipt.Committed {
		t.Errorf("the apply did not commit: %+v", out.Receipt)
	}
	// The user's unrelated work is untouched — the write path never had any reason to touch it, and
	// that is why the tree's cleanliness was never the right thing to gate on.
	if b, _ := os.ReadFile(bystander); string(b) != "package a // mine\n" {
		t.Errorf("the user's uncommitted file was modified: %q", string(b))
	}
	// The dirtiness is still RECORDED, so a run read later says the tree was not clean.
	if !slices.Contains(events, "workspace_dirty") {
		t.Errorf("a dirty tree must still be recorded as an event, even though it is not refused: %v", events)
	}
}

// TestApply_APlainFolderIsNotRefused. `folder init` is a supported shape, and a plain folder has no
// uncommitted state to reason about — refusing every non-repo apply would break it on the strength of
// a question that cannot be asked there.
func TestApply_APlainFolderIsNotRefused(t *testing.T) {
	m, ws := remediateFixture(t, true)
	out, err := m.Remediate(context.Background(), baseRequest(t, ws, review.ModeApply))
	if err != nil {
		t.Fatalf("a tree with no VCS must not be refused: %v", err)
	}
	if !out.Receipt.Committed {
		t.Errorf("receipt = %+v, want committed", out.Receipt)
	}
}

// TestEnsureDurableRunDir_OnlyForTheTreeWithNoOtherWayBack.
//
// The tree with no VCS and no `init` is simultaneously the one whose only undo is the run's patch and
// the one whose run directory defaults to OS temp. Creating the state directory for it is the one
// case where writing unasked is right — we are already about to write into this tree by explicit
// instruction, and declining to also write the means of undoing that is restraint aimed at the wrong
// thing.
func TestEnsureDurableRunDir_OnlyForTheTreeWithNoOtherWayBack(t *testing.T) {
	t.Run("no VCS and no init: the state dir is created", func(t *testing.T) {
		ws := t.TempDir()
		// The "no init" half of the premise is judged from the process cwd (localstate.ProjectHome
		// walks up from it), so run from the workspace itself: otherwise an `.aimesh/` at the root of
		// THIS repository — created by any `aimesh init` or apply run made here — satisfies the lookup
		// and the test fails for a reason that has nothing to do with the code under test.
		t.Chdir(ws)
		got, ok := EnsureDurableRunDir(context.Background(), ws, "apply", localstate.RunDir(reviewComponent, ""))
		if !ok {
			t.Fatal("the tree with no other way back must get a durable run directory")
		}
		want := filepath.Join(ws, localstate.HomeDirName, reviewComponent, localstate.RunsSubdir)
		if got != want {
			t.Errorf("dir = %q, want %q", got, want)
		}
		if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
			t.Errorf("the directory was not created: %v", err)
		}
	})

	t.Run("a repository is left alone", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRepo(t, ws)
		if _, ok := EnsureDurableRunDir(context.Background(), ws, "apply", localstate.RunDir(reviewComponent, "")); ok {
			t.Error("a repository already has a way back; nothing should be created")
		}
		if _, err := os.Stat(filepath.Join(ws, localstate.HomeDirName)); !os.IsNotExist(err) {
			t.Error("a state directory was created in a repository that did not ask for one")
		}
	})

	t.Run("a report run is left alone", func(t *testing.T) {
		ws := t.TempDir()
		if _, ok := EnsureDurableRunDir(context.Background(), ws, "report", localstate.RunDir(reviewComponent, "")); ok {
			t.Error("a run that writes nothing needs no undo, so it needs no state directory")
		}
	})
}
