package setup

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

func TestPlanAdapterPathPatch_SingleMinimalOp(t *testing.T) {
	e := New()
	p, err := e.PlanAdapterPathPatch("claude-code", "/bin/claude")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(p.Ops) != 1 {
		t.Fatalf("want exactly one op, got %d", len(p.Ops))
	}
	op := p.Ops[0]
	if len(op.Path) != 3 || op.Path[0] != "adapters" || op.Path[1] != "claude-code" || op.Path[2] != "path" {
		t.Errorf("path = %v, want [adapters claude-code path]", op.Path)
	}
	if op.Value != "/bin/claude" {
		t.Errorf("value = %v, want /bin/claude", op.Value)
	}
	// applying it to an existing config preserves unrelated fields
	root := map[string]any{"defaultProfile": "x", "adapters": map[string]any{"codex-cli": map[string]any{"path": "/c"}}}
	if err := p.Apply(root); err != nil {
		t.Fatal(err)
	}
	if root["defaultProfile"] != "x" || root["adapters"].(map[string]any)["codex-cli"].(map[string]any)["path"] != "/c" {
		t.Error("adapter-path patch altered unrelated config")
	}
}

func TestPlanProfileCopy_FullSubtreeSetOp(t *testing.T) {
	e := New()
	content := map[string]any{"description": "d", "lanes": map[string]any{"reviewer": map[string]any{"adapter": "fake"}}}
	p, err := e.PlanProfileCopy("mycopy", content)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(p.Ops) != 1 {
		t.Fatalf("want one op, got %d", len(p.Ops))
	}
	op := p.Ops[0]
	if len(op.Path) != 2 || op.Path[0] != "profiles" || op.Path[1] != "mycopy" || op.Delete {
		t.Errorf("op = %+v, want set profiles.mycopy", op)
	}
	// applying replaces the whole target subtree (copy-to-existing semantics)
	root := map[string]any{"profiles": map[string]any{"mycopy": map[string]any{"stale": true}}}
	if err := p.Apply(root); err != nil {
		t.Fatal(err)
	}
	got := root["profiles"].(map[string]any)["mycopy"].(map[string]any)
	if _, stale := got["stale"]; stale {
		t.Error("copy-to-existing must replace the whole target subtree, not merge stale keys")
	}
}

func TestPlanProfileCopy_RejectsEmpty(t *testing.T) {
	e := New()
	if _, err := e.PlanProfileCopy("", map[string]any{}); err == nil {
		t.Error("empty target must be rejected")
	}
	if _, err := e.PlanProfileCopy("t", nil); err == nil {
		t.Error("nil content must be rejected")
	}
}

func TestPlanAdapterRemoval_DeleteOp(t *testing.T) {
	e := New()
	p, err := e.PlanAdapterRemoval("codex-cli")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(p.Ops) != 1 || !p.Ops[0].Delete {
		t.Fatalf("want one delete op, got %+v", p.Ops)
	}
	if op := p.Ops[0]; len(op.Path) != 2 || op.Path[0] != "adapters" || op.Path[1] != "codex-cli" {
		t.Errorf("path = %v, want [adapters codex-cli]", p.Ops[0].Path)
	}
	if _, err := e.PlanAdapterRemoval(""); err == nil {
		t.Error("empty adapter name must be rejected")
	}
}

func TestPlanAdapterPathPatch_RejectsEmpty(t *testing.T) {
	e := New()
	if _, err := e.PlanAdapterPathPatch("", "/bin/x"); err == nil {
		t.Error("empty adapter must be rejected")
	}
	if _, err := e.PlanAdapterPathPatch("claude-code", ""); err == nil {
		t.Error("empty path must be rejected")
	}
}

func TestPlanLaneChoices_MinimalOpsPerRole(t *testing.T) {
	e := New()
	p, err := e.PlanLaneChoices("native", []LaneChoice{
		{Role: "reviewer", Adapter: "codex-cli", Model: "openai-gpt-5.4-medium"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(p.Ops) != 2 {
		t.Fatalf("want 2 ops (adapter + model), got %d", len(p.Ops))
	}
	// both ops target the same lane under the profile, nothing else
	for _, op := range p.Ops {
		if op.Path[0] != "profiles" || op.Path[1] != "native" || op.Path[2] != "lanes" || op.Path[3] != "reviewer" {
			t.Errorf("op path not scoped to profiles.native.lanes.reviewer: %v", op.Path)
		}
	}
}

func TestPlanLaneChoices_RejectsMissingFieldsAndProfile(t *testing.T) {
	e := New()
	if _, err := e.PlanLaneChoices("", []LaneChoice{{Role: "reviewer", Adapter: "a", Model: "m"}}); err == nil {
		t.Error("empty profile must be rejected")
	}
	if _, err := e.PlanLaneChoices("native", []LaneChoice{{Role: "reviewer", Adapter: "", Model: "m"}}); err == nil {
		t.Error("missing adapter must be rejected")
	}
}

func TestPatchFor_RepairActions(t *testing.T) {
	e := New()
	// set_binary_path → same as PlanAdapterPathPatch
	p, err := e.PatchFor(RepairAction{Kind: ActionSetBinaryPath, Target: "claude-code"}, "/bin/claude")
	if err != nil || len(p.Ops) != 1 || p.Ops[0].Value != "/bin/claude" {
		t.Errorf("set_binary_path patch = %+v err=%v", p, err)
	}
	// reauthenticate → empty (no config write)
	rp, err := e.PatchFor(RepairAction{Kind: ActionReauthenticate, Target: "claude-code"}, "")
	if err != nil {
		t.Fatalf("reauthenticate: %v", err)
	}
	if !rp.IsEmpty() {
		t.Errorf("reauthenticate must produce no patch, got %+v", rp)
	}
	// unsupported kind → clear error, no guessed edit
	if _, err := e.PatchFor(RepairAction{Kind: "bogus", Target: "x"}, "v"); err == nil {
		t.Error("unsupported repair action kind must error")
	}
	// the produced patch type is config.ConfigPatch (compile-time + applyable)
	var _ config.ConfigPatch = p
}
