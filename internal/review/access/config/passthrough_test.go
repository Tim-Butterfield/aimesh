package config

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// passThroughConfig is a two-adapter config whose catalog defines ONE key, with the effort embedded
// in the key exactly as the convention does.
func passThroughConfig() Config {
	return Config{
		Adapters: map[string]Adapter{
			"claude-code": {Path: "/bin/true"},
			"codex-cli":   {Path: "/bin/true"},
		},
		ModelCatalog: map[string]CatalogEntry{
			"claude-opus-5-high": {
				Provider: "anthropic",
				Adapters: map[string]AdapterModel{"claude-code": {ModelArg: "opus", Effort: "high"}},
			},
		},
		Profiles: map[string]Profile{
			"p": {Reviewers: []Lane{{Execution: "adapter", Adapter: "claude-code", Model: "claude-opus-5-high"}}},
		},
	}
}

func available() map[string]bool { return map[string]bool{"claude-code": true, "codex-cli": true} }

// TestComposedSeat_PassesAnUnknownModelThrough is the change itself.
//
// The adapter carries trust and identity-evidence capability and is still validated fail-closed; the
// model string is handed to it. Requiring pre-registration established nothing the identity layer
// does not establish independently — which is why exploremesh's --explorer has always worked this way.
func TestComposedSeat_PassesAnUnknownModelThrough(t *testing.T) {
	c := passThroughConfig()
	seats, err := c.ResolvePanel(ResolveRequest{
		Profile:   "p",
		Available: available(),
		ReviewerPanel: []review.SeatSpec{
			{Adapter: "codex-cli", Model: "gpt-5.6-sol", Effort: "high"},
		},
	})
	if err != nil {
		t.Fatalf("a composed seat naming an unconfigured model must RUN, not refuse: %v", err)
	}
	if len(seats) != 1 {
		t.Fatalf("resolved %d seats, want 1", len(seats))
	}
	s := seats[0]
	if string(s.ModelArg) != "gpt-5.6-sol" {
		t.Errorf("modelArg = %q, want the string handed through verbatim", s.ModelArg)
	}
	if !s.ModelPassThrough {
		t.Error("the seat is not marked as pass-through — it must be RECORDED even though it is not refused")
	}
	if s.Effort != "high" {
		t.Errorf("effort = %q, want the caller's — there is no catalog entry to default from", s.Effort)
	}
}

// TestComposedSeat_TheAdapterIsStillFailClosed is the boundary that must NOT move. Relaxing the
// model string says nothing about the adapter: an adapter is a binary this host executes.
func TestComposedSeat_TheAdapterIsStillFailClosed(t *testing.T) {
	c := passThroughConfig()
	_, err := c.ResolvePanel(ResolveRequest{
		Profile:   "p",
		Available: available(),
		ReviewerPanel: []review.SeatSpec{
			{Adapter: "not-configured", Model: "anything"},
		},
	})
	if err == nil {
		t.Fatal("an unconfigured ADAPTER must still be refused — pass-through covers the model string only")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("refusal = %v, want the adapter's own reason", err)
	}
}

// TestProfileLane_StillHeldToTheCatalog: the split is on WHO WROTE THE SEAT, not on what the string
// looks like. A config file declares the vocabulary it is then read against, so an unknown key there
// is a typo to catch — if this ever passed through, every config typo would become a silent call to
// a provider.
func TestProfileLane_StillHeldToTheCatalog(t *testing.T) {
	c := passThroughConfig()
	c.Profiles["bad"] = Profile{
		Reviewers: []Lane{{Execution: "adapter", Adapter: "claude-code", Model: "claude-opus-5"}},
	}
	_, err := c.ResolvePanel(ResolveRequest{Profile: "bad", Available: available()})
	if err == nil {
		t.Fatal("a PROFILE lane naming an unknown catalog key must still be refused")
	}
	if !strings.Contains(err.Error(), "not in modelCatalog") {
		t.Errorf("refusal = %v, want the catalog reason", err)
	}
	// And it must name the key that was probably meant — the measured confusion is that catalog
	// keys EMBED effort, so the documented flag shape names a key that does not exist.
	if !strings.Contains(err.Error(), `did you mean "claude-opus-5-high"`) {
		t.Errorf("refusal = %v, want the nearest-key suggestion", err)
	}
}

// TestComposedSeat_CarriesTheHintAnyway. Under pass-through a typo no longer refuses, so the hint
// stops being a nicety and becomes the only warning: without it, "claude-opus-5" would go silently
// to the provider and fail there.
func TestComposedSeat_CarriesTheHintAnyway(t *testing.T) {
	c := passThroughConfig()
	seats, err := c.ResolvePanel(ResolveRequest{
		Profile:       "p",
		Available:     available(),
		ReviewerPanel: []review.SeatSpec{{Adapter: "claude-code", Model: "claude-opus-5"}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if seats[0].PassThroughHint != "claude-opus-5-high" {
		t.Errorf("hint = %q, want the catalog key this most plausibly meant", seats[0].PassThroughHint)
	}
}

// TestNearestCatalogKey_OnlyGuessesTheSuffixCase. The whole value of the hint is being right; a
// confident wrong name is worse than none, so the match is exactly "your string plus an effort-like
// suffix" and nothing wider.
func TestNearestCatalogKey_OnlyGuessesTheSuffixCase(t *testing.T) {
	c := Config{ModelCatalog: map[string]CatalogEntry{
		"claude-opus-5-high":     {},
		"claude-opus-5-high-ext": {},
		"gpt-5-codex":            {},
	}}
	if got := c.nearestCatalogKey("claude-opus-5"); got != "claude-opus-5-high" {
		t.Errorf("nearest = %q, want the SHORTEST suffix match", got)
	}
	// A different model that merely starts the same way is not a suggestion.
	if got := c.nearestCatalogKey("gpt"); got != "" {
		t.Errorf("nearest(%q) = %q, want none — 'gpt-5-codex' is a different model, not a suffix of it", "gpt", got)
	}
	// An exact key is not its own suggestion.
	if got := c.nearestCatalogKey("claude-opus-5-high"); got != "" {
		t.Errorf("nearest of an exact key = %q, want none", got)
	}
	if got := c.nearestCatalogKey("totally-unrelated"); got != "" {
		t.Errorf("nearest = %q, want none", got)
	}
}
