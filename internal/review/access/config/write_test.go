package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Generated config artifacts are deterministically LF (no CRLF), regardless of host OS —
// they come from yaml/json marshalers + writeFileAtomic, which never inject CRLF.
func TestConfigWrites_AreLF_NoCRLF(t *testing.T) {
	cases := map[string]func(string) error{
		"config.yaml": func(p string) error { return Default().WriteYAMLFile(p) },
		"config.json": func(p string) error { return Default().WriteJSONFile(p) },
		"patched.yaml": func(p string) error {
			return ApplyPatchToFile(p, ConfigPatch{Ops: []SetOp{{Path: []string{"adapters", "claude-code", "path"}, Value: "/bin/claude"}}})
		},
	}
	for name, write := range cases {
		p := filepath.Join(t.TempDir(), name)
		if err := write(p); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("\r\n")) {
			t.Errorf("%s: generated config must be LF, found CRLF", name)
		}
		if !bytes.Contains(b, []byte("\n")) {
			t.Errorf("%s: expected LF-separated content", name)
		}
	}
}

func noTempLeftover(t *testing.T, dir string) {
	t.Helper()
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Errorf("atomic write left temp files: %v", leftovers)
	}
}

func TestWriteYAMLFile_AtomicLoadableNoTempCreatesDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", ".aimesh", "review") // parent dirs must be created
	path := filepath.Join(dir, "config.yaml")
	if err := Default().WriteYAMLFile(path); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("written YAML not loadable: %v", err)
	}
	if cfg.DefaultProfile != "default" {
		t.Errorf("round-trip defaultProfile = %q, want default", cfg.DefaultProfile)
	}
	noTempLeftover(t, dir)
}

func TestWriteJSONFile_AtomicLoadableNoTempTrailingNewline(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", ".aimesh", "review")
	path := filepath.Join(dir, "config.json")
	if err := Default().WriteJSONFile(path); err != nil {
		t.Fatalf("write json: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Error("WriteJSONFile must preserve the trailing newline")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("written JSON not loadable: %v", err)
	}
	if cfg.DefaultProfile != "default" {
		t.Errorf("round-trip defaultProfile = %q, want default", cfg.DefaultProfile)
	}
	noTempLeftover(t, dir)
}
