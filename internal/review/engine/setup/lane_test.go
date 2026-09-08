package setup

import "testing"

func TestPlanLaneEdit_ExistingLaneOmitsExecution(t *testing.T) {
	e := New()
	patch, err := e.PlanLaneEdit("default", "reviewer", "claude-code", "claude-code-default", "")
	if err != nil {
		t.Fatalf("PlanLaneEdit: %v", err)
	}
	if len(patch.Ops) != 2 {
		t.Fatalf("ops=%d want 2 (adapter+model only, execution untouched)", len(patch.Ops))
	}
	for _, op := range patch.Ops {
		if op.Path[len(op.Path)-1] == "execution" {
			t.Fatalf("execution must not be set for an existing lane: %+v", op)
		}
	}
}

func TestPlanLaneEdit_NewLaneSetsExecution(t *testing.T) {
	e := New()
	patch, err := e.PlanLaneEdit("default", "cross_check", "codex-cli", "codex-cli-default", "adapter")
	if err != nil {
		t.Fatalf("PlanLaneEdit: %v", err)
	}
	if len(patch.Ops) != 3 {
		t.Fatalf("ops=%d want 3 (adapter+model+execution)", len(patch.Ops))
	}
	last := patch.Ops[2]
	if last.Path[len(last.Path)-1] != "execution" || last.Value != "adapter" {
		t.Fatalf("expected a trailing execution op, got %+v", last)
	}
}

func TestPlanProfileDeletion(t *testing.T) {
	e := New()
	if _, err := e.PlanProfileDeletion(""); err == nil {
		t.Error("empty name must fail")
	}
	if _, err := e.PlanProfileDeletion("default"); err == nil {
		t.Error("the `default` profile must never be deletable")
	}
	patch, err := e.PlanProfileDeletion("scratch-profile")
	if err != nil {
		t.Fatalf("PlanProfileDeletion: %v", err)
	}
	if len(patch.Ops) != 1 || !patch.Ops[0].Delete || patch.Ops[0].Path[1] != "scratch-profile" {
		t.Fatalf("expected a single delete op for profiles.scratch-profile, got %+v", patch.Ops)
	}
}

func TestPlanLaneClear(t *testing.T) {
	e := New()
	if _, err := e.PlanLaneClear("", "reviewer"); err == nil {
		t.Error("empty profile must fail")
	}
	if _, err := e.PlanLaneClear("p", ""); err == nil {
		t.Error("empty role must fail")
	}
	patch, err := e.PlanLaneClear("myprofile", "cross_check")
	if err != nil {
		t.Fatalf("PlanLaneClear: %v", err)
	}
	if len(patch.Ops) != 1 || !patch.Ops[0].Delete {
		t.Fatalf("expected a single delete op, got %+v", patch.Ops)
	}
	if p := patch.Ops[0].Path; len(p) != 4 || p[0] != "profiles" || p[1] != "myprofile" || p[2] != "lanes" || p[3] != "cross_check" {
		t.Fatalf("delete path = %v, want [profiles myprofile lanes cross_check] (never a modelCatalog key)", p)
	}
}

func TestPatchFor_SetDefaultProfileIsFixedValue(t *testing.T) {
	e := New()
	patch, err := e.PatchFor(RepairAction{Kind: ActionSetDefaultProfile}, "some-other-profile")
	if err != nil {
		t.Fatalf("PatchFor: %v", err)
	}
	if len(patch.Ops) != 1 || patch.Ops[0].Value != "default" {
		t.Fatalf("the repair must always write the literal \"default\" (never a caller value), got %+v", patch.Ops)
	}
}

func TestPlanLaneEdit_RequiresAllFields(t *testing.T) {
	e := New()
	if _, err := e.PlanLaneEdit("", "reviewer", "fake", "fake-model", ""); err == nil {
		t.Error("empty profile should fail")
	}
	if _, err := e.PlanLaneEdit("default", "reviewer", "", "fake-model", ""); err == nil {
		t.Error("empty adapter should fail")
	}
	if _, err := e.PlanLaneEdit("default", "reviewer", "fake", "", ""); err == nil {
		t.Error("empty model should fail")
	}
}
