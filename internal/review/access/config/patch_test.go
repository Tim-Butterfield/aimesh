package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPatch_Delete_RemovesLeafAndSubtree(t *testing.T) {
	root := map[string]any{
		"adapters": map[string]any{
			"codex-cli": map[string]any{"path": "/bin/codex"},
			"fake":      map[string]any{"modelIdentity": "self_report"},
		},
	}
	p := ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "codex-cli"}, Delete: true}}}
	if err := p.Apply(root); err != nil {
		t.Fatalf("delete apply: %v", err)
	}
	adapters := root["adapters"].(map[string]any)
	if _, ok := adapters["codex-cli"]; ok {
		t.Error("codex-cli subtree should have been deleted")
	}
	if _, ok := adapters["fake"]; !ok {
		t.Error("delete must preserve sibling keys")
	}
}

func TestConfigPatch_Delete_AbsentPathIsNoOp(t *testing.T) {
	root := map[string]any{"schemaVersion": 1}
	// deleting under a missing parent must not create maps nor error
	p := ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "nope"}, Delete: true}}}
	if err := p.Apply(root); err != nil {
		t.Fatalf("no-op delete errored: %v", err)
	}
	if _, ok := root["adapters"]; ok {
		t.Error("a no-op delete must not create the intermediate `adapters` map")
	}
}

func TestApplyPatchToFile_Delete_RemovesAdapterKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("schemaVersion: 1\ndefaultProfile: custom\nadapters:\n  codex-cli:\n    path: /bin/codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPatchToFile(p, ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "codex-cli"}, Delete: true}}}); err != nil {
		t.Fatalf("delete patch: %v", err)
	}
	// Inspect the RAW written file (config.Load would re-merge the shipped seed, which has its
	// own codex-cli). The user-layer key must be gone.
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "codex-cli") {
		t.Errorf("codex-cli should be gone from the written config:\n%s", b)
	}
	if cfg := loadCfg(t, p); cfg.DefaultProfile != "custom" {
		t.Errorf("unrelated field clobbered: defaultProfile=%q", cfg.DefaultProfile)
	}
}

func TestApplyPatchToFile_JSONTargetStaysJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"schemaVersion":1,"defaultProfile":"custom"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPatchToFile(p, ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "claude-code", "path"}, Value: "/bin/claude"}}}); err != nil {
		t.Fatalf("patch .json: %v", err)
	}
	b, _ := os.ReadFile(p)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Errorf("a .json target must remain valid JSON after patch: %v\n%s", err, b)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Error("patching a .json target must keep the trailing newline (match WriteJSONFile)")
	}
	cfg := loadCfg(t, p)
	if cfg.DefaultProfile != "custom" || cfg.Adapters["claude-code"].Path != "/bin/claude" {
		t.Errorf("json patch result wrong: profile=%q claude=%q", cfg.DefaultProfile, cfg.Adapters["claude-code"].Path)
	}
}

func TestApplyPatchToFile_PreservesUnrelatedAndCreatesAbsent(t *testing.T) {
	// absent → creates
	dir1 := t.TempDir()
	p1 := filepath.Join(dir1, "config.yaml")
	if err := ApplyPatchToFile(p1, ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "claude-code", "path"}, Value: "/bin/claude"}}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := loadCfg(t, p1).Adapters["claude-code"].Path; got != "/bin/claude" {
		t.Errorf("created path = %q", got)
	}
	// atomic write must leave no temp file behind
	if leftovers, _ := filepath.Glob(filepath.Join(dir1, "*.tmp")); len(leftovers) != 0 {
		t.Errorf("atomic write left temp files: %v", leftovers)
	}
	// existing valid → patch preserves unrelated fields
	p2 := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p2, []byte("schemaVersion: 1\ndefaultProfile: custom\nadapters:\n  codex-cli:\n    path: /old/codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPatchToFile(p2, ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "claude-code", "path"}, Value: "/bin/claude"}}}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	cfg := loadCfg(t, p2)
	if cfg.DefaultProfile != "custom" {
		t.Errorf("defaultProfile clobbered: %q", cfg.DefaultProfile)
	}
	if cfg.Adapters["codex-cli"].Path != "/old/codex" {
		t.Error("unrelated adapter path lost")
	}
	if cfg.Adapters["claude-code"].Path != "/bin/claude" {
		t.Errorf("new path not set: %q", cfg.Adapters["claude-code"].Path)
	}
}

func TestApplyPatchToFile_RefusesUnparseableAndLeavesUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	bad := "schemaVersion: 1\nbogusUnknownField: true\n" // strict parse rejects unknown fields
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPatchToFile(p, ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "x", "path"}, Value: "/b"}}}); err == nil {
		t.Fatal("must refuse an unparseable existing config")
	}
	if got, _ := os.ReadFile(p); string(got) != bad {
		t.Errorf("unparseable file modified:\n%s", got)
	}
}

func TestApplyPatchToFile_RefusesNonMapShapeAndLeavesUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	// YAML-valid but adapters is a scalar → patch must error, not clobber
	bad := "schemaVersion: 1\nadapters: not-a-map\n"
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPatchToFile(p, ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "x", "path"}, Value: "/b"}}}); err == nil {
		t.Fatal("must refuse a non-map intermediate shape")
	}
	if got, _ := os.ReadFile(p); string(got) != bad {
		t.Errorf("malformed-shape file modified:\n%s", got)
	}
}

func loadCfg(t *testing.T, path string) Config {
	t.Helper()
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load %q: %v", path, err)
	}
	return c
}

func TestConfigPatch_Apply_SetsNestedAndPreservesUnrelated(t *testing.T) {
	root := map[string]any{
		"schemaVersion":  1,
		"defaultProfile": "custom",
		"adapters": map[string]any{
			"codex-cli": map[string]any{"path": "/old/codex"},
		},
	}
	p := ConfigPatch{Ops: []SetOp{
		{Path: []string{"adapters", "claude-code", "path"}, Value: "/bin/claude"},
	}}
	if err := p.Apply(root); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// new nested key created
	got := root["adapters"].(map[string]any)["claude-code"].(map[string]any)["path"]
	if got != "/bin/claude" {
		t.Errorf("claude-code path = %v, want /bin/claude", got)
	}
	// unrelated fields preserved
	if root["defaultProfile"] != "custom" {
		t.Errorf("defaultProfile clobbered: %v", root["defaultProfile"])
	}
	if root["adapters"].(map[string]any)["codex-cli"].(map[string]any)["path"] != "/old/codex" {
		t.Error("unrelated adapter path lost")
	}
}

func TestConfigPatch_Apply_OverwritesLeafKeepsSiblings(t *testing.T) {
	root := map[string]any{"adapters": map[string]any{"x": map[string]any{"path": "/a", "keep": true}}}
	p := ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "x", "path"}, Value: "/b"}}}
	if err := p.Apply(root); err != nil {
		t.Fatal(err)
	}
	x := root["adapters"].(map[string]any)["x"].(map[string]any)
	if x["path"] != "/b" {
		t.Errorf("path = %v, want /b", x["path"])
	}
	if x["keep"] != true {
		t.Error("sibling key lost")
	}
}

func TestConfigPatch_Apply_ErrorsOnNonMapIntermediate(t *testing.T) {
	root := map[string]any{"adapters": "not-a-map"}
	p := ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "x", "path"}, Value: "/b"}}}
	if err := p.Apply(root); err == nil {
		t.Error("expected an error when an intermediate is not a mapping (must not overwrite)")
	}
	if root["adapters"] != "not-a-map" {
		t.Error("malformed shape must be left unchanged")
	}
}

func TestConfigPatch_Apply_TypedNilIntermediateDoesNotPanic(t *testing.T) {
	// a typed-nil map passes the map[string]any assertion but cannot be written to
	root := map[string]any{"adapters": map[string]any(nil)}
	p := ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "claude-code", "path"}, Value: "/bin/claude"}}}
	if err := p.Apply(root); err != nil {
		t.Fatalf("apply over a typed-nil intermediate should succeed, got %v", err)
	}
	got := root["adapters"].(map[string]any)["claude-code"].(map[string]any)["path"]
	if got != "/bin/claude" {
		t.Errorf("path = %v, want /bin/claude", got)
	}
}

func TestConfigPatch_EmptyIsNoOp(t *testing.T) {
	if !(ConfigPatch{}).IsEmpty() {
		t.Error("zero ConfigPatch should be empty")
	}
	root := map[string]any{"a": 1}
	if err := (ConfigPatch{}).Apply(root); err != nil {
		t.Fatal(err)
	}
	if len(root) != 1 || root["a"] != 1 {
		t.Error("empty patch must change nothing")
	}
}
