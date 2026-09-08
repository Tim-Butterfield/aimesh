package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// failingSeat exits non-zero with a chosen stderr, so a panel can be given two seats that fail for
// two DIFFERENT, independently fixable reasons — the situation measured on 2026-08-11.
type failingSeat struct {
	name   string
	stderr string
}

func (f failingSeat) Name() string                      { return f.name }
func (f failingSeat) Available() (bool, string)         { return true, "failing seat (test)" }
func (f failingSeat) Evidence() review.IdentityEvidence { return review.EvidenceInvocationTag }
func (f failingSeat) Invoke(context.Context, model.Call) (model.Result, error) {
	return model.Result{ExitCode: 1, Stderr: []byte(f.stderr)}, nil
}

// TestPanelHalt_EverySeatRecordsItsOwnCause: a panel whose seats fail for different reasons records
// BOTH, each with its own signal. Before this, the run-level Failure carried the first-by-index seat
// and the other seat's cause existed only inside the run directory — so an operator fixed one blocker,
// paid for the whole panel again, and met the next one.
func TestPanelHalt_EverySeatRecordsItsOwnCause(t *testing.T) {
	m := panelManager(t,
		seatFake{name: "seat-a", title: "a", file: "a.go"},
		seatFake{name: "seat-b", title: "b", file: "b.go"},
	)
	// Replace both seats with distinct real-world failures (the two actually observed).
	m.Adapters["seat-a"] = failingSeat{
		name:   "seat-a",
		stderr: `ERROR: {"error":{"message":"The 'gpt-5-codex' model is not supported on your account."}}`,
	}
	m.Adapters["seat-b"] = failingSeat{
		name:   "seat-b",
		stderr: "Error: not logged in — please sign in first",
	}
	ws, _ := makeWorkspace(t)

	out, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel",
	})
	if err == nil {
		t.Fatal("a panel whose every seat failed did not halt")
	}
	if len(out.Panel) != 2 {
		t.Fatalf("roster has %d seat(s), want 2", len(out.Panel))
	}

	bySeat := map[string]review.SeatStatus{}
	for _, s := range out.Panel {
		bySeat[s.SeatID] = s
	}
	for _, tc := range []struct{ seat, wantSignal string }{
		{"reviewer", "model_invalid"},
		{"reviewer-2", "login_required"},
	} {
		s, ok := bySeat[tc.seat]
		if !ok {
			t.Fatalf("no roster entry for %q", tc.seat)
		}
		if s.Status != "halted" {
			t.Errorf("%s status = %q, want halted", tc.seat, s.Status)
		}
		if s.Signal != tc.wantSignal {
			t.Errorf("%s signal = %q, want %q — each seat must carry ITS OWN cause, not the halting seat's", tc.seat, s.Signal, tc.wantSignal)
		}
		if s.Detail == "" {
			t.Errorf("%s records no detail, so its cause is unreadable without the run directory", tc.seat)
		}
	}
	// The run-level failure contract is unchanged: it is still the FIRST seat by index.
	if out.Failure == nil || out.Failure.Signal != "model_invalid" {
		t.Errorf("run-level Failure = %+v, want the first-by-index seat's (model_invalid)", out.Failure)
	}

	// And the audit record says the same thing, so a reader of the directory alone is not misled.
	b, rerr := os.ReadFile(filepath.Join(out.RunDir, "panel", "roster.json"))
	if rerr != nil {
		t.Fatalf("panel/roster.json: %v", rerr)
	}
	var rec struct {
		Seats []review.SeatStatus `json:"seats"`
	}
	if uerr := json.Unmarshal(b, &rec); uerr != nil {
		t.Fatalf("roster.json: %v", uerr)
	}
	signals := map[string]bool{}
	for _, s := range rec.Seats {
		signals[s.Signal] = true
	}
	if !signals["model_invalid"] || !signals["login_required"] {
		t.Errorf("roster.json carries signals %v, want both seats' own causes", signals)
	}
}
