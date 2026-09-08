package setup

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
)

// TestAdapterDisplayName_EveryImplementedAdapter proves the Go-owned projection gives a readable
// product name for each implemented adapter, and that the raw KEY stays stable/distinct from the
// display name (the SPA renders the display name; the key stays canonical in config/API/testids).
//
// It ENUMERATES shell.Recipes() rather than a hand-written list, per the repo's discover-don't-hardcode
// rule: a frozen list silently stops covering adapters added later. (It did — `cursor-cli` shipped as a
// recipe without a display name, so reviewmesh rendered "Cursor Cli" from the key fallback while
// exploremesh rendered "Cursor". This test now fails until a new recipe gets a real product name.)
func TestAdapterDisplayName_EveryImplementedAdapter(t *testing.T) {
	want := map[string]string{
		"codex-cli":   "Codex",
		"claude-code": "Claude Code",
		"agy-cli":     "Antigravity",
		"devin-cli":   "Devin",
		"gemini-cli":  "Gemini",
		"cursor-cli":  "Cursor",
		"ollama":      "Ollama",
		"fake":        "Fake",
	}
	for key, display := range want {
		if got := AdapterDisplayName(key); got != display {
			t.Errorf("AdapterDisplayName(%q) = %q, want %q", key, got, display)
		}
		if display == key {
			t.Errorf("display name for %q must differ from the raw key", key)
		}
	}

	// Every SHIPPED recipe must have an explicit display name — not the Title-Cased key fallback.
	for key := range shell.Recipes() {
		if _, ok := want[key]; !ok {
			t.Errorf("shell recipe %q is not covered by this test — add its expected display name", key)
		}
		if _, ok := adapterDisplayNames[key]; !ok {
			t.Errorf("shell recipe %q has no entry in adapterDisplayNames: the UI would render the "+
				"Title-Cased key %q instead of a product name", key, AdapterDisplayName(key))
		}
	}
}

// TestAdapterDisplayName_Fallback proves an unknown key gets a deterministic Title-Cased fallback
// (never a blind "text before the hyphen" rule, which would drop meaningful words).
func TestAdapterDisplayName_Fallback(t *testing.T) {
	cases := map[string]string{
		// A trailing `-cli` is an INTERNAL disambiguator (terminal binary vs the vendor's desktop app),
		// never part of the product name — it must not reach a user-visible label.
		"new-vendor-cli": "New Vendor",
		"opencode-cli":   "Opencode",
		"single":         "Single",
		"under_score":    "Under Score",
		"":               "",
		// `code` IS a real product word (Claude Code), so only `cli` is dropped.
		"some-code": "Some Code",
		// Never strip the only segment.
		"cli": "Cli",
	}
	for key, display := range cases {
		if got := AdapterDisplayName(key); got != display {
			t.Errorf("AdapterDisplayName(%q) = %q, want %q", key, got, display)
		}
	}
	// The naive "before the hyphen" rule would yield "Claude" for claude-code — the projection
	// must not do that.
	if got := AdapterDisplayName("claude-code"); got == "Claude" {
		t.Errorf("display name must not be a naive prefix split, got %q", got)
	}
}
