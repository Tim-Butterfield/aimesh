package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

// savedModelMgr builds a manager whose USER layer defines a generated codex saved-model key with
// content that DIFFERS from what modelArg "gpt-5.5"+effort "high" would generate — so a discovered
// choice of gpt-5.5/high collides. `conflict-user.reviewer` references the key (used-by), and
// `codex-cli-gpt-4-low` is an unused user entry for the delete tests.
func savedModelMgr(t *testing.T) (*Manager, string) {
	t.Helper()
	m := mutMgr(t)
	userPath, err := config.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(userPath), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "" +
		"schemaVersion: 1\n" +
		"modelCatalog:\n" +
		"  codex-cli-gpt-5.5-high:\n" +
		"    provider: openai\n" +
		"    displayName: gpt-OLD (high)\n" +
		"    adapters:\n" +
		"      codex-cli:\n" +
		"        modelArg: gpt-OLD\n" +
		"        effort: high\n" +
		"  codex-cli-gpt-4-low:\n" +
		"    provider: openai\n" +
		"    displayName: gpt-4 (low)\n" +
		"    adapters:\n" +
		"      codex-cli:\n" +
		"        modelArg: gpt-4\n" +
		"        effort: low\n" +
		"profiles:\n" +
		"  conflict-user:\n" +
		"    lanes:\n" +
		"      reviewer:\n" +
		"        execution: adapter\n" +
		"        adapter: codex-cli\n" +
		"        model: codex-cli-gpt-5.5-high\n"
	if err := os.WriteFile(userPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(userPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m.Cfg = cfg
	m.Layers.UserPath, m.Layers.UserLoaded = userPath, true
	return m, userPath
}

func TestCheckLane_PredictsSaveMaterializationConflict(t *testing.T) {
	m, _ := savedModelMgr(t)
	in := LaneChoiceInput{Source: SourceDiscoveredModel, ModelArg: "gpt-5.5", Effort: "high"}

	// Check: the generated key collides with different content → NOT ok, with a conflict.
	chk, err := m.CheckLane("conflict-user", "reviewer", "codex-cli", in, nil)
	if err != nil {
		t.Fatalf("CheckLane: %v", err)
	}
	if chk.OK || chk.Conflict == nil {
		t.Fatalf("Check must fail with a conflict, got %+v", chk)
	}
	if chk.Conflict.Key != "codex-cli-gpt-5.5-high" || !chk.Conflict.Replaceable {
		t.Errorf("conflict=%+v", chk.Conflict)
	}
	if len(chk.Conflict.UsedBy) == 0 || chk.Conflict.UsedBy[0] != "conflict-user.reviewer" {
		t.Errorf("used-by must name the referencing lane: %+v", chk.Conflict.UsedBy)
	}

	// Save WITHOUT replace: the SAME conflict (Check predicted Save), nothing written.
	res, err := m.EditProfileLane("conflict-user", "reviewer", "codex-cli", in, nil)
	if err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	if res.ConfigWritten || res.Conflict == nil || res.Conflict.Key != chk.Conflict.Key {
		t.Fatalf("Save must return the same conflict, unwritten: %+v", res)
	}
}

func TestEditProfileLane_ReplaceUpdatesEntryAndSaves(t *testing.T) {
	m, userPath := savedModelMgr(t)
	in := LaneChoiceInput{Source: SourceDiscoveredModel, ModelArg: "gpt-5.5", Effort: "high", Replace: true}
	res, err := m.EditProfileLane("conflict-user", "reviewer", "codex-cli", in, nil)
	if err != nil {
		t.Fatalf("EditProfileLane replace: %v", err)
	}
	if !res.ConfigWritten || res.Conflict != nil {
		t.Fatalf("replace must write and clear the conflict: %+v", res)
	}
	// The catalog entry now describes the NEW content.
	cfg, err := config.Load(userPath)
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.ModelCatalog["codex-cli-gpt-5.5-high"]
	if e.Adapters["codex-cli"].ModelArg != "gpt-5.5" {
		t.Errorf("replaced entry modelArg=%q, want gpt-5.5", e.Adapters["codex-cli"].ModelArg)
	}
}

func TestEditProfileLane_IdenticalGeneratedKeyReused(t *testing.T) {
	m, _ := savedModelMgr(t)
	// Choosing gpt-4/low matches the existing codex-cli-gpt-4-low content exactly → reuse (no
	// conflict, no rewrite of the catalog entry).
	in := LaneChoiceInput{Source: SourceDiscoveredModel, ModelArg: "gpt-4", Effort: "low"}
	plan := m.planMaterialization("codex-cli", resolvedLaneChoice{ModelArg: "gpt-4", Effort: "low"})
	if plan.Action != "reuse" {
		t.Errorf("identical content must reuse, got %q", plan.Action)
	}
	chk, err := m.CheckLane("conflict-user", "reviewer", "codex-cli", in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !chk.OK || chk.Conflict != nil {
		t.Errorf("identical reuse must pass Check: %+v", chk)
	}
}

// A discovered/manual choice that collides with a SHIPPED key (e.g. modelArg "default" →
// codex-cli-default) must NOT be reported replaceable — a user-layer override would partially
// merge with the shipped fields (authoritative/adapterDefault). Regression for the design gate's
// layer-awareness requirement.
func TestSavedModelConflict_ShippedKeyNotReplaceable(t *testing.T) {
	m := mutMgr(t)
	in := LaneChoiceInput{Source: SourceManualEntry, ModelArg: "default"} // slugs to codex-cli-default (shipped)
	chk, err := m.CheckLane("default", "reviewer", "codex-cli", in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if chk.Conflict == nil {
		t.Fatalf("a shipped-key collision must be a conflict: %+v", chk)
	}
	if chk.Conflict.Replaceable {
		t.Errorf("a shipped-key collision must NOT be replaceable via the user layer: %+v", chk.Conflict)
	}
	// And an explicit Replace of a non-replaceable key is refused (not written).
	res, err := m.EditProfileLane("default", "reviewer", "codex-cli", LaneChoiceInput{Source: SourceManualEntry, ModelArg: "default", Replace: true}, nil)
	if err == nil && res.ConfigWritten {
		t.Error("replace of a shipped-colliding key must be refused, not written")
	}
}

func TestIsGeneratedCatalogEntry(t *testing.T) {
	m, _ := savedModelMgr(t)
	// The materialized user entry looks generated.
	if !isGeneratedCatalogEntry("codex-cli-gpt-4-low", "codex-cli", m.Cfg.ModelCatalog["codex-cli-gpt-4-low"]) {
		t.Error("a materialized-shaped entry must look generated")
	}
	// A shipped adapter default does NOT look generated.
	if isGeneratedCatalogEntry("codex-cli-default", "codex-cli", m.Cfg.ModelCatalog["codex-cli-default"]) {
		t.Error("an adapter-default/authoritative entry must NOT look generated")
	}
}

func TestDeleteSavedModel_UnusedSucceeds(t *testing.T) {
	m, userPath := savedModelMgr(t)
	res, err := m.DeleteSavedModel("codex-cli-gpt-4-low")
	if err != nil {
		t.Fatalf("delete unused: %v", err)
	}
	if !res.ConfigWritten || res.Blocked {
		t.Fatalf("unused delete must succeed: %+v", res)
	}
	cfg, _ := config.Load(userPath)
	if _, ok := cfg.ModelCatalog["codex-cli-gpt-4-low"]; ok {
		t.Error("entry must be gone after delete")
	}
}

func TestDeleteSavedModel_UsedIsBlocked(t *testing.T) {
	m, _ := savedModelMgr(t)
	res, err := m.DeleteSavedModel("codex-cli-gpt-5.5-high")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Blocked || len(res.UsedBy) == 0 {
		t.Fatalf("a used saved model must be blocked with the using lanes: %+v", res)
	}
}

func TestDeleteSavedModel_AdapterDefaultRefused(t *testing.T) {
	m, _ := savedModelMgr(t)
	if _, err := m.DeleteSavedModel("codex-cli-default"); err == nil {
		t.Error("an adapter-default entry must not be deletable as a saved model")
	}
}

func TestDeleteSavedModel_HigherLayerBlocked(t *testing.T) {
	m, _ := savedModelMgr(t)
	// A project layer that also defines the (unused) key → deletion via the user layer is blocked.
	proj := filepath.Join(t.TempDir(), "project.yaml")
	if err := os.WriteFile(proj, []byte("schemaVersion: 1\nmodelCatalog:\n  codex-cli-gpt-4-low:\n    provider: openai\n    adapters:\n      codex-cli:\n        modelArg: gpt-4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Layers.ProjectPath, m.Layers.ProjectLoaded = proj, true
	res, err := m.DeleteSavedModel("codex-cli-gpt-4-low")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Blocked {
		t.Errorf("a project-layer-defined key must block user-layer delete: %+v", res)
	}
}
