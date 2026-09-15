package manager

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// secondProfile is a valid three-explorer profile distinct from fakeRoster, with a non-default mode.
func secondProfile() profile.Profile {
	return profile.Profile{
		Explorers: []roster.Explorer{
			{Adapter: "fake", Model: "wide-1", Effort: "high"},
			{Adapter: "fake", Model: "wide-2", Effort: "medium"},
			{Adapter: "fake", Model: "wide-3", Effort: "low"},
		},
		Collator:    roster.Collator{Adapter: "fake", Model: "wide-collate"},
		DefaultMode: "synthesize",
	}
}

// A fresh manager over an empty home seeds a one-profile `default` set from the starting roster.
func TestProfiles_SeedsDefaultProfile(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	set := m.Profiles()
	if set.DefaultProfile != profile.DefaultProfileName {
		t.Fatalf("default profile = %q, want %q", set.DefaultProfile, profile.DefaultProfileName)
	}
	p, ok := set.Get(profile.DefaultProfileName)
	if len(set.Profiles) != 1 || !ok || len(p.Explorers) != 2 {
		t.Fatalf("seeded profiles = %+v, want one 2-explorer default", set.Profiles)
	}
}

// Adding then replacing a named profile reports each write, and the result is what a new manager loads.
func TestSaveProfile_AddReplaceAndRoundTrip(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("AIMESH_HOME", home)

	m, err := New(cwd, fakeRoster())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msgs, err := m.SaveProfile("wide", secondProfile())
	if err != nil {
		t.Fatalf("SaveProfile add: %v", err)
	}
	if len(msgs) == 0 || !strings.Contains(msgs[0], "Added") {
		t.Errorf("add should report Added, got %v", msgs)
	}

	replaced := secondProfile()
	replaced.Explorers = replaced.Explorers[:2]
	replaced.DefaultMode = "map"
	msgs, err = m.SaveProfile("wide", replaced)
	if err != nil {
		t.Fatalf("SaveProfile replace: %v", err)
	}
	if len(msgs) == 0 || !strings.Contains(msgs[0], "Updated") {
		t.Errorf("replace should report Updated, got %v", msgs)
	}

	m2, err := New(cwd, fakeRoster())
	if err != nil {
		t.Fatalf("New m2: %v", err)
	}
	set := m2.Profiles()
	d, ok := set.Get("wide")
	if !ok {
		t.Fatal("the saved profile was not loaded")
	}
	if len(d.Explorers) != 2 || d.DefaultMode != "map" || set.DefaultProfile == "wide" {
		t.Fatalf("loaded profile = %+v, want the 2-explorer map profile (not the default)", d)
	}
	// Profiles keep their authored order.
	if d.Explorers[0].Model != "wide-1" || d.Explorers[1].Model != "wide-2" {
		t.Errorf("profile explorers must keep their authored order: %+v", d.Explorers)
	}
}

// An empty name, an invalid roster and an unknown mode are refused, persisting nothing.
func TestSaveProfile_Rejections(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	if _, err := m.SaveProfile("  ", secondProfile()); err == nil {
		t.Error("an empty profile name must be rejected")
	}
	bad := secondProfile()
	bad.Explorers = bad.Explorers[:1] // <2 explorers
	if _, err := m.SaveProfile("thin", bad); err == nil {
		t.Error("a <2-explorer profile must be rejected")
	}
	badMode := secondProfile()
	badMode.DefaultMode = "not-a-mode"
	if _, err := m.SaveProfile("odd", badMode); err == nil {
		t.Error("an unknown defaultMode must be rejected")
	}
	if got := len(m.Profiles().Profiles); got != 1 {
		t.Fatalf("profiles after rejected writes = %d, want just the seeded default", got)
	}
}

// Saving another profile does not move the default, and the cached roster keeps tracking `default`.
func TestDefaultProfileIsPinned(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	if _, err := m.SaveProfile("wide", secondProfile()); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if got := m.Profiles().DefaultProfile; got != profile.DefaultProfileName {
		t.Fatalf("default profile = %q — saving another profile must never move the default", got)
	}
	if got := len(m.RosterView().Explorers); got != 2 {
		t.Fatalf("default roster explorers = %d, want the default profile's 2", got)
	}
}

// Deleting an unknown or the default profile is refused; a non-default profile deletes.
func TestDeleteProfile(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	if _, err := m.DeleteProfile("nope"); err == nil {
		t.Error("deleting an unknown profile must error")
	}
	_, err := m.DeleteProfile(m.Profiles().DefaultProfile)
	if err == nil {
		t.Fatal("deleting the default profile must be refused")
	}
	if !strings.Contains(err.Error(), "default profile") {
		t.Errorf("refusal should explain the default-profile rule: %v", err)
	}

	if _, err := m.SaveProfile("wide", secondProfile()); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if _, err := m.DeleteProfile("wide"); err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	set := m.Profiles()
	if got := len(set.Profiles); got != 1 {
		t.Fatalf("profiles after delete = %d, want 1", got)
	}
	if _, ok := set.Get("wide"); ok {
		t.Error("the deleted profile must be gone from the persisted set")
	}
}

// Profiles returns a copy that does not alias the manager's map or explorer slices.
func TestProfiles_ReturnsADeepCopy(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	got := m.Profiles()
	delete(got.Profiles, profile.DefaultProfileName)
	if set := m.Profiles(); set.DefaultProfile != profile.DefaultProfileName || len(set.Profiles) != 1 {
		t.Fatalf("mutating the returned set reached the manager's state: %+v", set)
	}
	ex := m.Profiles().Profiles[profile.DefaultProfileName].Explorers
	ex[0].Model = "clobbered"
	if m.RosterView().Explorers[0].Model == "clobbered" {
		t.Error("mutating a returned profile's explorer slice reached the manager's roster")
	}
}
