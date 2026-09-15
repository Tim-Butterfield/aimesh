package setup

import "testing"

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
