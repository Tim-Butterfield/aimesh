package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// okValidator accepts anything — the store's own tests exercise the mechanism, not a schema.
func okValidator(string, []byte) error { return nil }

func noTempLeftover(t *testing.T, dir string) {
	t.Helper()
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Errorf("atomic write left temp files: %v", leftovers)
	}
}

// WriteFileAtomic replaces the target in place and leaves no temp file.
func TestWriteFileAtomic_ReplacesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new")); err != nil {
		t.Fatalf("atomic write: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("content = %q, want new", got)
	}
	noTempLeftover(t, dir)
}

func TestPatch_Apply_SetsNestedAndPreservesUnrelated(t *testing.T) {
	root := map[string]any{
		"defaultProfile": "custom",
		"adapters":       map[string]any{"codex-cli": map[string]any{"path": "/old/codex"}},
	}
	p := Patch{Ops: []SetOp{{Path: []string{"adapters", "claude-code", "path"}, Value: "/bin/claude"}}}
	if err := p.Apply(root); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := root["adapters"].(map[string]any)["claude-code"].(map[string]any)["path"]; got != "/bin/claude" {
		t.Errorf("claude-code path = %v, want /bin/claude", got)
	}
	if root["defaultProfile"] != "custom" {
		t.Errorf("defaultProfile clobbered: %v", root["defaultProfile"])
	}
	if root["adapters"].(map[string]any)["codex-cli"].(map[string]any)["path"] != "/old/codex" {
		t.Error("unrelated adapter path lost")
	}
}

func TestPatch_Apply_ErrorsOnNonMapIntermediate(t *testing.T) {
	root := map[string]any{"adapters": "not-a-map"}
	p := Patch{Ops: []SetOp{{Path: []string{"adapters", "x", "path"}, Value: "/b"}}}
	if err := p.Apply(root); err == nil {
		t.Error("expected an error when an intermediate is not a mapping (must not overwrite)")
	}
	if root["adapters"] != "not-a-map" {
		t.Error("malformed shape must be left unchanged")
	}
}

func TestPatch_Delete_RemovesLeafAndSubtree(t *testing.T) {
	root := map[string]any{"adapters": map[string]any{
		"codex-cli": map[string]any{"path": "/bin/codex"},
		"fake":      map[string]any{"detect": "fake"},
	}}
	p := Patch{Ops: []SetOp{{Path: []string{"adapters", "codex-cli"}, Delete: true}}}
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

func TestPatch_EmptyIsNoOp(t *testing.T) {
	if !(Patch{}).IsEmpty() {
		t.Error("zero Patch should be empty")
	}
}

// ApplyPatchToFile runs the existing + written bytes through the injected validator and preserves
// unrelated fields; a validator rejection on the existing file leaves it byte-for-byte unchanged.
func TestApplyPatchToFile_ValidatesAndPreserves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("schemaVersion: 1\ndefaultProfile: custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPatchToFile(p, Patch{Ops: []SetOp{{Path: []string{"adapters", "claude-code", "path"}, Value: "/bin/claude"}}}, okValidator); err != nil {
		t.Fatalf("patch: %v", err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "/bin/claude") || !strings.Contains(string(b), "custom") {
		t.Errorf("patch result wrong (want new path + preserved profile):\n%s", b)
	}
}

func TestApplyPatchToFile_RejectingValidatorLeavesUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	orig := "schemaVersion: 1\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	reject := func(string, []byte) error { return os.ErrInvalid }
	if err := ApplyPatchToFile(p, Patch{Ops: []SetOp{{Path: []string{"x"}, Value: "y"}}}, reject); err == nil {
		t.Fatal("a rejecting validator must refuse the write")
	}
	if got, _ := os.ReadFile(p); string(got) != orig {
		t.Errorf("rejected file was modified:\n%s", got)
	}
}
