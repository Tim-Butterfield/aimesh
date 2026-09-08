package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// audit owns the per-run directory both apps record through: reviewmesh's run artifacts and
// exploremesh's `--dump-run` capture (which the golden-run gate compares). Its guarantees are
// modest but load-bearing: a deterministic run ID, parent-dir creation, APPEND-only event logging
// (never truncating a prior event), and an in-process progress sink that is independent of disk.

func TestNewRun_GeneratedIDIsTimestampedAndDirsExist(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 7, 24, 15, 4, 5, 600000000, time.UTC)

	r, err := NewRun(base, "", now)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	// The generated ID is UTC-timestamped + a sub-second suffix, so runs sort chronologically and
	// two runs in the same second do not collide.
	if want := "20260724T150405-6000"; r.ID != want {
		t.Errorf("generated ID = %q, want %q (UTC timestamp + tenths-of-ms suffix)", r.ID, want)
	}
	if r.Dir != filepath.Join(base, r.ID) {
		t.Errorf("Dir = %q, want <base>/<id>", r.Dir)
	}
	if fi, err := os.Stat(filepath.Join(r.Dir, "calls")); err != nil || !fi.IsDir() {
		t.Errorf("NewRun must create the calls/ subdirectory: %v", err)
	}
}

func TestNewRun_ExplicitIDAndNestedBase(t *testing.T) {
	// A base several levels deep must be created, and an explicit ID used verbatim (the caller
	// owns run identity when it has one — e.g. a replay fixture).
	base := filepath.Join(t.TempDir(), "deep", "nested", "base")
	r, err := NewRun(base, "my-run-id", time.Now())
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if r.ID != "my-run-id" {
		t.Errorf("explicit ID must be used verbatim, got %q", r.ID)
	}
	if _, err := os.Stat(filepath.Join(base, "my-run-id", "calls")); err != nil {
		t.Errorf("nested base dirs must be created: %v", err)
	}
}

func TestRun_WriteTextAndJSON_CreateParents(t *testing.T) {
	r, err := NewRun(t.TempDir(), "run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteText("logs/deep/note.txt", "hello"); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(r.Dir, "logs", "deep", "note.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("WriteText must create parents and write verbatim: %q, %v", got, err)
	}

	type payload struct {
		A int      `json:"a"`
		B []string `json:"b"`
	}
	if err := r.WriteJSON("state/run.json", payload{A: 1, B: []string{"x"}}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(r.Dir, "state", "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Indented + newline-terminated: these artifacts are read by humans and diffed by the golden gate.
	if !strings.Contains(string(raw), "\n  \"a\": 1") {
		t.Errorf("WriteJSON must indent for human/diff readability:\n%s", raw)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("WriteJSON must newline-terminate (clean diffs, POSIX-friendly)")
	}
	var back payload
	if err := json.Unmarshal(raw, &back); err != nil || back.A != 1 || len(back.B) != 1 {
		t.Errorf("WriteJSON must round-trip: %+v, %v", back, err)
	}
}

func TestRun_CallDir(t *testing.T) {
	r := &Run{ID: "r", Dir: "/tmp/r"}
	if got := r.CallDir("call-7"); got != filepath.Join("calls", "call-7") {
		t.Errorf("CallDir = %q, want a RUN-RELATIVE calls/<id> path", got)
	}
}

// Events must APPEND — a second event may never truncate the first, or a run's history is lost.
func TestRun_Event_AppendsJSONL(t *testing.T) {
	r, err := NewRun(t.TempDir(), "run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 7, 24, 1, 2, 3, 0, time.UTC)
	if err := r.Event(t0, "info", "start", "first", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("Event: %v", err)
	}
	if err := r.Event(t0.Add(time.Second), "error", "halt", "second", nil); err != nil {
		t.Fatalf("Event: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(r.Dir, "logs", "events.jsonl"))
	if err != nil {
		t.Fatalf("events.jsonl must exist: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 appended lines, got %d:\n%s", len(lines), raw)
	}

	var first EventLine
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("each line must be a standalone JSON object: %v", err)
	}
	if first.SchemaVersion != 1 || first.RunID != "run" || first.Level != "info" ||
		first.EventType != "start" || first.Message != "first" || first.Data["k"] != "v" {
		t.Errorf("event fields not recorded as given: %+v", first)
	}
	if first.Timestamp != "2026-07-24T01:02:03Z" {
		t.Errorf("timestamp must be RFC3339 UTC, got %q", first.Timestamp)
	}
	// Omitempty fields stay absent rather than emitting empty strings (the schema's contract).
	if strings.Contains(lines[1], `"callId"`) || strings.Contains(lines[1], `"haltClass"`) {
		t.Errorf("unset optional fields must be omitted, got: %s", lines[1])
	}
}

// The in-process progress sink fires for every event, in order, and is independent of the disk write.
func TestRun_Event_OnEventSink(t *testing.T) {
	r, err := NewRun(t.TempDir(), "run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var seen []EventLine
	r.OnEvent = func(e EventLine) { seen = append(seen, e) }

	now := time.Now()
	for _, m := range []string{"a", "b", "c"} {
		if err := r.Event(now, "info", "progress", m, nil); err != nil {
			t.Fatalf("Event: %v", err)
		}
	}
	if len(seen) != 3 || seen[0].Message != "a" || seen[2].Message != "c" {
		t.Fatalf("OnEvent must fire once per event, in order: %+v", seen)
	}
	// The sink receives the SAME record that is persisted (not a partially-built one).
	if seen[0].RunID != "run" || seen[0].SchemaVersion != 1 || seen[0].EventType != "progress" {
		t.Errorf("the sink must receive the fully-built event: %+v", seen[0])
	}
	// A nil sink is simply skipped (the CLI path) — no panic.
	r.OnEvent = nil
	if err := r.Event(now, "info", "progress", "d", nil); err != nil {
		t.Errorf("a nil OnEvent must be a no-op, got %v", err)
	}
}
