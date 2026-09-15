package acp

import (
	"runtime"
	"testing"
)

// Substituting windowsPaths exercises the case-folding branch of the over-broad-root rule on any
// platform. The drive-letter-agnostic match cannot be reached off Windows: filepath.VolumeName and
// filepath.Clean are chosen by build constraint, so on Unix a `D:\Windows` path never matches. That
// half is covered on Windows by TestMatchSystemRoot_WindowsIsDriveLetterAgnostic; these tests check
// that the Windows arm does not misclassify ordinary directories.

func withWindowsPaths(t *testing.T, v bool) {
	t.Helper()
	prev := windowsPaths
	t.Cleanup(func() { windowsPaths = prev })
	windowsPaths = v
}

func TestMatchSystemRoot_WindowsBranch_DoesNotMisclassifyAnOrdinaryDirectory(t *testing.T) {
	withWindowsPaths(t, true)
	// A project directory is not a system root.
	for _, p := range []string{`C:\code\project`, `D:\work\thing`, `/code/project`} {
		if got := matchSystemRoot(p); got != "" {
			t.Errorf("matchSystemRoot(%q) = %q; an ordinary project directory must not be refused as a system root", p, got)
		}
	}
}

func TestSameRootPath_WindowsBranch_FoldsCase(t *testing.T) {
	withWindowsPaths(t, true)
	// Use `/` separators: separator handling cannot be substituted, so a `\` fixture would test nothing
	// about Windows.
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
