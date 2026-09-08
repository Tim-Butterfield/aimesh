package setup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/utility/doctor"
	"github.com/Tim-Butterfield/aimesh/internal/review/utility/prompt"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
	"gopkg.in/yaml.v3"
)

func TestRepair_MissingAdapterGuidanceIncludesSetupCommand(t *testing.T) {
	var buf bytes.Buffer
	m := &Manager{Adapters: map[string]model.Adapter{"claude-code": fake.New(fake.Valid)}, Out: &buf}
	rep := doctor.Report{OK: false, Checks: []doctor.Check{
		{Name: "adapter: claude-code", OK: false, Detail: `binary "claude" not found on PATH`},
	}}
	m.Repair(t.TempDir(), rep, nil)
	if !strings.Contains(buf.String(), "reviewmesh setup --adapter claude-code --path") {
		t.Errorf("repair should print the exact setup command:\n%s", buf.String())
	}
}

func mgr() *Manager {
	return &Manager{Adapters: map[string]model.Adapter{"fake": fake.New(fake.Valid)}, Out: io.Discard}
}

// mgrWithAdapters registers configurable adapter names (backed by fakes) so the
// name-validation in SetAdapterPath accepts them.
func mgrWithAdapters() *Manager {
	return &Manager{Adapters: map[string]model.Adapter{
		"fake":        fake.New(fake.Valid),
		"claude-code": fake.New(fake.Valid),
		"codex-cli":   fake.New(fake.Valid),
	}, Out: io.Discard}
}

func execFile(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSetAdapterPath_WritesYAML(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir()) // isolate the user-scope shared adapters.yaml
	bin := execFile(t, "claude")
	res, err := mgrWithAdapters().SetAdapterPath(t.TempDir(), "claude-code", bin)
	if err != nil {
		t.Fatalf("set adapter path: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatal("expected config written")
	}
	// The binary path is recorded in the user-scope shared adapters.yaml, not config.yaml.
	if got := sharedAdapterPath(t, userSharedPath(t), "claude-code"); got == nil || *got != bin {
		t.Errorf("path = %v, want %q", got, bin)
	}
}

func TestSetAdapterPath_PreservesExistingAndUpdates(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir())
	shared := userSharedPath(t)
	// a pre-existing shared adapters.yaml with an unrelated codex path
	if err := config.SetSharedAdapterPath(shared, "codex-cli", "/old/codex"); err != nil {
		t.Fatalf("seed shared: %v", err)
	}
	bin := execFile(t, "claude")
	if _, err := mgrWithAdapters().SetAdapterPath(t.TempDir(), "claude-code", bin); err != nil {
		t.Fatalf("set adapter path: %v", err)
	}
	if got := sharedAdapterPath(t, shared, "codex-cli"); got == nil || *got != "/old/codex" {
		t.Errorf("unrelated adapter path lost: %v", got)
	}
	if got := sharedAdapterPath(t, shared, "claude-code"); got == nil || *got != bin {
		t.Errorf("new path not set: %v", got)
	}
	// update again → last value wins, no corruption
	bin2 := execFile(t, "claude2")
	if _, err := mgrWithAdapters().SetAdapterPath(t.TempDir(), "claude-code", bin2); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := sharedAdapterPath(t, shared, "claude-code"); got == nil || *got != bin2 {
		t.Errorf("update path = %v, want %q", got, bin2)
	}
}

// SetAdapterPath must produce the SAME shared-file result as a direct config.SetSharedAdapterPath —
// the manager owns validation + the legacy strip, but the actual path write is the shared-file seam,
// with no divergent write logic. (Adapter binary paths no longer go through the config.yaml patch
// path; that engine primitive is covered by the engine's own tests.)
func TestSetAdapterPath_MatchesDirectSharedWrite(t *testing.T) {
	bin := execFile(t, "claude")

	// (A) via the manager (user scope → AIMESH_HOME's shared adapters.yaml)
	homeA := t.TempDir()
	t.Setenv("AIMESH_HOME", homeA)
	sharedA := userSharedPath(t)
	if err := config.SetSharedAdapterPath(sharedA, "codex-cli", "/old/codex"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := mgrWithAdapters().SetAdapterPath(t.TempDir(), "claude-code", bin); err != nil {
		t.Fatalf("manager SetAdapterPath: %v", err)
	}

	// (B) via the direct shared-write primitive onto an identical baseline
	sharedB := filepath.Join(t.TempDir(), ".aimesh", "adapters.yaml")
	if err := config.SetSharedAdapterPath(sharedB, "codex-cli", "/old/codex"); err != nil {
		t.Fatalf("seed B: %v", err)
	}
	if err := config.SetSharedAdapterPath(sharedB, "claude-code", bin); err != nil {
		t.Fatalf("direct write: %v", err)
	}

	for _, name := range []string{"claude-code", "codex-cli"} {
		a, b := sharedAdapterPath(t, sharedA, name), sharedAdapterPath(t, sharedB, name)
		if a == nil || b == nil || *a != *b {
			t.Errorf("%s: manager=%v direct=%v diverge", name, a, b)
		}
	}
	if got := sharedAdapterPath(t, sharedA, "claude-code"); got == nil || *got != bin {
		t.Errorf("claude-code path = %v, want %q", got, bin)
	}
}

func loadCfgT(t *testing.T, path string) config.Config {
	t.Helper()
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("load %q: %v", path, err)
	}
	return c
}

func TestSetAdapterPath_RejectsUnknownAndFake(t *testing.T) {
	base := t.TempDir()
	bin := execFile(t, "x")
	if _, err := mgrWithAdapters().SetAdapterPath(base, "bogus", bin); err == nil {
		t.Error("unknown adapter should be rejected")
	}
	if _, err := mgrWithAdapters().SetAdapterPath(base, "fake", bin); err == nil {
		t.Error("fake should not be path-configurable")
	}
	if _, err := os.Stat(config.ProjectConfigPath(base)); !os.IsNotExist(err) {
		t.Error("no config should be written on validation failure")
	}
}

func TestSetAdapterPath_RejectsBadPaths(t *testing.T) {
	base := t.TempDir()
	if _, err := mgrWithAdapters().SetAdapterPath(base, "claude-code", ""); err == nil {
		t.Error("empty path should be rejected")
	}
	if _, err := mgrWithAdapters().SetAdapterPath(base, "claude-code", t.TempDir()); err == nil {
		t.Error("directory path should be rejected")
	}
	if _, err := mgrWithAdapters().SetAdapterPath(base, "claude-code", "/no/such/bin"); err == nil {
		t.Error("missing path should be rejected")
	}
	if runtime.GOOS != "windows" {
		ne := filepath.Join(t.TempDir(), "noexec")
		if err := os.WriteFile(ne, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := mgrWithAdapters().SetAdapterPath(base, "claude-code", ne); err == nil {
			t.Error("non-executable path should be rejected")
		}
	}
}

func TestSetAdapterPath_NoPartialCorruption(t *testing.T) {
	base := t.TempDir()
	path := config.ProjectConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "schemaVersion: 1\ndefaultProfile: custom\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgrWithAdapters().SetAdapterPath(base, "claude-code", "/no/such/bin"); err == nil {
		t.Fatal("expected failure on invalid path")
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("config was modified on a failed update:\n%s", got)
	}
}

func TestSetup_NonInteractive_WritesProjectConfig(t *testing.T) {
	base := t.TempDir()
	res, err := mgr().Setup(base, nil) // non-interactive
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatal("expected config to be written")
	}
	path := config.ProjectConfigPath(base)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("written config not loadable: %v", err)
	}
	if cfg.DefaultProfile != "default" {
		t.Errorf("defaultProfile = %q, want default", cfg.DefaultProfile)
	}
}

func TestSetup_CancelViaPrompter_WritesNothing(t *testing.T) {
	base := t.TempDir()
	ask := &prompt.Scripted{Confirms: []bool{false}} // decline the write
	res, err := mgr().Setup(base, ask)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if res.ConfigWritten {
		t.Error("config should not be written when the user declines")
	}
	if _, err := os.Stat(config.ProjectConfigPath(base)); !os.IsNotExist(err) {
		t.Error("no config file should exist after cancel")
	}
}

func TestSetup_PreservesExistingConfig(t *testing.T) {
	base := t.TempDir()
	path := config.ProjectConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"defaultProfile":"custom"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := mgr().Setup(base, nil)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if res.ConfigWritten {
		t.Error("an existing project config must be preserved, not overwritten")
	}
	cfg, _ := config.Load(path)
	if cfg.DefaultProfile != "custom" {
		t.Errorf("existing config was clobbered: defaultProfile = %q", cfg.DefaultProfile)
	}
}

func TestSetup_ConfirmViaPrompter_Writes(t *testing.T) {
	base := t.TempDir()
	ask := &prompt.Scripted{Confirms: []bool{true}}
	res, err := mgr().Setup(base, ask)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !res.ConfigWritten {
		t.Error("config should be written when the user confirms")
	}
}

// SetupLocalOllama creates when absent and preserves an existing valid config (it shares
// writeConfigIfAbsent with plain Setup) — current behavior, not the target wizard.
func TestSetupLocalOllama_AbsentCreates_ExistingPreserved(t *testing.T) {
	// absent → creates a config with the fully-local-ollama profile available
	base := t.TempDir()
	res, err := mgr().SetupLocalOllama(base, "qwen2.5-coder:14b", nil)
	if err != nil || !res.ConfigWritten {
		t.Fatalf("expected create when absent: err=%v res=%+v", err, res)
	}
	cfgCreated, err := config.Load(config.ProjectConfigPath(base))
	if err != nil {
		t.Fatalf("written config not loadable: %v", err)
	}
	if _, ok := cfgCreated.Profiles["fully-local-ollama"]; !ok {
		t.Error("written config should include the fully-local-ollama profile")
	}
	// existing valid → preserved unchanged
	base2 := t.TempDir()
	path := config.ProjectConfigPath(base2)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"defaultProfile":"custom"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := mgr().SetupLocalOllama(base2, "qwen2.5-coder:14b", nil)
	if err != nil {
		t.Fatalf("setup local ollama: %v", err)
	}
	if res2.ConfigWritten {
		t.Error("an existing valid config must be preserved, not overwritten")
	}
	cfgKept, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfgKept.DefaultProfile != "custom" {
		t.Errorf("existing config clobbered: defaultProfile = %q", cfgKept.DefaultProfile)
	}
}

// SetAdapterPath writes the binary path to the clean shared adapters.yaml even when the user-scope
// legacy config.yaml is unparseable: it never writes INTO config.yaml, and the cosmetic legacy strip
// is best-effort — it skips (and never rewrites) an unreadable legacy file, leaving it byte-for-byte
// unchanged.
func TestSetAdapterPath_UnparseableLegacyLeftUntouched(t *testing.T) {
	reviewHome := t.TempDir()
	t.Setenv("AIMESH_HOME", reviewHome)
	legacy := config.ProjectConfigPath(reviewHome) // ~/.reviewmesh/config.yaml (user scope)
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := "schemaVersion: 1\nbogusUnknownField: true\n" // strict parse rejects unknown fields
	if err := os.WriteFile(legacy, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := execFile(t, "claude")
	if _, err := mgrWithAdapters().SetAdapterPath(t.TempDir(), "claude-code", bin); err != nil {
		t.Fatalf("SetAdapterPath should still write the shared file: %v", err)
	}
	if got := sharedAdapterPath(t, userSharedPath(t), "claude-code"); got == nil || *got != bin {
		t.Errorf("path not recorded in the shared file: %v", got)
	}
	if got, _ := os.ReadFile(legacy); string(got) != bad {
		t.Errorf("unparseable legacy config was modified:\n%s", got)
	}
}

// Plain Setup must not overwrite an existing file even if it does not parse (it preserves
// whatever is there rather than clobbering it).
func TestSetup_PreservesExistingEvenIfUnparseable(t *testing.T) {
	base := t.TempDir()
	path := config.ProjectConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := "bogusUnknownField: true\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := mgr().Setup(base, nil)
	if err != nil {
		t.Fatalf("setup should not error on a pre-existing file: %v", err)
	}
	if res.ConfigWritten {
		t.Error("setup must not overwrite an existing config")
	}
	if got, _ := os.ReadFile(path); string(got) != bad {
		t.Errorf("existing file modified:\n%s", got)
	}
}

// The config written by setup/path-capture must never contain secret-bearing
// material — reviewmesh keeps authentication with each adapter's own CLI (RB).
// It inspects config KEYS (recursively), not raw bytes, so a legitimate path
// value never trips the check.
func TestSetup_WritesNoSecrets(t *testing.T) {
	secretKeys := []string{"token", "secret", "password", "passwd", "apikey", "api_key", "authorization", "bearer", "credential", "private_key", "privatekey"}
	var walk func(t *testing.T, path string, v any)
	walk = func(t *testing.T, path string, v any) {
		t.Helper()
		switch m := v.(type) {
		case map[string]any:
			for k, child := range m {
				lk := strings.ToLower(k)
				for _, s := range secretKeys {
					if strings.Contains(lk, s) {
						t.Errorf("written config %q has a secret-bearing key %q", path, k)
					}
				}
				walk(t, path, child)
			}
		case []any:
			for _, child := range m {
				walk(t, path, child)
			}
		}
	}
	assertClean := func(t *testing.T, path string) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := yaml.Unmarshal(raw, &doc); err != nil { // YAML superset also parses the JSON form
			t.Fatalf("parse %q: %v", path, err)
		}
		walk(t, path, doc)
	}

	// (a) default setup
	base := t.TempDir()
	if _, err := mgr().Setup(base, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	assertClean(t, config.ProjectConfigPath(base))

	// (b) adapter-path capture → the shared adapters.yaml (path-only; never secrets)
	t.Setenv("AIMESH_HOME", t.TempDir())
	if _, err := mgrWithAdapters().SetAdapterPath(t.TempDir(), "claude-code", execFile(t, "claude")); err != nil {
		t.Fatalf("set adapter path: %v", err)
	}
	assertClean(t, userSharedPath(t))

	// (c) local-ollama
	base3 := t.TempDir()
	if _, err := mgr().SetupLocalOllama(base3, "qwen2.5-coder:14b", nil); err != nil {
		t.Fatalf("setup local ollama: %v", err)
	}
	assertClean(t, config.ProjectConfigPath(base3))
}

// profileIndex returns the position of a profile name in the wizard's sorted profile list
// (so scripted Choose answers stay correct even if the seed profile set changes).
func profileIndex(t *testing.T, name string) int {
	t.Helper()
	names := make([]string, 0)
	for n := range config.Default().Profiles {
		// The wizard hides the shipped-but-hidden fake profile from the choices it offers, so mirror
		// that here (its index must not shift the offered list).
		if config.IsHiddenProfile(n) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		if n == name {
			return i
		}
	}
	t.Fatalf("profile %q not found in seed", name)
	return -1
}

// Wizard: accept defaults (configure the `default` profile, record no adapter paths, confirm)
// writes a loadable config whose defaultProfile is the fixed `default`.
func TestSetupWizard_AcceptDefaultsWrites(t *testing.T) {
	base := t.TempDir()
	ask := &prompt.Scripted{Choices: []int{profileIndex(t, "default")}, Confirms: []bool{true}} // fake has no configurable adapters → no record loop; confirm write
	res, err := mgr().SetupWizard(base, ask)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatal("expected config written")
	}
	cfg := loadCfgT(t, config.ProjectConfigPath(base))
	if cfg.DefaultProfile != "default" {
		t.Errorf("defaultProfile = %q, want default", cfg.DefaultProfile)
	}
}

// Wizard: choosing a profile CONFIGURES that profile — it never repoints defaultProfile
// (the one default profile is the profile named `default`; changing the effective default
// means editing/copying into `default`, the same fixed semantics as the web UI).
func TestSetupWizard_ProfileChoiceDoesNotRepointDefault(t *testing.T) {
	base := t.TempDir()
	// `default` is the only profile the wizard offers to configure (the shipped fake profile is
	// hidden); choosing it must still never repoint defaultProfile.
	ask := &prompt.Scripted{Choices: []int{profileIndex(t, "default")}, Confirms: []bool{true}}
	if _, err := mgr().SetupWizard(base, ask); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	if cfg := loadCfgT(t, config.ProjectConfigPath(base)); cfg.DefaultProfile != "default" {
		t.Errorf("defaultProfile = %q — the wizard must never repoint it away from `default`", cfg.DefaultProfile)
	}
}

// Wizard: declining the final confirm writes nothing.
func TestSetupWizard_CancelWritesNothing(t *testing.T) {
	base := t.TempDir()
	ask := &prompt.Scripted{Choices: []int{profileIndex(t, "default")}, Confirms: []bool{false}} // decline write
	res, err := mgr().SetupWizard(base, ask)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	if res.ConfigWritten {
		t.Error("config must not be written when the user declines")
	}
	if _, err := os.Stat(config.ProjectConfigPath(base)); !os.IsNotExist(err) {
		t.Error("no config file should exist after cancel")
	}
}

// Wizard: record a valid adapter path; it is written under adapters.<name>.path.
func TestSetupWizard_RecordsAdapterPath(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir())
	base := t.TempDir()
	bin := execFile(t, "claude")
	ask := &prompt.Scripted{
		Choices:  []int{profileIndex(t, "default"), 0}, // profile=default; adapter index 0 = claude-code (sorted configurable)
		Answers:  []string{bin},
		Confirms: []bool{true, false, true}, // record? yes → (set path) → record again? no → write? yes
	}
	if _, err := mgrWithAdapters().SetupWizard(base, ask); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	// The adapter path goes to the shared adapters.yaml, not config.yaml.
	if got := sharedAdapterPath(t, userSharedPath(t), "claude-code"); got == nil || *got != bin {
		t.Errorf("claude-code path = %v, want %q", got, bin)
	}
}

// Wizard: an invalid path is rejected (not recorded), and the wizard still completes.
func TestSetupWizard_InvalidPathRejectedNotRecorded(t *testing.T) {
	base := t.TempDir()
	ask := &prompt.Scripted{
		Choices:  []int{profileIndex(t, "default"), 0},
		Answers:  []string{"/no/such/binary"}, // invalid
		Confirms: []bool{true, false, true},
	}
	if _, err := mgrWithAdapters().SetupWizard(base, ask); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	cfg := loadCfgT(t, config.ProjectConfigPath(base))
	if cfg.Adapters["claude-code"].Path != "" {
		t.Errorf("invalid path should not be recorded, got %q", cfg.Adapters["claude-code"].Path)
	}
}

// Wizard: an existing config is surgically patched — only the new adapter path changes;
// every unrelated field is preserved, INCLUDING a legacy defaultProfile pointer (the wizard
// never writes defaultProfile; a stale pointer is repaired only via the explicit, confirmed
// web-UI/doctor repair, never silently by another flow).
func TestSetupWizard_PreservesExistingConfig(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir())
	base := t.TempDir()
	path := config.ProjectConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("schemaVersion: 1\ndefaultProfile: fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := execFile(t, "claude")
	ask := &prompt.Scripted{
		Choices:  []int{profileIndex(t, "default"), 0}, // configure the (only) `default` profile; record claude-code
		Answers:  []string{bin},
		Confirms: []bool{true, false, true},
	}
	if _, err := mgrWithAdapters().SetupWizard(base, ask); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	// The wizard records the path in the shared file and leaves the existing config.yaml (its
	// legacy defaultProfile pointer especially) untouched — it never writes defaultProfile.
	cfg := loadCfgT(t, path)
	if cfg.DefaultProfile != "fake" {
		t.Errorf("defaultProfile = %q — the wizard must leave the existing (even legacy) pointer untouched", cfg.DefaultProfile)
	}
	if got := sharedAdapterPath(t, userSharedPath(t), "claude-code"); got == nil || *got != bin {
		t.Errorf("claude-code path = %v, want %q", got, bin)
	}
}

// Wizard: when the catalog has >=2 entries for a lane's adapter, the user picks one and it is
// patched into profiles.<p>.lanes.<role>.model (unrelated lanes/fields preserved). With the
// shipped one-entry-per-adapter catalog there is no such prompt (covered by the other wizard
// tests, whose Confirms sequences contain no model-choice step).
func TestSetupWizard_SelectsLaneModelFromCatalog(t *testing.T) {
	base := t.TempDir()
	path := config.ProjectConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DefaultProfile = "cc-review"
	// A SECOND catalog entry for claude-code so the lane has a real choice.
	cfg.ModelCatalog["claude-code-opus"] = config.CatalogEntry{
		Provider: "anthropic", CanonicalModel: "claude-opus", DisplayName: "Claude Opus (via claude-code)",
		Adapters: map[string]config.AdapterModel{"claude-code": {ModelArg: "opus"}},
	}
	cfg.Profiles["cc-review"] = config.Profile{
		Description:       "review via claude-code",
		AdapterPreference: []string{"claude-code"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
			"reviewer":          {Execution: "adapter", Adapter: "claude-code", Model: "claude-code-default"},
		},
	}
	if err := cfg.WriteYAMLFile(path); err != nil {
		t.Fatal(err)
	}
	// Index of "cc-review" among the loaded config's sorted profile names.
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	ccIdx := -1
	for i, n := range names {
		if n == "cc-review" {
			ccIdx = i
		}
	}
	// keep cc-review default; decline path record; choose model (sorted keys:
	// claude-code-default[0], claude-code-opus[1]) → opus; confirm write.
	ask := &prompt.Scripted{
		Choices:  []int{ccIdx, 1},
		Confirms: []bool{false, true, true},
	}
	if _, err := mgrWithAdapters().SetupWizard(base, ask); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got := loadCfgT(t, path)
	if m := got.Profiles["cc-review"].Lanes["reviewer"].Model; m != "claude-code-opus" {
		t.Errorf("reviewer lane model = %q, want claude-code-opus", m)
	}
	if a := got.Profiles["cc-review"].Lanes["reviewer"].Adapter; a != "claude-code" {
		t.Errorf("reviewer lane adapter = %q, want claude-code (unchanged)", a)
	}
	if got.Profiles["cc-review"].Lanes["author_remediator"].Model != "fake-model" {
		t.Error("unrelated author_remediator lane must be preserved")
	}
}

func TestCatalogKeysForAdapterAndLabel(t *testing.T) {
	cfg := config.Default()
	cfg.ModelCatalog["claude-code-opus"] = config.CatalogEntry{
		Provider: "anthropic", DisplayName: "Claude Opus", Effort: "high",
		Adapters: map[string]config.AdapterModel{"claude-code": {ModelArg: "opus"}},
	}
	keys := catalogKeysForAdapter(cfg, "claude-code")
	if len(keys) != 2 || keys[0] != "claude-code-default" || keys[1] != "claude-code-opus" {
		t.Errorf("claude-code catalog keys = %v, want [claude-code-default claude-code-opus]", keys)
	}
	if catalogKeysForAdapter(cfg, "") != nil {
		t.Error("empty adapter should yield no keys")
	}
	if got := catalogLabel(cfg, "claude-code-opus"); got != "Claude Opus [high] (claude-code-opus)" {
		t.Errorf("label = %q", got)
	}
	if got := catalogLabel(cfg, "no-such-key"); got != "no-such-key" {
		t.Errorf("unknown key label = %q, want the key itself", got)
	}
}

// Wizard: refuse to write over an existing config that does not parse strictly; leave it
// byte-for-byte unchanged.
func TestSetupWizard_RefusesUnparseableExisting(t *testing.T) {
	base := t.TempDir()
	path := config.ProjectConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := "schemaVersion: 1\nbogusUnknownField: true\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	ask := &prompt.Scripted{Choices: []int{profileIndex(t, "default")}, Confirms: []bool{true}}
	if _, err := mgrWithAdapters().SetupWizard(base, ask); err == nil {
		t.Fatal("wizard must refuse an unparseable existing config")
	}
	if got, _ := os.ReadFile(path); string(got) != bad {
		t.Errorf("unparseable config was modified:\n%s", got)
	}
}

// Wizard: a nil Prompter (non-interactive surface) prints guidance, returns a usage error,
// and writes nothing.
func TestSetupWizard_NilPrompterGuidanceNoWrite(t *testing.T) {
	base := t.TempDir()
	res, err := mgr().SetupWizard(base, nil)
	if err == nil {
		t.Fatal("nil prompter must return a usage error")
	}
	if res.ConfigWritten {
		t.Error("nil prompter must not write config")
	}
	if _, statErr := os.Stat(config.ProjectConfigPath(base)); !os.IsNotExist(statErr) {
		t.Error("no config should be written without a prompter")
	}
}

// Wizard: the written config contains no secret-bearing keys.
func TestSetupWizard_WritesNoSecrets(t *testing.T) {
	base := t.TempDir()
	bin := execFile(t, "claude")
	ask := &prompt.Scripted{
		Choices:  []int{profileIndex(t, "default"), 0},
		Answers:  []string{bin},
		Confirms: []bool{true, false, true},
	}
	if _, err := mgrWithAdapters().SetupWizard(base, ask); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	raw, err := os.ReadFile(config.ProjectConfigPath(base))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"token", "secret", "password", "apikey", "api_key", "authorization", "bearer", "credential", "private_key"} {
		if hasSecretKey(doc, s) {
			t.Errorf("wizard-written config has a secret-bearing key matching %q", s)
		}
	}
}

// hasSecretKey reports whether any map key in v (recursively) contains substr.
func hasSecretKey(v any, substr string) bool {
	switch m := v.(type) {
	case map[string]any:
		for k, child := range m {
			if strings.Contains(strings.ToLower(k), substr) || hasSecretKey(child, substr) {
				return true
			}
		}
	case []any:
		for _, child := range m {
			if hasSecretKey(child, substr) {
				return true
			}
		}
	}
	return false
}

// doctor --fix (Repair) only reports — it must never write config.
func TestRepair_WritesNothing(t *testing.T) {
	var buf bytes.Buffer
	m := &Manager{Adapters: map[string]model.Adapter{"claude-code": fake.New(fake.Valid)}, Out: &buf}
	rep := doctor.Report{OK: false, Checks: []doctor.Check{
		{Name: "adapter: claude-code", OK: false, Detail: `binary "claude" not found on PATH`},
		{Name: "profile: native ready", OK: false, Detail: "requires unavailable adapter"},
	}}
	res := m.Repair(t.TempDir(), rep, nil)
	if res.ConfigWritten {
		t.Error("doctor --fix must not write config")
	}
	if res.Path != "" {
		t.Errorf("doctor --fix must not reference a written config path: %q", res.Path)
	}
}

// Guided repair (interactive): an adapter binary issue → confirm → enter a valid path →
// the path is written via the shared ConfigPatch path.
func TestRepair_Interactive_SetsBinaryPath(t *testing.T) {
	base := t.TempDir()
	bin := execFile(t, "claude")
	m := &Manager{Adapters: map[string]model.Adapter{"fake": fake.New(fake.Valid), "claude-code": fake.New(fake.Valid)}, Out: io.Discard}
	rep := doctor.Report{OK: false, Checks: []doctor.Check{{Name: "adapter: claude-code", OK: false, Detail: `binary "claude" not found on PATH`}}}
	ask := &prompt.Scripted{Confirms: []bool{true}, Answers: []string{bin}}
	res := m.Repair(base, rep, ask)
	if !res.ConfigWritten {
		t.Fatal("expected guided repair to write config")
	}
	if cfg := loadCfgT(t, config.ProjectConfigPath(base)); cfg.Adapters["claude-code"].Path != bin {
		t.Errorf("claude-code path = %q, want %q", cfg.Adapters["claude-code"].Path, bin)
	}
}

// Guided repair: declining the offer writes nothing.
func TestRepair_Interactive_DeclineWritesNothing(t *testing.T) {
	base := t.TempDir()
	m := &Manager{Adapters: map[string]model.Adapter{"fake": fake.New(fake.Valid), "claude-code": fake.New(fake.Valid)}, Out: io.Discard}
	rep := doctor.Report{OK: false, Checks: []doctor.Check{{Name: "adapter: claude-code", OK: false, Detail: `binary "claude" not found on PATH`}}}
	res := m.Repair(base, rep, &prompt.Scripted{Confirms: []bool{false}})
	if res.ConfigWritten {
		t.Error("declining guided repair must not write config")
	}
	if _, err := os.Stat(config.ProjectConfigPath(base)); !os.IsNotExist(err) {
		t.Error("no config should be written when repair is declined")
	}
}

// Guided repair: an invalid path is rejected, not written.
func TestRepair_Interactive_InvalidPathRejected(t *testing.T) {
	base := t.TempDir()
	m := &Manager{Adapters: map[string]model.Adapter{"fake": fake.New(fake.Valid), "claude-code": fake.New(fake.Valid)}, Out: io.Discard}
	rep := doctor.Report{OK: false, Checks: []doctor.Check{{Name: "adapter: claude-code", OK: false, Detail: `binary "claude" not found on PATH`}}}
	res := m.Repair(base, rep, &prompt.Scripted{Confirms: []bool{true}, Answers: []string{"/no/such/binary"}})
	if res.ConfigWritten {
		t.Error("an invalid repair path must not be written")
	}
	if _, err := os.Stat(config.ProjectConfigPath(base)); !os.IsNotExist(err) {
		t.Error("no config should be written for an invalid repair path")
	}
}

// Guided repair: a non-adapter issue is guidance-only (no prompt-driven write).
func TestRepair_Interactive_NonAdapterIssueGuidanceOnly(t *testing.T) {
	base := t.TempDir()
	m := &Manager{Adapters: map[string]model.Adapter{"fake": fake.New(fake.Valid)}, Out: io.Discard}
	rep := doctor.Report{OK: false, Checks: []doctor.Check{{Name: "profile: native ready", OK: false, Detail: "requires unavailable adapter"}}}
	res := m.Repair(base, rep, &prompt.Scripted{Confirms: []bool{true}, Answers: []string{"/whatever"}})
	if res.ConfigWritten {
		t.Error("a non-adapter issue must not write config (guidance only)")
	}
}

// writeUserConfig sets AIMESH_HOME to a temp dir and writes a user/global config there, so
// promotion reads it as the source (never the real home). It also isolates AIMESH_HOME to a fresh temp
// dir so the user-scope shared adapters.yaml is per-test (promotion sources adapter paths from there,
// not from config.yaml).
func writeUserConfig(t *testing.T, yamlBody string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	p := config.ProjectConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Promotion: with no user config there is nothing to promote → error, no write.
func TestPromote_NoUserConfig_Errors(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir()) // empty home
	if _, err := mgr().PromoteUserToProject(t.TempDir(), false, true, nil); err == nil {
		t.Fatal("expected an error when no user/global config exists")
	}
}

// Promotion (default): promotes defaultProfile only; machine-specific adapter paths are skipped.
func TestPromote_DefaultProfileOnly_SkipsAdapterPaths(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\ndefaultProfile: native-three-provider\nadapters:\n  claude-code:\n    path: /personal/claude\n")
	proj := t.TempDir()
	res, err := mgr().PromoteUserToProject(proj, false, true, nil)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatal("expected promotion to write")
	}
	if cfg := loadCfgT(t, config.ProjectConfigPath(proj)); cfg.DefaultProfile != "native-three-provider" {
		t.Errorf("defaultProfile not promoted: %q", cfg.DefaultProfile)
	}
	raw, _ := os.ReadFile(config.ProjectConfigPath(proj))
	if strings.Contains(string(raw), "/personal/claude") {
		t.Errorf("adapter path must NOT be promoted by default:\n%s", raw)
	}
}

// Promotion with --include-adapter-paths: the user's saved paths (from the shared adapters.yaml) are
// copied into the PROJECT shared adapters.yaml (dual-target: defaultProfile → config.yaml, paths →
// .aimesh/adapters.yaml).
func TestPromote_IncludeAdapterPaths(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\ndefaultProfile: native-three-provider\n")
	if err := config.SetSharedAdapterPath(userSharedPath(t), "claude-code", "/personal/claude"); err != nil {
		t.Fatalf("seed user shared path: %v", err)
	}
	proj := t.TempDir()
	if err := os.Mkdir(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err) // project scope for adapter paths is root-anchored
	}
	if _, err := mgr().PromoteUserToProject(proj, true, true, nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	projShared, ok := config.SharedProjectLocationsPath(proj)
	if !ok {
		t.Fatal("proj should resolve a repo root")
	}
	if got := sharedAdapterPath(t, projShared, "claude-code"); got == nil || *got != "/personal/claude" {
		t.Errorf("adapter path should be promoted into the project shared file: %v", got)
	}
}

// PlanProjectPromotion previews what would be written WITHOUT writing (the web dialog's summary):
// defaultProfile always, adapter paths only when opted in, and Exists detection — no file is created.
func TestPlanProjectPromotion_PreviewNoWrite(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\ndefaultProfile: native-three-provider\n")
	if err := config.SetSharedAdapterPath(userSharedPath(t), "claude-code", "/personal/claude"); err != nil {
		t.Fatalf("seed user shared path: %v", err)
	}
	proj := t.TempDir()
	pv, err := mgr().PlanProjectPromotion(proj, false)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pv.DefaultProfile != "native-three-provider" {
		t.Errorf("preview defaultProfile = %q, want native-three-provider", pv.DefaultProfile)
	}
	if len(pv.AdapterPaths) != 0 {
		t.Errorf("adapter paths must be opt-in (absent by default): %+v", pv.AdapterPaths)
	}
	if pv.Exists {
		t.Error("no project config should exist yet")
	}
	if config.DiscoverProjectConfig(proj) != "" {
		t.Error("PlanProjectPromotion must NOT write a project config (preview only)")
	}
	if pv2, _ := mgr().PlanProjectPromotion(proj, true); len(pv2.AdapterPaths) != 1 || pv2.AdapterPaths[0] != "claude-code" {
		t.Errorf("adapter path should be previewed with includePaths: %+v", pv2.AdapterPaths)
	}
	if _, err := mgr().PromoteUserToProject(proj, false, true, nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if pv3, _ := mgr().PlanProjectPromotion(proj, false); !pv3.Exists {
		t.Error("preview must report Exists after a project config is written")
	}
}

// CreateProjectConfig writes a valid minimal override even with NOTHING to promote (no defaultProfile
// in the raw user config) — never a blank/invalid file — and refuses when a project config exists.
func TestCreateProjectConfig_WritesMinimalValidOverride(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\nadapters:\n  claude-code:\n    path: /personal/claude\n")
	proj := t.TempDir()
	res, err := mgr().CreateProjectConfig(proj, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatal("expected a write (a minimal override), got none")
	}
	pp := config.ProjectConfigPath(proj)
	if cfg := loadCfgT(t, pp); cfg.SchemaVersion != 1 {
		t.Errorf("minimal override must set schemaVersion: got %d", cfg.SchemaVersion)
	}
	if raw, _ := os.ReadFile(pp); strings.Contains(string(raw), "/personal/claude") {
		t.Errorf("adapter path must NOT be written without opt-in:\n%s", raw)
	}
	if _, err := mgr().CreateProjectConfig(proj, false); err == nil {
		t.Error("expected refusal when a project config already exists")
	}
}

// CreateProjectConfig promotes defaultProfile (→ project config.yaml) + (opt-in) the user's saved
// adapter paths (→ project shared adapters.yaml).
func TestCreateProjectConfig_PromotesDefaultAndOptInPaths(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\ndefaultProfile: native-three-provider\n")
	if err := config.SetSharedAdapterPath(userSharedPath(t), "claude-code", "/personal/claude"); err != nil {
		t.Fatalf("seed user shared path: %v", err)
	}
	proj := t.TempDir()
	if err := os.Mkdir(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr().CreateProjectConfig(proj, true); err != nil {
		t.Fatalf("create: %v", err)
	}
	if cfg := loadCfgT(t, config.ProjectConfigPath(proj)); cfg.DefaultProfile != "native-three-provider" {
		t.Errorf("defaultProfile not promoted: %q", cfg.DefaultProfile)
	}
	projShared, ok := config.SharedProjectLocationsPath(proj)
	if !ok {
		t.Fatal("proj should resolve a repo root")
	}
	if got := sharedAdapterPath(t, projShared, "claude-code"); got == nil || *got != "/personal/claude" {
		t.Errorf("opt-in adapter path not promoted into the project shared file: %v", got)
	}
}

// Promotion never bulk-copies other sections (e.g. profiles/modelCatalog).
func TestPromote_NoBulkCopy(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\ndefaultProfile: fake\nprofiles:\n  myproj:\n    lanes:\n      reviewer:\n        execution: adapter\n        adapter: fake\n        model: fake-model\n")
	proj := t.TempDir()
	if _, err := mgr().PromoteUserToProject(proj, true, true, nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	raw, _ := os.ReadFile(config.ProjectConfigPath(proj))
	if strings.Contains(string(raw), "myproj") {
		t.Errorf("promotion must not bulk-copy profiles into project:\n%s", raw)
	}
}

// Promotion preserves the existing project config (unrelated fields kept).
func TestPromote_PreservesExistingProjectConfig(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\ndefaultProfile: native-three-provider\n")
	proj := t.TempDir()
	pp := config.ProjectConfigPath(proj)
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pp, []byte("schemaVersion: 1\nadapters:\n  codex-cli:\n    path: /proj/codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr().PromoteUserToProject(proj, false, true, nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	cfg := loadCfgT(t, pp)
	if cfg.DefaultProfile != "native-three-provider" {
		t.Errorf("promoted defaultProfile missing: %q", cfg.DefaultProfile)
	}
	if cfg.Adapters["codex-cli"].Path != "/proj/codex" {
		t.Errorf("existing project adapter path lost: %q", cfg.Adapters["codex-cli"].Path)
	}
}

// Promotion refuses a malformed existing project config and leaves it unchanged.
func TestPromote_RefusesMalformedProject(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\ndefaultProfile: native-three-provider\n")
	proj := t.TempDir()
	pp := config.ProjectConfigPath(proj)
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := "bogusUnknownField: true\n"
	if err := os.WriteFile(pp, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr().PromoteUserToProject(proj, false, true, nil); err == nil {
		t.Fatal("promotion must refuse a malformed project config")
	}
	if got, _ := os.ReadFile(pp); string(got) != bad {
		t.Errorf("malformed project file modified:\n%s", got)
	}
}

// Promotion on a non-interactive surface requires --yes; otherwise it prints the plan and writes nothing.
func TestPromote_NonInteractiveRequiresYes(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\ndefaultProfile: native-three-provider\n")
	proj := t.TempDir()
	res, err := mgr().PromoteUserToProject(proj, false, false, nil)
	if err == nil {
		t.Fatal("non-interactive promotion without --yes must error")
	}
	if res.ConfigWritten {
		t.Error("must not write without --yes")
	}
	if _, statErr := os.Stat(config.ProjectConfigPath(proj)); !os.IsNotExist(statErr) {
		t.Error("no project config should be written")
	}
}

// Promotion: interactive confirm writes; cancel does not.
func TestPromote_InteractiveConfirmAndCancel(t *testing.T) {
	writeUserConfig(t, "schemaVersion: 1\ndefaultProfile: native-three-provider\n")
	projCancel := t.TempDir()
	if res, err := mgr().PromoteUserToProject(projCancel, false, false, &prompt.Scripted{Confirms: []bool{false}}); err != nil || res.ConfigWritten {
		t.Errorf("cancel must not write (err=%v written=%v)", err, res.ConfigWritten)
	}
	projWrite := t.TempDir()
	if res, err := mgr().PromoteUserToProject(projWrite, false, false, &prompt.Scripted{Confirms: []bool{true}}); err != nil || !res.ConfigWritten {
		t.Errorf("confirm must write (err=%v written=%v)", err, res.ConfigWritten)
	}
}
