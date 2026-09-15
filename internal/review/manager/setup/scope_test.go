package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

// seedProject writes a minimal project config + wires the manager's project layer + user layer, and
// returns the project path. The user layer is the mutMgr's AIMESH_HOME config.
func seedProject(t *testing.T, m *Manager, body string) string {
	t.Helper()
	projPath := filepath.Join(t.TempDir(), ".aimesh", "review", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(projPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Layers.ProjectPath, m.Layers.ProjectLoaded = projPath, true
	if up, err := config.UserConfigPath(); err == nil {
		m.Layers.UserPath, m.Layers.UserLoaded = up, true
	}
	return projPath
}

// TestNonUserLayerModelKeys_ProjectScope: under project scope a key defined only
// in the PROJECT layer is NOT treated as an untouchable "other-layer" key (the project layer IS the write
// layer), so model-key cleanup can operate on it. Only STRICTLY-HIGHER (explicit) keys are excluded.
func TestNonUserLayerModelKeys_ProjectScope(t *testing.T) {
	m := mutMgr(t)
	seedProject(t, m, "schemaVersion: 1\nmodelCatalog:\n  proj-model:\n    provider: anthropic\n")
	m.WriteScope = "project"
	if m.nonUserLayerModelKeys()["proj-model"] {
		t.Error("a project-layer key must NOT be excluded under project scope (project is the write layer)")
	}
}
