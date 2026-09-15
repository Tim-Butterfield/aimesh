package config

import (
	"slices"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// composedConfig has adapters and nothing else: no profiles, no catalog, and a default profile name
// that does not exist, so any lookup of saved configuration would fail the test.
func composedConfig() Config {
	return Config{
		Adapters: map[string]Adapter{
			"claude-code": {Path: "/bin/true"},
			"codex-cli":   {Path: "/bin/true"},
		},
		DefaultProfile: "absent",
	}
}

func composedRequest(roles map[review.Role]review.SeatSpec) ResolveRequest {
	return ResolveRequest{
		Surface:       "mcp",
		Mode:          review.ModeReport,
		Available:     available(),
		ReviewerPanel: []review.SeatSpec{{Adapter: "codex-cli", Model: "gpt-5.6-sol"}},
		ComposedRoles: roles,
	}
}

func TestComposedRoles_EveryRolePassesItsModelThrough(t *testing.T) {
	plan, err := composedConfig().Resolve(composedRequest(map[review.Role]review.SeatSpec{
		review.RoleAuthorRemediator: {Adapter: "claude-code", Model: "claude-opus-5-high"},
		review.RoleCrossCheck:       {Adapter: "codex-cli", Model: "gpt-5.6-sol-high"},
		review.RoleVerifier:         {Adapter: "claude-code", Model: "Claude Sonnet 5"},
	}))
	if err != nil {
		t.Fatalf("a call-composed panel with no saved configuration must resolve: %v", err)
	}
	cases := map[review.Role]struct{ exec, model string }{
		review.RoleAuthorRemediator: {"host", "claude-opus-5-high"},
		review.RoleCrossCheck:       {"adapter", "gpt-5.6-sol-high"},
		review.RoleVerifier:         {"adapter", "Claude Sonnet 5"},
	}
	for role, want := range cases {
		l, ok := plan.Lanes[role]
		if !ok {
			t.Fatalf("lane %s missing from %v", role, plan.Lanes)
		}
		if l.Execution != want.exec || string(l.ModelArg) != want.model || !l.ModelPassThrough {
			t.Errorf("lane %s = exec %q modelArg %q passthrough %v; want exec %q modelArg %q passthrough true",
				role, l.Execution, l.ModelArg, l.ModelPassThrough, want.exec, want.model)
		}
	}
	if _, ok := plan.Lanes[review.RoleReviewer]; !ok {
		t.Error("the reviewer alias must be rebuilt from the composed panel's first seat")
	}
	if got := review.SkippedOptionalSteps(plan); len(got) != 0 {
		t.Errorf("skipped = %v, want none", got)
	}
}

func TestComposedRoles_OptionalStepsRunOnlyWhenNamed(t *testing.T) {
	plan, err := composedConfig().Resolve(composedRequest(map[review.Role]review.SeatSpec{
		review.RoleAuthorRemediator: {Adapter: "claude-code", Model: "sonnet"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []review.Role{review.RoleCrossCheck, review.RoleVerifier} {
		if _, ok := plan.Lanes[role]; ok {
			t.Errorf("lane %s must not exist when the call did not name it", role)
		}
	}
	if got, want := review.SkippedOptionalSteps(plan), []string{"cross_check", "verifier"}; !slices.Equal(got, want) {
		t.Errorf("skipped = %v, want %v", got, want)
	}
}

func TestComposedRoles_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		roles  map[review.Role]review.SeatSpec
		reason string
	}{
		{"missing author_remediator", map[review.Role]review.SeatSpec{
			review.RoleCrossCheck: {Adapter: "codex-cli", Model: "x"},
		}, ReasonCallPanelMissingHost},
		{"empty roles map", map[review.Role]review.SeatSpec{}, ReasonCallPanelMissingHost},
		{"reviewer as a single seat", map[review.Role]review.SeatSpec{
			review.RoleAuthorRemediator: {Adapter: "claude-code", Model: "sonnet"},
			review.RoleReviewer:         {Adapter: "codex-cli", Model: "x"},
		}, ReasonCallPanelRoleInvalid},
		{"unconfigured adapter", map[review.Role]review.SeatSpec{
			review.RoleAuthorRemediator: {Adapter: "not-configured", Model: "sonnet"},
		}, "lane_adapter_unconfigured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := composedConfig().Resolve(composedRequest(tc.roles))
			if err == nil {
				t.Fatalf("want refusal %q", tc.reason)
			}
			if r := fault.ReasonOf(err); r != tc.reason {
				t.Fatalf("reason = %q, want %q (err %v)", r, tc.reason, err)
			}
		})
	}
}

func TestComposedRoles_PanelResolvesWithoutProfiles(t *testing.T) {
	seats, err := composedConfig().ResolvePanel(composedRequest(map[review.Role]review.SeatSpec{
		review.RoleAuthorRemediator: {Adapter: "claude-code", Model: "sonnet"},
	}))
	if err != nil || len(seats) != 1 || string(seats[0].ModelArg) != "gpt-5.6-sol" {
		t.Fatalf("seats = %+v, err = %v", seats, err)
	}
}

// TestCLISetOverride_StaysCatalogOnly: the CLI's `--set` travels in AdapterOverride/ModelOverride,
// not ComposedRoles, and a saved profile's lane is still held to the catalog.
func TestCLISetOverride_StaysCatalogOnly(t *testing.T) {
	c := passThroughConfig()
	c.Profiles["p"] = Profile{
		Reviewers: []Lane{{Execution: "adapter", Adapter: "claude-code", Model: "claude-opus-5-high"}},
		Lanes: map[string]Lane{
			"author_remediator": {Execution: "host", Adapter: "claude-code", Model: "claude-opus-5-high"},
		},
	}
	_, err := c.Resolve(ResolveRequest{
		Profile: "p", Surface: "cli", Available: available(),
		ModelOverride: map[review.Role]string{review.RoleAuthorRemediator: "not-a-catalog-key"},
	})
	if fault.ReasonOf(err) != "lane_model_unknown" {
		t.Fatalf("a CLI --set to a non-catalog model must still be refused: err = %v", err)
	}
}
