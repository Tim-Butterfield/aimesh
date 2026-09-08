package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionRecordSchema is the on-disk schema version for a persisted ACP session record. A
// record with a different version is treated as malformed (forward/backward incompatible).
const SessionRecordSchema = 1

// sessionTTL bounds how long a persisted session may be resumed after its last update. A
// stale record is rejected (ErrSessionExpired) so the store cannot accumulate unbounded
// resumable handles across long restarts.
const sessionTTL = 14 * 24 * time.Hour

// Sentinel errors the resume handler maps to structured JSON-RPC errors.
var (
	ErrSessionNotFound  = errors.New("session not found")
	ErrSessionMalformed = errors.New("session record malformed")
	ErrSessionExpired   = errors.New("session expired")
)

// SessionRecord is the MINIMAL, safe session metadata persisted for cross-restart resume.
// It deliberately holds NO prompt text, model output, provider metadata, reviewed-file or
// inline-workspace content, run artifacts, secrets, or conversation history — only the
// protocol session handle + the workspace cwd needed to make a post-reconnect prompt usable.
type SessionRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	SessionID     string `json:"sessionId"`
	CWD           string `json:"cwd"`
	CreatedAt     string `json:"createdAt"` // RFC3339 UTC
	UpdatedAt     string `json:"updatedAt"` // RFC3339 UTC
}

// SessionStore is the durable persistence the ACP surface uses for `session/resume` across an
// agent process restart. It is ACP-surface-local: only the ACP session lifecycle uses it, and
// it never touches the ReviewManager / domain. A nil store means resume is unsupported (not
// advertised; `session/resume` → method-not-found).
type SessionStore interface {
	Save(rec SessionRecord) error
	Load(sessionID string) (SessionRecord, error)
}

// fileSessionStore persists one JSON file per session under a directory rooted at the
// reviewmesh home (REVIEWMESH_HOME-aware via the caller). It tolerates a missing directory,
// rejects malformed/expired records, and is path-safe against a hostile session id.
type fileSessionStore struct{ dir string }

// NewFileSessionStore returns a durable file-backed session store rooted at dir (typically
// `<reviewmesh-home>/.reviewmesh/acp-sessions`). Nothing is created until the first Save.
func NewFileSessionStore(dir string) SessionStore { return &fileSessionStore{dir: dir} }

// safeSessionID guards the on-disk filename against path traversal: a session id must be a
// short non-empty token of `[A-Za-z0-9._-]` with no separators and no `..`.
func safeSessionID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." || strings.Contains(id, "..") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func (s *fileSessionStore) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *fileSessionStore) Save(rec SessionRecord) error {
	if !safeSessionID(rec.SessionID) {
		return fmt.Errorf("%w: unsafe session id %q", ErrSessionMalformed, rec.SessionID)
	}
	rec.SchemaVersion = SessionRecordSchema
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// Write atomically (temp + rename) so a concurrent/interrupted Save never leaves a
	// half-written record that would later read as malformed.
	tmp, err := os.CreateTemp(s.dir, rec.SessionID+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path(rec.SessionID))
}

func (s *fileSessionStore) Load(id string) (SessionRecord, error) {
	var rec SessionRecord
	if !safeSessionID(id) {
		return rec, fmt.Errorf("%w: unsafe session id %q", ErrSessionMalformed, id)
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return rec, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
		}
		return rec, err // a missing parent dir also surfaces as not-exist above
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, fmt.Errorf("%w: %v", ErrSessionMalformed, err)
	}
	if rec.SchemaVersion != SessionRecordSchema || rec.SessionID != id {
		return rec, fmt.Errorf("%w: schema/version or id mismatch", ErrSessionMalformed)
	}
	if t, perr := time.Parse(time.RFC3339, rec.UpdatedAt); perr != nil {
		return rec, fmt.Errorf("%w: bad updatedAt %q", ErrSessionMalformed, rec.UpdatedAt)
	} else if time.Since(t) > sessionTTL {
		return rec, fmt.Errorf("%w: last updated %s", ErrSessionExpired, rec.UpdatedAt)
	}
	return rec, nil
}
