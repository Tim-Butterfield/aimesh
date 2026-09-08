package setup

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/utility/doctor"
)

func TestRepairOptions_AdapterIssueIsApplyable(t *testing.T) {
	m := mutMgr(t)
	rep := doctor.Report{Checks: []doctor.Check{
		{Name: "config: profiles present", OK: true, Detail: "ok"},
		{Name: "adapter: codex-cli", OK: false, Detail: "required — binary not found"},
		{Name: "profile: native-three-provider ready", OK: false, Detail: "requires unavailable adapter codex-cli"},
	}}
	opts := m.RepairOptions(rep)
	// The passing check is skipped; the two failing ones are projected.
	if len(opts) != 2 {
		t.Fatalf("want 2 options, got %d: %+v", len(opts), opts)
	}
	var adapterOpt, guidanceOpt *RepairOption
	for i := range opts {
		switch opts[i].Check {
		case "adapter: codex-cli":
			adapterOpt = &opts[i]
		case "profile: native-three-provider ready":
			guidanceOpt = &opts[i]
		}
	}
	if adapterOpt == nil || !adapterOpt.Applyable || adapterOpt.Kind != "set_binary_path" || adapterOpt.Target != "codex-cli" {
		t.Errorf("adapter issue should be an applyable set_binary_path repair: %+v", adapterOpt)
	}
	if adapterOpt != nil && adapterOpt.Command == "" {
		t.Error("adapter repair should carry the exact CLI command as guidance too")
	}
	if guidanceOpt == nil || guidanceOpt.Applyable || guidanceOpt.Kind != "guidance" {
		t.Errorf("a non-adapter issue must be guidance-only, not applyable: %+v", guidanceOpt)
	}
}

func TestApplyRepair_SetBinaryPath_Writes(t *testing.T) {
	m := mutMgr(t)
	bin := execFile(t, "codex")
	res, err := m.ApplyRepair("set_binary_path", "codex-cli", bin)
	if err != nil {
		t.Fatalf("apply repair: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatal("expected config written")
	}
	// The set_binary_path repair records the path in the shared adapters.yaml (not config.yaml).
	if got := sharedAdapterPath(t, userSharedPath(t), "codex-cli"); got == nil || *got != bin {
		t.Errorf("codex-cli path = %v, want %q", got, bin)
	}
}

func TestApplyRepair_GuidanceKindRejected(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.ApplyRepair("reauthenticate", "codex-cli", "irrelevant"); err == nil {
		t.Error("a guidance-only repair kind must be rejected (no automatic write)")
	}
}

func TestApplyRepair_InvalidPathRejected(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.ApplyRepair("set_binary_path", "codex-cli", "/no/such/binary-xyz"); err == nil {
		t.Error("a nonexistent binary path must be rejected before any write")
	}
}
