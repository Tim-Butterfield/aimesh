package run

import (
	"runtime"
	"testing"
)

// `sameCanonicalPath` is the path half of the reviewed-root IDENTITY check: before a remediation opens
// its write window it re-resolves the workspace and requires it to be the tree the review judged. The
// device+inode half is platform-independent; this half folds case only on Windows, and until now that
// branch was compiled on every machine and executed on none.
//
// Both directions are load-bearing. Failing to fold on Windows would halt a legitimate remediation
// because the operator typed `c:\proj` where the review recorded `C:\Proj`; folding off Windows would
// accept `/Proj` for `/proj`, which are two trees — and accepting a substitute tree is exactly what
// this check exists to prevent.

func withReviewFoldPaths(t *testing.T, v bool) {
	t.Helper()
	prev := foldPaths
	t.Cleanup(func() { foldPaths = prev })
	foldPaths = v
}

func TestSameCanonicalPath_WindowsBranch_FoldsCase(t *testing.T) {
	withReviewFoldPaths(t, true)
	if !sameCanonicalPath(`/Proj/service`, `/proj/service`) {
		t.Error("sameCanonicalPath must fold case on the Windows branch")
	}
	if sameCanonicalPath(`/proj/service`, `/proj/other`) {
		t.Error("folding must not make two different trees the same tree")
	}
}

func TestSameCanonicalPath_UnixBranch_DoesNotFoldCase(t *testing.T) {
	withReviewFoldPaths(t, false)
	if sameCanonicalPath(`/Proj/service`, `/proj/service`) {
		t.Error("sameCanonicalPath must NOT fold case off the Windows branch — accepting a differently-cased path would accept a different tree than the one that was reviewed")
	}
}

func TestReviewFoldPaths_DefaultsToThePlatform(t *testing.T) {
	if want := runtime.GOOS == "windows"; foldPaths != want {
		t.Fatalf("foldPaths = %v on %s, want %v", foldPaths, runtime.GOOS, want)
	}
}
