package workspace

import (
	"runtime"
	"testing"
)

// `samePath` is the last un-branch-tested Windows conditional in this package (stream aliasing and
// the reparse attribute are already substituted in hardening_test.go). It compares canonical paths on
// the write path — the check that a commit lands on exactly the destination the guard approved — so
// its Windows half is containment code that, on a repository not gated on Windows, would otherwise be
// compiled everywhere and executed nowhere.

func withFoldPaths(t *testing.T, v bool) {
	t.Helper()
	prev := foldPaths
	t.Cleanup(func() { foldPaths = prev })
	foldPaths = v
}

func TestSamePath_WindowsBranch_FoldsCase(t *testing.T) {
	withFoldPaths(t, true)
	if !samePath(`/Work/Copy/main.go`, `/work/copy/main.go`) {
		t.Error("samePath must fold case on the Windows branch — the two spellings name one file there, and refusing the write would be a false containment failure")
	}
	if samePath(`/work/copy/main.go`, `/work/copy/other.go`) {
		t.Error("folding must not make two different files the same file")
	}
}

func TestSamePath_UnixBranch_DoesNotFoldCase(t *testing.T) {
	withFoldPaths(t, false)
	if samePath(`/Work/Copy/main.go`, `/work/copy/main.go`) {
		t.Error("samePath must NOT fold case off the Windows branch — those are two files, and treating them as one would let a commit land somewhere the guard did not approve")
	}
}

func TestWorkspaceFoldPaths_DefaultsToThePlatform(t *testing.T) {
	if want := runtime.GOOS == "windows"; foldPaths != want {
		t.Fatalf("foldPaths = %v on %s, want %v", foldPaths, runtime.GOOS, want)
	}
}
