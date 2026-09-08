package acp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionStore_SaveLoadRoundTrip(t *testing.T) {
	st := NewFileSessionStore(t.TempDir())
	now := time.Now().UTC().Format(time.RFC3339)
	in := SessionRecord{SessionID: "s-0001", CWD: "/ws/project", CreatedAt: now, UpdatedAt: now}
	if err := st.Save(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load("s-0001")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.SessionID != "s-0001" || got.CWD != "/ws/project" || got.SchemaVersion != SessionRecordSchema {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestSessionStore_NotFound(t *testing.T) {
	st := NewFileSessionStore(t.TempDir()) // dir does not even exist yet
	if _, err := st.Load("s-0001"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("missing record should be ErrSessionNotFound, got %v", err)
	}
}

func TestSessionStore_Malformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s-0001.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileSessionStore(dir).Load("s-0001"); !errors.Is(err, ErrSessionMalformed) {
		t.Errorf("malformed record should be ErrSessionMalformed, got %v", err)
	}
}

func TestSessionStore_Expired(t *testing.T) {
	st := NewFileSessionStore(t.TempDir())
	old := time.Now().Add(-2 * sessionTTL).UTC().Format(time.RFC3339)
	if err := st.Save(SessionRecord{SessionID: "s-0001", CWD: "/ws", CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load("s-0001"); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("stale record should be ErrSessionExpired, got %v", err)
	}
}

func TestSessionStore_RejectsUnsafeID(t *testing.T) {
	st := NewFileSessionStore(t.TempDir())
	for _, bad := range []string{"", "..", "../escape", "a/b", `a\b`, "with space"} {
		if err := st.Save(SessionRecord{SessionID: bad, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}); err == nil {
			t.Errorf("Save(%q) should be rejected as an unsafe id", bad)
		}
		if _, err := st.Load(bad); err == nil {
			t.Errorf("Load(%q) should be rejected as an unsafe id", bad)
		}
	}
}
