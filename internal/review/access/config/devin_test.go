package config

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// The pure RenderDevinModelArg unit tests moved with the function to meshcore/model
// (adapter-specific slug rendering). These resolution tests stay here — they exercise the
// config resolver end-to-end (Devin display name → resolved lane ModelArg).

func TestResolve_DevinModelArgRenderedFromDisplayName(t *testing.T) {
	cfg := WithExampleProfiles(Default())
	plan, err := cfg.Resolve(ResolveRequest{Profile: "devin-cli-smoke", Surface: "cli"})
	if err != nil {
		t.Fatalf("resolve devin-cli-smoke: %v", err)
	}
	lane, ok := plan.Lanes[review.Role("reviewer")]
	if !ok {
		t.Fatal("no reviewer lane resolved")
	}
	if string(lane.ModelArg) != "claude-opus-4-8-medium" {
		t.Errorf("resolved devin ModelArg = %q, want claude-opus-4-8-medium", lane.ModelArg)
	}
	if strings.ContainsAny(string(lane.ModelArg), " .") {
		t.Errorf("resolved devin ModelArg %q must have no spaces/periods", lane.ModelArg)
	}
}

func TestResolve_DevinAmbiguousModelErrors(t *testing.T) {
	cfg := Default()
	entry := cfg.ModelCatalog["devin-cli-default"]
	entry.Adapters = map[string]AdapterModel{"devin-cli": {ModelArg: "Opus 4.8", Effort: "medium"}}
	cfg.ModelCatalog["devin-cli-default"] = entry
	if _, err := cfg.Resolve(ResolveRequest{Profile: "devin-cli-smoke", Surface: "cli"}); err == nil {
		t.Error("an ambiguous Devin display name (\"Opus 4.8\") should fail resolution")
	}
}
