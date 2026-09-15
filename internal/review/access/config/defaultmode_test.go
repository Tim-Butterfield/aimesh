package config

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// modeTestConfig is panelTestConfig plus a profile that resolves every role, so these tests fail
// on the MODE and never on adapter resolution.
func modeTestConfig() Config {
	c := panelTestConfig()
	c.Profiles["p"] = Profile{
		Lanes: map[string]Lane{
			"author_remediator": {Execution: "adapter", Adapter: "a", Model: "a-model"},
			"cross_check":       {Execution: "adapter", Adapter: "b", Model: "b-model"},
			"verifier":          {Execution: "adapter", Adapter: "a", Model: "a-model"},
		},
		Reviewers: []Lane{{Execution: "adapter", Adapter: "b", Model: "b-model"}},
	}
	return c
}

// TestUnspecifiedModeIsReport_OnEverySurface is the safe-default half. The CLI's ceiling is `apply`
// (it has to be, or `--apply` could not work), and an unspecified mode must still resolve to report:
// naming the command is consent to review, not consent for a model to edit your tree.
func TestUnspecifiedModeIsReport_OnEverySurface(t *testing.T) {
	c := modeTestConfig()
	for _, surface := range []string{"cli", "ci", "acp", "mcp"} {
		plan, err := c.Resolve(ResolveRequest{Profile: "p", Surface: surface})
		if err != nil {
			t.Fatalf("%s: %v", surface, err)
		}
		if plan.Mode != review.ModeReport {
			t.Errorf("%s: an unspecified mode resolved to %q — a run nobody asked to write must not write", surface, plan.Mode)
		}
	}
}

// TestExplicitApplyStillWorks is the other half: the cli entry is read as the CEILING, so an explicit
// --apply reaches apply even though an unspecified mode resolves to report.
func TestExplicitApplyStillWorks(t *testing.T) {
	c := modeTestConfig()
	plan, err := c.Resolve(ResolveRequest{Profile: "p", Surface: "cli", Mode: review.ModeApply})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.Mode != review.ModeApply {
		t.Fatalf("explicit --apply resolved to %q — the ceiling was lowered along with the default", plan.Mode)
	}
	// ...and patch, the middle mode, is neither clamped nor promoted.
	plan, err = c.Resolve(ResolveRequest{Profile: "p", Surface: "cli", Mode: review.ModePatch})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.Mode != review.ModePatch {
		t.Errorf("explicit --patch resolved to %q", plan.Mode)
	}
}

// TestTheCeilingStillClamps: a CLI surface whose shipped ceiling is `report` clamps an explicit apply.
// (The agent surfaces are gated by their launch grant instead; see TestResolve_AgentSurfacesIgnoreConfiguredCeilings.)
func TestTheCeilingStillClamps(t *testing.T) {
	c := modeTestConfig()
	for _, surface := range []string{"ci"} {
		plan, err := c.Resolve(ResolveRequest{Profile: "p", Surface: surface, Mode: review.ModeApply})
		if err != nil {
			t.Fatalf("%s: %v", surface, err)
		}
		if plan.Mode != review.ModeReport {
			t.Errorf("%s: an explicit apply resolved to %q, want it clamped to report by the ceiling", surface, plan.Mode)
		}
	}
}
