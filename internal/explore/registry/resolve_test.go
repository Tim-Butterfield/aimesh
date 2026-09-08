package registry

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// planOf builds a validated plan from an explorer/collator triple set (2 explorers is the roster minimum).
// It also unlocks the internal fake harness: several fixtures here fill non-load-bearing slots with the
// env-gated `fake` key, which resolves only under the gate.
func planOf(t *testing.T, r roster.Roster) roster.Plan {
	t.Helper()
	t.Setenv("AIMESH_INTERNAL_FAKE", "1")
	plan, err := r.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan
}

// TestBuild_ShellRecipeResolves proves a roster naming a code-owned shell recipe is backed by the REAL
// shell adapter (not a fake, not an unknown): the registered value is a *shell.Adapter carrying that
// recipe's name.
func TestBuild_ShellRecipeResolves(t *testing.T) {
	plan := planOf(t, roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "claude-code", Model: "opus", Effort: "high"},
			{Adapter: "ollama", Model: "llama3:8b", Effort: "medium"},
		},
		Collator: roster.Collator{Adapter: "codex-cli", Model: "gpt-5-codex", Effort: "high"},
	})
	reg, unknown := Build(plan, nil, nil, 0)
	if len(unknown) != 0 {
		t.Fatalf("shell recipes must resolve, got unknown = %v", unknown)
	}
	for _, name := range []string{"claude-code", "ollama", "codex-cli"} {
		a, ok := reg[name]
		if !ok {
			t.Fatalf("recipe %q did not resolve", name)
		}
		sa, ok := a.(*shell.Adapter)
		if !ok {
			t.Fatalf("recipe %q resolved to %T, want *shell.Adapter (the REAL adapter)", name, a)
		}
		if sa.Name() != name {
			t.Errorf("recipe %q resolved to an adapter named %q", name, sa.Name())
		}
	}
}

// TestBuild_ACPInstanceResolves proves a USER-DEFINED ACP instance (from the shared adapters.yaml
// `acpAdapters`) resolves to the real ACP-client adapter — the open half of the registry, which has no
// fixed CLI list and therefore cannot be covered by the recipe table.
func TestBuild_ACPInstanceResolves(t *testing.T) {
	plan := planOf(t, roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "my-acp-agent", Model: "m1", Effort: "high"},
			{Adapter: FakeAdapter, Model: "m2", Effort: "high"},
		},
		Collator: roster.Collator{Adapter: FakeAdapter, Model: "mc", Effort: "high"},
	})
	insts := map[string]acpagent.Instance{
		"my-acp-agent": {Name: "my-acp-agent", Detect: "my-agent", Path: "/opt/bin/my-agent", Args: []string{"--acp"}},
	}
	reg, unknown := Build(plan, nil, insts, 0)
	if len(unknown) != 0 {
		t.Fatalf("a configured ACP instance must resolve, got unknown = %v", unknown)
	}
	aa, ok := reg["my-acp-agent"].(*acpagent.Adapter)
	if !ok {
		t.Fatalf("ACP instance resolved to %T, want *acpagent.Adapter", reg["my-acp-agent"])
	}
	// The instance's configured binary path + detect name travel into the adapter (a user-defined
	// instance is useless if the registry drops the very fields that locate its binary).
	if aa.Path != "/opt/bin/my-agent" {
		t.Errorf("ACP adapter Path = %q, want the instance's configured path", aa.Path)
	}
	if aa.DetectName() != "my-agent" {
		t.Errorf("ACP adapter detect = %q, want my-agent", aa.DetectName())
	}
}

// TestBuild_PathOverrideApplied proves the layered binary-path override (adapters.yaml `adapters.<n>.path`)
// reaches the built shell adapter — the reason `paths` is threaded through Build at all.
func TestBuild_PathOverrideApplied(t *testing.T) {
	plan := planOf(t, roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "codex-cli", Model: "m1", Effort: "high"},
			{Adapter: FakeAdapter, Model: "m2", Effort: "high"},
		},
		Collator: roster.Collator{Adapter: FakeAdapter, Model: "mc", Effort: "high"},
	})
	reg, unknown := Build(plan, map[string]string{"codex-cli": "/custom/bin/codex"}, nil, 0)
	if len(unknown) != 0 {
		t.Fatalf("unexpected unknown adapters: %v", unknown)
	}
	sa, ok := reg["codex-cli"].(*shell.Adapter)
	if !ok {
		t.Fatalf("codex-cli resolved to %T, want *shell.Adapter", reg["codex-cli"])
	}
	if sa.Path != "/custom/bin/codex" {
		t.Errorf("path override not applied: Path = %q, want /custom/bin/codex", sa.Path)
	}
	// The override is per-NAME: an adapter that was not named in `paths` must not pick it up. Checked on a
	// second shell recipe rather than on `fake` (which is not a shell adapter at all).
	plan2 := planOf(t, roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "codex-cli", Model: "m1", Effort: "high"},
			{Adapter: "claude-code", Model: "m2", Effort: "high"},
		},
		Collator: roster.Collator{Adapter: FakeAdapter, Model: "mc", Effort: "high"},
	})
	reg2, _ := Build(plan2, map[string]string{"codex-cli": "/custom/bin/codex"}, nil, 0)
	if other, ok := reg2["claude-code"].(*shell.Adapter); ok && other.Path != "" {
		t.Errorf("an unnamed adapter picked up a path override: %q", other.Path)
	}
}

// TestBuild_ACPInstanceWinsOverShellRecipe pins the PRECEDENCE when one name exists in both maps: Build
// layers the user-defined ACP instances OVER the code-owned shell recipes, so a user who configures an
// ACP instance under a recipe's name gets THEIR agent. The precedence is load-bearing (it is how a user
// overrides a shipped recipe) and silent either way, so it is pinned rather than left to `maps.Copy`.
func TestBuild_ACPInstanceWinsOverShellRecipe(t *testing.T) {
	plan := planOf(t, roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "codex-cli", Model: "m1", Effort: "high"},
			{Adapter: FakeAdapter, Model: "m2", Effort: "high"},
		},
		Collator: roster.Collator{Adapter: FakeAdapter, Model: "mc", Effort: "high"},
	})
	insts := map[string]acpagent.Instance{
		"codex-cli": {Name: "codex-cli", Detect: "codex", Path: "/opt/bin/codex-acp"},
	}
	// The shell path override for the SAME name is supplied too, so the test proves which map won rather
	// than which map happened to be non-empty.
	reg, unknown := Build(plan, map[string]string{"codex-cli": "/usr/bin/codex"}, insts, 0)
	if len(unknown) != 0 {
		t.Fatalf("unexpected unknown adapters: %v", unknown)
	}
	aa, ok := reg["codex-cli"].(*acpagent.Adapter)
	if !ok {
		t.Fatalf("codex-cli resolved to %T, want *acpagent.Adapter (the ACP instance must win)", reg["codex-cli"])
	}
	if aa.Path != "/opt/bin/codex-acp" {
		t.Errorf("the winning adapter carries Path %q, want the ACP instance's path", aa.Path)
	}
}
