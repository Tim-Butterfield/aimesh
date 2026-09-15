package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

func writeLayer(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	return p
}

// TestReviewerSeats_LegacySpellingIsAPanelOfOne pins the migration rule: `lanes.reviewer` IS
// `reviewers[0]`. Nothing reads the legacy key to decide what the primary stage is.
func TestReviewerSeats_LegacySpellingIsAPanelOfOne(t *testing.T) {
	p := Profile{Lanes: map[string]Lane{"reviewer": {Execution: "adapter", Adapter: "fake", Model: "fake-model"}}}
	seats := p.ReviewerSeats()
	if len(seats) != 1 || seats[0].Adapter != "fake" {
		t.Fatalf("legacy lanes.reviewer must normalize to a one-seat panel, got %+v", seats)
	}
	if p.HasPanelSpelling() {
		t.Error("a legacy profile must not report the panel spelling (the migration is a READ normalization, not a rewrite)")
	}
	panel := Profile{Reviewers: []Lane{{Adapter: "a", Model: "m"}, {Adapter: "b", Model: "m"}}}
	if got := panel.ReviewerSeats(); len(got) != 2 {
		t.Fatalf("explicit panel = %d seats, want 2", len(got))
	}
}

// TestConfigLoad_BothSpellingsInOneLayerIsAnError: a file naming `reviewers` AND `lanes.reviewer`
// for the same profile is refused — accepting it would mean silently choosing a winner.
func TestConfigLoad_BothSpellingsInOneLayerIsAnError(t *testing.T) {
	p := writeLayer(t, `
profiles:
  dual:
    reviewers:
      - {execution: adapter, adapter: fake, model: fake-model}
    lanes:
      reviewer: {execution: adapter, adapter: fake, model: fake-model}
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("naming both `reviewers` and `lanes.reviewer` must be a config error")
	}
	if !strings.Contains(err.Error(), "reviewers") || !strings.Contains(err.Error(), "lanes.reviewer") {
		t.Errorf("the error must name BOTH spellings so the fix is obvious, got: %v", err)
	}
}

// TestMerge_PanelSpellingReplacesLegacyAcrossLayers: a layer declaring `reviewers` replaces a lower
// layer's `lanes.reviewer`, so the merged profile carries one spelling.
func TestMerge_PanelSpellingReplacesLegacyAcrossLayers(t *testing.T) {
	base := Config{Profiles: map[string]Profile{"p": {Lanes: map[string]Lane{
		"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
	}}}}
	over := Config{Profiles: map[string]Profile{"p": {Reviewers: []Lane{
		{Execution: "adapter", Adapter: "a", Model: "m"},
		{Execution: "adapter", Adapter: "b", Model: "m"},
	}}}}
	got := merge(base, over).Profiles["p"]
	if _, still := got.Lanes["reviewer"]; still {
		t.Error("the one-seat spelling must be dropped when a layer supplies a panel")
	}
	if len(got.ReviewerSeats()) != 2 {
		t.Errorf("effective panel = %d seats, want 2", len(got.ReviewerSeats()))
	}
	if _, kept := got.Lanes["author_remediator"]; !kept {
		t.Error("a panel override must not disturb the single-slot lanes")
	}
	// The reverse: a layer that declares lanes.reviewer drops an inherited panel.
	back := merge(merge(base, over), Config{Profiles: map[string]Profile{"p": {Lanes: map[string]Lane{
		"reviewer": {Execution: "adapter", Adapter: "c", Model: "m"},
	}}}}).Profiles["p"]
	if len(back.Reviewers) != 0 || len(back.ReviewerSeats()) != 1 {
		t.Errorf("re-declaring lanes.reviewer must replace the inherited panel, got %+v", back)
	}
}

func panelTestConfig() Config {
	c := Default()
	for _, name := range []string{"a", "b"} {
		c.Adapters[name] = Adapter{ModelIdentity: "self_report"}
		c.ModelCatalog[name+"-model"] = CatalogEntry{
			Provider: "x", CanonicalModel: "m",
			Adapters: map[string]AdapterModel{name: {ModelArg: name + "-arg"}},
		}
	}
	return c
}

// TestResolvePanel_OrderedSeatsAndIDs: the resolved panel keeps the authored order and assigns seat
// ids, with seat 1 named "reviewer".
func TestResolvePanel_OrderedSeatsAndIDs(t *testing.T) {
	c := panelTestConfig()
	c.Profiles["p"] = Profile{Reviewers: []Lane{
		{Execution: "adapter", Adapter: "b", Model: "b-model"},
		{Execution: "adapter", Adapter: "a", Model: "a-model"},
	}}
	seats, err := c.ResolvePanel(ResolveRequest{Profile: "p"})
	if err != nil {
		t.Fatalf("resolve panel: %v", err)
	}
	if len(seats) != 2 || seats[0].Adapter != "b" || seats[1].Adapter != "a" {
		t.Fatalf("authored order must be preserved, got %+v", seats)
	}
	if seats[0].SeatID != "reviewer" || seats[1].SeatID != "reviewer-2" {
		t.Errorf("seat ids = %q,%q; want reviewer,reviewer-2", seats[0].SeatID, seats[1].SeatID)
	}
}

// TestResolvePanel_FailsClosed covers every pre-spend refusal: an oversized panel, a duplicate seat
// identity, an unresolvable seat, and an empty panel. None of them is ever silently trimmed.
func TestResolvePanel_FailsClosed(t *testing.T) {
	big := make([]Lane, 0, review.MaxReviewerSeats+1)
	for i := 0; i <= review.MaxReviewerSeats; i++ {
		big = append(big, Lane{Execution: "adapter", Adapter: "a", Model: "a-model"})
	}
	cases := []struct {
		name  string
		seats []Lane
		want  string
	}{
		{"too large", big, "exceeding the cap"},
		{"duplicate identity", []Lane{
			{Execution: "adapter", Adapter: "a", Model: "a-model"},
			{Execution: "adapter", Adapter: "a", Model: "a-model"},
		}, "no independent vantage"},
		{"unknown adapter", []Lane{{Execution: "adapter", Adapter: "nope", Model: "a-model"}}, "not configured/enabled"},
		{"unknown model", []Lane{{Execution: "adapter", Adapter: "a", Model: "nope"}}, "not in modelCatalog"},
		{"empty panel", nil, "configures no blind primary reviewer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := panelTestConfig()
			c.Profiles["p"] = Profile{Reviewers: tc.seats}
			_, err := c.ResolvePanel(ResolveRequest{Profile: "p"})
			if err == nil {
				t.Fatalf("%s must be refused before any spend", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to explain %q", err, tc.want)
			}
		})
	}
}

// TestResolvePanel_ReportsEveryUnresolvableSeat: one refusal names every unresolvable seat.
func TestResolvePanel_ReportsEveryUnresolvableSeat(t *testing.T) {
	c := panelTestConfig()
	c.Profiles["p"] = Profile{Reviewers: []Lane{
		{Execution: "adapter", Adapter: "nope", Model: "a-model"}, // bad adapter
		{Execution: "adapter", Adapter: "a", Model: "a-model"},    // fine
		{Execution: "adapter", Adapter: "a", Model: "alsonope"},   // bad model
	}}
	_, err := c.ResolvePanel(ResolveRequest{Profile: "p"})
	if err == nil {
		t.Fatal("a panel with unresolvable seats must be refused before any spend")
	}
	msg := err.Error()
	for _, want := range []string{
		"not configured/enabled", // seat 1's blocker
		"not in modelCatalog",    // seat 3's blocker
		"reviewer seat 1 of 3",
		"reviewer seat 3 of 3",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("one refusal must name every blocker; %q missing from:\n%s", want, msg)
		}
	}
	// The reason code is the first failure's.
	if got := fault.ReasonOf(err); got != "lane_adapter_unconfigured" {
		t.Errorf("reasonCode = %q, want the first failure's (lane_adapter_unconfigured)", got)
	}
}

// A panel whose seats all resolve is still refused for a duplicate identity, which is checked after
// resolution.
func TestResolvePanel_DuplicateIsCaughtAfterResolution(t *testing.T) {
	c := panelTestConfig()
	c.Profiles["p"] = Profile{Reviewers: []Lane{
		{Execution: "adapter", Adapter: "a", Model: "a-model"},
		{Execution: "adapter", Adapter: "b", Model: "b-model"},
		{Execution: "adapter", Adapter: "a", Model: "a-model"},
	}}
	_, err := c.ResolvePanel(ResolveRequest{Profile: "p"})
	if err == nil || !strings.Contains(err.Error(), "no independent vantage") {
		t.Fatalf("a duplicate seat identity must still be refused, got %v", err)
	}
	if got := fault.ReasonOf(err); got != "panel_duplicate_seat" {
		t.Errorf("reasonCode = %q, want panel_duplicate_seat", got)
	}
}

// TestResolvePanel_AdHocReplacesProfilePanel: an invocation-composed panel replaces the profile's
// rather than merging, and its seats resolve by the same rule.
func TestResolvePanel_AdHocReplacesProfilePanel(t *testing.T) {
	c := panelTestConfig()
	c.Profiles["p"] = Profile{Reviewers: []Lane{{Execution: "adapter", Adapter: "a", Model: "a-model"}}}
	seats, err := c.ResolvePanel(ResolveRequest{Profile: "p", ReviewerPanel: []review.SeatSpec{
		{Adapter: "b", Model: "b-model"},
		{Adapter: "b", Model: "b-model", Effort: "high"}, // same model, different effort = a distinct vantage
	}})
	if err != nil {
		t.Fatalf("resolve ad-hoc panel: %v", err)
	}
	if len(seats) != 2 {
		t.Fatalf("ad-hoc panel = %d seats, want 2 (it replaces the profile's, never merges)", len(seats))
	}
	if seats[0].Adapter != "b" || seats[1].Effort != "high" {
		t.Errorf("ad-hoc seats resolved unexpectedly: %+v", seats)
	}
}

// TestResolve_PanelProfileStillExposesTheReviewerLane: a profile spelled with `reviewers` resolves a
// Lanes[reviewer] alias for seat 1, and the alias carries no seat id.
func TestResolve_PanelProfileStillExposesTheReviewerLane(t *testing.T) {
	c := panelTestConfig()
	c.Profiles["p"] = Profile{
		Lanes:     map[string]Lane{"author_remediator": {Execution: "host", Adapter: "a", Model: "a-model"}},
		Reviewers: []Lane{{Execution: "adapter", Adapter: "b", Model: "b-model"}, {Execution: "adapter", Adapter: "a", Model: "a-model"}},
	}
	plan, err := c.Resolve(ResolveRequest{Profile: "p", Surface: "cli"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	lane, ok := plan.Lanes[review.RoleReviewer]
	if !ok {
		t.Fatal("a panel profile must still expose the reviewer lane alias")
	}
	if lane.Adapter != "b" {
		t.Errorf("the alias is the FIRST seat, got adapter %q", lane.Adapter)
	}
	if lane.SeatID != "" {
		t.Errorf("the alias must carry no seat id (kept out of the role map's recorded shape), got %q", lane.SeatID)
	}
}
