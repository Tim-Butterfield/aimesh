package rootfile

import (
	"runtime"
	"testing"
)

// `samePathPrefix` picks which authorized root a path is read THROUGH — the longest containing one.
// Its Windows half folds case, and until this test existed it never ran anywhere: the repository is
// not gated on Windows, so an inline platform check meant the branch was compiled and never executed.
//
// Getting it wrong is not cosmetic. Fold when you should not and a path under `/Root` is read through
// a handle on `/root`, a different directory on a case-sensitive filesystem. Fail to fold when you
// should and the boundary picker finds no containing root at all, silently falling back to the file's
// own parent — a weaker boundary than the one the human actually consented to.

func withFoldPaths(t *testing.T, v bool) {
	t.Helper()
	prev := foldPaths
	t.Cleanup(func() { foldPaths = prev })
	foldPaths = v
}

func TestSamePathPrefix_WindowsBranch_FoldsCase(t *testing.T) {
	withFoldPaths(t, true)
	if !samePathPrefix(`/Project/`, `/project/`) {
		t.Error("samePathPrefix must fold case on the Windows branch")
	}
	if samePathPrefix(`/project/`, `/other/`) {
		t.Error("folding must not make two different prefixes equal")
	}
}

func TestSamePathPrefix_UnixBranch_DoesNotFoldCase(t *testing.T) {
	withFoldPaths(t, false)
	if samePathPrefix(`/Project/`, `/project/`) {
		t.Error("samePathPrefix must NOT fold case off the Windows branch — the two prefixes name different directories on a case-sensitive filesystem")
	}
	if !samePathPrefix(`/project/`, `/project/`) {
		t.Error("an exact prefix must still match")
	}
}

func TestRootfileFoldPaths_DefaultsToThePlatform(t *testing.T) {
	if want := runtime.GOOS == "windows"; foldPaths != want {
		t.Fatalf("foldPaths = %v on %s, want %v", foldPaths, runtime.GOOS, want)
	}
}
