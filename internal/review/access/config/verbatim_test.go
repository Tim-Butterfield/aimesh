package config

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// A request composed on a surface that reads no saved configuration passes every seat's model string to
// the adapter verbatim — even one that happens to match a catalog key, which must not be rewritten to the
// catalog's model argument.
func TestComposedRoles_ModelMatchingACatalogKeyPassesThroughVerbatim(t *testing.T) {
	c := Default()
	var key, adapter string
	for k, entry := range c.ModelCatalog {
		for a, am := range entry.Adapters {
			if am.ModelArg != "" && am.ModelArg != k {
				key, adapter = k, a
				break
			}
		}
		if key != "" {
			break
		}
	}
	if key == "" {
		t.Skip("the shipped catalog has no key whose model argument differs from the key")
	}
	seat := review.SeatSpec{Adapter: adapter, Model: key}
	plan, err := c.Resolve(ResolveRequest{
		Surface: "mcp", Mode: review.ModeReport,
		ReviewerPanel: []review.SeatSpec{seat},
		ComposedRoles: map[review.Role]review.SeatSpec{review.RoleAuthorRemediator: seat},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for role, lane := range plan.Lanes {
		if string(lane.ModelArg) != key || !lane.ModelPassThrough {
			t.Errorf("%s: modelArg = %q passThrough=%v, want %q passed through verbatim", role, lane.ModelArg, lane.ModelPassThrough, key)
		}
	}
}

// devin-cli carries reasoning effort inside its model identifier, so a verbatim identifier cannot also
// take a separate effort. The seat is refused rather than resolved with the effort silently dropped.
func TestComposedRoles_DevinEffortBesideAVerbatimModelIsRefused(t *testing.T) {
	seat := review.SeatSpec{Adapter: "devin-cli", Model: "some-model", Effort: "high"}
	_, err := Default().Resolve(ResolveRequest{
		Surface: "mcp", Mode: review.ModeReport,
		ReviewerPanel: []review.SeatSpec{seat},
		ComposedRoles: map[review.Role]review.SeatSpec{review.RoleAuthorRemediator: seat},
	})
	if err == nil {
		t.Fatal("a devin-cli seat naming both a verbatim model and an effort must be refused")
	}
}
