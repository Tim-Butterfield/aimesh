package setup

// This file models each adapter's lane configuration: how models are discovered, whether manual
// entry is allowed, how effort works, what the effective invocation looks like, and what cannot be
// verified. Callers render these projections and submit choices; the Manager re-validates every
// submission before writing.

import (
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// DiscoveredModelView is one discovered model.
type DiscoveredModelView struct {
	Arg           string   `json:"arg"`
	DefaultEffort string   `json:"defaultEffort,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
}

// DiscoveryView projects an adapter's model-discovery mechanism and its last on-demand result.
// Discovery never runs automatically, so status starts at not_run or unsupported.
type DiscoveryView struct {
	Mechanism string                `json:"mechanism,omitempty"` // e.g. "ollama list"
	Kind      string                `json:"kind,omitempty"`      // local | bundled | best_effort
	Status    string                `json:"status"`              // unsupported | not_run | ok | unavailable | failed | stale
	Detail    string                `json:"detail,omitempty"`
	Models    []DiscoveredModelView `json:"models,omitempty"`
	Seq       int                   `json:"seq,omitempty"` // run marker, for showing freshness
}

// LaneModelConfig is an adapter's lane configuration. Its model sources are kept separate: an adapter
// default is never included in SavedModels.
type LaneModelConfig struct {
	// SelectionMode summarizes how models are chosen (discovered_list | bundled_catalog |
	// catalog_plus_manual | catalog_only).
	SelectionMode string `json:"selectionMode"`
	// AdapterDefault is the adapter's adapterDefault-marked catalog entry, reported separately from
	// SavedModels.
	AdapterDefault *AdapterDefaultView `json:"adapterDefault,omitempty"`
	// SavedModels are the non-default catalog entries usable with the adapter; SavedModelsEmptyMessage
	// explains an empty list.
	SavedModels             []SavedModelChoice `json:"savedModels"`
	SavedModelsEmptyMessage string             `json:"savedModelsEmptyMessage,omitempty"`
	Discovery               DiscoveryView      `json:"discovery"`
	ManualEntry             bool               `json:"manualEntry"`
	ManualLabel             string             `json:"manualLabel,omitempty"`
	EffortMode              string             `json:"effortMode"` // unsupported | separate | requested_unverified | encoded_in_model
	AllowedEfforts          []string           `json:"allowedEfforts,omitempty"`
	DefaultEffort           string             `json:"defaultEffort,omitempty"`
	Caveats                 []string           `json:"caveats,omitempty"`
}

// AdapterDefaultView describes an adapter's default model, with the effective-argument preview and
// any caveats.
type AdapterDefaultView struct {
	Available                bool     `json:"available"`
	CatalogKey               string   `json:"catalogKey,omitempty"`
	Label                    string   `json:"label"`
	ModelArg                 string   `json:"modelArg,omitempty"`
	Effort                   string   `json:"effort,omitempty"`
	EffectiveArgumentPreview string   `json:"effectiveArgumentPreview,omitempty"`
	Caveats                  []string `json:"caveats,omitempty"`
}

// SavedModelChoice is one non-default saved model with a readable label.
type SavedModelChoice struct {
	Key      string `json:"key"`
	Label    string `json:"label"` // "<displayName> — <modelArg>[ · effort]"
	ModelArg string `json:"modelArg"`
	Effort   string `json:"effort,omitempty"`
	Provider string `json:"provider,omitempty"`
	Local    bool   `json:"local"`
	// UsedBy lists the lanes using this key; Generated reports whether it looks editor-materialized
	// (advisory); Deletable reports whether it can be deleted, with DeleteReason when not.
	UsedBy       []string `json:"usedBy,omitempty"`
	Generated    bool     `json:"generated"`
	Deletable    bool     `json:"deletable"`
	DeleteReason string   `json:"deleteReason,omitempty"`
}

// DiscoveryResult is the outcome of an on-demand discovery run. Callers cache it; the Manager holds
// no state.
type DiscoveryResult struct {
	Status string // ok | unavailable | failed
	Detail string
	Models []model.DiscoveredModel
	Seq    int
	Stale  bool // a config write happened after this run
}

// LaneChoiceInput is a submitted lane model choice: EITHER a catalog key (an existing
// modelCatalog entry — an adapter default or a saved model) OR an adapter-specific model
// argument (from the discovered list or manual entry) plus, where the adapter supports it, a
// separate effort. Source names the caller's intended model source; "" is server-derived
// from the shape + catalog marker (back-compat). The server validates source/key consistency.
type LaneChoiceInput struct {
	Source     string `json:"source"` // adapter_default | saved_model | discovered_model | manual_entry | ""
	CatalogKey string `json:"catalogKey"`
	ModelArg   string `json:"modelArg"`
	Effort     string `json:"effort"`
	// Replace authorizes EditProfileLane to overwrite an existing generated catalog key whose
	// content differs from this discovered/manual choice (the governed saved-model conflict
	// resolution). Ignored by CheckLane (which is static). Only honored when the key is
	// user-layer-owned and not shadowed by a higher config layer.
	Replace bool `json:"replace"`
}

// Source constants for LaneChoiceInput.Source.
const (
	SourceAdapterDefault  = "adapter_default"
	SourceSavedModel      = "saved_model"
	SourceDiscoveredModel = "discovered_model"
	SourceManualEntry     = "manual_entry"
)

// LaneCheckResult is the static, no-model-call check of a pending lane choice: shape
// validation per the adapter-owned rules, static adapter availability, the effective
// argument preview, and honest warnings for everything that CANNOT be verified statically.
// It never writes config and never invokes a model.
type LaneCheckResult struct {
	OK                bool     `json:"ok"`
	Errors            []string `json:"errors,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
	AdapterAvailable  bool     `json:"adapterAvailable"`
	AdapterDetail     string   `json:"adapterDetail"`
	EffectiveModelArg string   `json:"effectiveModelArg,omitempty"`
	Preview           string   `json:"preview,omitempty"`
	// Conflict is set when the choice would materialize a catalog key that already exists with
	// different content — the SAME condition that fails Save, surfaced here so Check predicts it
	// and the UI can offer a governed Replace. Field maps to model/effort.
	Conflict *SavedModelConflict `json:"conflict,omitempty"`
}

// laneCatalogKey derives a deterministic modelCatalog key for a discovered/manual choice.
func laneCatalogKey(adapter, modelArg, effort string) string {
	slug := strings.ToLower(modelArg)
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == ':':
			return r
		default:
			return '-'
		}
	}, slug)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	key := adapter + "-" + slug
	if effort != "" {
		key += "-" + strings.ToLower(effort)
	}
	return key
}
