package setup

// This file is the ADAPTER-OWNED lane model/effort configuration projection (Area: lane
// editing as a real workbench). Everything adapter-specific — how models are discovered,
// whether manual entry is allowed, how effort works, what the effective invocation looks
// like, and what cannot be verified — is modeled HERE (and in the ModelAccess recipes),
// never in the SPA. The SPA renders these projections and submits choices; the Manager
// re-validates every submission before any write.

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// DiscoveredModelView is one discovered model, projected for the SPA.
type DiscoveredModelView struct {
	Arg           string   `json:"arg"`
	DefaultEffort string   `json:"defaultEffort,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
}

// DiscoveryView projects an adapter's model-discovery mechanism and its last on-demand
// result. Discovery NEVER runs automatically — status starts at not_run (or unsupported).
type DiscoveryView struct {
	Mechanism string                `json:"mechanism,omitempty"` // e.g. "ollama list"
	Kind      string                `json:"kind,omitempty"`      // local | bundled | best_effort
	Status    string                `json:"status"`              // unsupported | not_run | ok | unavailable | failed | stale
	Detail    string                `json:"detail,omitempty"`
	Models    []DiscoveredModelView `json:"models,omitempty"`
	Seq       int                   `json:"seq,omitempty"` // run marker so the UI can show freshness
}

// LaneModelConfig is the per-adapter configuration projection the lane editor renders. Its
// model source is one of FOUR distinct, explicitly-separated categories — an adapter default
// is NEVER folded into savedModels. The SPA renders these; it owns no adapter/model rule.
type LaneModelConfig struct {
	// SelectionMode is a legacy hint (discovered_list | bundled_catalog | catalog_plus_manual
	// | catalog_only) retained for continuity; the UI drives off the four explicit sources.
	SelectionMode string `json:"selectionMode"`
	// AdapterDefault is the adapter's own default/fallback (an adapterDefault-marked catalog
	// entry), projected SEPARATELY from savedModels — it is not a saved-model preset.
	AdapterDefault *AdapterDefaultView `json:"adapterDefault,omitempty"`
	// SavedModels are genuine (non-default) modelCatalog entries compatible with the adapter,
	// with model-relevant labels. Empty for a fresh install → SavedModelsEmptyMessage.
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

// AdapterDefaultView projects an adapter's default/fallback model source — shown separately
// from saved models, with the effective-argument preview and an honest caveat.
type AdapterDefaultView struct {
	Available                bool     `json:"available"`
	CatalogKey               string   `json:"catalogKey,omitempty"`
	Label                    string   `json:"label"`
	ModelArg                 string   `json:"modelArg,omitempty"`
	Effort                   string   `json:"effort,omitempty"`
	EffectiveArgumentPreview string   `json:"effectiveArgumentPreview,omitempty"`
	Caveats                  []string `json:"caveats,omitempty"`
}

// SavedModelChoice projects one genuine (non-default) saved model, with a Go-computed,
// model-relevant label so the SPA reconstructs no model semantics.
type SavedModelChoice struct {
	Key      string `json:"key"`
	Label    string `json:"label"` // "<displayName> — <modelArg>[ · effort]"
	ModelArg string `json:"modelArg"`
	Effort   string `json:"effort,omitempty"`
	Provider string `json:"provider,omitempty"`
	Local    bool   `json:"local"`
	// Management projection (for the saved-model list): the lanes using this key (stable order),
	// whether it LOOKS editor-materialized (advisory), and whether the UI can delete it here
	// (user-layer-owned, unshadowed, unused, not shipped-revealing) with the reason when not.
	UsedBy       []string `json:"usedBy,omitempty"`
	Generated    bool     `json:"generated"`
	Deletable    bool     `json:"deletable"`
	DeleteReason string   `json:"deleteReason,omitempty"`
}

// DiscoveryResult is the host-cached outcome of an on-demand discovery run (the webui
// Server owns the per-process cache; the Manager stays stateless).
type DiscoveryResult struct {
	Status string // ok | unavailable | failed
	Detail string
	Models []model.DiscoveredModel
	Seq    int
	Stale  bool // a config write happened after this run
}

// DiscoverModels runs an adapter's discovery mechanism ON DEMAND (a metadata/listing
// command only — never a model invocation). Adapters without the capability report an
// error; callers project that as unsupported.
func (m *Manager) DiscoverModels(ctx context.Context, adapter string) (DiscoveryResult, error) {
	a, ok := m.Adapters[adapter]
	if !ok {
		return DiscoveryResult{}, fault.New(fault.Usage, fmt.Sprintf("adapter %q is not implemented in this build", adapter))
	}
	lister, ok := a.(model.Lister)
	if !ok {
		return DiscoveryResult{}, fault.New(fault.Usage, fmt.Sprintf("adapter %q has no model-discovery mechanism", adapter))
	}
	if cmdName, _ := lister.DiscoveryMechanism(); cmdName == "" {
		return DiscoveryResult{}, fault.New(fault.Usage, fmt.Sprintf("adapter %q has no model-discovery mechanism", adapter))
	}
	models, err := lister.ListModels(ctx)
	if err != nil {
		status := "failed"
		if strings.Contains(err.Error(), "unavailable") {
			status = "unavailable"
		}
		return DiscoveryResult{Status: status, Detail: err.Error()}, nil
	}
	return DiscoveryResult{Status: "ok", Models: models}, nil
}

// laneProviderFor is the provider recorded on catalog entries the lane editor creates,
// consistent with the shipped seed's conventions.
var laneProviderFor = map[string]string{
	"fake": "fake", "ollama": "ollama", "claude-code": "anthropic", "codex-cli": "openai",
	"agy-cli": "google", "gemini-cli": "google", "devin-cli": "devin",
}

const (
	claudeEffortValues = "low medium high xhigh max"
	codexEffortValues  = "low medium high xhigh"
	devinEffortValues  = "low medium high"
)

func effortList(s string) []string { return strings.Fields(s) }

// laneModelConfig builds the per-adapter projection, folding in the last on-demand
// discovery result (nil = not run) and the split adapter-default / saved-model sources.
func (m *Manager) laneModelConfig(name string, disc *DiscoveryResult) LaneModelConfig {
	cfg := m.laneModelConfigBase(name, disc)
	cfg.AdapterDefault, cfg.SavedModels, cfg.SavedModelsEmptyMessage = m.laneModelSources(name)
	return cfg
}

// laneModelSources splits the catalog entries reachable through an adapter into its ADAPTER
// DEFAULT (adapterDefault-marked) and its genuine SAVED MODELS (unmarked) — the core of the
// hierarchy correction: a default is never returned as a saved model. Both buckets carry
// Go-computed, model-relevant labels; the SPA reconstructs no model semantics.
func (m *Manager) laneModelSources(adapter string) (*AdapterDefaultView, []SavedModelChoice, string) {
	keys := make([]string, 0, len(m.Cfg.ModelCatalog))
	for k, e := range m.Cfg.ModelCatalog {
		if _, ok := e.Adapters[adapter]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var def *AdapterDefaultView
	saved := make([]SavedModelChoice, 0, len(keys))
	for _, k := range keys {
		e := m.Cfg.ModelCatalog[k]
		am := e.Adapters[adapter]
		effort := am.Effort
		if effort == "" {
			effort = e.Effort
		}
		if e.IsAdapterDefault() {
			// The single-adapter invariant means one default per adapter; if a config
			// (mis)marks more than one, the first (sorted) wins and the rest are SKIPPED — an
			// adapterDefault-marked entry is NEVER rendered as a saved model (that would
			// reintroduce the very conflation this correction removes).
			if def == nil {
				def = m.adapterDefaultView(adapter, k, e, am.ModelArg, effort)
			}
			continue
		}
		sc := savedModelChoice(adapter, k, e, am.ModelArg, effort)
		sc.UsedBy = m.catalogKeyUsedBy(k)
		sc.Generated = isGeneratedCatalogEntry(k, adapter, e)
		sc.Deletable, sc.DeleteReason = m.savedModelDeletable(k, len(sc.UsedBy))
		saved = append(saved, sc)
	}
	if def == nil {
		def = &AdapterDefaultView{Available: false, Label: "No adapter default is configured for this adapter."}
	}
	empty := ""
	if len(saved) == 0 {
		empty = "No saved models are configured for this adapter. Use discovery or manual entry to add one."
	}
	return def, saved, empty
}

// adapterDefaultView builds the AdapterDefault projection incl. the effective-argument preview
// (devin's name-bound slug is rendered; the preview passes no separate effort for name-bound
// adapters, matching CheckLane).
func (m *Manager) adapterDefaultView(adapter, key string, e config.CatalogEntry, modelArg, effort string) *AdapterDefaultView {
	effective := modelArg
	if adapter == "devin-cli" {
		if slug, err := model.RenderDevinModelArg(modelArg, effort); err == nil {
			effective = slug
		}
	}
	previewEffort := effort
	if adapter == "devin-cli" || adapter == "agy-cli" {
		previewEffort = ""
	}
	label := "Adapter default"
	if effective != "" {
		label = "Adapter default — " + effective
	} else if e.DisplayName != "" {
		label = "Adapter default — " + e.DisplayName
	}
	return &AdapterDefaultView{
		Available: true, CatalogKey: key, Label: label,
		ModelArg: modelArg, Effort: effort,
		EffectiveArgumentPreview: m.PreviewLane(adapter, effective, previewEffort),
		Caveats: []string{
			"This is the adapter default/fallback. It is not a saved model preset and does not prove provider availability, nor that reviewmesh knows the provider's current model catalog.",
		},
	}
}

// savedModelChoice builds a model-relevant SavedModelChoice label ("<displayName> — <modelArg>[ · effort]").
func savedModelChoice(adapter, key string, e config.CatalogEntry, modelArg, effort string) SavedModelChoice {
	// One clear human label — never the same model/effort concept twice. When the displayName
	// already conveys the modelArg (e.g. a materialized entry "opus (high)") we do NOT append it
	// again; likewise effort is appended only when the label doesn't already carry it. Full
	// technical detail lives in the effective-argument preview, not the dropdown.
	label := savedModelLabel(e.DisplayName, modelArg, effort)
	return SavedModelChoice{
		Key: key, Label: label, ModelArg: modelArg, Effort: effort,
		Provider: e.Provider, Local: localAdapters[adapter] || e.Runtime == "local",
	}
}

// savedModelLabel composes a de-duplicated saved-model label from displayName/modelArg/effort.
func savedModelLabel(displayName, modelArg, effort string) string {
	label := modelArg
	if displayName != "" {
		label = displayName
		if !containsFold(displayName, modelArg) {
			label = displayName + " — " + modelArg
		}
	}
	if effort != "" && !containsFold(label, effort) {
		label += " · " + effort
	}
	return label
}

// containsFold reports whether sub occurs in s, case-insensitively (sub != "").
func containsFold(s, sub string) bool {
	return sub != "" && strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// laneModelConfigBase builds the per-adapter discovery/manual/effort skeleton (the model
// sources are attached by laneModelConfig).
func (m *Manager) laneModelConfigBase(name string, disc *DiscoveryResult) LaneModelConfig {
	discView := func(mechanism, kind string) DiscoveryView {
		v := DiscoveryView{Mechanism: mechanism, Kind: kind, Status: "not_run"}
		if disc != nil {
			v.Status, v.Detail, v.Seq = disc.Status, disc.Detail, disc.Seq
			if disc.Stale && disc.Status == "ok" {
				v.Status = "stale"
				v.Detail = "the configuration changed after this discovery run — refresh to re-list"
			}
			for _, dm := range disc.Models {
				v.Models = append(v.Models, DiscoveredModelView{Arg: dm.Arg, DefaultEffort: dm.DefaultEffort, Efforts: dm.Efforts})
			}
		}
		return v
	}
	unsupported := DiscoveryView{Status: "unsupported"}

	switch name {
	case "fake":
		return LaneModelConfig{
			SelectionMode: "catalog_only", Discovery: unsupported, EffortMode: "unsupported",
			Caveats: []string{"Built-in deterministic adapter — no external command, no effort control."},
		}
	case "ollama":
		return LaneModelConfig{
			SelectionMode: "discovered_list", Discovery: discView("ollama list", "local"),
			ManualEntry: true, ManualLabel: "locally installed model tag (as shown by `ollama list`)",
			EffortMode: "unsupported",
			Caveats: []string{
				"Models are listed from the LOCAL Ollama runtime — nothing leaves the machine.",
				"A manual tag is validated against the discovered local tags when discovery has run.",
			},
		}
	case "codex-cli":
		return LaneModelConfig{
			SelectionMode: "bundled_catalog", Discovery: discView("codex debug models --bundled", "bundled"),
			ManualEntry: true, ManualLabel: "model slug (e.g. gpt-5.5)",
			EffortMode: "separate", AllowedEfforts: effortList(codexEffortValues),
			Caveats: []string{
				"The bundled catalog is OFFLINE CLI metadata; account availability is proven only when the model is invoked.",
				"Older Codex versions may lack `debug models` — the editor then falls back to catalog/manual entry with the reason shown.",
			},
		}
	case "claude-code":
		return LaneModelConfig{
			SelectionMode: "catalog_plus_manual", Discovery: DiscoveryView{Status: "unsupported",
				Detail: "the Claude CLI documents no model/effort enumeration command"},
			ManualEntry: true, ManualLabel: "model alias (opus, sonnet, haiku, fable) or full model ID",
			EffortMode: "requested_unverified", AllowedEfforts: effortList(claudeEffortValues),
			Caveats: []string{
				"Effort is REQUESTED only: the Claude CLI silently falls back to the highest supported level at or below the request and does not report the effective level.",
				"Model identity IS verified from the response envelope after invocation; effective effort cannot be verified from CLI output.",
			},
		}
	case "devin-cli":
		return LaneModelConfig{
			SelectionMode: "catalog_plus_manual", Discovery: DiscoveryView{Status: "unsupported",
				Detail: "the Devin CLI documents no non-interactive model listing"},
			ManualEntry: true, ManualLabel: "Devin display model name (e.g. \"Claude Opus 4.8\") — or a full combined slug with effort left empty",
			EffortMode: "encoded_in_model", AllowedEfforts: effortList(devinEffortValues), DefaultEffort: "medium",
			Caveats: []string{
				"Effort/thinking is name-bound: reviewmesh renders the final Devin --model slug (e.g. \"Claude Opus 4.8\" + medium → claude-opus-4-8-medium).",
				"Availability cannot be listed through a documented Devin CLI command — it is proven only when Devin is invoked.",
			},
		}
	case "agy-cli":
		return LaneModelConfig{
			SelectionMode: "discovered_list", Discovery: discView("agy models", "best_effort"),
			ManualEntry: true, ManualLabel: "agy display model name (e.g. \"Gemini 3.1 Pro (High)\")",
			EffortMode: "encoded_in_model",
			Caveats: []string{
				"Reasoning tier is encoded in the model-name variant (e.g. \"(High)\", \"(Thinking)\") — there is no separate effort setting.",
				"Listing is best-effort; if it fails or changes shape, the editor falls back to catalog/manual entry with the reason shown.",
			},
		}
	case "gemini-cli":
		return LaneModelConfig{
			SelectionMode: "catalog_plus_manual", Discovery: DiscoveryView{Status: "unsupported",
				Detail: "gemini-cli exposes no model-listing command"},
			ManualEntry: true, ManualLabel: "model name (e.g. \"gemini-3.1-pro-preview\")",
			EffortMode: "unsupported",
			Caveats: []string{
				"gemini-cli does not report the answering model, so calls are UNVERIFIED (pass-with-caveat) — the requested model is recorded but never confirmed.",
				"There is no separate effort setting, and no model-listing command to validate the name against — enter it exactly as the CLI expects.",
			},
		}
	default:
		// A user-defined generic ACP adapter (acpAdapters.<name>): its default model is the one its ACP
		// session reports (captured at add-time, surfaced as the adapter default), but the user may also
		// enter a specific model ID the CLI supports — so manual entry is ENABLED (falling through to
		// catalog_only would let the user neither discover nor manually enter a model).
		// Discovery lists the session's advertised models when it has been run (see runDiscovery).
		if _, isACP := m.Layers.ACPInstances[name]; isACP {
			return LaneModelConfig{
				SelectionMode: "catalog_plus_manual", Discovery: discView("ACP session/new availableModels", "acp"),
				ManualEntry: true, ManualLabel: "model ID the ACP session accepts (e.g. gemini-3.5-flash)",
				EffortMode: "unsupported",
				Caveats: []string{
					"This is a user-defined ACP adapter. The default is the model its ACP session reported when you added it; you may also enter a specific model ID the CLI supports.",
					"The ACP session authoritatively reports its active model — identity is verified against it after invocation.",
				},
			}
		}
		return LaneModelConfig{
			SelectionMode: "catalog_only", Discovery: unsupported, EffortMode: "unsupported",
		}
	}
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

// validateSourceContract enforces consistency between a declared Source and the submitted
// shape/catalog marker. An empty Source is accepted (server-derived). Returns a hard error
// string (empty when consistent). Adapter-independent: the per-adapter rules live in
// validateLaneChoice's switch, this checks only the source/shape contract.
func (m *Manager) validateSourceContract(in LaneChoiceInput) string {
	switch in.Source {
	case "":
		return "" // server-derived from shape below
	case SourceAdapterDefault:
		if in.CatalogKey == "" {
			return "an adapter-default choice must reference the adapter-default catalog entry"
		}
		e, ok := m.Cfg.ModelCatalog[in.CatalogKey]
		if !ok || !e.IsAdapterDefault() {
			return fmt.Sprintf("model %q is not an adapter-default entry", in.CatalogKey)
		}
	case SourceSavedModel:
		if in.CatalogKey == "" {
			return "a saved-model choice must reference a saved catalog entry"
		}
		e, ok := m.Cfg.ModelCatalog[in.CatalogKey]
		if !ok {
			return fmt.Sprintf("model %q is not in the model catalog", in.CatalogKey)
		}
		if e.IsAdapterDefault() {
			return fmt.Sprintf("model %q is an adapter default, not a saved model — choose it under Adapter default", in.CatalogKey)
		}
	case SourceDiscoveredModel, SourceManualEntry:
		if in.CatalogKey != "" {
			return "a discovered/manual choice submits a model argument, not a catalog key"
		}
		if in.ModelArg == "" {
			return "a discovered/manual choice requires a model argument"
		}
	default:
		return fmt.Sprintf("unknown model source %q", in.Source)
	}
	return ""
}

// resolvedLaneChoice is the server-validated outcome of a LaneChoiceInput.
type resolvedLaneChoice struct {
	CatalogKey        string // set when the choice resolves to an EXISTING catalog entry
	ModelArg          string // the configured model argument (display name for devin)
	Effort            string
	EffectiveModelArg string // what the CLI actually receives (devin: the rendered slug)
}

// claudeModelArgError flags a claude-code modelArg shaped like a DISPLAY NAME rather than a Claude Code
// `--model` value. Claude Code passes the modelArg VERBATIM to `--model`, whose values are slugs/aliases
// (e.g. `opus`, `sonnet`, `claude-opus-4-8`) that never contain whitespace — so a whitespace-bearing
// value (e.g. "Opus 4.8") is rejected at Check/Save, before it can reach a real `--model` and fail at
// ACP spend. Pattern-based only (no hard-coded, stale model catalog). Returns "" when the value is fine.
func claudeModelArgError(modelArg string) string {
	if strings.TrimSpace(modelArg) != "" && strings.ContainsAny(modelArg, " \t\n\r\f\v") {
		return "Claude Code model argument looks like a display name, not a Claude Code --model value (it contains whitespace). Use a Claude Code model argument such as `opus`, `sonnet`, or a provider-supported slug such as `claude-opus-4-8`."
	}
	return ""
}

// claudeAliases are the bare family aliases the Claude CLI accepts as a whole --model value. A bare
// family name with a version appended (`opus-5`) is NOT one of them, and is the mistake below.
var claudeAliases = []string{"opus", "sonnet", "haiku", "fable"}

// claudeModelArgShapeWarning flags a --model value that is neither a bare family alias nor a
// `claude-`-prefixed model ID. `opus-5` is the motivating case: it looks right, passes every check
// reviewmesh can make from config alone, and is then REJECTED by the CLI at invocation ("There's an
// issue with the selected model (opus-5)") — turning a typo into a failed run late, after spend.
//
// This is a WARNING, not an error: Claude Code also accepts gateway-qualified IDs (Bedrock/Vertex
// forms such as `anthropic.claude-opus-4-...`), so refusing everything unrecognized would block
// legitimate values. reviewmesh cannot enumerate Claude models — the CLI exposes no listing command —
// so the honest posture is a visible, specific suggestion rather than a hard verdict.
func claudeModelArgShapeWarning(modelArg string) string {
	arg := strings.ToLower(strings.TrimSpace(modelArg))
	if arg == "" || strings.ContainsAny(arg, " \t\n\r\f\v") {
		return "" // empty, or already reported as a display name by claudeModelArgError
	}
	if slices.Contains(claudeAliases, arg) || strings.Contains(arg, "claude") {
		return ""
	}
	// A bare family + version (`opus-5`, `sonnet-4-6`) — suggest the two forms that do work.
	for _, family := range claudeAliases {
		if strings.HasPrefix(arg, family+"-") {
			return fmt.Sprintf("%q is probably not a valid Claude Code --model value: a version needs the "+
				"`claude-` prefix. Use %q (full ID) or %q (alias for the latest). The CLI rejects an unknown "+
				"model only when it is invoked, so this would fail mid-run.",
				modelArg, "claude-"+arg, family)
		}
	}
	return fmt.Sprintf("%q is not a recognized Claude Code model alias (%s) and does not contain `claude` — "+
		"verify it is a model your account can use. The CLI rejects an unknown model only when it is invoked, "+
		"so a wrong value fails mid-run.", modelArg, strings.Join(claudeAliases, ", "))
}

// validateLaneChoice applies the adapter-owned rules to a submitted choice. It returns
// hard errors (must not save) and warnings (savable, but the user should know).
func (m *Manager) validateLaneChoice(adapter string, in LaneChoiceInput, disc *DiscoveryResult) (resolvedLaneChoice, []string, []string) {
	var errs, warns []string
	res := resolvedLaneChoice{}

	if msg := m.validateSourceContract(in); msg != "" {
		return res, []string{msg}, nil
	}
	if (in.CatalogKey == "") == (in.ModelArg == "") {
		return res, []string{"choose exactly one of: a saved model / adapter default (catalog entry), or a model (discovered or manual)"}, nil
	}

	// Catalog-entry path: the entry must exist and support this adapter; its own effort
	// applies (a separate effort alongside a catalog key is ambiguous and refused).
	if in.CatalogKey != "" {
		entry, ok := m.Cfg.ModelCatalog[in.CatalogKey]
		if !ok {
			return res, []string{fmt.Sprintf("model %q is not in the model catalog", in.CatalogKey)}, nil
		}
		am, ok := entry.Adapters[adapter]
		if !ok {
			return res, []string{fmt.Sprintf("model %q has no %q entry in the model catalog (it is not reachable through that adapter)", in.CatalogKey, adapter)}, nil
		}
		if in.Effort != "" {
			return res, []string{"a catalog entry carries its own effort — pick a discovered/manual model to choose a different effort"}, nil
		}
		res.CatalogKey, res.ModelArg = in.CatalogKey, am.ModelArg
		res.Effort = am.Effort
		if res.Effort == "" {
			res.Effort = entry.Effort
		}
		res.EffectiveModelArg = am.ModelArg
		if adapter == "claude-code" {
			// A saved catalog entry whose claude-code modelArg is a display name (e.g. "Opus 4.8")
			// would pass here but be rejected by the real `--model`; fail Check/Save the same way the
			// manual path does, so a saved bad value is caught before ACP spend.
			if e := claudeModelArgError(am.ModelArg); e != "" {
				return res, []string{e}, nil
			}
		}
		if adapter == "devin-cli" {
			// The devin slug is name-bound; a catalog entry whose modelArg cannot render
			// (e.g. an ambiguous partial name) would pass here but fail at runtime resolution.
			// Fail the check with the same error the discovered/manual path raises, so catalog
			// and manual validation cannot diverge.
			slug, err := model.RenderDevinModelArg(am.ModelArg, res.Effort)
			if err != nil {
				return res, []string{fmt.Sprintf("catalog entry %q is invalid for devin-cli: %v", in.CatalogKey, err)}, nil
			}
			res.EffectiveModelArg = slug
		}
		return res, nil, nil
	}

	// Discovered/manual path — adapter-owned rules.
	res.ModelArg, res.Effort, res.EffectiveModelArg = in.ModelArg, in.Effort, in.ModelArg
	inDiscovered := func() bool {
		if disc == nil || disc.Status != "ok" {
			return false
		}
		for _, dm := range disc.Models {
			if dm.Arg == in.ModelArg {
				return true
			}
		}
		return false
	}

	switch adapter {
	case "fake":
		errs = append(errs, "the built-in fake adapter is catalog-only (fake-model)")
	case "ollama":
		if in.Effort != "" {
			errs = append(errs, "ollama has no effort control — leave effort empty")
		}
		if disc != nil && disc.Status == "ok" && !disc.Stale {
			if !inDiscovered() {
				errs = append(errs, fmt.Sprintf("model tag %q is not among the locally installed tags (per `ollama list`) — pull it first or pick a listed tag", in.ModelArg))
			}
		} else {
			warns = append(warns, "not validated against local tags (discovery "+discStatus(disc)+") — run discovery to validate")
		}
	case "codex-cli":
		allowed := effortList(codexEffortValues)
		var perModel []string
		if disc != nil && disc.Status == "ok" {
			for _, dm := range disc.Models {
				if dm.Arg == in.ModelArg {
					perModel = dm.Efforts
					break
				}
			}
			if perModel == nil {
				warns = append(warns, fmt.Sprintf("model %q is not in the bundled Codex catalog — account availability is unverified", in.ModelArg))
			}
		} else {
			warns = append(warns, "bundled catalog not consulted (discovery "+discStatus(disc)+")")
		}
		if in.Effort != "" {
			set := allowed
			if perModel != nil {
				set = perModel
			}
			if !slices.Contains(set, in.Effort) {
				errs = append(errs, fmt.Sprintf("effort %q is not supported for this model (supported: %s)", in.Effort, strings.Join(set, ", ")))
			}
		}
	case "claude-code":
		if e := claudeModelArgError(in.ModelArg); e != "" {
			errs = append(errs, e)
		}
		if in.Effort != "" && !slices.Contains(effortList(claudeEffortValues), in.Effort) {
			errs = append(errs, fmt.Sprintf("effort %q is not a Claude effort level (low, medium, high, xhigh, max)", in.Effort))
		}
		// A suspicious model shape comes FIRST: it is the actionable finding, and the editor surfaces
		// the first warning next to the Check button. The effort caveat is always-on background.
		if w := claudeModelArgShapeWarning(in.ModelArg); w != "" {
			warns = append(warns, w)
		}
		warns = append(warns, "effort is requested only — the Claude CLI may silently downgrade and does not report the effective level")
	case "devin-cli":
		slug, err := model.RenderDevinModelArg(in.ModelArg, in.Effort)
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			res.EffectiveModelArg = slug
			warns = append(warns, "availability is not listable through a documented Devin CLI command — verified only when Devin is invoked")
		}
	case "agy-cli":
		if in.Effort != "" {
			errs = append(errs, "agy reasoning tiers are name-bound (pick a model-name variant such as \"(High)\") — leave effort empty")
		}
		if disc != nil && disc.Status == "ok" && !disc.Stale {
			if !inDiscovered() {
				warns = append(warns, fmt.Sprintf("%q is not in the `agy models` listing — it may be rejected at invocation", in.ModelArg))
			}
		} else {
			warns = append(warns, "not validated against `agy models` (discovery "+discStatus(disc)+")")
		}
	case "gemini-cli":
		if in.Effort != "" {
			errs = append(errs, "gemini-cli exposes no effort flag — leave effort empty")
		}
		warns = append(warns, "gemini-cli does not report the answering model — calls stay unverified")
	default:
		errs = append(errs, fmt.Sprintf("adapter %q has no lane-configuration rules", adapter))
	}
	return res, errs, warns
}

func discStatus(d *DiscoveryResult) string {
	if d == nil {
		return "not run"
	}
	if d.Stale {
		return "stale"
	}
	return d.Status
}

// PreviewLane renders the EFFECTIVE invocation for a validated choice — composed from the
// same ModelAccess recipe real calls use (never assembled in the SPA).
func (m *Manager) PreviewLane(adapter, effectiveModelArg, effort string) string {
	if adapter == "fake" {
		return "(built-in fake adapter — deterministic, no external command)"
	}
	a, ok := m.Adapters[adapter]
	if !ok {
		return ""
	}
	p, ok := a.(model.ArgPreviewer)
	if !ok {
		return ""
	}
	parts := p.PreviewArgs(effectiveModelArg, effort)
	quoted := make([]string, len(parts))
	for i, s := range parts {
		if strings.ContainsAny(s, " \t\"'") {
			quoted[i] = fmt.Sprintf("%q", s)
		} else {
			quoted[i] = s
		}
	}
	return strings.Join(quoted, " ")
}

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

// CheckLane statically validates a pending lane choice (see LaneCheckResult). The devin
// effort note: the preview passes no separate effort for name-bound adapters — the tier is
// already encoded in the effective model argument.
func (m *Manager) CheckLane(profile, role, adapter string, in LaneChoiceInput, disc *DiscoveryResult) (LaneCheckResult, error) {
	out := LaneCheckResult{}
	if err := m.laneTargetGuards(profile, role, adapter); err != nil {
		return out, err
	}
	res, errs, warns := m.validateLaneChoice(adapter, in, disc)
	out.Errors, out.Warnings = errs, warns
	out.OK = len(errs) == 0
	if a, ok := m.Adapters[adapter]; ok {
		out.AdapterAvailable, out.AdapterDetail = a.Available()
		if !out.AdapterAvailable {
			out.Warnings = append(out.Warnings, "adapter binary not available: "+out.AdapterDetail)
		}
	}
	if out.OK {
		// Predict Save's materialization outcome via the SHARED planner: a generated-key conflict
		// that would fail Save must fail Check too, with the same actionable explanation.
		if plan := m.planMaterialization(adapter, res); plan.Action == "conflict" {
			out.OK = false
			out.Conflict = plan.Conflict
			msg := fmt.Sprintf("saving this would generate the saved-model key %q, which already exists with different content (%s)",
				plan.Conflict.Key, plan.Conflict.ExistingLabel)
			if plan.Conflict.Replaceable {
				msg += " — replace the existing saved model, or choose a different model/effort"
			} else {
				msg += " — " + plan.Conflict.Reason
			}
			out.Errors = append(out.Errors, msg)
		}
	}
	if out.OK {
		out.EffectiveModelArg = res.EffectiveModelArg
		previewEffort := res.Effort
		if adapter == "devin-cli" || adapter == "agy-cli" {
			previewEffort = "" // name-bound: the tier is already in the effective model arg
		}
		out.Preview = m.PreviewLane(adapter, res.EffectiveModelArg, previewEffort)
	}
	return out, nil
}

// laneTargetGuards are the shared profile/role/adapter guards for lane check + save.
func (m *Manager) laneTargetGuards(profile, role, adapter string) error {
	if profile == "" || role == "" || adapter == "" {
		return fault.New(fault.Usage, "profile, role, and adapter are all required")
	}
	if _, ok := m.Cfg.Profiles[profile]; !ok {
		return fault.New(fault.Usage, fmt.Sprintf("profile %q does not exist", profile))
	}
	validRole := false
	for _, r := range laneRolePurposes {
		if r.Role == role {
			validRole = true
			break
		}
	}
	if !validRole {
		return fault.New(fault.Usage, fmt.Sprintf("unknown role %q (valid: author_remediator, reviewer, cross_check, verifier)", role))
	}
	if _, implemented := m.Adapters[adapter]; !implemented {
		return fault.New(fault.Usage, fmt.Sprintf("adapter %q is not implemented in this build", adapter))
	}
	if a, ok := m.Cfg.Adapters[adapter]; ok && !a.IsEnabled() {
		return fault.New(fault.Usage, fmt.Sprintf("adapter %q is disabled in the configuration", adapter))
	}
	return nil
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

// catalogEntryFor builds the modelCatalog entry map (json-tag keys, re-validated by the
// write path) for a discovered/manual lane choice.
func catalogEntryFor(adapter, modelArg, effort string) map[string]any {
	display := modelArg
	if effort != "" {
		display += " (" + effort + ")"
	}
	adapterEntry := map[string]any{"modelArg": modelArg}
	if effort != "" {
		adapterEntry["effort"] = effort
	}
	entry := map[string]any{
		"provider":    laneProviderFor[adapter],
		"displayName": display,
		"adapters":    map[string]any{adapter: adapterEntry},
	}
	if adapter == "ollama" {
		entry["runtime"] = "local"
	}
	return entry
}
