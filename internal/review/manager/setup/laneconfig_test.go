package setup

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
)

// Adapter-owned lane model/effort projection + validation (Area: the model/effort/query
// mechanism is business logic per adapter — modeled in Go, never in Svelte).

// laneMgr is mutMgr with the REAL shell recipes registered (nothing is executed by these
// tests — recipes are consulted only for argv PREVIEW composition and capability asserts).
func laneMgr(t *testing.T) *Manager {
	t.Helper()
	m := mutMgr(t)
	for name, r := range shell.Recipes() {
		m.Adapters[name] = shell.New(r, "", 0)
	}
	return m
}

func TestLaneCatalogKeyDeterministic(t *testing.T) {
	cases := []struct{ adapter, arg, effort, want string }{
		{"claude-code", "opus", "medium", "claude-code-opus-medium"},
		{"ollama", "qwen2.5-coder:14b", "", "ollama-qwen2.5-coder:14b"},
		{"agy-cli", "Gemini 3.1 Pro (High)", "", "agy-cli-gemini-3.1-pro-high"},
		{"devin-cli", "Claude Opus 4.8", "medium", "devin-cli-claude-opus-4.8-medium"},
	}
	for _, c := range cases {
		if got := laneCatalogKey(c.adapter, c.arg, c.effort); got != c.want {
			t.Errorf("laneCatalogKey(%q,%q,%q)=%q want %q", c.adapter, c.arg, c.effort, got, c.want)
		}
	}
}
