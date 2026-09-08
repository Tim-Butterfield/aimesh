package config

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// TestComposedPanel_DoesNotRequireTheProfilesReviewerLane.
//
// `--reviewer adapter=…,model=…` REPLACES the profile's panel entirely — the flag's own help says a
// panel is composed OR selected. But resolution still resolved the profile's `lanes.reviewer` first
// and failed on it, so composing a panel against an unconfigured profile was refused because of a
// lane the composed panel was about to overwrite.
//
// It fired on exactly the fresh-install path those flags exist to serve, and it was masked by
// alphabetical lane order: `author_remediator` failed first, so this only surfaced once that one
// was satisfied. Found by driving the real binary, not by a unit test.
func TestComposedPanel_DoesNotRequireTheProfilesReviewerLane(t *testing.T) {
	c := panelTestConfig()
	// A profile shaped like the shipped default: lanes declared, nothing configured in them.
	c.Profiles["p"] = Profile{Lanes: map[string]Lane{
		"reviewer":          {},
		"author_remediator": {Execution: "adapter", Adapter: "a", Model: "a-model"},
	}}

	plan, err := c.Resolve(ResolveRequest{
		Profile: "p", Surface: "cli",
		ReviewerPanel: []review.SeatSpec{{Adapter: "b", Model: "b-model"}},
	})
	if err != nil {
		t.Fatalf("a composed panel must not require the profile's reviewer lane: %v", err)
	}
	lane, ok := plan.Lanes[review.RoleReviewer]
	if !ok {
		t.Fatal("the reviewer alias is missing — role-shaped consumers depend on it")
	}
	if lane.Adapter != "b" {
		t.Errorf("the alias is seat 1 of the COMPOSED panel, got adapter %q", lane.Adapter)
	}
}

// TestUnconfiguredReviewerLaneStillFailsWithoutAPanel. The skip is licensed by the panel replacing
// the lane; with no panel there is nothing to replace it, and an unconfigured lane must still be
// the clear config error it always was.
func TestUnconfiguredReviewerLaneStillFailsWithoutAPanel(t *testing.T) {
	c := panelTestConfig()
	c.Profiles["p"] = Profile{Lanes: map[string]Lane{
		"reviewer":          {},
		"author_remediator": {Execution: "adapter", Adapter: "a", Model: "a-model"},
	}}
	_, err := c.Resolve(ResolveRequest{Profile: "p", Surface: "cli"})
	if err == nil {
		t.Fatal("an unconfigured reviewer lane with no composed panel must still fail")
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Errorf("the failure must name the role: %v", err)
	}
}
