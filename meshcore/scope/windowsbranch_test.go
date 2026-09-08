package scope

import (
	"runtime"
	"testing"
)

// Case folding is the second Windows-conditional rule in this package (the first, NTFS stream
// aliasing, is covered in denylist_test.go by substituting `streamAliasing`). It had no branch test
// at all: `foldPaths` was a var, but nothing ever set it, so on every machine this repository is
// gated on, `equalPath` and `hasPrefixPath` only ever ran their unix half.
//
// That matters here more than almost anywhere, because these two functions decide CONTAINMENT. Their
// asymmetry is deliberate and is what the table below pins: folding on Windows prevents a false
// refusal (the filesystem really is case-insensitive), and NOT folding elsewhere prevents a false
// admission (`/ROOT/x` and `/root/x` are two directories on a case-sensitive filesystem, and treating
// them as one would widen the root).
//
// It proves the DECISION, not the filesystem. Nothing here has run on NTFS; see docs/security.md's
// platform matrix.

func withFoldPaths(t *testing.T, v bool) {
	t.Helper()
	prev := foldPaths
	t.Cleanup(func() { foldPaths = prev })
	foldPaths = v
}

func TestPathComparison_WindowsBranch_FoldsCase(t *testing.T) {
	withFoldPaths(t, true)
	if !equalPath(`/Root/Project`, `/root/project`) {
		t.Error("equalPath must fold case on the Windows branch — an exact comparison would refuse a path that names the very directory that was authorized")
	}
	if !hasPrefixPath(`/Root/Project/src/main.go`, `/root/project/`) {
		t.Error("hasPrefixPath must fold case on the Windows branch")
	}
	// Folding must not make a SHORTER path match a longer prefix.
	if hasPrefixPath(`/root`, `/root/project/`) {
		t.Error("hasPrefixPath must still refuse a path shorter than the prefix")
	}
}

func TestPathComparison_UnixBranch_DoesNotFoldCase(t *testing.T) {
	withFoldPaths(t, false)
	if equalPath(`/Root/Project`, `/root/project`) {
		t.Error("equalPath must NOT fold case off the Windows branch — on a case-sensitive filesystem those are two directories, and admitting one for the other widens the root")
	}
	if hasPrefixPath(`/Root/Project/src/main.go`, `/root/project/`) {
		t.Error("hasPrefixPath must NOT fold case off the Windows branch")
	}
	if !equalPath(`/root/project`, `/root/project`) {
		t.Error("an exact match must still match")
	}
}

// The seam must agree with the platform it is compiled for, or every test above proves the wrong thing.
func TestFoldPaths_DefaultsToThePlatform(t *testing.T) {
	if want := runtime.GOOS == "windows"; foldPaths != want {
		t.Fatalf("foldPaths = %v on %s, want %v", foldPaths, runtime.GOOS, want)
	}
}
