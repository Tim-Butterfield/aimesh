package setup

import (
	"os"
	"path/filepath"
	"strings"
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

// TestEditProfileLane_ProjectScopeWritesProjectFile: a project-scope lane edit lands in the PROJECT
// config, not the user config.
func TestEditProfileLane_ProjectScopeWritesProjectFile(t *testing.T) {
	m := mutMgr(t)
	projPath := seedProject(t, m, "schemaVersion: 1\n")
	m.WriteScope = "project"
	res, err := m.EditProfileLane("default", "reviewer", "claude-code", LaneChoiceInput{ModelArg: "opus", Effort: "medium"}, nil)
	if err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	if !res.ConfigWritten || res.Path != projPath {
		t.Fatalf("expected a write to the project path %q, got written=%v path=%q", projPath, res.ConfigWritten, res.Path)
	}
	if !rawLayerHasLane(projPath, "default", "reviewer") {
		t.Error("the reviewer lane must be written into the PROJECT config")
	}
}

// TestEditProfileLane_ProjectScopeMissingConfigBlocked: project scope with no project config is BLOCKED
// (never a silent user write), and revalidated at write time (a deleted project config also blocks).
func TestEditProfileLane_ProjectScopeMissingConfigBlocked(t *testing.T) {
	m := mutMgr(t)
	m.WriteScope = "project" // no project layer loaded
	if _, err := m.EditProfileLane("default", "reviewer", "claude-code", LaneChoiceInput{ModelArg: "opus"}, nil); err == nil {
		t.Fatal("project scope with no project config must be blocked")
	}
	// Stale delete: project loaded at snapshot but the file is gone at write time.
	projPath := seedProject(t, m, "schemaVersion: 1\n")
	_ = os.Remove(projPath)
	if _, err := m.EditProfileLane("default", "reviewer", "claude-code", LaneChoiceInput{ModelArg: "opus"}, nil); err == nil {
		t.Fatal("a deleted project config must block the write, not recreate it")
	}
}

// TestClearProfileLane_ProjectScopeRevealsUserLane: clearing a lane in the PROJECT scope reveals the
// LOWER user-layer lane (user is lower than project and merges field-by-field).
func TestClearProfileLane_ProjectScopeRevealsUserLane(t *testing.T) {
	m := mutMgr(t)
	// User layer defines the reviewer lane.
	writeUserConfig(t, "schemaVersion: 1\nprofiles:\n  default:\n    lanes:\n      reviewer:\n        adapter: fake\n        model: fake-model\n")
	// Project layer ALSO defines it (this is what we clear).
	projPath := seedProject(t, m, "schemaVersion: 1\nprofiles:\n  default:\n    lanes:\n      reviewer:\n        adapter: fake\n        model: fake-model\n")
	m.WriteScope = "project"
	res, err := m.ClearProfileLane("default", "reviewer", "reviewer")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !res.Cleared || !res.RevealedLower {
		t.Fatalf("clearing the project lane must reveal the lower user lane, got %+v", res)
	}
	if !strings.Contains(res.Message, "user") {
		t.Errorf("the reveal message should name the user layer, got %q", res.Message)
	}
	if rawLayerHasLane(projPath, "default", "reviewer") {
		t.Error("the project-layer reviewer lane must be deleted")
	}
}

// TestValidateLaneChoice_ClaudeModelArgDisplayName: a display-name-shaped claude-code modelArg (with
// whitespace) is flagged at Check/Save; a valid slug is not.
func TestValidateLaneChoice_ClaudeModelArgDisplayName(t *testing.T) {
	m := mutMgr(t)
	// Manual entry with a display name → error.
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{ModelArg: "Opus 4.8"}, nil); len(errs) == 0 || !strings.Contains(strings.Join(errs, " "), "display name") {
		t.Errorf("a whitespace claude modelArg (%q) must be flagged, got %+v", "Opus 4.8", errs)
	}
	// A valid slug → not flagged for shape.
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{ModelArg: "claude-opus-4-8"}, nil); strings.Contains(strings.Join(errs, " "), "display name") {
		t.Errorf("a valid slug must not be flagged as a display name, got %+v", errs)
	}
	// A saved CATALOG entry with a display-name modelArg → also flagged (catalog path).
	m.Cfg.ModelCatalog["bad-claude"] = config.CatalogEntry{Provider: "anthropic", Adapters: map[string]config.AdapterModel{"claude-code": {ModelArg: "Opus 4.8"}}}
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{CatalogKey: "bad-claude"}, nil); len(errs) == 0 || !strings.Contains(strings.Join(errs, " "), "display name") {
		t.Errorf("a saved catalog claude modelArg with a display name must be flagged, got %+v", errs)
	}
}

// TestCatalogKeyLayers_ProjectScope locks the scope-aware saved-model layer analysis: under project
// scope a catalog key defined in the PROJECT file is the WRITE layer (editable/deletable), not a
// shadowing "higher" layer; a LOWER user-layer definition is what would be revealed.
func TestCatalogKeyLayers_ProjectScope(t *testing.T) {
	m := mutMgr(t)
	seedProject(t, m, "schemaVersion: 1\nmodelCatalog:\n  proj-model:\n    provider: anthropic\n")
	m.WriteScope = "project"
	writeDefines, higher, _ := m.catalogKeyLayers("proj-model")
	if !writeDefines {
		t.Error("a project-defined catalog key must be seen as write-scope-defined under project scope (deletable), not shadowed")
	}
	if higher != "" {
		t.Errorf("no higher layer should shadow a project-defined key under project scope, got %q", higher)
	}
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
