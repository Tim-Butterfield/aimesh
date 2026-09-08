package mcp

import (
	"runtime"
	"testing"
)

// The root-intersection rule has a WINDOWS BRANCH — path comparison folds case there, because the
// filesystem itself does — and on a unix machine that branch is compiled and never executed. Until
// `foldRootPaths` became a substitutable var it could not be executed anywhere but on Windows, which for
// this repository means "not executed at all": Windows is not part of the standard gate and has never
// been run against a real CLI (see docs/security.md's platform matrix, and the root README's stated
// limitations).
//
// Substituting the variable is the same pattern `streamAliasing` and `reparseAttr` already use, and it
// covers exactly the same thing they do: the CASE-FOLDING half of the platform rule. What it cannot cover
// is stated plainly rather than implied —
//
//   - the paths below use `/`, because the SEPARATOR half comes from `path/filepath`, which is chosen at
//     compile time and is not substitutable. A `\`-separated path is one opaque component on unix, so a
//     backslash fixture here would test nothing about Windows and would merely fail;
//   - none of it proves the guarantee on a real NTFS volume. Nothing that runs on this machine can. See
//     docs/security.md's platform matrix for what is verified where.

func withFoldRootPaths(t *testing.T, v bool) {
	t.Helper()
	prev := foldRootPaths
	t.Cleanup(func() { foldRootPaths = prev })
	foldRootPaths = v
}

func TestIntersectRoots_WindowsBranch_FoldsCase(t *testing.T) {
	withFoldRootPaths(t, true)

	// A client root that differs from the server's only in CASE names the SAME directory on Windows. A
	// comparison that missed it would compute an EMPTY intersection and fail closed on a client that
	// merely spelled the path differently — a refusal with no cause behind it.
	got := intersectRoots([]string{"/Proj"}, []string{"/proj/Sub"})
	if len(got) != 1 || got[0] != "/proj/Sub" {
		t.Fatalf("intersectRoots (Windows branch) = %v, want the client's narrower root", got)
	}
	// And the reverse, which is the half that must never slip: a case-differing WIDER client root
	// leaves the SERVER's own narrower root in force. Case folding makes the pair comparable; it must
	// not make the client's wider root win.
	got = intersectRoots([]string{"/Proj/Sub"}, []string{"/proj"})
	if len(got) != 1 || got[0] != "/Proj/Sub" {
		t.Fatalf("intersectRoots (Windows branch, wider client) = %v, want the SERVER's narrower root — a client never widens", got)
	}
	if !underRoot("/Proj/Sub/file", "/proj") {
		t.Error("underRoot must fold case on the Windows branch")
	}
	if !sameRoot("/Proj", "/proj") {
		t.Error("sameRoot must fold case on the Windows branch")
	}
}

func TestIntersectRoots_UnixBranch_DoesNotFoldCase(t *testing.T) {
	withFoldRootPaths(t, false)

	// On a case-sensitive filesystem `/proj` and `/PROJ` are two directories, and treating them as one
	// would be the widening the rule forbids.
	if got := intersectRoots([]string{"/proj"}, []string{"/PROJ/sub"}); len(got) != 0 {
		t.Fatalf("intersectRoots (unix branch) = %v, want nothing — disjoint paths must fail closed", got)
	}
	if underRoot("/PROJ/sub", "/proj") {
		t.Error("underRoot must NOT fold case off the Windows branch")
	}
	if sameRoot("/Proj", "/proj") {
		t.Error("sameRoot must NOT fold case off the Windows branch")
	}
}

// The var must AGREE with the platform it is compiled for; a substitutable seam that defaulted wrongly
// would make every other test in this file prove the wrong thing.
func TestFoldRootPaths_DefaultsToThePlatform(t *testing.T) {
	if want := runtime.GOOS == "windows"; foldRootPaths != want {
		t.Fatalf("foldRootPaths = %v on %s, want %v", foldRootPaths, runtime.GOOS, want)
	}
}
