package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

// The lane-editor mutation (EditProfileLane) is the web-UI "set all aspects of a profile"
// write path: server-side validation of every choice, planned by the pure engine, written
// through the single config-write path.

func TestEditProfileLane_ExistingLaneKeepsExecution(t *testing.T) {
	m := mutMgr(t)
	res, err := m.EditProfileLane("default", "reviewer", "claude-code", LaneChoiceInput{CatalogKey: "claude-code-default"}, nil)
	if err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatalf("expected a config write, got %+v", res)
	}
	// The raw user layer records only adapter+model (execution merges from the seed lane).
	raw := rawUserConfig(t)
	if !strings.Contains(raw, "claude-code-default") {
		t.Errorf("raw user config should record the model key:\n%s", raw)
	}
	if strings.Contains(raw, "execution") {
		t.Errorf("editing an existing lane must not write execution:\n%s", raw)
	}
	// Merged view: adapter/model updated, execution inherited from the seed.
	cfg := userCfg(t)
	lane := cfg.Profiles["default"].Lanes["reviewer"]
	if lane.Adapter != "claude-code" || lane.Model != "claude-code-default" {
		t.Errorf("merged lane=%+v", lane)
	}
}

func TestEditProfileLane_NewLaneGetsExecution(t *testing.T) {
	m := mutMgr(t)
	// `default` ships with author_remediator+reviewer only; cross_check is a NEW lane.
	if _, err := m.EditProfileLane("default", "cross_check", "codex-cli", LaneChoiceInput{CatalogKey: "codex-cli-default"}, nil); err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	cfg := userCfg(t)
	lane, ok := cfg.Profiles["default"].Lanes["cross_check"]
	if !ok {
		t.Fatal("cross_check lane not written")
	}
	if lane.Execution != "adapter" || lane.Adapter != "codex-cli" || lane.Model != "codex-cli-default" {
		t.Errorf("new lane=%+v", lane)
	}
}

func TestEditProfileLane_RejectsInvalidChoices(t *testing.T) {
	m := mutMgr(t)
	cases := []struct {
		name                          string
		profile, role, adapter, model string
	}{
		{"unknown profile", "nope", "reviewer", "fake", "fake-model"},
		{"unknown role", "default", "moderator", "fake", "fake-model"},
		{"unimplemented adapter", "default", "reviewer", "not-an-adapter", "fake-model"},
		{"unknown model", "default", "reviewer", "fake", "not-a-model"},
		{"model not reachable via adapter", "default", "reviewer", "codex-cli", "claude-code-default"},
		{"missing fields", "default", "", "fake", "fake-model"},
	}
	for _, c := range cases {
		if _, err := m.EditProfileLane(c.profile, c.role, c.adapter, LaneChoiceInput{CatalogKey: c.model}, nil); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
	if userConfigExists(t) {
		t.Error("no rejected edit may write config")
	}
}

func TestCopyProfile_LossyReplaceBlocked(t *testing.T) {
	m := mutMgr(t)
	// The only SHIPPED profile is `default` (author_remediator + reviewer). A source that lacks
	// `reviewer` would leave default's shipped `reviewer` lane in place (it merges in from the
	// seed), so copying over `default` is not a faithful replace → blocked.
	m.Cfg.Profiles["author-only"] = config.Profile{Lanes: map[string]config.Lane{
		"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
	}}
	res, err := m.CopyProfile("author-only", "default", ReplaceProfileConfirm("default", "author-only"))
	if err != nil {
		t.Fatalf("CopyProfile: %v", err)
	}
	if !res.Blocked || res.Written {
		t.Fatalf("a lossy copy-to-existing must be blocked, got %+v", res)
	}
	if !strings.Contains(res.Message, "reviewer") {
		t.Errorf("the block must name the surviving lane: %q", res.Message)
	}
	if userConfigExists(t) {
		t.Error("a blocked copy must not write config")
	}
}

func TestCopyProfile_FaithfulReplaceAllowed(t *testing.T) {
	m := mutMgr(t)
	// A source that covers all of the target's roles is a faithful replace → allowed.
	// default and fully-local-ollama both have {author_remediator, reviewer}.
	res, err := m.CopyProfile("fully-local-ollama", "default", ReplaceProfileConfirm("default", "fully-local-ollama"))
	if err != nil {
		t.Fatalf("CopyProfile: %v", err)
	}
	if res.Blocked || !res.Written {
		t.Fatalf("a role-covering copy onto default must succeed, got %+v", res)
	}
}

func TestCopyProfile_UserOnlyTargetWithExtraLanesNotBlocked(t *testing.T) {
	m := mutMgr(t)
	// A PURELY user-created target with MORE lanes than the source must NOT be blocked: a
	// full-subtree user-layer write cleanly removes the old lanes (nothing survives from a
	// lower layer, since the target is not a shipped profile). (agy cross-check regression.)
	m.Cfg.Profiles["user-wide"] = config.Profile{Lanes: map[string]config.Lane{
		"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
		"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		"cross_check":       {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		"verifier":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
	}}
	// source `default` has only {author_remediator, reviewer} — fewer than the target.
	res, err := m.CopyProfile("default", "user-wide", ReplaceProfileConfirm("user-wide", "default"))
	if err != nil {
		t.Fatalf("CopyProfile: %v", err)
	}
	if res.Blocked {
		t.Fatalf("copying onto a purely user-created target must not be blocked, got %+v", res)
	}
	if !res.Written {
		t.Fatalf("expected a write, got %+v", res)
	}
}

func TestCopyProfile_HigherLayerTargetBlocked(t *testing.T) {
	m := mutMgr(t)
	projDir := t.TempDir()
	projPath := filepath.Join(projDir, "config.yaml")
	if err := os.WriteFile(projPath, []byte("schemaVersion: 1\nprofiles:\n  myproj:\n    lanes:\n      reviewer:\n        execution: adapter\n        adapter: fake\n        model: fake-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Layers.ProjectPath, m.Layers.ProjectLoaded = projPath, true
	m.Cfg.Profiles["myproj"] = m.Cfg.Profiles["default"] // present in the merged view too

	res, err := m.CopyProfile("default", "myproj", ReplaceProfileConfirm("myproj", "default"))
	if err != nil {
		t.Fatalf("CopyProfile: %v", err)
	}
	if !res.Blocked || !strings.Contains(res.Message, "project config layer") {
		t.Errorf("copying onto a project-layer profile must be blocked, got %+v", res)
	}
}

func TestEditProfileLane_NeverTouchesDefaultProfilePointer(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.EditProfileLane("fully-local-ollama", "reviewer", "ollama", LaneChoiceInput{CatalogKey: "ollama-local"}, nil); err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	raw := rawUserConfig(t)
	if strings.Contains(raw, "defaultProfile") {
		t.Errorf("a lane edit must not write defaultProfile:\n%s", raw)
	}
}

func TestSnapshotConfig_ProfileOverrideOnlyInSnapshot(t *testing.T) {
	m := mutMgr(t)
	home := t.TempDir()
	if err := m.SnapshotConfig(home, "fully-local-ollama"); err != nil {
		t.Fatalf("SnapshotConfig: %v", err)
	}
	snap, err := config.Load(config.ProjectConfigPath(home))
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if snap.DefaultProfile != "fully-local-ollama" {
		t.Errorf("snapshot defaultProfile=%q want fully-local-ollama", snap.DefaultProfile)
	}
	// The manager's live config is untouched (no "set as default" side effect).
	if m.Cfg.DefaultProfile != "default" {
		t.Errorf("live defaultProfile=%q — the override must apply only to the snapshot", m.Cfg.DefaultProfile)
	}
}

func TestSnapshotConfig_UnknownProfileRejected(t *testing.T) {
	m := mutMgr(t)
	if err := m.SnapshotConfig(t.TempDir(), "no-such-profile"); err == nil {
		t.Fatal("expected an error for an unknown profile override")
	}
}

func TestLaneOptions_ProjectsRolesAdaptersModels(t *testing.T) {
	m := mutMgr(t)
	opts := m.LaneOptions(nil)
	if len(opts.Roles) != 4 {
		t.Fatalf("roles=%d want 4", len(opts.Roles))
	}
	for _, r := range opts.Roles {
		if r.Purpose == "" {
			t.Errorf("role %s must carry a purpose description", r.Role)
		}
	}
	byName := map[string]AdapterOption{}
	for _, a := range opts.Adapters {
		byName[a.Name] = a
	}
	// The built-in `fake` adapter must NOT be offered in the lane editor: it is a deterministic test
	// fixture, not a provider a user assigns to a lane (it stays resolvable by name for the hidden
	// fake-smoke profile and tests).
	if _, ok := byName["fake"]; ok {
		t.Errorf("the fake adapter must not appear in LaneOptions, got %+v", byName["fake"])
	}
	// The seed's single per-adapter entry is the ADAPTER DEFAULT — projected under
	// adapterDefault, NOT as a saved model preset (the hierarchy correction).
	cc, ok := byName["claude-code"]
	if !ok || cc.Config.AdapterDefault == nil || cc.Config.AdapterDefault.CatalogKey != "claude-code-default" ||
		len(cc.Config.SavedModels) != 0 {
		t.Errorf("claude-code option=%+v (default must be adapterDefault, savedModels empty)", cc)
	}
	// gemini-cli is present and NOT spec-only: it is verified running against the real CLI
	// (per-recipe status: docs/adapters.md). It is still identity-uncaptured — a caveat, not a blocker.
	if g, ok := byName["gemini-cli"]; !ok || g.SpecOnly {
		t.Errorf("gemini-cli must be present and NOT spec-only, got %+v", g)
	}
}

// TestSmokePlanView_SpecOnlyAdapterBlocks covers the spec-only GATE itself. The shipped set is empty
// (every recipe is verified running), so the test registers a temporary member and proves that a lane
// using it blocks ACP validation — a runnability blocker, distinct from a mere identity caveat.
func TestSmokePlanView_SpecOnlyAdapterBlocks(t *testing.T) {
	if len(specOnlyAdapters) != 0 {
		t.Fatalf("precondition: the shipped spec-only set must be empty, got %v", specOnlyAdapters)
	}
	specOnlyAdapters["claude-code"] = true
	t.Cleanup(func() { delete(specOnlyAdapters, "claude-code") })

	if !acpSpecOnly("claude-code") {
		t.Fatal("acpSpecOnly must consult the registry")
	}
	if acpSpecOnly("gemini-cli") {
		t.Error("a non-member must not be reported spec-only")
	}
}
