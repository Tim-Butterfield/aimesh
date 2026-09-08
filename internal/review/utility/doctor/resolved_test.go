package doctor

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// readinessOf returns the "profile: <name> ready" check, or a failure.
func readinessOf(t *testing.T, in Input, profile string) (ok bool, detail string) {
	t.Helper()
	r := Run(in)
	for _, c := range r.Checks {
		if strings.HasPrefix(c.Name, "profile: "+profile+" ready") {
			return c.OK, c.Detail
		}
	}
	t.Fatalf("no readiness check for profile %q in %+v", profile, r.Checks)
	return false, ""
}

// TestReadiness_JudgesTheCALLERSPlanWhenGivenOne.
//
// The static preflight used to re-resolve from config, which meant it could not see a `--set` role
// override or a composed `--reviewer` panel. A run whose plan had ALREADY resolved was then halted
// Class A — "no adapter resolvable for role X" — with that exact role overridden on the command
// line. It was answering "is the PROFILE ready" while the run needed "is what I am about to run
// ready", and those diverge the moment an invocation overrides anything.
func TestReadiness_JudgesTheCallersPlanWhenGivenOne(t *testing.T) {
	c := config.Default()
	c.Adapters["fake"] = config.Adapter{ModelIdentity: "self_report"}
	// A profile whose lanes are declared but UNCONFIGURED — the shipped shape, and the one a bare
	// resolve cannot satisfy.
	c.Profiles["p"] = config.Profile{Lanes: map[string]config.Lane{
		"reviewer":          {},
		"author_remediator": {},
	}}
	adapters := map[string]model.Adapter{"fake": fake.New(fake.Valid)}

	// WITHOUT the caller's plan: unchanged behaviour, and it correctly reports the profile as unready.
	ok, _ := readinessOf(t, Input{Config: c, Adapters: adapters, Profile: "p"}, "p")
	if ok {
		t.Error("a bare doctor must still report an unconfigured profile as unready — that IS the question it was asked")
	}

	// WITH it: the lanes the run will actually invoke, which resolved fine.
	resolved := review.RunPlan{Surface: "cli", Lanes: map[review.Role]review.LaneResolution{
		review.RoleReviewer:         {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		review.RoleAuthorRemediator: {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
	}}
	ok, detail := readinessOf(t, Input{Config: c, Adapters: adapters, Profile: "p", Resolved: &resolved}, "p")
	if !ok {
		t.Fatalf("a resolved plan whose adapters are available must be READY, got: %s", detail)
	}
}

// TestReadiness_ChecksEverySeatOfAComposedPanel. `Lanes[reviewer]` is only seat 1, so a panel whose
// seats sit on different adapters would otherwise have the rest go unchecked — and an unavailable
// adapter would be discovered at dispatch instead of at preflight, which is the whole point of
// having a preflight.
func TestReadiness_ChecksEverySeatOfAComposedPanel(t *testing.T) {
	c := config.Default()
	c.Adapters["fake"] = config.Adapter{ModelIdentity: "self_report"}
	c.Adapters["ghost"] = config.Adapter{ModelIdentity: "self_report"}
	c.Profiles["p"] = config.Profile{Lanes: map[string]config.Lane{"reviewer": {}}}
	// REGISTERED but reporting unavailable — the state this check exists to catch. An adapter that
	// is not registered at all is skipped here by design; resolution refuses it earlier.
	adapters := map[string]model.Adapter{
		"fake":  fake.New(fake.Valid),
		"ghost": stubAdapter{"ghost", false},
	}

	resolved := review.RunPlan{Surface: "cli", Lanes: map[review.Role]review.LaneResolution{
		review.RoleReviewer: {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
	}}
	// Seat 1 is fine; seat 2 names an adapter this process cannot run.
	seats := []review.LaneResolution{
		{Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		{Execution: "adapter", Adapter: "ghost", Model: "ghost-model"},
	}
	ok, detail := readinessOf(t, Input{
		Config: c, Adapters: adapters, Profile: "p",
		Resolved: &resolved, ResolvedSeats: seats,
	}, "p")
	if ok {
		t.Errorf("seat 2's unavailable adapter was not checked — only the reviewer ALIAS was (detail: %s)", detail)
	}
}
