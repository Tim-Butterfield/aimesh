// Package audit is the AuditAccess layer: it creates the per-run directory and
// writes the run artifacts (run-state, resolved-plan, summary, per-call records,
// patch summary, and the events log). It never writes secrets.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Run owns one run directory.
type Run struct {
	ID  string
	Dir string
	// OnEvent, if set, is called with every event as it is logged — a non-ACP, in-process
	// progress sink. The Manager stays surface-agnostic: it only logs EventLines; a surface
	// (e.g. ACP) may set this to stream progress. It must not block or panic.
	OnEvent func(EventLine)
}

// NewRun creates baseDir/<run-id>/ (and calls/). If id is empty, one is generated.
func NewRun(baseDir, id string, now time.Time) (*Run, error) {
	if id == "" {
		id = now.UTC().Format("20060102T150405") + fmt.Sprintf("-%04d", now.Nanosecond()/1e5)
	}
	dir := filepath.Join(baseDir, id)
	if err := os.MkdirAll(filepath.Join(dir, "calls"), 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	return &Run{ID: id, Dir: dir}, nil
}

// WriteJSON writes v as indented JSON to a run-relative path.
func (r *Run) WriteJSON(rel string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return r.WriteText(rel, string(b)+"\n")
}

// WriteText writes text to a run-relative path (creating parent dirs).
func (r *Run) WriteText(rel, content string) error {
	p := filepath.Join(r.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// CallDir ensures and returns the run-relative directory for a call.
func (r *Run) CallDir(callID string) string { return filepath.Join("calls", callID) }

// EventLine is one line of events.jsonl (docs/schema/event-log-line.schema.json).
type EventLine struct {
	SchemaVersion int            `json:"schemaVersion"`
	Timestamp     string         `json:"timestamp"`
	Level         string         `json:"level"`
	EventType     string         `json:"eventType"`
	RunID         string         `json:"runId"`
	CallID        string         `json:"callId,omitempty"`
	Role          string         `json:"role,omitempty"`
	Phase         string         `json:"phase,omitempty"`
	Adapter       string         `json:"adapter,omitempty"`
	Model         string         `json:"model,omitempty"`
	Message       string         `json:"message"`
	Data          map[string]any `json:"data,omitempty"`
	HaltClass     string         `json:"haltClass,omitempty"`
	ReasonCode    string         `json:"reasonCode,omitempty"`
}

// Event appends one event to logs/events.jsonl.
func (r *Run) Event(now time.Time, level, eventType, message string, data map[string]any) error {
	ev := EventLine{
		SchemaVersion: 1,
		Timestamp:     now.UTC().Format(time.RFC3339Nano),
		Level:         level,
		EventType:     eventType,
		RunID:         r.ID,
		Message:       message,
		Data:          data,
	}
	// Best-effort in-process progress sink (independent of disk write).
	if r.OnEvent != nil {
		r.OnEvent(ev)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	p := filepath.Join(r.Dir, "logs", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
