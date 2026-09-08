package localstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := FindRoot(sub)
	if !ok {
		t.Fatal("expected to find the repo root from a subdir")
	}
	// resolve symlinks (macOS /var → /private/var) so the comparison is stable
	wantR, _ := filepath.EvalSymlinks(root)
	gotR, _ := filepath.EvalSymlinks(got)
	if gotR != wantR {
		t.Errorf("FindRoot = %q, want %q", gotR, wantR)
	}
}

func TestFindRoot_GitFile_Worktree(t *testing.T) {
	root := t.TempDir()
	// a .git FILE (worktree/submodule), not a directory
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindRoot(root); !ok {
		t.Error("a .git file (worktree) must still count as a repo root")
	}
}

func TestFindRoot_None(t *testing.T) {
	// a bare temp dir with no VCS marker up to the fs root — expect not found unless the temp path
	// happens to sit under a repo; guard by using a nested plain dir.
	dir := filepath.Join(t.TempDir(), "plain")
	_ = os.MkdirAll(dir, 0o755)
	// Not asserting !ok (the test host's temp may be under a repo); assert FindRoot doesn't panic.
	_, _ = FindRoot(dir)
}

func TestInit_Repo_ExcludesAndCreatesDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Init(root, InitRepo)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !res.Repo {
		t.Error("expected Repo=true")
	}
	if _, err := os.Stat(filepath.Join(root, HomeDirName, TempSubdir)); err != nil {
		t.Errorf(".aimesh/temp not created: %v", err)
	}
	excl, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !strings.Contains(string(excl), "/.aimesh/") {
		t.Errorf("exclude missing /.aimesh/: %q", excl)
	}
	// idempotent: a second Init must not duplicate the line
	if _, err := Init(root, InitRepo); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	excl2, _ := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if strings.Count(string(excl2), "/.aimesh/") != 1 {
		t.Errorf("exclude line duplicated: %q", excl2)
	}
}

func TestInit_Folder_NoVCSIgnore(t *testing.T) {
	dir := t.TempDir() // no .git
	res, err := Init(dir, InitFolder)
	if err != nil {
		t.Fatalf("Init folder: %v", err)
	}
	if res.Repo || res.Excluded != "" {
		t.Errorf("folder init must not touch VCS: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, HomeDirName, TempSubdir)); err != nil {
		t.Errorf(".aimesh/temp not created: %v", err)
	}
}

func TestInit_ModeMismatch(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	if _, err := Init(root, InitFolder); err != ErrInsideRepo {
		t.Errorf("folder init inside a repo must error ErrInsideRepo, got %v", err)
	}
	bare := filepath.Join(t.TempDir(), "nested", "plain")
	_ = os.MkdirAll(bare, 0o755)
	if _, ok := FindRoot(bare); !ok { // only assert if genuinely not in a repo
		if _, err := Init(bare, InitRepo); err != ErrRepoNotFound {
			t.Errorf("repo init outside a repo must error ErrRepoNotFound, got %v", err)
		}
	}
}
