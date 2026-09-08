package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

// A caller with a DECISION WINDOW — read the file, decide, write it back — cannot be protected by
// identity re-verification alone: an editor that saves in place keeps the inode, so os.SameFile is
// satisfied by a file whose every byte changed. These tests pin the content half.

func pinFixture(t *testing.T) (*Access, *Handle, string) {
	t.Helper()
	live := t.TempDir()
	target := filepath.Join(live, "a.txt")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	a := New(t.TempDir())
	h, err := a.Copy(live, false, "pin")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	t.Cleanup(func() { a.Cleanup(h) })
	if err := os.WriteFile(filepath.Join(h.Root, "a.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatalf("stage: %v", err)
	}
	return a, h, target
}

// TestCommitExpecting_RefusesAnInPlaceConcurrentEdit is the case os.SameFile cannot see.
func TestCommitExpecting_RefusesAnInPlaceConcurrentEdit(t *testing.T) {
	a, h, target := pinFixture(t)
	pins := map[string]string{"a.txt": ContentPin([]byte("original\n"))}

	// A concurrent in-place save: same inode, different bytes.
	f, err := os.OpenFile(target, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteAt([]byte("concurrent!\n"), 0); err != nil {
		t.Fatalf("in-place write: %v", err)
	}
	f.Close()

	_, cerr := a.CommitExpecting(h, pins)
	if cerr == nil {
		t.Fatal("a commit whose destination content changed must be refused")
	}
	if got := ReasonOf(cerr); got != ReasonDestinationContentChanged {
		t.Fatalf("reason = %q, want %q (err=%v)", got, ReasonDestinationContentChanged, cerr)
	}
	body, _ := os.ReadFile(target)
	if string(body) != "concurrent!\n" {
		t.Fatalf("the concurrent edit was overwritten: %q", body)
	}
	// The unpinned Commit is the historical contract, and it is what the refusal above is worth:
	// with no pins, the same commit silently discards the concurrent edit.
	if _, err := a.Commit(h); err != nil {
		t.Fatalf("unpinned commit: %v", err)
	}
	if body, _ := os.ReadFile(target); string(body) != "staged\n" {
		t.Fatalf("unpinned commit did not write: %q", body)
	}
}

// TestCommitExpecting_RefusesAnUnpinnedDestination: a pin set is EXHAUSTIVE. A destination the
// caller recorded nothing about is a write whose "what was I replacing" is unanswerable.
func TestCommitExpecting_RefusesAnUnpinnedDestination(t *testing.T) {
	a, h, target := pinFixture(t)
	_, cerr := a.CommitExpecting(h, map[string]string{"somethingelse.txt": ContentPin(nil)})
	if got := ReasonOf(cerr); got != ReasonDestinationUnpinned {
		t.Fatalf("reason = %q, want %q (err=%v)", got, ReasonDestinationUnpinned, cerr)
	}
	if body, _ := os.ReadFile(target); string(body) != "original\n" {
		t.Fatalf("an unpinned destination was written: %q", body)
	}
}

// TestCommitExpecting_MatchingPinsWrite: the pin is a guard, not an obstacle.
func TestCommitExpecting_MatchingPinsWrite(t *testing.T) {
	a, h, target := pinFixture(t)
	pins := map[string]string{"a.txt": ContentPin([]byte("original\n"))}
	committed, err := a.CommitExpecting(h, pins)
	if err != nil {
		t.Fatalf("a matching pin must not refuse: %v", err)
	}
	if len(committed) != 1 || committed[0] != "a.txt" {
		t.Fatalf("committed = %v, want [a.txt]", committed)
	}
	if body, _ := os.ReadFile(target); string(body) != "staged\n" {
		t.Fatalf("commit did not write: %q", body)
	}
}

// TestContentPin_AbsenceIsAValue: a file that appeared after the pin was taken is a mismatch like
// any other, not an untracked key.
func TestContentPin_AbsenceIsAValue(t *testing.T) {
	live := t.TempDir()
	a := New(t.TempDir())
	h, err := a.Copy(live, false, "pin")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	t.Cleanup(func() { a.Cleanup(h) })
	if err := os.WriteFile(filepath.Join(h.Root, "new.txt"), []byte("created by the write\n"), 0o644); err != nil {
		t.Fatalf("stage: %v", err)
	}
	// Pinned as absent and still absent: the create is allowed.
	if _, cerr := a.CommitExpecting(h, map[string]string{"new.txt": ContentAbsent}); cerr != nil {
		t.Fatalf("creating a file pinned as absent must be allowed: %v", cerr)
	}
	// Now it exists, so the same pin is a mismatch.
	if err := os.WriteFile(filepath.Join(h.Root, "new.txt"), []byte("different\n"), 0o644); err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	_, cerr := a.CommitExpecting(h, map[string]string{"new.txt": ContentAbsent})
	if got := ReasonOf(cerr); got != ReasonDestinationContentChanged {
		t.Fatalf("reason = %q, want %q", got, ReasonDestinationContentChanged)
	}
}

// --- grouped edits ---

// TestSnapshotRestoreFiles_UndoesAGroupOfEdits is what makes "this group applied, or none of it
// did" true. Without it a group whose second edit fails leaves its first edit in the copy — and the
// copy is what a commit ships.
func TestSnapshotRestoreFiles_UndoesAGroupOfEdits(t *testing.T) {
	live := t.TempDir()
	if err := os.WriteFile(filepath.Join(live, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	a := New(t.TempDir())
	h, err := a.Copy(live, false, "group")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	defer a.Cleanup(h)

	snap, serr := a.SnapshotFiles(h, []string{"a.txt", "b.txt"})
	if serr != nil {
		t.Fatalf("snapshot: %v", serr)
	}
	if snap.Len() != 2 {
		t.Fatalf("snapshot holds %d file(s), want 2", snap.Len())
	}
	// The first edit succeeds; the second has no anchor and fails.
	if aerr := a.ApplyEdit(h, core.Edit{File: "a.txt", Anchor: "one", Replacement: "two"}); aerr != nil {
		t.Fatalf("first edit: %v", aerr)
	}
	if err := os.WriteFile(filepath.Join(h.Root, "b.txt"), []byte("invented\n"), 0o644); err != nil {
		t.Fatalf("create b: %v", err)
	}
	if aerr := a.ApplyEdit(h, core.Edit{File: "a.txt", Anchor: "no such anchor", Replacement: "x"}); aerr == nil {
		t.Fatal("the second edit was supposed to fail")
	}

	if rerr := a.RestoreFiles(h, snap); rerr != nil {
		t.Fatalf("restore: %v", rerr)
	}
	body, _ := os.ReadFile(filepath.Join(h.Root, "a.txt"))
	if string(body) != "one\n" {
		t.Fatalf("the group's first edit survived its rollback: %q", body)
	}
	if _, err := os.Stat(filepath.Join(h.Root, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("a file the group created survived its rollback")
	}
	// And the copy is still committable: the rollback restores state, it does not poison it.
	if _, cerr := a.Commit(h); cerr != nil {
		t.Fatalf("commit after rollback: %v", cerr)
	}
	if body, _ := os.ReadFile(filepath.Join(live, "a.txt")); string(body) != "one\n" {
		t.Fatalf("the live tree changed although the group was rolled back: %q", body)
	}
}

// TestSnapshotFiles_RefusesAnEscapingPath keeps the staging primitive inside the same discipline as
// every other path in this package.
func TestSnapshotFiles_RefusesAnEscapingPath(t *testing.T) {
	live := t.TempDir()
	a := New(t.TempDir())
	h, err := a.Copy(live, false, "group")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	defer a.Cleanup(h)
	for _, rel := range []string{"../escape.txt", "/etc/passwd", ".git/config"} {
		if _, serr := a.SnapshotFiles(h, []string{rel}); serr == nil {
			t.Fatalf("SnapshotFiles accepted %q", rel)
		}
	}
}
