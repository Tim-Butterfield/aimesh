package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// A7 — A WITHHELD FILE IS NEVER A SILENT OMISSION.
//
// meshcore withholds a hardlinked regular file from the containment copy instead of halting:
// its innocuous in-tree name may be a second name for protected material, but an in-tree
// `cp -al` tree or dedup store is ordinary, and halting on one would hand any writer inside a
// trusted tree an availability switch. What makes that trade SAFE is that the omission is
// stated — otherwise "this file was not reviewed" is indistinguishable from "there was no such
// file", and a reviewer's silence about a file it was never shown reads as approval.
//
// This is the end-to-end pin: a hardlinked file in the workspace yields a COMPLETED run whose
// outcome, run record and machine projection all name the withheld path and the rule.
func TestRun_WithheldHardlinkIsSurfacedEverywhere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("link count is not knowable from a Win32FileAttributeData")
	}
	rec := &callRecorder{}
	m := recorderManager(t, rec)
	ws, _ := makeWorkspace(t)

	// The withheld file: a perfectly ordinary-looking source file that happens to have a
	// second name. Nothing about its own path is suspicious — that is the point.
	linked := filepath.Join(ws, "linked.go")
	if err := os.WriteFile(linked, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(linked, filepath.Join(ws, "second-name.go")); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}

	outcome, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "recorder"})
	if err != nil {
		t.Fatalf("the run must COMPLETE (a withheld file is not a halt): %v", err)
	}
	if outcome.Status != "stable" {
		t.Errorf("status = %q, want stable", outcome.Status)
	}

	// 1. The outcome names it, with meshcore's own machine reason code.
	found := map[string]review.WithheldFile{}
	for _, w := range outcome.Withheld {
		found[w.Path] = w
	}
	got, ok := found["linked.go"]
	if !ok {
		t.Fatalf("outcome.Withheld does not name the hardlinked file: %+v", outcome.Withheld)
	}
	if got.Reason != string(workspace.ReasonHardlink) {
		t.Errorf("reason = %q, want %q", got.Reason, workspace.ReasonHardlink)
	}
	if got.Rule == "" {
		t.Error("the rule that withheld the file must be recorded, not just its code")
	}
	if got.Stage != stageCopy {
		t.Errorf("stage = %q, want %q (it never reached the isolated copy at all)", got.Stage, stageCopy)
	}
	// The file's OTHER name is withheld too — both are hardlinks to the same inode.
	if _, ok := found["second-name.go"]; !ok {
		t.Errorf("both names of a hardlinked file must be withheld: %+v", outcome.Withheld)
	}
	// Deduped: one entry per (path, stage), however many seats/attempts/cycles saw it.
	seen := map[string]int{}
	for _, w := range outcome.Withheld {
		seen[w.Stage+"|"+w.Path]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s recorded %d times, want 1", k, n)
		}
	}

	// 2. The machine PROJECTION names it, alongside identityCaveats, with a host-computed
	//    count a consumer never has to derive.
	view := runview.Build(runview.Input{Outcome: outcome})
	if view.Counts.Withheld != len(outcome.Withheld) {
		t.Errorf("counts.withheld = %d, want %d", view.Counts.Withheld, len(outcome.Withheld))
	}
	named := false
	for _, w := range view.Withheld {
		if w.Path == "linked.go" && w.Reason == string(workspace.ReasonHardlink) && w.Rule != "" {
			named = true
		}
	}
	if !named {
		t.Fatalf("the projection must name the withheld path AND the rule: %+v", view.Withheld)
	}
	// The array serializes as a real key, so a consumer can read it without null-checking.
	b, jerr := json.Marshal(view)
	if jerr != nil {
		t.Fatal(jerr)
	}
	if !strings.Contains(string(b), `"withheld"`) || !strings.Contains(string(b), "linked.go") {
		t.Errorf("projection JSON does not carry withheld[]: %s", b)
	}

	// 3. The RUN RECORD names it, so the audit trail stands on its own.
	state := read(t, filepath.Join(outcome.RunDir, "run-state.json"))
	if !strings.Contains(state, `"withheld"`) || !strings.Contains(state, "linked.go") ||
		!strings.Contains(state, string(workspace.ReasonHardlink)) {
		t.Errorf("run-state.json does not record the withheld file:\n%s", state)
	}
	// 4. ...and the EVENT LOG says it at the moment it happened.
	events := read(t, filepath.Join(outcome.RunDir, "logs", "events.jsonl"))
	if !strings.Contains(events, "workspace_file_withheld") {
		t.Error("no workspace_file_withheld event was logged")
	}
}

// A run with nothing withheld says so: an empty (never null) array, and a zero count. "Nothing
// was withheld" has to be a statement a consumer can read, not an absent key it must interpret.
func TestRunView_WithheldIsAlwaysPresent(t *testing.T) {
	view := runview.Build(runview.Input{Outcome: review.RunOutcome{Status: "stable"}})
	if view.Withheld == nil {
		t.Fatal("withheld must be an empty array, never null")
	}
	if view.Counts.Withheld != 0 {
		t.Errorf("counts.withheld = %d, want 0", view.Counts.Withheld)
	}
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"withheld":[]`) {
		t.Errorf("projection JSON must carry an empty withheld array: %s", b)
	}
}
