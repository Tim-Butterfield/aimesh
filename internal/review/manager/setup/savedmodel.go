package setup

// Saved-model materialization + governed conflict/delete handling. A discovered/manual lane
// choice materializes a first-class, non-default modelCatalog entry (the "saved model"); its key
// is deterministic (laneCatalogKey). The SAME planner is used by both lane Check and Save so
// Check predicts Save's outcome exactly — including the case where the generated key already
// exists with different content. All decisions are LAYER-AWARE: the web UI writes the user layer,
// but the resolved config merges shipped→user→project→explicit, so a user-layer write can be
// shadowed (project/explicit) or a delete can reveal a lower (shipped) entry — the planner
// reports that honestly instead of writing something that won't take effect.

import (
	"fmt"
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

// SavedModelConflict is the governed response when a discovered/manual choice would generate a
// catalog key that already exists with DIFFERENT content. It is projected to the UI so the user
// can see the impact and explicitly Replace (never a silent overwrite).
type SavedModelConflict struct {
	Key           string   `json:"key"`
	ExistingLabel string   `json:"existingLabel"`
	PendingLabel  string   `json:"pendingLabel"`
	UsedBy        []string `json:"usedBy"`            // "profile.role" lanes resolving to this key, stable order
	Generated     bool     `json:"generated"`         // the existing entry LOOKS editor-materialized (advisory only)
	Replaceable   bool     `json:"replaceable"`       // a user-layer write would actually take effect
	Sources       []string `json:"sources,omitempty"` // config layers defining the key (shipped/user/project/explicit)
	Reason        string   `json:"reason,omitempty"`  // why not replaceable, when applicable
}

// materializationPlan is the shared outcome of resolving a discovered/manual choice to a catalog
// key: reuse an identical existing entry, create a new one, or a governed conflict.
type materializationPlan struct {
	CatalogKey string
	Action     string // "reuse" | "create" | "conflict"
	Conflict   *SavedModelConflict
}

// planMaterialization is the SINGLE source of truth for how a resolved lane choice maps to a
// catalog key (used by CheckLane AND EditProfileLane — no inline Save-only branch remains).
func (m *Manager) planMaterialization(adapter string, choice resolvedLaneChoice) materializationPlan {
	if choice.CatalogKey != "" {
		// An existing catalog entry (adapter default / saved model) — no materialization, no conflict.
		return materializationPlan{CatalogKey: choice.CatalogKey, Action: "reuse"}
	}
	key := laneCatalogKey(adapter, choice.ModelArg, choice.Effort)
	existing, exists := m.Cfg.ModelCatalog[key]
	if !exists {
		return materializationPlan{CatalogKey: key, Action: "create"}
	}
	// Exists — identical content for this adapter → reuse; otherwise a governed conflict.
	am, ok := existing.Adapters[adapter]
	eff := am.Effort
	if eff == "" {
		eff = existing.Effort
	}
	if ok && am.ModelArg == choice.ModelArg && eff == choice.Effort {
		return materializationPlan{CatalogKey: key, Action: "reuse"}
	}
	return materializationPlan{CatalogKey: key, Action: "conflict", Conflict: m.buildSavedModelConflict(adapter, key, existing, choice)}
}

func (m *Manager) buildSavedModelConflict(adapter, key string, existing config.CatalogEntry, choice resolvedLaneChoice) *SavedModelConflict {
	existArg := existing.Adapters[adapter].ModelArg
	existEff := existing.Adapters[adapter].Effort
	if existEff == "" {
		existEff = existing.Effort
	}
	c := &SavedModelConflict{
		Key:           key,
		ExistingLabel: savedModelLabel(existing.DisplayName, existArg, existEff),
		PendingLabel:  savedModelLabel(pendingDisplayName(choice.ModelArg, choice.Effort), choice.ModelArg, choice.Effort),
		UsedBy:        m.catalogKeyUsedBy(key),
		Generated:     isGeneratedCatalogEntry(key, adapter, existing),
		Sources:       m.catalogKeySources(key),
	}
	// Replaceable requires the SAME ownership semantics as delete: the key must be present in the
	// USER layer, not shadowed by a higher (project/explicit) layer, and not backed by a lower
	// (shipped) layer whose omitted fields (authoritative/adapterDefault) would survive the
	// field-granular merge. Replace-when-used is allowed (the modal discloses the using lanes and
	// requires an explicit confirmation) — that is NOT an ownership problem.
	c.Replaceable, c.Reason = m.savedModelReplaceable(key)
	return c
}

// savedModelReplaceable reports whether the web UI can effectively overwrite a saved-model key at
// the user layer (and the honest reason when not). It mirrors savedModelDeletable's layer rules
// but omits the used-by check — replacing a used entry is a disclosed, confirmed action, not an
// ownership violation.
func (m *Manager) savedModelReplaceable(key string) (bool, string) {
	userDefines, higher, lower := m.catalogKeyLayers(key)
	if higher != "" {
		return false, fmt.Sprintf("this saved model is defined in the %s config layer — the web UI only edits the user layer, so a replace here would not take effect; edit it in that config file", higher)
	}
	if lower {
		return false, "this key collides with a shipped catalog entry; the web UI will not overwrite it (a user-layer override would partially merge with the shipped fields). Choose a different model/effort."
	}
	if !userDefines {
		return false, "this key is not owned by your user config, so the web UI cannot safely overwrite it here"
	}
	return true, ""
}

// pendingDisplayName mirrors catalogEntryFor's display convention ("<modelArg> (<effort>)").
func pendingDisplayName(modelArg, effort string) string {
	if effort != "" {
		return modelArg + " (" + effort + ")"
	}
	return modelArg
}

// isGeneratedCatalogEntry reports whether an entry LOOKS like one the lane editor materialized
// (single-adapter, non-authoritative, non-adapter-default, and its key equals laneCatalogKey of
// its own content). ADVISORY ONLY — a hand-authored entry can match this shape, so it never
// reduces the Replace confirmation; it only tunes the warning wording.
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

// catalogKeyUsedBy returns the "profile.role" lanes whose model resolves to key, in stable order.
func (m *Manager) catalogKeyUsedBy(key string) []string {
	var used []string
	for pname, p := range m.Cfg.Profiles {
		for role, lane := range p.Lanes {
			if lane.Model == key {
				used = append(used, pname+"."+role)
			}
		}
	}
	sort.Strings(used)
	return used
}

// catalogKeyLayers analyzes modelCatalog.<key> RELATIVE to the write scope: whether the WRITE layer
// (user or project) defines it, which STRICTLY-HIGHER layer shadows a write to that scope, and whether a
// STRICTLY-LOWER layer (the user layer below a project write, then shipped) would be revealed by deleting
// the write-layer entry. Despite its name, `userDefines` reports the WRITE-scope definition.
func (m *Manager) catalogKeyLayers(key string) (userDefines bool, higherLayer string, lowerLayerDefines bool) {
	rawHas := func(p string) bool {
		if p == "" {
			return false
		}
		raw, err := config.LoadRawMap(p)
		if err != nil {
			return false
		}
		cat, ok := raw["modelCatalog"].(map[string]any)
		if !ok {
			return false
		}
		_, present := cat[key]
		return present
	}
	userDefines = rawHas(m.scopeConfigPath())
	for _, l := range m.higherWriteLayers() {
		if rawHas(l.path) {
			higherLayer = l.layer
			break
		}
	}
	for _, l := range m.lowerWriteFileLayers() {
		if rawHas(l.path) {
			lowerLayerDefines = true
			break
		}
	}
	if !lowerLayerDefines {
		if _, ok := config.Default().ModelCatalog[key]; ok {
			lowerLayerDefines = true
		}
	}
	return userDefines, higherLayer, lowerLayerDefines
}

// savedModelDeletable projects whether the saved-model list may offer Delete for a key (and the
// reason when not) — it MIRRORS the DeleteSavedModel guards so the button matches the endpoint.
func (m *Manager) savedModelDeletable(key string, usedByCount int) (bool, string) {
	if usedByCount > 0 {
		return false, fmt.Sprintf("in use by %d lane(s) — re-point those lanes first", usedByCount)
	}
	userDefines, higher, lower := m.catalogKeyLayers(key)
	if higher != "" {
		return false, "defined in the " + higher + " config layer (edit it there)"
	}
	if !userDefines {
		return false, "not defined in your user config"
	}
	if lower {
		return false, "deleting the user override would reveal the shipped entry"
	}
	return true, ""
}

// catalogKeySources lists the layers defining the key (shipped/user/project/explicit), for display.
func (m *Manager) catalogKeySources(key string) []string {
	var s []string
	if _, ok := config.Default().ModelCatalog[key]; ok {
		s = append(s, "shipped")
	}
	userDefines, higher, _ := m.catalogKeyLayers(key)
	if userDefines {
		s = append(s, "user")
	}
	if higher != "" {
		s = append(s, higher)
	}
	return s
}
