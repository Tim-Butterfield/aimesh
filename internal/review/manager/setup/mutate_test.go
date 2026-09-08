package setup

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// userSharedPath is the current user-scope shared adapters.yaml path (AIMESH_HOME-anchored). Adapter
// binary PATHS now persist here, not in config.yaml.
func userSharedPath(t *testing.T) string {
	t.Helper()
	p, err := config.SharedUserLocationsPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sharedAdapterPath loads the shared adapters.yaml at path and returns name's saved binary path
// (nil = no entry / not set). Helpers return *string so non-config test files need no direct
// adapterlocations import.
func sharedAdapterPath(t *testing.T, path, name string) *string {
	t.Helper()
	loc, err := adapterlocations.Load(path)
	if err != nil {
		t.Fatalf("load shared adapters.yaml %q: %v", path, err)
	}
	return loc.Adapters[name].Path
}

// rawUserLaneExists reports whether profiles.<profile>.lanes.<role> is present in the RAW user-layer
// config file (config.Load re-merges the seed, hiding user-layer deletions).
func rawUserLaneExists(t *testing.T, profile, role string) bool {
	t.Helper()
	p, err := config.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := config.LoadRawMap(p)
	if err != nil {
		return false
	}
	profs, _ := raw["profiles"].(map[string]any)
	pm, _ := profs[profile].(map[string]any)
	lanes, _ := pm["lanes"].(map[string]any)
	_, ok := lanes[role]
	return ok
}

// TestClearProfileLane_ClearsUserLaneKeepsCatalog: clearing a user-layer lane deletes only the lane
// (confirmation required), leaves the shared modelCatalog entry, and a second clear is a no-op.
func TestClearProfileLane_ClearsUserLaneKeepsCatalog(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.EditProfileLane("default", "reviewer", "claude-code", LaneChoiceInput{ModelArg: "opus", Effort: "medium"}, nil); err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	if !rawUserLaneExists(t, "default", "reviewer") {
		t.Fatal("precondition: the user-layer reviewer lane should exist after EditProfileLane")
	}
	res, err := m.ClearProfileLane("default", "reviewer", "")
	if err != nil {
		t.Fatalf("clear(no confirm): %v", err)
	}
	if !res.ConfirmRequired || res.Cleared {
		t.Fatalf("first call must require confirmation, got %+v", res)
	}
	res, err = m.ClearProfileLane("default", "reviewer", res.Message)
	if err != nil {
		t.Fatalf("clear(confirm): %v", err)
	}
	if !res.Cleared {
		t.Fatalf("expected cleared, got %+v", res)
	}
	if rawUserLaneExists(t, "default", "reviewer") {
		t.Error("the user-layer reviewer lane must be deleted after clear")
	}
	if _, ok := userCfg(t).ModelCatalog["claude-code-opus-medium"]; !ok {
		t.Error("lane clear must NOT delete the shared modelCatalog entry")
	}
	res, err = m.ClearProfileLane("default", "reviewer", "reviewer")
	if err != nil {
		t.Fatalf("clear(again): %v", err)
	}
	if !res.NoOp {
		t.Errorf("clearing an already-inherited lane must be a no-op, got %+v", res)
	}
}

// TestClearProfileLane_BlockedByHigherLayer: a user-layer clear is blocked when a higher (project)
// layer defines the SAME lane (the clear would be shadowed).
func TestClearProfileLane_BlockedByHigherLayer(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.EditProfileLane("default", "reviewer", "claude-code", LaneChoiceInput{ModelArg: "opus", Effort: "medium"}, nil); err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	projPath := filepath.Join(t.TempDir(), "project.yaml")
	if err := os.WriteFile(projPath, []byte("profiles:\n  default:\n    lanes:\n      reviewer:\n        adapter: fake\n        model: fake-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Layers.ProjectPath, m.Layers.ProjectLoaded = projPath, true
	res, err := m.ClearProfileLane("default", "reviewer", "reviewer")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !res.Blocked {
		t.Fatalf("a higher-layer-defined lane must block the user-layer clear, got %+v", res)
	}
	if !rawUserLaneExists(t, "default", "reviewer") {
		t.Error("a blocked clear must not delete the user-layer lane")
	}
}

// TestClearProfileLane_LowerOnlyIsNoOp: clearing a lane with no user-layer override (it comes only from
// a lower/shipped layer, or nothing has been written) is a no-op — never a phantom "cleared".
func TestClearProfileLane_LowerOnlyIsNoOp(t *testing.T) {
	m := mutMgr(t)
	res, err := m.ClearProfileLane("default", "reviewer", "reviewer")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !res.NoOp || res.Cleared {
		t.Fatalf("a lane with no user-layer override must be a no-op, got %+v", res)
	}
}

// TestClearProfileLane_HigherProfileWithoutLaneAllowed: a higher layer that defines the PROFILE but not
// the target ROLE does NOT block a user-layer clear of that role (the lane-level guard is role-specific).
func TestClearProfileLane_HigherProfileWithoutLaneAllowed(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.EditProfileLane("default", "reviewer", "claude-code", LaneChoiceInput{ModelArg: "opus", Effort: "medium"}, nil); err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	// A higher (project) layer defines `default` but only its cross_check lane — NOT reviewer.
	projPath := filepath.Join(t.TempDir(), "project.yaml")
	if err := os.WriteFile(projPath, []byte("profiles:\n  default:\n    lanes:\n      cross_check:\n        adapter: fake\n        model: fake-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Layers.ProjectPath, m.Layers.ProjectLoaded = projPath, true
	res, err := m.ClearProfileLane("default", "reviewer", "reviewer")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if res.Blocked || !res.Cleared {
		t.Fatalf("a higher layer defining the profile but not this role must NOT block the clear, got %+v", res)
	}
	if rawUserLaneExists(t, "default", "reviewer") {
		t.Error("the user-layer reviewer lane should be deleted")
	}
}

// mutMgr builds a Manager wired with the full adapter registry + the shipped seed config,
// pointing AIMESH_HOME at a temp dir so user-scope writes are isolated.
func mutMgr(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	return &Manager{
		Adapters: map[string]model.Adapter{
			"fake": fake.New(fake.Valid), "claude-code": fake.New(fake.Valid),
			"codex-cli": fake.New(fake.Valid), "agy-cli": fake.New(fake.Valid),
			"devin-cli": fake.New(fake.Valid), "ollama": fake.New(fake.Valid),
			"gemini-cli": fake.New(fake.Valid),
		},
		Out: io.Discard,
		// Tests exercise the opt-in/smoke profiles (native-three-provider, fully-local-ollama,
		// …) which are no longer in the shipped seed — merge them in as the example profiles a
		// user would have created.
		Cfg: config.WithExampleProfiles(config.Default()),
	}
}

func userCfg(t *testing.T) config.Config {
	t.Helper()
	p, err := config.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("load user config: %v", err)
	}
	return cfg
}

func userConfigExists(t *testing.T) bool {
	t.Helper()
	p, err := config.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(p)
	return statErr == nil
}

// rawUserConfig returns the RAW user-layer config file (config.Load would re-merge the shipped
// seed, hiding user-layer deletions and un-set fields).
func rawUserConfig(t *testing.T) string {
	t.Helper()
	p, err := config.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read raw user config: %v", err)
	}
	return string(b)
}

func TestCopyProfile_ToNew(t *testing.T) {
	m := mutMgr(t)
	// Copy the shipped-but-hidden fake profile (the `default` profile now ships unconfigured, so it
	// carries no lanes to copy); the copy must carry the fake lanes.
	res, err := m.CopyProfile(config.FakeProfile, "mycopy", "")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !res.Written || res.ReplaceRequired {
		t.Fatalf("expected a clean write, got %+v", res)
	}
	cfg := userCfg(t)
	cp, ok := cfg.Profiles["mycopy"]
	if !ok {
		t.Fatalf("mycopy not written: %+v", cfg.Profiles)
	}
	if cp.Lanes["reviewer"].Adapter != "fake" {
		t.Errorf("copy should carry the fake profile's (fake) lanes: %+v", cp.Lanes)
	}
	// No "set as default": CopyProfile never writes defaultProfile (inspect the raw user layer,
	// since config.Load would merge the seed's defaultProfile).
	if raw := rawUserConfig(t); strings.Contains(raw, "defaultProfile") {
		t.Errorf("CopyProfile must not write defaultProfile; raw user config:\n%s", raw)
	}
}

func TestCopyProfile_ToExisting_RequiresConfirm(t *testing.T) {
	m := mutMgr(t)
	res, err := m.CopyProfile("fully-local-ollama", "default", "")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if res.Written {
		t.Fatal("copy onto an existing profile must not write without confirmation")
	}
	if !res.ReplaceRequired {
		t.Fatal("expected ReplaceRequired")
	}
	if want := ReplaceProfileConfirm("default", "fully-local-ollama"); res.Message != want {
		t.Errorf("confirm message = %q, want %q", res.Message, want)
	}
	if userConfigExists(t) {
		t.Error("no config file should have been written when confirmation is required")
	}
	// A WRONG (non-empty) confirmation string must also be rejected — only the exact echo works.
	if r2, _ := m.CopyProfile("fully-local-ollama", "default", "yes, do it"); r2.Written || !r2.ReplaceRequired {
		t.Errorf("a non-matching confirmation must not write: %+v", r2)
	}
}

func TestCopyProfile_ToExisting_WithConfirmReplaces(t *testing.T) {
	m := mutMgr(t)
	res, err := m.CopyProfile("fully-local-ollama", "default", ReplaceProfileConfirm("default", "fully-local-ollama"))
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !res.Written {
		t.Fatal("expected write once the exact confirmation is echoed")
	}
	cfg := userCfg(t)
	if got := cfg.Profiles["default"].Lanes["reviewer"].Adapter; got != "ollama" {
		t.Errorf("default profile should now be a copy of fully-local-ollama (reviewer adapter=%q, want ollama)", got)
	}
}

func TestCopyProfile_UnknownSource(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.CopyProfile("nope", "x", ""); err == nil {
		t.Error("copying a nonexistent source must error")
	}
}

func TestRemoveAdapter_BlockedWhileUsed(t *testing.T) {
	m := mutMgr(t)
	res, err := m.RemoveAdapter("codex-cli") // used by native-three-provider + codex-cli-smoke
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !res.Blocked || len(res.UsedBy) == 0 {
		t.Fatalf("expected blocked-with-uses, got %+v", res)
	}
	if res.Removed {
		t.Error("a used adapter must not be removed")
	}
}

func TestRemoveAdapter_FakeRejected(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.RemoveAdapter("fake"); err == nil {
		t.Error("the built-in fake adapter must not be removable")
	}
}

func TestRemoveAdapter_UnusedRemoved(t *testing.T) {
	m := mutMgr(t)
	bin := execFile(t, "gemini")
	if _, err := m.ConfigureAdapterPath("gemini-cli", bin); err != nil {
		t.Fatalf("configure gemini-cli: %v", err)
	}
	// gemini-cli is registered but not referenced by any shipped profile → removable.
	res, err := m.RemoveAdapter("gemini-cli")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if res.Blocked {
		t.Fatalf("gemini-cli should be unused, got blocked by %+v", res.UsedBy)
	}
	if !res.Removed {
		t.Fatal("expected removal")
	}
	// The saved path now lives in the shared adapters.yaml; clearing must remove the entry there.
	if p := sharedAdapterPath(t, userSharedPath(t), "gemini-cli"); p != nil {
		t.Errorf("gemini-cli path should be cleared from the shared adapters.yaml, got %q", *p)
	}
}

func TestConfigureAdapterPath_WritesUserScope(t *testing.T) {
	m := mutMgr(t)
	bin := execFile(t, "codex")
	res, err := m.ConfigureAdapterPath("codex-cli", bin)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatal("expected config written")
	}
	// The path is recorded in the user-scope shared adapters.yaml (not config.yaml).
	if got := sharedAdapterPath(t, userSharedPath(t), "codex-cli"); got == nil || *got != bin {
		t.Errorf("codex-cli path = %v, want %q", got, bin)
	}
}

func TestConfigureAdapterPath_RejectsFake(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.ConfigureAdapterPath("fake", "/x"); err == nil {
		t.Error("configuring a path for the built-in fake adapter must be rejected")
	}
}

func TestIsFakeOnlyProfile(t *testing.T) {
	m := mutMgr(t)
	if !m.IsFakeOnlyProfile(config.FakeProfile) {
		t.Error("the shipped hidden fake profile is fake-only")
	}
	if m.IsFakeOnlyProfile("default") {
		t.Error("the shipped default profile is now UNCONFIGURED — it must not be fake-only")
	}
	if m.IsFakeOnlyProfile("fully-local-ollama") {
		t.Error("a fully-local Ollama profile is local but NOT fake-only")
	}
	if m.IsFakeOnlyProfile("native-three-provider") {
		t.Error("a cloud profile is not fake-only")
	}
	if m.IsFakeOnlyProfile("does-not-exist") {
		t.Error("a missing profile is not fake-only")
	}
}
