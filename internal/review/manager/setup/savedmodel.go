package setup

// Saved-model materialization and conflict/delete handling. A discovered or manual lane choice
// becomes a non-default modelCatalog entry keyed by laneCatalogKey. Check and Save share one planner,
// so Check predicts Save exactly. Decisions are layer-aware: setup writes one layer, but the resolved
// config merges shipped, user, project and explicit layers, so a write can be shadowed and a delete
// can reveal a lower entry; the planner reports that instead of writing something without effect.

import (
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

// SavedModelConflict is returned when a discovered or manual choice would generate a catalog key
// that already exists with different content, so the caller can see the impact and explicitly
// replace it.
type SavedModelConflict struct {
	Key           string   `json:"key"`
	ExistingLabel string   `json:"existingLabel"`
	PendingLabel  string   `json:"pendingLabel"`
	UsedBy        []string `json:"usedBy"`            // "profile.role" lanes resolving to this key, stable order
	Generated     bool     `json:"generated"`         // the existing entry looks editor-materialized (advisory only)
	Replaceable   bool     `json:"replaceable"`       // a user-layer write would actually take effect
	Sources       []string `json:"sources,omitempty"` // config layers defining the key (shipped/user/project/explicit)
	Reason        string   `json:"reason,omitempty"`  // why not replaceable, when applicable
}

// isGeneratedCatalogEntry reports whether an entry looks lane-editor materialized: single-adapter,
// non-authoritative, not an adapter default, and keyed by laneCatalogKey of its own content. It is
// advisory: a hand-authored entry can match, so it only tunes warning wording.
func isGeneratedCatalogEntry(key, adapter string, e config.CatalogEntry) bool {
	if e.IsAdapterDefault() || (e.Authoritative != nil && *e.Authoritative) {
		return false
	}
	if len(e.Adapters) != 1 {
		return false
	}
	am, ok := e.Adapters[adapter]
	if !ok {
		return false
	}
	eff := am.Effort
	if eff == "" {
		eff = e.Effort
	}
	return key == laneCatalogKey(adapter, am.ModelArg, eff)
}
