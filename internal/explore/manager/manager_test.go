package manager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// testEnv points AIMESH_HOME at a temp dir so a manager under test never reads or writes the real
// ~/.aimesh, and returns a non-repo cwd (so resolution lands on the user scope under AIMESH_HOME rather
// than a project scope anchored at the real repo root). One variable covers both the roster/profiles and
// the shared adapters file now that they share a state root.
func testEnv(t *testing.T) (home, cwd string) {
	t.Helper()
	home = t.TempDir()
	cwd = t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	// Most fixtures here run on fakeRoster(); the fake key resolves only under the internal gate.
	t.Setenv("AIMESH_INTERNAL_FAKE", "1")
	return home, cwd
}

// fakeRoster is a valid 2-explorer + collator roster backed entirely by the deterministic `fake`
// adapter (distinct models keep the triples unique).
func fakeRoster() roster.Roster {
	return roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "fake", Model: "fake-a", Effort: "high"},
			{Adapter: "fake", Model: "fake-b", Effort: "medium"},
		},
		Collator: roster.Collator{Adapter: "fake", Model: "fake-c", Effort: "high"},
	}
}

func newTestManager(t *testing.T, r roster.Roster) *Manager {
	t.Helper()
	_, cwd := testEnv(t)
	m, err := New(cwd, r)
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	return m
}

// defaultName is the manager's current DEFAULT profile name (Go's answer — never assumed by a caller).
func defaultName(m *Manager) string { return m.ProfilesView().DefaultProfile }

// defaultProfileDraft is a deep copy of the DEFAULT profile: the "client draft" every roster write starts
// from, since SaveProfile (the only roster write seam) persists a profile WHOLE. It is the exact shape the
// webui posts to /api/profiles/save.
func defaultProfileDraft(m *Manager, t *testing.T) profile.Profile {
	t.Helper()
	set := m.Profiles()
	p, ok := set.Get(set.DefaultProfile)
	if !ok {
		t.Fatalf("no default profile %q in the set (have: %v)", set.DefaultProfile, set.Names())
	}
	return p
}

// TestSaveDefaultProfile_EditsTheCachedRoster: the profile seam is the ONLY roster write path, so an
// edit to the DEFAULT profile must re-derive the cached roster the projections read (and bump once).
func TestSaveDefaultProfile_EditsTheCachedRoster(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	if got := m.Generation(); got != 0 {
		t.Fatalf("initial generation = %d, want 0", got)
	}

	p := defaultProfileDraft(m, t)
	p.Explorers = append(p.Explorers, roster.Explorer{Adapter: "fake", Model: "fake-d", Effort: "low"})
	p.Explorers[0].Model = "fake-z"
	p.Collator = roster.Collator{Adapter: "fake", Model: "fake-collate", Effort: "high"}
	if _, err := m.SaveProfile(defaultName(m), p); err != nil {
		t.Fatalf("SaveProfile default: %v", err)
	}

	// ONE write ⇒ ONE bump, however many slots it touched (the seam is whole-profile).
	if got := m.Generation(); got != 1 {
		t.Fatalf("generation after the whole-profile save = %d, want 1", got)
	}
	if got := len(m.Roster().Explorers); got != 3 {
		t.Fatalf("explorers after save = %d, want 3", got)
	}
	if got := m.Roster().Explorers[0].Model; got != "fake-z" {
		t.Fatalf("explorer 0 model after save = %q, want fake-z", got)
	}
	c := m.RosterView().Collator
	if c.Model != "fake-collate" || c.Effort != "high" {
		t.Fatalf("collator after save = %+v, want model fake-collate at high effort", c)
	}
}

// TestSaveDefaultProfile_RejectsDuplicateTriple: an identical (adapter,model,effort) triple adds no
// independent vantage — the roster rule is enforced on the profile seam too, and persists nothing.
func TestSaveDefaultProfile_RejectsDuplicateTriple(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	p := defaultProfileDraft(m, t)
	p.Explorers = append(p.Explorers, p.Explorers[0]) // exact duplicate of explorer 0
	if _, err := m.SaveProfile(defaultName(m), p); err == nil {
		t.Fatal("expected a duplicate-triple rejection, got nil")
	}
	if got := m.Generation(); got != 0 {
		t.Fatalf("generation after rejected save = %d, want 0", got)
	}
	if got := len(m.Roster().Explorers); got != 2 {
		t.Fatalf("explorers after rejected save = %d, want 2", got)
	}
}

// TestSaveDefaultProfile_RejectsBelowTwo: dropping to a single explorer is refused (a panel needs 2+).
func TestSaveDefaultProfile_RejectsBelowTwo(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	p := defaultProfileDraft(m, t)
	p.Explorers = p.Explorers[:1]
	if _, err := m.SaveProfile(defaultName(m), p); err == nil {
		t.Fatal("expected a <2-explorers rejection, got nil")
	}
	if got := m.Generation(); got != 0 {
		t.Fatalf("generation after rejected save = %d, want 0", got)
	}
	if got := len(m.Roster().Explorers); got != 2 {
		t.Fatalf("explorers after rejected save = %d, want 2", got)
	}
}

func TestPersistsAndReloads(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	t.Setenv("AIMESH_HOME", home)

	m1, err := New(cwd, fakeRoster())
	if err != nil {
		t.Fatalf("New m1: %v", err)
	}
	p := defaultProfileDraft(m1, t)
	p.Explorers = append(p.Explorers, roster.Explorer{Adapter: "fake", Model: "fake-d", Effort: "low"})
	if _, err := m1.SaveProfile(defaultName(m1), p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	// A second manager over the SAME cwd+home starts from a different roster, then Reload reads the
	// persisted one m1 wrote (3 explorers).
	m2, err := New(cwd, roster.Roster{
		Explorers: []roster.Explorer{{Adapter: "fake", Model: "x", Effort: "high"}, {Adapter: "fake", Model: "y", Effort: "low"}},
		Collator:  roster.Collator{Adapter: "fake", Model: "z", Effort: "high"},
	})
	if err != nil {
		t.Fatalf("New m2: %v", err)
	}
	if err := m2.reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(m2.Roster().Explorers); got != 3 {
		t.Fatalf("reloaded explorers = %d, want 3 (persisted by m1)", got)
	}
}

func TestConfigureAdapterPath_WritesSharedYAML(t *testing.T) {
	home, cwd := testEnv(t)
	m, err := New(cwd, fakeRoster())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "codexbin")
	if werr := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); werr != nil {
		t.Fatalf("write fake bin: %v", werr)
	}

	if _, err := m.ConfigureAdapterPath("codex-cli", bin); err != nil {
		t.Fatalf("ConfigureAdapterPath: %v", err)
	}
	if got := m.Generation(); got != 1 {
		t.Fatalf("generation after configure = %d, want 1", got)
	}

	// The path landed in the user-scope shared adapters.yaml (under AIMESH_HOME=home).
	loc, err := adapterlocations.Load(filepath.Join(home, ".aimesh", "adapters.yaml"))
	if err != nil {
		t.Fatalf("load shared adapters: %v", err)
	}
	e, ok := loc.Adapters["codex-cli"]
	if !ok || e.Path == nil || *e.Path != bin {
		t.Fatalf("shared adapters codex-cli = %+v, want path %q", e, bin)
	}
}

// TestOverview_AdapterCountCountsOnlyConfigured pins the Overview count to adapters the user has
// actually SET UP. A recipe exploremesh knows how to drive but that has no recorded path is AVAILABLE,
// not configured, and must not be counted; nor must the built-in `fake`, which is a deterministic test
// fixture rather than a provider the user configured.
func TestOverview_AdapterCountCountsOnlyConfigured(t *testing.T) {
	_, cwd := testEnv(t)
	m, err := New(cwd, fakeRoster())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Nothing configured yet: every shell recipe is merely available, and `fake` does not count.
	if got := m.Overview().AdapterCount; got != 0 {
		t.Errorf("adapterCount with nothing configured = %d, want 0 (recipes are available, not configured)", got)
	}

	bin := filepath.Join(t.TempDir(), "codexbin")
	if werr := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); werr != nil {
		t.Fatalf("write fake bin: %v", werr)
	}
	if _, err := m.ConfigureAdapterPath("codex-cli", bin); err != nil {
		t.Fatalf("ConfigureAdapterPath: %v", err)
	}
	if got := m.Overview().AdapterCount; got != 1 {
		t.Errorf("adapterCount after configuring one adapter = %d, want 1", got)
	}

	// And it must agree with the Adapters tab: configured tiles, excluding the built-in fake.
	configured := 0
	for _, av := range m.AdapterViews() {
		if av.Configured && av.Name != registry.FakeAdapter {
			configured++
		}
	}
	if got := m.Overview().AdapterCount; got != configured {
		t.Errorf("adapterCount=%d but the Adapters tab shows %d configured (excluding fake) — the "+
			"Overview must not disagree with the tab", got, configured)
	}
}

func TestRemoveAdapter_BlockedWhenUsed(t *testing.T) {
	// A roster whose explorer 0 uses codex-cli — removing it must be blocked with the using slot.
	r := roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "codex-cli", Model: "gpt-x", Effort: "high"},
			{Adapter: "fake", Model: "fake-b", Effort: "medium"},
		},
		Collator: roster.Collator{Adapter: "fake", Model: "fake-c", Effort: "high"},
	}
	m := newTestManager(t, r)
	_, err := m.RemoveAdapter("codex-cli")
	if err == nil {
		t.Fatal("expected a blocked error, got nil")
	}
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("error type = %T, want *BlockedError", err)
	}
	if len(be.UsedBy) == 0 || be.UsedBy[0].Ref != "explorer-0" {
		t.Fatalf("blocked usedBy = %+v, want the explorer-0 slot", be.UsedBy)
	}
	if got := m.Generation(); got != 0 {
		t.Fatalf("generation after blocked remove = %d, want 0", got)
	}
}

// TestTitleCaseKey proves the unknown-adapter fallback never leaks the internal `-cli` key suffix into a
// user-visible product name (it distinguishes a terminal binary from the vendor's desktop app; it is not
// part of the product's name). Mirrors reviewmesh's TestAdapterDisplayName_Fallback.
func TestTitleCaseKey(t *testing.T) {
	cases := map[string]string{
		"new-vendor-cli": "New Vendor",
		"opencode-cli":   "Opencode",
		"single":         "Single",
		"under_score":    "Under Score",
		"":               "",
		"some-code":      "Some Code", // `code` IS a product word (Claude Code) — only `cli` is dropped
		"cli":            "Cli",       // never strip the only segment
	}
	for key, want := range cases {
		if got := titleCaseKey(key); got != want {
			t.Errorf("titleCaseKey(%q) = %q, want %q", key, got, want)
		}
	}
}
