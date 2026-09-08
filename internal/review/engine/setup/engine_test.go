package setup

import (
	"strings"
	"testing"
)

func TestPlanInitialSetup_WriteWhenAbsentPreserveWhenPresent(t *testing.T) {
	e := New()
	for _, kind := range []SetupKind{SetupKindDefault, SetupKindFullyLocalOllama} {
		if p := e.PlanInitialSetup(kind, ConfigPresence{Exists: false}); !p.ShouldWrite {
			t.Errorf("%s absent → ShouldWrite should be true (got %+v)", kind, p)
		}
		if p := e.PlanInitialSetup(kind, ConfigPresence{Exists: true}); p.ShouldWrite {
			t.Errorf("%s present → ShouldWrite must be false (preserve), got %+v", kind, p)
		}
	}
}

func TestValidateAdapterForPathCapture(t *testing.T) {
	e := New()
	configurable := []string{"agy-cli", "claude-code", "codex-cli"} // sorted, excludes fake
	if err := e.ValidateAdapterForPathCapture("claude-code", configurable); err != nil {
		t.Errorf("known adapter should be accepted: %v", err)
	}
	if err := e.ValidateAdapterForPathCapture("", configurable); err == nil {
		t.Error("empty adapter name must be rejected")
	}
	if err := e.ValidateAdapterForPathCapture("bogus", configurable); err == nil {
		t.Error("unknown adapter must be rejected")
	}
	// `fake` is not in the configurable list → rejected (not path-configurable)
	if err := e.ValidateAdapterForPathCapture("fake", configurable); err == nil {
		t.Error("fake must not be path-configurable")
	}
}

func TestRepairGuidance_AdapterIssueMapsToSetupCommand(t *testing.T) {
	e := New()
	g := e.RepairGuidance(RepairIssue{Name: "adapter: claude-code", Detail: `binary "claude" not found`, BinHint: "claude"})
	if g.WritesConfig {
		t.Error("doctor --fix guidance must not write config")
	}
	if g.Command != "reviewmesh setup --adapter claude-code --path /full/path/to/claude" {
		t.Errorf("adapter command = %q", g.Command)
	}
	if !strings.Contains(g.Message, "adapter: claude-code") {
		t.Errorf("message should name the issue: %q", g.Message)
	}
	// no BinHint → falls back to the adapter name
	g2 := e.RepairGuidance(RepairIssue{Name: "adapter: devin-cli", Detail: "x"})
	if g2.Command != "reviewmesh setup --adapter devin-cli --path /full/path/to/devin-cli" {
		t.Errorf("fallback hint command = %q", g2.Command)
	}
}

func TestRepairGuidance_NonAdapterIssueHasNoCommand(t *testing.T) {
	e := New()
	g := e.RepairGuidance(RepairIssue{Name: "profile: native ready", Detail: "requires unavailable adapter"})
	if g.Command != "" {
		t.Errorf("a non-adapter issue should have no single command, got %q", g.Command)
	}
	if g.WritesConfig {
		t.Error("guidance must not write config")
	}
	if !strings.Contains(g.Message, "Guidance:") {
		t.Errorf("non-adapter message should include general guidance: %q", g.Message)
	}
}
