package setup

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

// TestCatalogViews_ResolvesEffortTheSameWayALaneDoes.
//
// The catalog key is the exact token a composed panel must name, and effort is EMBEDDED in that key
// by convention rather than carried in it. An inventory that reported a blank effort for an entry
// whose effort lives at the entry level (not the binding) would tell a reader "no effort" about a
// run that will in fact send one — so this pins that the projection falls back exactly as
// laneModelSources does.
func TestCatalogViews_ResolvesEffortTheSameWayALaneDoes(t *testing.T) {
	m := laneMgr(t)
	m.Cfg.ModelCatalog = map[string]config.CatalogEntry{
		// Effort on the ENTRY, not the binding — the fallback case.
		"entry-level": {
			Provider: "anthropic", CanonicalModel: "opus", Effort: "medium",
			Adapters: map[string]config.AdapterModel{"claude-code": {ModelArg: "opus"}},
		},
		// Effort on the BINDING — it wins.
		"binding-level": {
			Provider: "openai",
			Adapters: map[string]config.AdapterModel{"codex-cli": {ModelArg: "gpt-5", Effort: "high"}},
		},
	}
	byKey := map[string]CatalogEntryView{}
	for _, v := range m.CatalogViews() {
		byKey[v.Key] = v
	}
	if len(byKey) != 2 {
		t.Fatalf("projected %d entries, want 2: %+v", len(byKey), byKey)
	}
	entry := byKey["entry-level"]
	if len(entry.Adapters) != 1 || entry.Adapters[0].Effort != "medium" {
		t.Errorf("entry-level effort did not fall through to the binding: %+v", entry.Adapters)
	}
	if entry.Adapters[0].ModelArg != "opus" {
		t.Errorf("modelArg = %q, want the argument the adapter is actually given", entry.Adapters[0].ModelArg)
	}
	if entry.CanonicalModel != "opus" {
		t.Errorf("canonicalModel = %q", entry.CanonicalModel)
	}
	bind := byKey["binding-level"]
	if len(bind.Adapters) != 1 || bind.Adapters[0].Effort != "high" {
		t.Errorf("a binding-level effort must win over the entry: %+v", bind.Adapters)
	}
}

// TestCatalogViews_IsSortedAndTotal: every key is projected, in a stable order. An inventory that
// dropped keys would be worse than none — a caller would compose against a partial vocabulary and
// be refused by a rule it could not see.
func TestCatalogViews_IsSortedAndTotal(t *testing.T) {
	m := laneMgr(t)
	m.Cfg.ModelCatalog = map[string]config.CatalogEntry{
		"c": {Adapters: map[string]config.AdapterModel{"x": {ModelArg: "1"}}},
		"a": {Adapters: map[string]config.AdapterModel{"x": {ModelArg: "2"}}},
		"b": {Adapters: map[string]config.AdapterModel{"x": {ModelArg: "3"}}},
	}
	views := m.CatalogViews()
	if len(views) != 3 {
		t.Fatalf("projected %d of 3 keys", len(views))
	}
	for i, want := range []string{"a", "b", "c"} {
		if views[i].Key != want {
			t.Errorf("position %d = %q, want %q (sorted by key)", i, views[i].Key, want)
		}
	}
}
