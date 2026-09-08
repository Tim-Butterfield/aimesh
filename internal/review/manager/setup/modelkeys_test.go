package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

// TestModelDisplayLabel proves the readable model label never leaks a raw adapter key or a
// "(via …)" suffix, for shipped defaults, in-catalog generated entries, and orphaned generated keys.
func TestModelDisplayLabel(t *testing.T) {
	m := mutMgr(t)
	if got := m.ModelDisplayLabel("codex-cli-default"); strings.Contains(got, "via") || strings.Contains(got, "-cli") {
		t.Errorf("shipped adapter-default label leaks key/via: %q", got)
	}
	// Orphaned generated key (not in the catalog): the adapter-key prefix is stripped.
	if got := m.ModelDisplayLabel("codex-cli-gpt-5.5-high"); strings.Contains(got, "codex-cli") {
		t.Errorf("orphaned generated-key label still has the adapter prefix: %q", got)
	}
	// In-catalog generated entry (no DisplayName) → CanonicalModel + effort, normalized to product
	// style (GPT acronym uppercased, effort title-cased).
	m.Cfg.ModelCatalog["codex-cli-gpt-5.5-high"] = genEntry()
	if got := m.ModelDisplayLabel("codex-cli-gpt-5.5-high"); got != "GPT-5.5 High" {
		t.Errorf("in-catalog label = %q, want %q", got, "GPT-5.5 High")
	}
}

// genEntry is a generated-shape catalog entry for adapter codex-cli / model gpt-5.5 / effort high.
// Its canonical generated key is laneCatalogKey("codex-cli","gpt-5.5","high") = "codex-cli-gpt-5.5-high".
func genEntry() config.CatalogEntry {
	return config.CatalogEntry{
		Provider: "openai", CanonicalModel: "gpt-5.5", Effort: "high",
		Adapters: map[string]config.AdapterModel{"codex-cli": {ModelArg: "gpt-5.5"}},
	}
}

// writeUserConfig writes a raw user config (JSON = valid YAML) at the manager's user-config target
// and returns that path.
func writeRawUserConfig(t *testing.T, m *Manager, raw map[string]any) string {
	t.Helper()
	path, err := m.userConfigTarget()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNormalizeModelLabel(t *testing.T) {
	cases := map[string]string{
		"gpt-5.5 (high)":         "GPT-5.5 High",
		"Opus 4.8 (high)":        "Opus 4.8 High",
		"Gemini 3.1 Pro (High)":  "Gemini 3.1 Pro High",
		"claude-opus-4-8 (high)": "Claude-opus-4-8 High",
		"":                       "",
	}
	for in, want := range cases {
		if got := normalizeModelLabel(in); got != want {
			t.Errorf("normalizeModelLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDescriptionDisplay(t *testing.T) {
	// Raw `-cli`/`-code` adapter keys are mapped to display names; generic words are untouched.
	if got := descriptionDisplay("Single-adapter smoke for codex-cli."); got != "Single-adapter smoke for Codex." {
		t.Errorf("descriptionDisplay = %q, want the codex-cli key mapped to Codex", got)
	}
	if got := descriptionDisplay("Built-in deterministic fake adapter (the safe default)."); got != "Built-in deterministic fake adapter (the safe default)." {
		t.Errorf("descriptionDisplay must not touch the word 'fake' (not an adapter-key token): %q", got)
	}
	// A key embedded in a LONGER identifier is not a standalone token → left untouched.
	if got := descriptionDisplay("codex-cli-compatible shim"); got != "codex-cli-compatible shim" {
		t.Errorf("descriptionDisplay must not mangle a key inside a longer token: %q", got)
	}
	// Token at start of string is still replaced.
	if got := descriptionDisplay("codex-cli reviewer"); got != "Codex reviewer" {
		t.Errorf("descriptionDisplay should map a leading standalone key: %q", got)
	}
}

func TestEditProfileLane_BlocksHigherLayerShadowedProfile(t *testing.T) {
	m := mutMgr(t)
	// A project-layer config defines the profile; a user-layer lane write would be shadowed by it.
	m.Cfg.Profiles["projprof"] = config.Profile{Lanes: map[string]config.Lane{
		"reviewer": {Execution: "adapter", Adapter: "codex-cli", Model: "codex-cli-default"},
	}}
	projPath := filepath.Join(t.TempDir(), "project.yaml")
	projRaw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "profiles": map[string]any{
		"projprof": map[string]any{"lanes": map[string]any{"reviewer": map[string]any{"execution": "adapter", "adapter": "codex-cli", "model": "codex-cli-default"}}},
	}})
	if err := os.WriteFile(projPath, projRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	m.Layers.ProjectPath, m.Layers.ProjectLoaded = projPath, true
	_, err := m.EditProfileLane("projprof", "reviewer", "codex-cli", LaneChoiceInput{Source: "adapter_default"}, nil)
	if err == nil {
		t.Fatal("editing a profile defined in a higher (project) layer must be blocked, not silently shadowed")
	}
	if !strings.Contains(err.Error(), "shadowed") {
		t.Errorf("block reason should explain shadowing: %v", err)
	}
}

func TestModelKeyCleanup_RenamesUserLayerGeneratedKey(t *testing.T) {
	m := mutMgr(t)
	const oldKey = "codex-cli-gpt-5.5-high"
	m.Cfg.ModelCatalog[oldKey] = genEntry()
	m.Cfg.Profiles["myprof"] = config.Profile{Lanes: map[string]config.Lane{
		"reviewer": {Execution: "adapter", Adapter: "codex-cli", Model: oldKey},
	}}
	path := writeRawUserConfig(t, m, map[string]any{
		"schemaVersion": 1,
		"modelCatalog": map[string]any{oldKey: map[string]any{
			"provider": "openai", "canonicalModel": "gpt-5.5", "effort": "high",
			"adapters": map[string]any{"codex-cli": map[string]any{"modelArg": "gpt-5.5"}},
		}},
		"profiles": map[string]any{"myprof": map[string]any{"lanes": map[string]any{
			"reviewer": map[string]any{"execution": "adapter", "adapter": "codex-cli", "model": oldKey},
		}}},
	})

	plan := m.PlanModelKeyCleanup()
	if len(plan.Renames) != 1 {
		t.Fatalf("want 1 rename, got %+v", plan)
	}
	if plan.Renames[0].OldKey != oldKey || plan.Renames[0].NewKey != "gpt-5.5-high" {
		t.Fatalf("rename = %+v, want codex-cli-gpt-5.5-high → gpt-5.5-high", plan.Renames[0])
	}
	if len(plan.Renames[0].UsedBy) != 1 || plan.Renames[0].UsedBy[0] != "myprof.reviewer" {
		t.Errorf("usedBy = %v, want [myprof.reviewer]", plan.Renames[0].UsedBy)
	}

	if _, err := m.ApplyModelKeyCleanup(ModelKeyCleanupConfirm()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The written user config: new key present, old key gone, lane repointed.
	raw, err := config.LoadRawMap(path)
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := raw["modelCatalog"].(map[string]any)
	if _, ok := cat["gpt-5.5-high"]; !ok {
		t.Errorf("new key gpt-5.5-high not written: %v", cat)
	}
	if _, ok := cat[oldKey]; ok {
		t.Errorf("old adapter-prefixed key still present: %v", cat)
	}
	pm, _ := raw["profiles"].(map[string]any)
	lane := pm["myprof"].(map[string]any)["lanes"].(map[string]any)["reviewer"].(map[string]any)
	if lane["model"] != "gpt-5.5-high" {
		t.Errorf("lane not repointed: model = %v", lane["model"])
	}
}

func TestProfileDisplayName(t *testing.T) {
	cases := map[string]string{
		"codex-cli-smoke":              "Codex smoke",
		"agy-cli-smoke":                "Antigravity smoke",
		"devin-cli-smoke":              "Devin smoke",
		"native-three-provider":        "Three-provider",
		"devin-gateway-three-provider": "Devin gateway",
		"fully-local-ollama":           "Fully local (Ollama)",
		"default":                      "Default",        // the built-in profile shows as "Default"
		"my-own-profile":               "my-own-profile", // user names pass through unchanged
	}
	for in, want := range cases {
		if got := ProfileDisplayName(in); got != want {
			t.Errorf("ProfileDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
	// No mapped example display name may itself contain a raw adapter key.
	for _, disp := range exampleProfileDisplayNames {
		for _, k := range []string{"codex-cli", "agy-cli", "devin-cli", "gemini-cli", "claude-code"} {
			if strings.Contains(disp, k) {
				t.Errorf("example display name %q leaks adapter key %q", disp, k)
			}
		}
	}
}

func TestModelKeyCleanup_TakesBackupBeforeWrite(t *testing.T) {
	m := mutMgr(t)
	const oldKey = "codex-cli-gpt-5.5-high"
	m.Cfg.ModelCatalog[oldKey] = genEntry()
	writeRawUserConfig(t, m, map[string]any{
		"schemaVersion": 1,
		"modelCatalog":  map[string]any{oldKey: map[string]any{"provider": "openai", "canonicalModel": "gpt-5.5", "effort": "high", "adapters": map[string]any{"codex-cli": map[string]any{"modelArg": "gpt-5.5"}}}},
	})
	res, err := m.ApplyModelKeyCleanup(ModelKeyCleanupConfirm())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Backup == "" {
		t.Fatal("apply must record a backup path")
	}
	if _, err := os.Stat(res.Backup); err != nil {
		t.Errorf("backup file not written at %q: %v", res.Backup, err)
	}
	if !strings.HasSuffix(res.Backup, ".bak") {
		t.Errorf("backup path %q should end in .bak", res.Backup)
	}
}

func TestModelKeyCleanup_ConfirmationRequired(t *testing.T) {
	m := mutMgr(t)
	m.Cfg.ModelCatalog["codex-cli-gpt-5.5-high"] = genEntry()
	writeRawUserConfig(t, m, map[string]any{"schemaVersion": 1, "modelCatalog": map[string]any{"codex-cli-gpt-5.5-high": map[string]any{
		"provider": "openai", "canonicalModel": "gpt-5.5", "effort": "high", "adapters": map[string]any{"codex-cli": map[string]any{"modelArg": "gpt-5.5"}}}}})
	if _, err := m.ApplyModelKeyCleanup("wrong"); err == nil {
		t.Error("apply without the exact confirmation must be rejected")
	}
}

func TestModelKeyCleanup_SkipsHigherLayerRef(t *testing.T) {
	m := mutMgr(t)
	const oldKey = "codex-cli-gpt-5.5-high"
	m.Cfg.ModelCatalog[oldKey] = genEntry()
	// A profile lane in the merged config references the key, but it is NOT in the user layer
	// (simulating a project/explicit-layer lane) — so the rename must be skipped, not applied.
	m.Cfg.Profiles["projprof"] = config.Profile{Lanes: map[string]config.Lane{
		"reviewer": {Execution: "adapter", Adapter: "codex-cli", Model: oldKey},
	}}
	writeRawUserConfig(t, m, map[string]any{ // user layer defines the CATALOG key but NOT the profile lane
		"schemaVersion": 1,
		"modelCatalog":  map[string]any{oldKey: map[string]any{"provider": "openai", "canonicalModel": "gpt-5.5", "effort": "high", "adapters": map[string]any{"codex-cli": map[string]any{"modelArg": "gpt-5.5"}}}},
	})
	plan := m.PlanModelKeyCleanup()
	if len(plan.Renames) != 0 {
		t.Errorf("a key referenced by a non-user-layer lane must NOT be auto-renamed, got %+v", plan.Renames)
	}
	if len(plan.Skipped) != 1 {
		t.Errorf("the key must be reported as skipped, got %+v", plan.Skipped)
	}
}

func TestModelKeyCleanup_SkipsDifferentContentCollision(t *testing.T) {
	m := mutMgr(t)
	const oldKey = "codex-cli-gpt-5.5-high"
	m.Cfg.ModelCatalog[oldKey] = genEntry()
	// The target key already exists with DIFFERENT content → must be skipped (no coalescing).
	m.Cfg.ModelCatalog["gpt-5.5-high"] = config.CatalogEntry{Provider: "openai", CanonicalModel: "gpt-4o", Adapters: map[string]config.AdapterModel{"codex-cli": {ModelArg: "gpt-4o"}}}
	writeRawUserConfig(t, m, map[string]any{
		"schemaVersion": 1,
		"modelCatalog": map[string]any{
			oldKey:         map[string]any{"provider": "openai", "canonicalModel": "gpt-5.5", "effort": "high", "adapters": map[string]any{"codex-cli": map[string]any{"modelArg": "gpt-5.5"}}},
			"gpt-5.5-high": map[string]any{"provider": "openai", "canonicalModel": "gpt-4o", "adapters": map[string]any{"codex-cli": map[string]any{"modelArg": "gpt-4o"}}},
		},
	})
	plan := m.PlanModelKeyCleanup()
	if len(plan.Renames) != 0 {
		t.Errorf("a different-content target collision must NOT be auto-renamed, got %+v", plan.Renames)
	}
	if len(plan.Skipped) != 1 {
		t.Errorf("the colliding key must be reported as skipped, got %+v", plan.Skipped)
	}
}

func TestModelKeyCleanup_SkipsSameTargetCollision(t *testing.T) {
	m := mutMgr(t)
	// Two generated keys for DIFFERENT adapters both strip to "gpt-5.5-high". Renaming both would
	// overwrite one entry — the second must be skipped (manual cleanup), not silently coalesced.
	m.Cfg.ModelCatalog["codex-cli-gpt-5.5-high"] = genEntry()
	m.Cfg.ModelCatalog["claude-code-gpt-5.5-high"] = config.CatalogEntry{
		Provider: "anthropic", CanonicalModel: "gpt-5.5", Effort: "high",
		Adapters: map[string]config.AdapterModel{"claude-code": {ModelArg: "gpt-5.5"}},
	}
	writeRawUserConfig(t, m, map[string]any{
		"schemaVersion": 1,
		"modelCatalog": map[string]any{
			"codex-cli-gpt-5.5-high":   map[string]any{"provider": "openai", "canonicalModel": "gpt-5.5", "effort": "high", "adapters": map[string]any{"codex-cli": map[string]any{"modelArg": "gpt-5.5"}}},
			"claude-code-gpt-5.5-high": map[string]any{"provider": "anthropic", "canonicalModel": "gpt-5.5", "effort": "high", "adapters": map[string]any{"claude-code": map[string]any{"modelArg": "gpt-5.5"}}},
		},
	})
	plan := m.PlanModelKeyCleanup()
	if len(plan.Renames) != 1 || len(plan.Skipped) != 1 {
		t.Fatalf("two keys mapping to the same target → exactly 1 rename + 1 skip, got renames=%+v skipped=%+v", plan.Renames, plan.Skipped)
	}
}

func TestModelKeyCleanup_SkipsKeyReferencedByProjectLayer(t *testing.T) {
	m := mutMgr(t)
	const oldKey = "codex-cli-gpt-5.5-high"
	m.Cfg.ModelCatalog[oldKey] = genEntry()
	writeRawUserConfig(t, m, map[string]any{ // user layer owns the generated catalog key
		"schemaVersion": 1,
		"modelCatalog":  map[string]any{oldKey: map[string]any{"provider": "openai", "canonicalModel": "gpt-5.5", "effort": "high", "adapters": map[string]any{"codex-cli": map[string]any{"modelArg": "gpt-5.5"}}}},
	})
	// A PROJECT-layer config references the same key in a lane — even if the user layer shadows it,
	// deleting the key would leave the project layer dangling. Must be skipped.
	projPath := filepath.Join(t.TempDir(), "project.yaml")
	projRaw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "profiles": map[string]any{
		"projprof": map[string]any{"lanes": map[string]any{"reviewer": map[string]any{"execution": "adapter", "adapter": "codex-cli", "model": oldKey}}},
	}})
	if err := os.WriteFile(projPath, projRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	m.Layers.ProjectPath, m.Layers.ProjectLoaded = projPath, true

	plan := m.PlanModelKeyCleanup()
	if len(plan.Renames) != 0 {
		t.Errorf("a key referenced by a project-layer lane must NOT be renamed, got %+v", plan.Renames)
	}
	if len(plan.Skipped) != 1 {
		t.Errorf("the key must be reported as skipped, got %+v", plan.Skipped)
	}
}

func TestModelKeyCleanup_LeavesAdapterDefaultsAlone(t *testing.T) {
	m := mutMgr(t) // the seed's *-default entries are adapterDefault/authoritative
	writeRawUserConfig(t, m, map[string]any{"schemaVersion": 1})
	plan := m.PlanModelKeyCleanup()
	if len(plan.Renames) != 0 {
		t.Errorf("shipped adapter-default keys (codex-cli-default, …) must never be renamed, got %+v", plan.Renames)
	}
}
