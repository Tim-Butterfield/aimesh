package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ReadUnder is the exported form of the rule this package already applies to every containment read. It
// exists because reading a file back OUT of a directory this process wrote — a run record's artifacts,
// served to a client over a protocol — is the same act as reading one into a prompt, and a second
// implementation of "carefully read a file under a root" is how two code paths that both look careful end
// up disagreeing.

func writeFixture(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
}

func TestReadUnder_ReadsARegularFileUnderItsRoot(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "patches/changes.patch", "diff --git a b\n")
	b, err := ReadUnder(dir, "patches/changes.patch", 0)
	if err != nil {
		t.Fatalf("ReadUnder: %v", err)
	}
	if string(b) != "diff --git a b\n" {
		t.Fatalf("ReadUnder returned %q", b)
	}
}

func TestReadUnder_RefusesAPathThatLeavesItsRoot(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, rel := range []string{"../secret.txt", "a/../../secret.txt", outside} {
		b, err := ReadUnder(dir, rel, 0)
		if err == nil {
			t.Fatalf("ReadUnder(%q) succeeded and returned %q", rel, b)
		}
		if strings.Contains(string(b), "PRIVATE") {
			t.Fatalf("ReadUnder(%q) returned content from outside the root", rel)
		}
	}
}

// A symlink is never followed — including one that points at something perfectly innocuous, because the
// rule is about the mechanism and not about where any particular link happens to go.
func TestReadUnder_RefusesASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows; the reparse rule is covered by hardening_test.go's substituted attribute probe")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "real.txt")
	if err := os.WriteFile(target, []byte("linked"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := ReadUnder(dir, "link.txt", 0); err == nil {
		t.Fatal("ReadUnder followed a symlink")
	} else if ReasonOf(err) != ReasonReparse {
		t.Fatalf("reason = %q, want %q", ReasonOf(err), ReasonReparse)
	}
}

// Oversize is a REFUSAL, not a truncation. A caller that asked for a whole artifact and silently received
// part of one has no way to tell the difference — and a truncated patch that still parses reads as
// complete, which is the whole reason the receipt links instead of inlining.
func TestReadUnder_RefusesAnOversizedArtifactRatherThanTruncatingIt(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "big.patch", strings.Repeat("x", 4096))
	b, err := ReadUnder(dir, "big.patch", 1024)
	if err == nil {
		t.Fatalf("ReadUnder returned %d bytes for a file over the limit", len(b))
	}
	if ReasonOf(err) != ReasonTooLarge {
		t.Fatalf("reason = %q, want %q", ReasonOf(err), ReasonTooLarge)
	}
	if len(b) != 0 {
		t.Fatalf("a refused read must return no bytes at all, got %d", len(b))
	}
}

func TestReadUnder_RefusesADirectoryAndAnEmptyRoot(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "sub/x.txt", "x")
	if _, err := ReadUnder(dir, "sub", 0); err == nil {
		t.Fatal("ReadUnder returned a directory as file content")
	}
	if _, err := ReadUnder("", "x.txt", 0); err == nil {
		t.Fatal("ReadUnder with no root succeeded")
	}
}

func TestStatUnder_ReportsSizeWithoutReading(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "changes.patch", "abcdef")
	n, err := StatUnder(dir, "changes.patch")
	if err != nil {
		t.Fatalf("StatUnder: %v", err)
	}
	if n != 6 {
		t.Fatalf("StatUnder = %d, want 6", n)
	}
	if _, err := StatUnder(dir, "../escape"); err == nil {
		t.Fatal("StatUnder accepted a path that leaves its root")
	}
}
