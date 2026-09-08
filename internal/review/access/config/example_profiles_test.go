package config

import (
	"strings"
	"testing"
)

// TestSeed_ShippedDefaultIsUnconfigured is the release-readiness invariant: the shipped `default`
// profile is UNCONFIGURED — its required lanes (author_remediator + reviewer) are PRESENT but wired
// to NO adapter, so a fresh install is honestly not-ready (Doctor flags it) rather than pre-wired to
// the fake adapter. The deterministic fake coverage lives in the shipped-but-hidden FakeProfile.
func TestSeed_ShippedDefaultIsUnconfigured(t *testing.T) {
	seed := Default().Profiles
	def, ok := seed["default"]
	if !ok {
		t.Fatal("the shipped seed must contain a `default` profile")
	}
	// The required roles must be PRESENT (the lanes exist) but UNCONFIGURED (no adapter/model).
	for _, role := range []string{"author_remediator", "reviewer"} {
		lane, ok := def.Lanes[role]
		if !ok {
			t.Errorf("shipped default must still define the required %q lane (present but unconfigured)", role)
			continue
		}
		if lane.Adapter != "" || lane.Model != "" {
			t.Errorf("shipped default %q lane must be UNCONFIGURED (empty adapter/model), got adapter=%q model=%q", role, lane.Adapter, lane.Model)
		}
	}
	if len(def.AdapterPreference) != 0 {
		t.Errorf("shipped default must have no adapterPreference, got %v", def.AdapterPreference)
	}
}

// TestSeed_HiddenFakeProfileShippedAndFakeOnly proves the fake coverage moved to the shipped-but-
// hidden FakeProfile: it is present in the seed (so tests/golden/the ACP smoke can select it without
// the user opting in) and every lane uses the built-in `fake` adapter.
func TestSeed_HiddenFakeProfileShippedAndFakeOnly(t *testing.T) {
	seed := Default().Profiles
	fk, ok := seed[FakeProfile]
	if !ok {
		t.Fatalf("the shipped seed must contain the hidden fake profile %q", FakeProfile)
	}
	if !IsHiddenProfile(FakeProfile) {
		t.Errorf("%q must report as a hidden profile", FakeProfile)
	}
	for role, lane := range fk.Lanes {
		if lane.Adapter != "fake" {
			t.Errorf("hidden fake profile lane %q uses adapter %q — it must be fake-only", role, lane.Adapter)
		}
	}
	if _, ok := fk.Lanes["author_remediator"]; !ok {
		t.Error("hidden fake profile must define author_remediator")
	}
	if _, ok := fk.Lanes["reviewer"]; !ok {
		t.Error("hidden fake profile must define reviewer")
	}
}

// TestExampleProfiles_AreNotInTheSeed guards the split: the seven opt-in/smoke example profiles are
// available via ExampleProfiles but must NOT leak into the shipped seed.
func TestExampleProfiles_AreNotInTheSeed(t *testing.T) {
	seed := Default().Profiles
	ex := ExampleProfiles()
	wantExamples := []string{
		"fully-local-ollama", "native-three-provider", "devin-gateway-three-provider",
		"claude-code-smoke", "codex-cli-smoke", "agy-cli-smoke", "devin-cli-smoke",
	}
	if len(ex) != len(wantExamples) {
		t.Errorf("ExampleProfiles count = %d, want %d", len(ex), len(wantExamples))
	}
	for _, name := range wantExamples {
		if _, ok := ex[name]; !ok {
			t.Errorf("ExampleProfiles missing %q", name)
		}
		if _, leaked := seed[name]; leaked {
			t.Errorf("example profile %q leaked into the shipped seed", name)
		}
	}
}

// TestWithExampleProfiles_MergesExistingWins confirms WithExampleProfiles adds the examples without
// clobbering the seed's `default` (and a user's own same-named profile wins).
func TestWithExampleProfiles_MergesExistingWins(t *testing.T) {
	base := Default()
	base.Profiles["native-three-provider"] = Profile{Description: "mine"}
	merged := WithExampleProfiles(base)
	if _, ok := merged.Profiles["default"]; !ok {
		t.Error("WithExampleProfiles dropped the seed `default`")
	}
	if got := merged.Profiles["native-three-provider"].Description; got != "mine" {
		t.Errorf("existing profile should win over the example, got description %q", got)
	}
	if _, ok := merged.Profiles["fully-local-ollama"]; !ok {
		t.Error("WithExampleProfiles did not add the example profiles")
	}
}

// TestProfileNotFoundGuidance points a legacy `--profile <example>` / defaultProfile at a moved
// example and asserts the resolve error carries actionable creation guidance (design refinement B).
func TestProfileNotFoundGuidance(t *testing.T) {
	if g := ProfileNotFoundGuidance("fully-local-ollama"); !strings.Contains(g, "setup --profile fully-local-ollama") {
		t.Errorf("example-profile guidance must name the setup command, got %q", g)
	}
	if g := ProfileNotFoundGuidance("totally-made-up"); g != "" {
		t.Errorf("a non-example name must get no guidance, got %q", g)
	}
	// End-to-end through Resolve: a missing example profile surfaces the guidance.
	_, err := Default().Resolve(ResolveRequest{Profile: "native-three-provider", Surface: "cli"})
	if err == nil || !strings.Contains(err.Error(), "example profile") {
		t.Fatalf("resolving a moved example profile must fail with creation guidance, got %v", err)
	}
}
