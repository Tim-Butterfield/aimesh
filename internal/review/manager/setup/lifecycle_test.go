package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

// Area-1/Area-3 lifecycle semantics: the fixed workbench default profile is the profile NAMED
// `default`; a stale defaultProfile pointer (e.g. `fake` from an older build) is surfaced as a
// legacy posture with a confirmed repair, never silently rewritten; profile deletion is
// governed (default protected, dangling pointer blocked, layered semantics honest).

// staleMgr simulates the observed stale state: the merged config's defaultProfile points at a
// legacy `fake` profile (both as an older build's user layer would produce).
func staleMgr(t *testing.T) *Manager {
	t.Helper()
	m := mutMgr(t)
	cfg := config.Default()
	cfg.Profiles["fake"] = cfg.Profiles[config.FakeProfile] // legacy fake-only profile left over from an old build
	cfg.DefaultProfile = "fake"
	m.Cfg = cfg
	return m
}

func TestConfigOverview_FreshDefaultsHaveNoLegacyPointer(t *testing.T) {
	m := mutMgr(t)
	ov := m.ConfigOverview()
	if ov.LegacyDefaultPointer != "" || ov.LegacyDefaultNote != "" {
		t.Errorf("fresh defaults must not report a legacy pointer: %+v", ov)
	}
	// The shipped `default` profile now ships UNCONFIGURED — it references no adapter, so the
	// overview reports no default adapters (Doctor flags it until the user configures a lane).
	if len(ov.DefaultAdapters) != 0 {
		t.Errorf("the unconfigured default profile must report no configured adapters, got %v", ov.DefaultAdapters)
	}
}

func TestConfigOverview_StalePointerIsProjectedHonestly(t *testing.T) {
	m := staleMgr(t)
	ov := m.ConfigOverview()
	if ov.LegacyDefaultPointer != "fake" {
		t.Fatalf("legacyDefaultPointer=%q want fake", ov.LegacyDefaultPointer)
	}
	if !strings.Contains(ov.LegacyDefaultNote, "default") || !strings.Contains(ov.LegacyDefaultNote, "fake") {
		t.Errorf("the note must explain the fake-vs-default posture: %q", ov.LegacyDefaultNote)
	}
	if userConfigExists(t) {
		t.Error("projecting the stale state must not write config (no silent rewrite)")
	}
}

func TestApplyRepair_SetDefaultProfile(t *testing.T) {
	m := staleMgr(t)
	res, err := m.ApplyRepair("set_default_profile", "", "")
	if err != nil {
		t.Fatalf("ApplyRepair: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatalf("expected a config write, got %+v", res)
	}
	raw := rawUserConfig(t)
	if !strings.Contains(raw, "defaultProfile: default") {
		t.Errorf("repair must set defaultProfile: default in the user layer:\n%s", raw)
	}
	// The repair only touches the pointer — the legacy profile is NOT deleted.
	if strings.Contains(raw, "profiles:") && strings.Contains(raw, "fake:") {
		t.Errorf("repair must not write profile content:\n%s", raw)
	}
}

func TestApplyRepair_SetDefaultProfile_AlreadyCanonicalRejected(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.ApplyRepair("set_default_profile", "", ""); err == nil {
		t.Fatal("repair with a canonical pointer should be rejected (nothing to repair)")
	}
	if userConfigExists(t) {
		t.Error("rejected repair must not write config")
	}
}

func TestDeleteProfile_DefaultCanNeverBeDeleted(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.DeleteProfile("default", DeleteProfileConfirm("default")); err == nil {
		t.Fatal("deleting `default` must be an error even with the confirmation echoed")
	}
}

func TestDeleteProfile_UnknownProfileRejected(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.DeleteProfile("no-such", ""); err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
}

func TestDeleteProfile_SeedOnlyProfileBlocked(t *testing.T) {
	m := mutMgr(t)
	res, err := m.DeleteProfile("fully-local-ollama", "")
	if err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	if !res.Blocked || !strings.Contains(res.Message, "not in the user/global config") {
		t.Errorf("a seed-only profile must be blocked with the layered explanation, got %+v", res)
	}
}

func TestDeleteProfile_DanglingDefaultPointerBlocked(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.CopyProfile("default", "mydefault", ""); err != nil {
		t.Fatal(err)
	}
	m.Cfg.Profiles["mydefault"] = m.Cfg.Profiles["default"]
	m.Cfg.DefaultProfile = "mydefault" // simulate a saved pointer at the profile being deleted
	res, err := m.DeleteProfile("mydefault", "")
	if err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	if !res.Blocked || !strings.Contains(res.Message, "defaultProfile") {
		t.Errorf("deleting the pointed-at profile must be blocked naming the pointer, got %+v", res)
	}
}

func TestDeleteProfile_HigherLayerDefinitionBlocked(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.CopyProfile("default", "dup", ""); err != nil {
		t.Fatal(err)
	}
	m.Cfg.Profiles["dup"] = m.Cfg.Profiles["default"]
	// A project layer also defines `dup` — the UI writes only the user layer, so deleting the
	// user-layer key would not remove the merged profile; the delete must be blocked honestly.
	projDir := t.TempDir()
	projPath := filepath.Join(projDir, "config.yaml")
	if err := os.WriteFile(projPath, []byte("schemaVersion: 1\nprofiles:\n  dup:\n    lanes:\n      reviewer:\n        execution: adapter\n        adapter: fake\n        model: fake-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Layers.ProjectPath, m.Layers.ProjectLoaded = projPath, true

	res, err := m.DeleteProfile("dup", "")
	if err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	if !res.Blocked || !strings.Contains(res.Message, "project config layer") {
		t.Errorf("a project-layer-defined profile must be blocked with the layered explanation, got %+v", res)
	}
}

func TestDeleteProfile_ConfirmHandshakeThenDelete(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.CopyProfile("default", "todelete", ""); err != nil {
		t.Fatal(err)
	}
	m.Cfg.Profiles["todelete"] = m.Cfg.Profiles["default"] // reflect the write in the merged view

	res, err := m.DeleteProfile("todelete", "")
	if err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	if !res.ConfirmRequired || res.Message != DeleteProfileConfirm("todelete") {
		t.Fatalf("expected the exact confirmation prompt, got %+v", res)
	}
	if res2, err := m.DeleteProfile("todelete", "yes"); err != nil || !res2.ConfirmRequired {
		t.Fatalf("a non-echoed confirmation must not delete, got %+v err=%v", res2, err)
	}

	res3, err := m.DeleteProfile("todelete", DeleteProfileConfirm("todelete"))
	if err != nil {
		t.Fatalf("confirmed delete: %v", err)
	}
	if !res3.Deleted {
		t.Fatalf("expected deletion, got %+v", res3)
	}
	if strings.Contains(rawUserConfig(t), "todelete") {
		t.Error("the user layer must no longer contain the deleted profile")
	}
}

func TestDeleteProfile_UserProfilePlainMessage(t *testing.T) {
	m := mutMgr(t)
	// With the trimmed seed (only `default` ships), a deletable user profile shadows no shipped
	// profile, so deletion gives the plain "Deleted … from the user config" message (the
	// "still ships" honest-reappearance path only applies to a name present in the seed, and the
	// only seed profile is the undeletable `default`).
	confirm := ReplaceProfileConfirm("my-native", "native-three-provider")
	if _, err := m.CopyProfile("native-three-provider", "my-native", confirm); err != nil {
		t.Fatal(err)
	}
	m.Cfg.Profiles["my-native"] = m.Cfg.Profiles["native-three-provider"] // reflect the write in the merged view
	res, err := m.DeleteProfile("my-native", DeleteProfileConfirm("my-native"))
	if err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	if !res.Deleted || strings.Contains(res.Message, "still ships") {
		t.Errorf("deleting a user profile must give the plain message (no shipped reappearance), got %+v", res)
	}
}
