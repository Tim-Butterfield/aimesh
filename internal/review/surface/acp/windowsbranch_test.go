package acp

import (
	"runtime"
	"testing"
)

// The over-broad-root refusal is the rule that stops `--root /`, `--root ~`, `--root C:\Windows` from
// being a way around trusted-root confinement — and its WINDOWS half is the half nobody here can run.
// It is also the half a comment in roots.go already records as having been wrong once: the list
// hardcoded `C:`, so `D:\Windows` on any machine with a second drive passed straight through.
//
// Substituting `windowsPaths` exercises the CASE-FOLDING branch on any platform. What it cannot reach
// is stated here rather than left as a passing test that proves nothing:
//
// `matchSystemRoot`'s Windows arm strips the volume with `filepath.VolumeName` and compares against
// `\`-separated entries with `filepath.Clean`. Both come from `path/filepath`, which is selected by
// BUILD CONSTRAINT and is not substitutable — on unix `VolumeName("D:\\Windows")` is `""` and
// `Clean` leaves the backslashes as a single opaque component, so the comparison cannot succeed no
// matter what `windowsPaths` says. The drive-letter-agnostic rule (the one whose earlier hardcoded
// `C:` let `D:\Windows` through) is therefore **verifiable only on Windows**, and this repository does
// not run there. It is recorded as an uncovered path in docs/security.md's platform matrix.
//
// What IS covered below is that entering the Windows arm does not misclassify: an ordinary directory
// is still not a system root.

func withWindowsPaths(t *testing.T, v bool) {
	t.Helper()
	prev := windowsPaths
	t.Cleanup(func() { windowsPaths = prev })
	windowsPaths = v
}

func TestMatchSystemRoot_WindowsBranch_DoesNotMisclassifyAnOrdinaryDirectory(t *testing.T) {
	withWindowsPaths(t, true)
	// A project directory is not a system root. This is the half of the branch that can be asserted
	// on a non-Windows filepath; the positive half cannot (see the note above).
	for _, p := range []string{`C:\code\project`, `D:\work\thing`, `/code/project`} {
		if got := matchSystemRoot(p); got != "" {
			t.Errorf("matchSystemRoot(%q) = %q; an ordinary project directory must not be refused as a system root", p, got)
		}
	}
}

func TestSameRootPath_WindowsBranch_FoldsCase(t *testing.T) {
	withWindowsPaths(t, true)
	// `/`-separated on purpose: the separator half is not substitutable (see the note above), so a
	// `\`-separated fixture would test the string comparison and nothing about Windows.
	if !sameRootPath(`/Code/Project`, `/code/project`) {
		t.Error("sameRootPath must fold case on the Windows branch")
	}
	withWindowsPaths(t, false)
	if sameRootPath(`/Code/Project`, `/code/project`) {
		t.Error("sameRootPath must NOT fold case off the Windows branch — those are two directories on a case-sensitive filesystem")
	}
}

func TestWindowsPaths_DefaultsToThePlatform(t *testing.T) {
	if want := runtime.GOOS == "windows"; windowsPaths != want {
		t.Fatalf("windowsPaths = %v on %s, want %v", windowsPaths, runtime.GOOS, want)
	}
}
