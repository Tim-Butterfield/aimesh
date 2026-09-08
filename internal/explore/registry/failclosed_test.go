package registry

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// TestBuild_FailClosed proves the registry resolves `fake` (under the internal gate) + real recipes
// but returns an unrecognized adapter name as an UNKNOWN (config error) rather than silently
// substituting a fake.
func TestBuild_FailClosed(t *testing.T) {
	t.Setenv("AIMESH_INTERNAL_FAKE", "1") // the fake key resolves only under the internal gate
	r := roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: FakeAdapter, Model: "m1", Effort: "high"},
			{Adapter: "codex-cli", Model: "m2", Effort: "high"}, // a real shell recipe
			{Adapter: "bogus-adapter", Model: "m3", Effort: "high"},
		},
		Collator: roster.Collator{Adapter: FakeAdapter, Model: "mc", Effort: "high"},
	}
	plan, err := r.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	reg, unknown := Build(plan, nil, nil, 0)

	if _, ok := reg[FakeAdapter]; !ok {
		t.Error("the explicit `fake` adapter must resolve")
	}
	if _, ok := reg["codex-cli"]; !ok {
		t.Error("a real shell recipe must resolve")
	}
	if _, ok := reg["bogus-adapter"]; ok {
		t.Error("an unknown adapter must NOT be registered — fail closed, never a silent fake")
	}
	if len(unknown) != 1 || unknown[0] != "bogus-adapter" {
		t.Errorf("unknown = %v, want [bogus-adapter]", unknown)
	}
}
