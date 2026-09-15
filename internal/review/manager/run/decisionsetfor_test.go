package run

import (
	"path/filepath"
	"testing"
)

// A server serving many workspaces keeps each run's record beside the workspace it reviewed, so a
// run is found by naming that workspace — and not by naming a different one.
func TestReadDecisionSetByWorkspace(t *testing.T) {
	m, runDir, ws := decisionSetFixture(t)
	art := filepath.Dir(runDir)
	m.ArtifactDir = ""
	m.ArtifactDirFor = func(workspace string) string {
		if workspace == ws {
			return art
		}
		return t.TempDir()
	}
	runID := filepath.Base(runDir)

	if set, _, err := m.ReadDecisionSetByID(ws, runID); err != nil || set.RunID != runID {
		t.Fatalf("ReadDecisionSetByID(workspace, id) = %+v, %v; want the recorded run", set, err)
	}
	if set, _, err := m.ReadDecisionSetFor(ws, runDir); err != nil || set.RunID != runID {
		t.Fatalf("ReadDecisionSetFor(workspace, runDir) = %+v, %v; want the recorded run", set, err)
	}
	other := t.TempDir()
	if _, _, err := m.ReadDecisionSetByID(other, runID); err == nil {
		t.Fatal("a run must not be found through a workspace whose records do not hold it")
	}
	if _, _, err := m.ReadDecisionSetByID(ws, "../"+runID); err == nil {
		t.Fatal("a run id is a single name; a traversal must be refused")
	}
}

func TestArtifactDirFor_FallsBackToTheFixedDirectory(t *testing.T) {
	m := &Manager{ArtifactDir: "/fixed"}
	if got := m.artifactDirFor("/ws"); got != "/fixed" {
		t.Fatalf("artifactDirFor without ArtifactDirFor = %q, want the fixed directory", got)
	}
	m.ArtifactDirFor = func(string) string { return "" }
	if got := m.artifactDirFor("/ws"); got != "/fixed" {
		t.Fatalf("an empty per-workspace answer must fall back to the fixed directory, got %q", got)
	}
}
