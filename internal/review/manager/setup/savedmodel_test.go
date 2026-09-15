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
