package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
)

// Adapter-owned lane model/effort projection + validation (Area: the model/effort/query
// mechanism is business logic per adapter — modeled in Go, never in Svelte).

func discOK(models ...model.DiscoveredModel) *DiscoveryResult {
	return &DiscoveryResult{Status: "ok", Models: models}
}

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

func TestLaneModelConfig_PerAdapterProjection(t *testing.T) {
	m := mutMgr(t)
	opts := m.LaneOptions(map[string]*DiscoveryResult{
		"ollama": discOK(model.DiscoveredModel{Arg: "qwen2.5-coder:14b"}),
	})
	cfgOf := func(name string) LaneModelConfig {
		for _, a := range opts.Adapters {
			if a.Name == name {
				return a.Config
			}
		}
		t.Fatalf("adapter %q missing from projection", name)
		return LaneModelConfig{}
	}

	// `fake` is deliberately absent from LaneOptions (a test fixture, never a lane choice).
	for _, a := range opts.Adapters {
		if a.Name == "fake" {
			t.Errorf("fake must not be projected as a lane option: %+v", a)
		}
	}
	if c := cfgOf("ollama"); c.SelectionMode != "discovered_list" || c.EffortMode != "unsupported" ||
		c.Discovery.Status != "ok" || len(c.Discovery.Models) != 1 || c.Discovery.Mechanism != "ollama list" {
		t.Errorf("ollama config=%+v", c)
	}
	if c := cfgOf("codex-cli"); c.SelectionMode != "bundled_catalog" || c.EffortMode != "separate" ||
		c.Discovery.Status != "not_run" || c.Discovery.Mechanism != "codex debug models --bundled" || !c.ManualEntry {
		t.Errorf("codex config=%+v", c)
	}
	if c := cfgOf("claude-code"); c.EffortMode != "requested_unverified" || len(c.AllowedEfforts) != 5 ||
		c.Discovery.Status != "unsupported" || !c.ManualEntry {
		t.Errorf("claude config=%+v", c)
	}
	if c := cfgOf("devin-cli"); c.EffortMode != "encoded_in_model" || c.Discovery.Status != "unsupported" || !c.ManualEntry {
		t.Errorf("devin config=%+v", c)
	}
	if c := cfgOf("agy-cli"); c.EffortMode != "encoded_in_model" || c.Discovery.Mechanism != "agy models" || c.Discovery.Kind != "best_effort" {
		t.Errorf("agy config=%+v", c)
	}
	// gemini-cli: no effort flag and no model-listing command, and it never reports the answering
	// model — so the caveat is about UNVERIFIED identity, not about being untested/spec-only.
	if c := cfgOf("gemini-cli"); c.EffortMode != "unsupported" || len(c.Caveats) == 0 || !strings.Contains(c.Caveats[0], "UNVERIFIED") {
		t.Errorf("gemini config=%+v", c)
	}
}

// The hierarchy correction: an adapter default is projected under adapterDefault, NOT as a
// saved model; savedModels holds only genuine (non-default) entries with model-relevant labels.
func TestLaneModelSources_AdapterDefaultNotSavedModel(t *testing.T) {
	m := laneMgr(t)
	cfgOf := func(name string) LaneModelConfig {
		for _, a := range m.LaneOptions(nil).Adapters {
			if a.Name == name {
				return a.Config
			}
		}
		t.Fatalf("adapter %q missing", name)
		return LaneModelConfig{}
	}

	// claude-code: default present under adapterDefault (with a preview + caveat), savedModels empty.
	c := cfgOf("claude-code")
	if c.AdapterDefault == nil || !c.AdapterDefault.Available || c.AdapterDefault.CatalogKey != "claude-code-default" {
		t.Fatalf("claude adapterDefault=%+v", c.AdapterDefault)
	}
	if !strings.Contains(c.AdapterDefault.EffectiveArgumentPreview, "--model sonnet") {
		t.Errorf("adapter-default preview must show the effective arg: %q", c.AdapterDefault.EffectiveArgumentPreview)
	}
	if len(c.AdapterDefault.Caveats) == 0 || !strings.Contains(c.AdapterDefault.Caveats[0], "not a saved model preset") {
		t.Errorf("adapter default must carry the honest caveat: %+v", c.AdapterDefault.Caveats)
	}
	if len(c.SavedModels) != 0 {
		t.Errorf("claude-code must have NO saved models out of the box, got %+v", c.SavedModels)
	}
	if c.SavedModelsEmptyMessage == "" {
		t.Error("empty saved models must carry the empty message")
	}
	// The default catalog key must NOT appear in savedModels for ANY adapter.
	for _, name := range []string{"ollama", "codex-cli", "devin-cli", "agy-cli"} {
		cc := cfgOf(name)
		for _, sm := range cc.SavedModels {
			if e, ok := m.Cfg.ModelCatalog[sm.Key]; ok && e.IsAdapterDefault() {
				t.Errorf("adapter %q: adapter-default entry %q leaked into savedModels", name, sm.Key)
			}
		}
	}
}

func TestLaneModelSources_ExtraAdapterDefaultNeverSavedModel(t *testing.T) {
	m := laneMgr(t)
	// A (mis)configured SECOND adapterDefault-marked entry for claude-code must NOT appear as a
	// saved model — that would reintroduce the conflation. It is skipped entirely.
	tr := true
	m.Cfg.ModelCatalog["claude-code-alt-default"] = config.CatalogEntry{
		Provider: "anthropic", DisplayName: "Alt", AdapterDefault: &tr,
		Adapters: map[string]config.AdapterModel{"claude-code": {ModelArg: "haiku"}},
	}
	var claude LaneModelConfig
	for _, a := range m.LaneOptions(nil).Adapters {
		if a.Name == "claude-code" {
			claude = a.Config
		}
	}
	for _, sm := range claude.SavedModels {
		if e := m.Cfg.ModelCatalog[sm.Key]; e.IsAdapterDefault() {
			t.Errorf("an adapterDefault-marked entry %q must never render as a saved model", sm.Key)
		}
	}
	if claude.AdapterDefault == nil || !claude.AdapterDefault.Available {
		t.Error("the first adapter default must still be projected")
	}
}

func TestLaneModelSources_SavedModelHasModelRelevantLabel(t *testing.T) {
	m := laneMgr(t)
	// Add a genuine (non-default) saved model for claude-code. Use a displayName that does NOT
	// already encode the modelArg, so the label surfaces the model arg (the de-dup rule only
	// collapses when the displayName already conveys it — covered by TestSavedModelLabel_NoDuplication).
	m.Cfg.ModelCatalog["claude-code-opus"] = config.CatalogEntry{
		Provider: "anthropic", DisplayName: "Anthropic flagship",
		Adapters: map[string]config.AdapterModel{"claude-code": {ModelArg: "opus", Effort: "high"}},
	}
	var claude LaneModelConfig
	for _, a := range m.LaneOptions(nil).Adapters {
		if a.Name == "claude-code" {
			claude = a.Config
		}
	}
	if len(claude.SavedModels) != 1 {
		t.Fatalf("expected 1 saved model, got %+v", claude.SavedModels)
	}
	sm := claude.SavedModels[0]
	// Label must be MODEL-relevant (show the modelArg), not the bare adapter/tool name.
	if !strings.Contains(sm.Label, "opus") || sm.ModelArg != "opus" || !strings.Contains(sm.Label, "high") {
		t.Errorf("saved-model label must be model-relevant: %+v", sm)
	}
	if strings.Contains(sm.Label, "(via claude-code)") {
		t.Errorf("saved-model label must not be the bare tool name: %q", sm.Label)
	}
}

func TestValidateSourceContract(t *testing.T) {
	m := laneMgr(t)
	// adapter_default requires an adapterDefault entry.
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{Source: SourceAdapterDefault, CatalogKey: "claude-code-default"}, nil); len(errs) != 0 {
		t.Errorf("adapter_default with the default key must validate: %v", errs)
	}
	// saved_model must NOT accept an adapter-default key.
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{Source: SourceSavedModel, CatalogKey: "claude-code-default"}, nil); len(errs) == 0 {
		t.Error("saved_model must reject an adapter-default catalog key")
	}
	// adapter_default must NOT accept a non-default key.
	m.Cfg.ModelCatalog["claude-code-opus"] = config.CatalogEntry{Provider: "anthropic", Adapters: map[string]config.AdapterModel{"claude-code": {ModelArg: "opus"}}}
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{Source: SourceAdapterDefault, CatalogKey: "claude-code-opus"}, nil); len(errs) == 0 {
		t.Error("adapter_default must reject a non-default catalog key")
	}
	// discovered/manual must not carry a catalog key.
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{Source: SourceManualEntry, CatalogKey: "claude-code-default"}, nil); len(errs) == 0 {
		t.Error("manual_entry must reject a catalog key")
	}
	// manual with a model arg validates.
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{Source: SourceManualEntry, ModelArg: "opus"}, nil); len(errs) != 0 {
		t.Errorf("manual_entry with a model arg must validate: %v", errs)
	}
	// empty source is server-derived (back-compat) — a catalog key still works.
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{CatalogKey: "claude-code-default"}, nil); len(errs) != 0 {
		t.Errorf("empty-source catalog key must validate (server-derived): %v", errs)
	}
}

func TestSavedModelLabel_NoDuplication(t *testing.T) {
	cases := []struct {
		name                          string
		displayName, modelArg, effort string
		want                          string
	}{
		// The reported defect: displayName already encodes model + effort → not repeated.
		{"display encodes both", "Opus 4.8 (high)", "Opus 4.8", "high", "Opus 4.8 (high)"},
		{"display encodes model, effort appended", "Claude Opus", "opus", "high", "Claude Opus · high"},
		{"no display, model+effort", "", "gpt-5.5", "high", "gpt-5.5 · high"},
		{"display differs from model", "Codex (via codex-cli)", "gpt-5-codex", "", "Codex (via codex-cli) — gpt-5-codex"},
		{"display encodes model only, no effort", "qwen (local)", "qwen", "", "qwen (local)"},
		{"effort already in display", "GPT-5.5 high", "gpt-5.5", "high", "GPT-5.5 high"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := savedModelLabel(c.displayName, c.modelArg, c.effort)
			if got != c.want {
				t.Errorf("savedModelLabel(%q,%q,%q) = %q, want %q", c.displayName, c.modelArg, c.effort, got, c.want)
			}
			// The same concept must never appear twice: for an overlapping display, the modelArg
			// token must occur at most once in the label.
			if c.effort != "" && strings.Count(strings.ToLower(got), strings.ToLower(c.effort)) > 1 {
				t.Errorf("effort %q duplicated in label %q", c.effort, got)
			}
		})
	}
}

func TestValidateLaneChoice_Ollama(t *testing.T) {
	m := mutMgr(t)
	disc := discOK(model.DiscoveredModel{Arg: "qwen2.5-coder:14b"})

	// Discovered tag → valid, no errors.
	if _, errs, _ := m.validateLaneChoice("ollama", LaneChoiceInput{ModelArg: "qwen2.5-coder:14b"}, disc); len(errs) != 0 {
		t.Errorf("listed tag must validate: %v", errs)
	}
	// Unlisted tag with discovery ok → ERROR naming the mechanism.
	if _, errs, _ := m.validateLaneChoice("ollama", LaneChoiceInput{ModelArg: "not-pulled:7b"}, disc); len(errs) == 0 || !strings.Contains(errs[0], "ollama list") {
		t.Errorf("unlisted tag must fail against discovery: %v", errs)
	}
	// Discovery not run → warning, not error (Go-owned fallback policy).
	if _, errs, warns := m.validateLaneChoice("ollama", LaneChoiceInput{ModelArg: "whatever:1b"}, nil); len(errs) != 0 || len(warns) == 0 {
		t.Errorf("without discovery: errs=%v warns=%v", errs, warns)
	}
	// Effort is unsupported.
	if _, errs, _ := m.validateLaneChoice("ollama", LaneChoiceInput{ModelArg: "qwen2.5-coder:14b", Effort: "high"}, disc); len(errs) == 0 {
		t.Error("ollama effort must be rejected")
	}
}

func TestValidateLaneChoice_CodexEfforts(t *testing.T) {
	m := mutMgr(t)
	disc := discOK(model.DiscoveredModel{Arg: "gpt-5.5", DefaultEffort: "medium", Efforts: []string{"low", "medium", "high", "xhigh"}})

	if _, errs, _ := m.validateLaneChoice("codex-cli", LaneChoiceInput{ModelArg: "gpt-5.5", Effort: "high"}, disc); len(errs) != 0 {
		t.Errorf("supported effort must validate: %v", errs)
	}
	if _, errs, _ := m.validateLaneChoice("codex-cli", LaneChoiceInput{ModelArg: "gpt-5.5", Effort: "max"}, disc); len(errs) == 0 {
		t.Error("an effort outside the model's supported set must fail")
	}
	// Unbundled model → warning (account availability unknown), effort against the generic set.
	if _, errs, warns := m.validateLaneChoice("codex-cli", LaneChoiceInput{ModelArg: "gpt-6-alpha", Effort: "high"}, disc); len(errs) != 0 || len(warns) == 0 {
		t.Errorf("unbundled model: errs=%v warns=%v", errs, warns)
	}
}

func TestValidateLaneChoice_ClaudeRequestedEffort(t *testing.T) {
	m := mutMgr(t)
	res, errs, warns := m.validateLaneChoice("claude-code", LaneChoiceInput{ModelArg: "opus", Effort: "xhigh"}, nil)
	if len(errs) != 0 {
		t.Fatalf("valid claude choice failed: %v", errs)
	}
	if res.EffectiveModelArg != "opus" {
		t.Errorf("effective=%q", res.EffectiveModelArg)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "requested only") {
			found = true
		}
	}
	if !found {
		t.Errorf("claude must warn that effort is requested/unverified: %v", warns)
	}
	if _, errs, _ := m.validateLaneChoice("claude-code", LaneChoiceInput{ModelArg: "opus", Effort: "turbo"}, nil); len(errs) == 0 {
		t.Error("an unknown claude effort must fail")
	}
}

func TestValidateLaneChoice_DevinSlugComposition(t *testing.T) {
	m := mutMgr(t)
	res, errs, _ := m.validateLaneChoice("devin-cli", LaneChoiceInput{ModelArg: "Claude Opus 4.8", Effort: "medium"}, nil)
	if len(errs) != 0 {
		t.Fatalf("devin display+effort failed: %v", errs)
	}
	if res.EffectiveModelArg != "claude-opus-4-8-medium" {
		t.Errorf("rendered slug=%q want claude-opus-4-8-medium", res.EffectiveModelArg)
	}
	// A full combined slug with empty effort passes through unchanged.
	res2, errs2, _ := m.validateLaneChoice("devin-cli", LaneChoiceInput{ModelArg: "gpt-5-5-high"}, nil)
	if len(errs2) != 0 || res2.EffectiveModelArg != "gpt-5-5-high" {
		t.Errorf("combined slug: errs=%v effective=%q", errs2, res2.EffectiveModelArg)
	}
	// Ambiguous partial names are rejected by the render rules.
	if _, errs3, _ := m.validateLaneChoice("devin-cli", LaneChoiceInput{ModelArg: "Opus 4.8", Effort: "medium"}, nil); len(errs3) == 0 {
		t.Error("ambiguous devin display name must fail")
	}
}

func TestValidateLaneChoice_AgyNameBound(t *testing.T) {
	m := mutMgr(t)
	disc := discOK(model.DiscoveredModel{Arg: "Gemini 3.1 Pro (High)"})
	if _, errs, _ := m.validateLaneChoice("agy-cli", LaneChoiceInput{ModelArg: "Gemini 3.1 Pro (High)"}, disc); len(errs) != 0 {
		t.Errorf("listed agy name failed: %v", errs)
	}
	if _, errs, _ := m.validateLaneChoice("agy-cli", LaneChoiceInput{ModelArg: "Gemini 3.1 Pro", Effort: "high"}, disc); len(errs) == 0 {
		t.Error("a separate agy effort must be rejected (name-bound)")
	}
	// Unlisted name → warning only (best-effort listing).
	if _, errs, warns := m.validateLaneChoice("agy-cli", LaneChoiceInput{ModelArg: "Custom Model (High)"}, disc); len(errs) != 0 || len(warns) == 0 {
		t.Errorf("unlisted agy name: errs=%v warns=%v", errs, warns)
	}
}

// TestValidateLaneChoice_ClaudeModelArgShape pins the check that catches `opus-5` — a value that looks
// plausible, passed every prior config-only check, and was then rejected by the CLI at invocation, so a
// typo only surfaced as a failed ACP validation run after spend. It stays a WARNING (savable), because
// Claude Code also accepts gateway-qualified IDs and reviewmesh cannot enumerate Claude models.
func TestValidateLaneChoice_ClaudeModelArgShape(t *testing.T) {
	m := mutMgr(t)

	// The motivating mistake: a bare family WITH a version and no `claude-` prefix.
	_, errs, warns := m.validateLaneChoice("claude-code", LaneChoiceInput{ModelArg: "opus-5"}, nil)
	if len(errs) != 0 {
		t.Errorf("opus-5 must be savable (warning, not error): errs=%v", errs)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "claude-opus-5") {
		t.Errorf("the FIRST warning must name the corrected slug (the editor shows warnings[0] beside "+
			"the Check button): warns=%v", warns)
	}

	// Valid forms must not warn about shape — the first warning stays the effort caveat.
	for _, ok := range []string{"opus", "sonnet", "haiku", "fable", "claude-opus-5", "claude-opus-4-8", "OPUS"} {
		_, errs, warns := m.validateLaneChoice("claude-code", LaneChoiceInput{ModelArg: ok}, nil)
		if len(errs) != 0 {
			t.Errorf("%q must be accepted: errs=%v", ok, errs)
		}
		for _, w := range warns {
			if strings.Contains(w, "probably not a valid") || strings.Contains(w, "not a recognized") {
				t.Errorf("%q must not draw a shape warning: %q", ok, w)
			}
		}
	}

	// A gateway-qualified ID contains `claude` and must not be second-guessed.
	if _, _, warns := m.validateLaneChoice("claude-code",
		LaneChoiceInput{ModelArg: "anthropic.claude-opus-4-20250101-v1:0"}, nil); len(warns) > 0 &&
		strings.Contains(warns[0], "not a recognized") {
		t.Errorf("a gateway-qualified Claude ID must not warn about shape: %v", warns)
	}

	// Something unrelated to Claude still gets a visible caution rather than silent acceptance.
	if _, _, warns := m.validateLaneChoice("claude-code", LaneChoiceInput{ModelArg: "gpt-5.2"}, nil); len(warns) == 0 ||
		!strings.Contains(warns[0], "not a recognized") {
		t.Errorf("a non-Claude model arg must warn: %v", warns)
	}
}

func TestValidateLaneChoice_GeminiUnverifiedIdentity(t *testing.T) {
	m := mutMgr(t)
	if _, errs, warns := m.validateLaneChoice("gemini-cli", LaneChoiceInput{ModelArg: "gemini-3.1-pro"}, nil); len(errs) != 0 || len(warns) == 0 {
		t.Errorf("gemini manual entry must be allowed with an unverified-identity warning: errs=%v warns=%v", errs, warns)
	}
	if _, errs, _ := m.validateLaneChoice("gemini-cli", LaneChoiceInput{ModelArg: "gemini-3.1-pro", Effort: "high"}, nil); len(errs) == 0 {
		t.Error("gemini effort must be rejected — the CLI exposes no effort flag")
	}
}

func TestCheckLane_StaticOnlyNeverWrites(t *testing.T) {
	m := laneMgr(t)
	res, err := m.CheckLane("default", "reviewer", "claude-code",
		LaneChoiceInput{ModelArg: "opus", Effort: "medium"}, nil)
	if err != nil {
		t.Fatalf("CheckLane: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected ok, got %+v", res)
	}
	if !strings.Contains(res.Preview, "claude") || !strings.Contains(res.Preview, "--model opus") ||
		!strings.Contains(res.Preview, "--effort medium") || !strings.Contains(res.Preview, "<prompt>") {
		t.Errorf("preview=%q", res.Preview)
	}
	if userConfigExists(t) {
		t.Error("a static check must never write config")
	}
	// Errors are reported, not written.
	res2, err := m.CheckLane("default", "reviewer", "ollama", LaneChoiceInput{ModelArg: "x:1b", Effort: "high"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res2.OK || len(res2.Errors) == 0 {
		t.Errorf("invalid choice must fail the check: %+v", res2)
	}
	if userConfigExists(t) {
		t.Error("a failed check must never write config")
	}
}

func TestCheckLane_DevinPreviewUsesRenderedSlug(t *testing.T) {
	m := laneMgr(t)
	res, err := m.CheckLane("default", "reviewer", "devin-cli",
		LaneChoiceInput{ModelArg: "Claude Opus 4.8", Effort: "medium"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.EffectiveModelArg != "claude-opus-4-8-medium" {
		t.Errorf("effective=%q", res.EffectiveModelArg)
	}
	if !strings.Contains(res.Preview, "--model claude-opus-4-8-medium") {
		t.Errorf("preview must use the rendered slug: %q", res.Preview)
	}
}

func TestEditProfileLane_ManualChoiceMaterializesCatalogEntry(t *testing.T) {
	m := mutMgr(t)
	res, err := m.EditProfileLane("default", "reviewer", "claude-code",
		LaneChoiceInput{ModelArg: "opus", Effort: "medium"}, nil)
	if err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatalf("expected write, got %+v", res)
	}
	cfg := userCfg(t)
	lane := cfg.Profiles["default"].Lanes["reviewer"]
	wantKey := "claude-code-opus-medium"
	if lane.Model != wantKey || lane.Adapter != "claude-code" {
		t.Errorf("lane=%+v want model %s", lane, wantKey)
	}
	entry, ok := cfg.ModelCatalog[wantKey]
	if !ok {
		t.Fatalf("catalog entry %q not written", wantKey)
	}
	am := entry.Adapters["claude-code"]
	if am.ModelArg != "opus" || am.Effort != "medium" {
		t.Errorf("catalog adapter entry=%+v (effort must live on the CATALOG entry)", am)
	}
	if entry.Provider != "anthropic" {
		t.Errorf("provider=%q", entry.Provider)
	}
}

func TestEditProfileLane_ConflictingCatalogKeyReturnsGovernedConflict(t *testing.T) {
	m := mutMgr(t)
	// Simulate the merged view containing a conflicting entry under the derived key (content
	// differs from opus/medium).
	entry := m.Cfg.ModelCatalog["claude-code-default"]
	m.Cfg.ModelCatalog["claude-code-opus-medium"] = entry // adapters entry has modelArg "sonnet"
	res, err := m.EditProfileLane("default", "cross_check", "claude-code",
		LaneChoiceInput{ModelArg: "opus", Effort: "medium"}, nil)
	// The conflict is now a GOVERNED response (not an opaque error): nothing written, the
	// conflict is returned so the UI can offer Replace.
	if err != nil {
		t.Fatalf("a conflict must be a governed response, not an error: %v", err)
	}
	if res.ConfigWritten || res.Conflict == nil || res.Conflict.Key != "claude-code-opus-medium" {
		t.Errorf("expected a governed conflict for the key, got %+v", res)
	}
}

func TestEditProfileLane_OllamaDiscoveredTag(t *testing.T) {
	m := mutMgr(t)
	disc := discOK(model.DiscoveredModel{Arg: "qwen2.5-coder:14b"})
	if _, err := m.EditProfileLane("fully-local-ollama", "reviewer", "ollama",
		LaneChoiceInput{ModelArg: "qwen2.5-coder:14b"}, disc); err != nil {
		t.Fatalf("EditProfileLane: %v", err)
	}
	cfg := userCfg(t)
	entry, ok := cfg.ModelCatalog["ollama-qwen2.5-coder:14b"]
	if !ok {
		t.Fatal("ollama catalog entry not written")
	}
	if entry.Runtime != "local" || entry.Adapters["ollama"].ModelArg != "qwen2.5-coder:14b" {
		t.Errorf("entry=%+v", entry)
	}
	// And an unlisted tag is refused when discovery is fresh.
	if _, err := m.EditProfileLane("fully-local-ollama", "reviewer", "ollama",
		LaneChoiceInput{ModelArg: "nope:1b"}, disc); err == nil {
		t.Error("unlisted tag must be refused when discovery has run")
	}
}

func TestDiscoverModels_UnsupportedAdapters(t *testing.T) {
	m := mutMgr(t)
	// fake registry adapters implement no Lister → explicit unsupported error.
	if _, err := m.DiscoverModels(context.Background(), "claude-code"); err == nil {
		t.Error("a non-Lister adapter must report no mechanism")
	}
	if _, err := m.DiscoverModels(context.Background(), "not-an-adapter"); err == nil {
		t.Error("an unknown adapter must be rejected")
	}
}

func TestValidateLaneChoice_DevinCatalogKeyMustRender(t *testing.T) {
	m := mutMgr(t)
	// A hand-crafted catalog entry whose devin modelArg cannot render (ambiguous partial name)
	// must fail the CATALOG path with the same error the manual path raises — not diverge.
	m.Cfg.ModelCatalog["bad-devin"] = m.Cfg.ModelCatalog["devin-cli-default"]
	e := m.Cfg.ModelCatalog["bad-devin"]
	e.Adapters = map[string]config.AdapterModel{"devin-cli": {ModelArg: "Opus 4.8", Effort: "medium"}}
	m.Cfg.ModelCatalog["bad-devin"] = e

	if _, errs, _ := m.validateLaneChoice("devin-cli", LaneChoiceInput{CatalogKey: "bad-devin"}, nil); len(errs) == 0 {
		t.Error("an unrenderable devin catalog entry must fail the check (catalog/manual must not diverge)")
	}
	// A good catalog entry still renders.
	if _, errs, _ := m.validateLaneChoice("devin-cli", LaneChoiceInput{CatalogKey: "devin-cli-default"}, nil); len(errs) != 0 {
		t.Errorf("valid devin catalog entry failed: %v", errs)
	}
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
