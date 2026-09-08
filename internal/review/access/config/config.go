// Package config is the ConfigAccess layer: it loads the layered configuration
// (shipped seed → user → project → invocation override) and exposes it for the
// Resolver. The seed ships as a Go literal; override files load as YAML
// (gopkg.in/yaml.v3) or JSON, chosen by file extension. Unknown struct fields are
// rejected (strict), while free map keys (profile/adapter/catalog/lane names) are
// allowed.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	store "github.com/Tim-Butterfield/aimesh/meshcore/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
	"gopkg.in/yaml.v3"
)

// ConfigPatch and SetOp are re-exported from meshcore's domain-free config store so existing
// reviewmesh code (setup/engine) keeps using `config.ConfigPatch`/`config.SetOp` unchanged. The
// mechanism (patch apply, atomic write, validated load) lives in meshcore/config; this package
// supplies the typed Config schema + its validator.
type (
	ConfigPatch = store.Patch
	SetOp       = store.SetOp
)

// Config is a configuration layer / the merged result (see docs/configuration.md).
type Config struct {
	SchemaVersion  int                     `json:"schemaVersion"`
	DefaultProfile string                  `json:"defaultProfile"`
	Profiles       map[string]Profile      `json:"profiles"`
	ModelCatalog   map[string]CatalogEntry `json:"modelCatalog"`
	Adapters       map[string]Adapter      `json:"adapters"`
	Defaults       Defaults                `json:"defaults"`
	Surfaces       Surfaces                `json:"surfaces"`
	Review         Review                  `json:"review"`
	// NOTE: this struct is the WHOLE accepted schema. Strict parsing rejects anything else, on
	// purpose — reviewmesh never accepts-and-ignores a setting. Reserved-for-later sections
	// (`policy`, `validation`, `containment`, `audit`, `timeouts`) were removed rather than parsed
	// into inert free maps: a knob that silently does nothing is worse than an honest load error.
	// Re-add a section here only together with the code that reads it.
	//
	// Two keys survived that rule by accident and have since been removed on the same grounds:
	// `defaults.autoDetect` and `profiles.<name>.lanes.<role>.optional`. Both parsed, both merged
	// across layers, and nothing anywhere read either one — so setting them looked like configuring
	// something. They now fail strict parsing like any other unknown field. If either concept is
	// wanted, it comes back with the code that reads it, not before.
}

// Review holds iteration/convergence caps. Pointers so an explicit value
// (including 0) is distinguishable from omission during merge. Both fields are
// enforced by the review loop (inner reviewer passes / outer cycles).
type Review struct {
	MaxInnerIterations *int `json:"maxInnerIterations,omitempty"`
	MaxOuterCycles     *int `json:"maxOuterCycles,omitempty"`
	// MaxPanelRounds is the RUN-LEVEL ceiling on the TOTAL number of blind-primary rounds a
	// single outer cycle may spend across ALL panel seats — the knob that stops N seats from
	// multiplying maxInnerIterations into an unbounded budget. Unset (the shipped default) means
	// maxInnerIterations rounds per seat, capped in code; a panel of one therefore gets exactly
	// maxInnerIterations, i.e. the historical inner loop unchanged. A value below the seat count
	// is a CONFIG ERROR: every requested seat must be able to run at least one round, since a
	// budget that quietly drops a seat is still silent degradation.
	MaxPanelRounds *int `json:"maxPanelRounds,omitempty"`
}

// boolPtr/intPtr are helpers for optional scalar config fields (nil = unspecified,
// so a merge keeps the base value rather than zeroing it — the merge-safety fix).
func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// Profile is a runtime profile (a named bundle of lane assignments).
type Profile struct {
	Description       string          `json:"description,omitempty"`
	AdapterPreference []string        `json:"adapterPreference,omitempty"`
	Lanes             map[string]Lane `json:"lanes"`
	// Reviewers is the ORDERED blind-primary PANEL: 1..review.MaxReviewerSeats seats, each
	// an independent (adapter, model, effort) vantage that reviews blind and in parallel. The
	// count is variable BY CONSTRUCTION — this is a slice, not a fixed set of slots, and no
	// count is privileged anywhere.
	//
	// MIGRATION / SPELLING RULE: `lanes.reviewer` is accepted as SUGAR for a one-seat panel and
	// is normalized to `reviewers[0]` on read (ReviewerSeats). A single config layer that names
	// BOTH spellings for the same profile is a CONFIG ERROR (validateProfileSpelling) — there
	// must never be a question of which one won. Across layers there is no ambiguity either: an
	// override layer that supplies one spelling REPLACES the other (mergeProfile), so the
	// effective config carries exactly one.
	//
	// cross_check / verifier / author_remediator stay in `lanes`: they are distinguished single
	// phase lanes (informed-vs-blind, applyable-vs-report-only), not seats.
	Reviewers []Lane `json:"reviewers,omitempty"`
}

// ReviewerSeats returns the profile's ordered blind-primary panel, normalizing the legacy
// spelling: an explicit `reviewers` list when present, else the single `lanes.reviewer` as a
// panel of one, else nil (no primary lane configured at all). This is THE accessor — no caller
// reads `lanes.reviewer` directly to decide what the primary stage is, so the migration cannot
// be half-applied.
func (p Profile) ReviewerSeats() []Lane {
	if len(p.Reviewers) > 0 {
		out := make([]Lane, len(p.Reviewers))
		copy(out, p.Reviewers)
		return out
	}
	if l, ok := p.Lanes[string(review.RoleReviewer)]; ok {
		return []Lane{l}
	}
	return nil
}

// HasPanelSpelling reports whether this profile uses the explicit `reviewers` spelling.
func (p Profile) HasPanelSpelling() bool { return len(p.Reviewers) > 0 }

// Lane is one role's assignment within a profile. There is deliberately no `optional` flag: it used
// to parse and merge here and was read by nothing, so a config that set it was configuring nothing.
// Whether a lane runs is decided by PRESENCE — a profile that defines no `cross_check` lane simply
// does not run that phase (the run records a `lane_skipped` event saying so), while the required
// roles fail resolution when they are absent. A boolean saying "this one may be missing" adds
// nothing to a map whose keys already say which lanes exist.
type Lane struct {
	Execution string `json:"execution"` // "host" | "adapter"
	Adapter   string `json:"adapter,omitempty"`
	Model     string `json:"model"`
}

// CatalogEntry is one canonical model profile.
type CatalogEntry struct {
	Provider       string `json:"provider"`
	Runtime        string `json:"runtime,omitempty"`
	CanonicalModel string `json:"canonicalModel"`
	Effort         string `json:"effort,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
	Authoritative  *bool  `json:"authoritative,omitempty"` // *bool: merge-safe
	// AdapterDefault marks this entry as an ADAPTER DEFAULT/fallback (not a saved model
	// preset). The web-UI lane editor projects flagged entries under "Adapter default" —
	// separate from user/discovered "saved models" — so a fallback is never rendered as a
	// saved preset. It does NOT affect resolution (a lane referencing the entry resolves the
	// same either way). *bool so user/project layers can override the seed truth (set false
	// to reclassify a default as a saved model). INVARIANT (this build): an AdapterDefault
	// entry maps a SINGLE adapter — true of every seed default and every discovered/manual
	// entry the editor materializes; a per-AdapterModel marker would be the move if
	// multi-adapter defaults ever arise.
	AdapterDefault *bool                   `json:"adapterDefault,omitempty"` // *bool: merge-safe
	Adapters       map[string]AdapterModel `json:"adapters"`
}

// IsAdapterDefault reports whether this catalog entry is an adapter default/fallback
// (default false). The web-UI lane editor uses it to project the entry under "Adapter
// default" rather than as a saved-model preset; it does not affect resolution.
func (c CatalogEntry) IsAdapterDefault() bool { return c.AdapterDefault != nil && *c.AdapterDefault }

// AdapterModel is the per-adapter argument for a catalog entry.
type AdapterModel struct {
	ModelArg string `json:"modelArg"`
	Effort   string `json:"effort,omitempty"`
}

// Adapter is per-adapter capability metadata. It is deliberately small: the runnable half of an
// adapter (its probe binary name, argv, identity extraction, effort handling) is CODE-owned — a
// meshcore shell recipe or the generic ACP adapter — so config carries only what config can
// actually change. There is deliberately no `detect` probe name, `defaultAuthoritative` fallback or
// `needsCapture` provenance marker here: none would be read by anything, and the
// concepts they name are owned elsewhere (recipe `Detect`, per-catalog-entry `authoritative`, and
// the per-recipe capture status in docs/adapters.md + the spec-only projection in views).
type Adapter struct {
	// Path is the adapter binary location. It is populated ONLY from the shared
	// `.aimesh/adapters.yaml` layers (LoadLayered clears any path a config layer carried), and is
	// what reaches the runnable registry.
	Path          string `json:"path,omitempty"`
	ModelIdentity string `json:"modelIdentity"`
	Enabled       *bool  `json:"enabled,omitempty"` // nil = enabled
}

// IsEnabled reports whether the adapter is enabled (default true).
func (a Adapter) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// Defaults holds resolution defaults. Both fields are consulted only when `defaultProfile` is
// empty. There is deliberately no `autoDetect`: it parsed and merged here and was read by nothing —
// adapter detection is a `setup`/`doctor` action a human runs, never a config-driven behavior — so
// it is rejected rather than accepted-and-ignored.
type Defaults struct {
	AdapterPreference []string          `json:"adapterPreference"`
	ProfileForAdapter map[string]string `json:"profileForAdapter"`
}

// Surfaces holds per-surface mode defaults and per-surface policy capabilities.
type Surfaces struct {
	DefaultModeBySurface map[string]string `json:"defaultModeBySurface"`
	// CapabilitiesBySurface grants named POLICY CAPABILITIES to a surface. A capability is
	// deliberately part of the config store rather than an out-of-band launch exception: the
	// write-authority ceiling a surface gets is a FUNCTION of this config (see SurfaceCeiling), so
	// granting a capability RAISES the ceiling instead of stepping around it. A ceiling that could
	// be bypassed by a flag would not be a ceiling at all — it would be a default.
	//
	// The only capability today is CapabilityAllowRemediate (the MCP surface's `review_remediate`
	// tool). `aimesh review mcp --allow-remediate` sets it on the in-memory `mcp` surface for that
	// server process; an operator can equally set it in config for every MCP server they launch.
	CapabilitiesBySurface      map[string][]string `json:"capabilitiesBySurface,omitempty"`
	DegradeWhenModeUnavailable *bool               `json:"degradeWhenModeUnavailable,omitempty"` // *bool: merge-safe
}

// CapabilityAllowRemediate is the policy capability that lets a surface WRITE as the result of a
// review: it raises that surface's write-authority ceiling to `apply`. Without it, a surface whose
// default mode is `report` cannot reach patch/apply however the request is spelled.
const CapabilityAllowRemediate = "allowRemediate"

// HasCapability reports whether `surface` has been granted `capability`.
func (s Surfaces) HasCapability(surface, capability string) bool {
	for _, c := range s.CapabilitiesBySurface[surface] {
		if c == capability {
			return true
		}
	}
	return false
}

// SurfaceCeiling is the write-authority ceiling for a surface: its configured default mode,
// RAISED to `apply` when the surface holds CapabilityAllowRemediate. A surface with no configured
// entry keeps the historical fall-through of `apply` — the ceiling is a config statement, and an
// absent statement is not a restriction (a surface that must fail closed, like `mcp`, ships with an
// explicit `report` entry).
//
// It is the ONE evaluation point: resolution, the surfaces, and the remediation path all read the
// ceiling from here, so no two of them can disagree about what a config permits.
func (c Config) SurfaceCeiling(surface string) review.Mode {
	ceiling := review.ModeApply
	if m, ok := c.Surfaces.DefaultModeBySurface[surface]; ok && m != "" {
		ceiling = review.Mode(m)
	}
	if c.Surfaces.HasCapability(surface, CapabilityAllowRemediate) {
		ceiling = maxMode(ceiling, review.ModeApply)
	}
	return ceiling
}

// WithSurfaceCapability returns a COPY of c with `capability` granted to `surface`. It copies the
// capability map rather than mutating it, so a launch-time grant cannot leak into a shared config
// snapshot other components are reading.
func WithSurfaceCapability(c Config, surface, capability string) Config {
	if c.Surfaces.HasCapability(surface, capability) {
		return c
	}
	next := make(map[string][]string, len(c.Surfaces.CapabilitiesBySurface))
	for k, v := range c.Surfaces.CapabilitiesBySurface {
		next[k] = append([]string(nil), v...)
	}
	next[surface] = append(next[surface], capability)
	c.Surfaces.CapabilitiesBySurface = next
	return c
}

// ExampleProfiles returns the opt-in / smoke profile definitions that are NOT part of the
// shipped seed inventory (the seed ships only `default`). A user creates any of them in the web
// UI's New-profile dialog or by copying/editing `default`; `fully-local-ollama` is additionally
// seedable via `setup --profile fully-local-ollama` into a fresh config. They are also referenced
// by docs as examples and used by tests — they never appear as shipped runtime profile inventory
// in the web UI or `Default()`, so the Profiles list shows only what the user actually created.
func ExampleProfiles() map[string]Profile {
	return map[string]Profile{
		// Fully local, no cloud: every lane uses the Ollama adapter. Model tag from
		// modelCatalog.ollama-local (set REVIEWMESH_OLLAMA_MODEL).
		"fully-local-ollama": {
			Description:       "Fully local review via the Ollama adapter (no cloud, opt-in).",
			AdapterPreference: []string{"ollama"},
			Lanes: map[string]Lane{
				"author_remediator": {Execution: "host", Adapter: "ollama", Model: "ollama-local"},
				"reviewer":          {Execution: "adapter", Adapter: "ollama", Model: "ollama-local"},
			},
		},
		// Real provider CLIs (installed + authenticated by the user). The ReviewManager runs
		// author_remediator, reviewer, cross_check, and verifier (verifier is report-only).
		"native-three-provider": {
			Description:       "Native provider CLIs: claude-code (host+reviewer), codex-cli (cross-check), agy-cli (verifier). Requires those CLIs installed + authenticated.",
			AdapterPreference: []string{"claude-code"},
			Lanes: map[string]Lane{
				"author_remediator": {Execution: "host", Adapter: "claude-code", Model: "claude-code-default"},
				"reviewer":          {Execution: "adapter", Adapter: "claude-code", Model: "claude-code-default"},
				"cross_check":       {Execution: "adapter", Adapter: "codex-cli", Model: "codex-cli-default"},
				"verifier":          {Execution: "adapter", Adapter: "agy-cli", Model: "agy-cli-default"},
			},
		},
		"devin-gateway-three-provider": {
			Description:       "Devin CLI gateway across all lanes (host + reviewer + cross-check + verifier). Requires the devin CLI installed + authenticated.",
			AdapterPreference: []string{"devin-cli"},
			Lanes: map[string]Lane{
				"author_remediator": {Execution: "host", Adapter: "devin-cli", Model: "devin-cli-default"},
				"reviewer":          {Execution: "adapter", Adapter: "devin-cli", Model: "devin-cli-default"},
				"cross_check":       {Execution: "adapter", Adapter: "devin-cli", Model: "devin-cli-default"},
				"verifier":          {Execution: "adapter", Adapter: "devin-cli", Model: "devin-cli-default"},
			},
		},
		// Single-adapter smoke profiles (host + reviewer on one CLI), for opt-in real smoke
		// tests gated by env (see docs/configuration.md).
		"claude-code-smoke": {
			Description:       "Single-adapter smoke for claude-code.",
			AdapterPreference: []string{"claude-code"},
			Lanes: map[string]Lane{
				"author_remediator": {Execution: "host", Adapter: "claude-code", Model: "claude-code-default"},
				"reviewer":          {Execution: "adapter", Adapter: "claude-code", Model: "claude-code-default"},
			},
		},
		"codex-cli-smoke": {
			Description:       "Single-adapter smoke for codex-cli.",
			AdapterPreference: []string{"codex-cli"},
			Lanes: map[string]Lane{
				"author_remediator": {Execution: "host", Adapter: "codex-cli", Model: "codex-cli-default"},
				"reviewer":          {Execution: "adapter", Adapter: "codex-cli", Model: "codex-cli-default"},
			},
		},
		"agy-cli-smoke": {
			Description:       "Single-adapter smoke for agy-cli.",
			AdapterPreference: []string{"agy-cli"},
			Lanes: map[string]Lane{
				"author_remediator": {Execution: "host", Adapter: "agy-cli", Model: "agy-cli-default"},
				"reviewer":          {Execution: "adapter", Adapter: "agy-cli", Model: "agy-cli-default"},
			},
		},
		"devin-cli-smoke": {
			Description:       "Single-adapter smoke for devin-cli.",
			AdapterPreference: []string{"devin-cli"},
			Lanes: map[string]Lane{
				"author_remediator": {Execution: "host", Adapter: "devin-cli", Model: "devin-cli-default"},
				"reviewer":          {Execution: "adapter", Adapter: "devin-cli", Model: "devin-cli-default"},
			},
		},
	}
}

// ExampleProfile returns a single example profile definition by name (ok=false if unknown).
func ExampleProfile(name string) (Profile, bool) {
	p, ok := ExampleProfiles()[name]
	return p, ok
}

// ProfileNotFoundGuidance returns a trailing guidance clause (leading " — …", or "") when a
// missing profile name is one of the EXAMPLE profiles (which the seed does not ship). It turns a
// bare "profile not found" into an actionable message pointing at how to create it, so legacy
// `--profile <example>` / `defaultProfile: <example>` usages migrate gracefully.
func ProfileNotFoundGuidance(name string) string {
	if _, ok := ExampleProfiles()[name]; !ok {
		return ""
	}
	// The creation route that always works is copying/editing `default` in the config file.
	// `fully-local-ollama` is additionally seedable via the CLI into a FRESH config; a plain
	// `setup --profile <other>` does not add a profile to an existing config, so the CLI hint is
	// offered only for ollama (this guidance fires when a config already exists).
	if name == "fully-local-ollama" {
		return fmt.Sprintf(" — %q is an example profile, not shipped by default; create it with `aimesh review setup --profile %s`, or by copying/editing `default` in the config file", name, name)
	}
	return fmt.Sprintf(" — %q is an example profile, not shipped by default; create it by copying/editing the `default` profile in the config file", name)
}

// WithExampleProfiles returns a copy of cfg with the ExampleProfiles merged in (existing
// profiles win). Used by the CLI `setup --profile <name>`, docs examples, and tests — NOT the
// shipped seed.
func WithExampleProfiles(cfg Config) Config {
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	merged := map[string]Profile{}
	for k, v := range ExampleProfiles() {
		merged[k] = v
	}
	for k, v := range cfg.Profiles {
		merged[k] = v
	}
	cfg.Profiles = merged
	return cfg
}

// FakeProfile is the SHIPPED but HIDDEN test-only profile key. Its lanes use the built-in
// deterministic `fake` adapter (the coverage the `default` profile used to carry), so tests, the
// golden-run baseline, and the ACP validation fake-only smoke have a runnable fake-only profile
// WITHOUT the shipped `default` profile being pre-wired to fake — a fresh install ships `default`
// UNCONFIGURED (honestly Doctor-flagged). Like the `fake` ADAPTER (hidden from AdapterViews), this
// profile is hidden from the normal SPA profile list + setup guidance (see setup.ProfileViews /
// the setup wizard); it is still present in the config by name so those flows can select it.
const FakeProfile = "fake-smoke"

// IsHiddenProfile reports whether a profile is hidden from the normal profile list / setup
// guidance (mirrors the way the `fake` ADAPTER is hidden from AdapterViews). Today only the
// shipped-but-test-only FakeProfile is hidden; it stays selectable by name for tests/golden/ACP.
func IsHiddenProfile(name string) bool { return name == FakeProfile }

// Default returns the shipped seed configuration. The delivered runtime profile is `default`,
// which now ships UNCONFIGURED (no adapters wired to its required lanes) — a fresh install is
// honestly unconfigured (Doctor flags it) rather than pre-wired to the `fake` adapter. The
// deterministic `fake` coverage moved to the SHIPPED-but-HIDDEN `fake-smoke` profile (FakeProfile),
// used by tests/golden/the ACP fake-only smoke but hidden from the normal profile list. Every other
// profile is user-created, copied, or a test fixture (see ExampleProfiles); the UI lists only what
// the user actually has (minus the hidden fake profile).
func Default() Config {
	return Config{
		SchemaVersion:  1,
		DefaultProfile: "default",
		Profiles: map[string]Profile{
			"default": {
				Description: "Delivered default profile — no adapters configured yet. Configure the author_remediator and reviewer lanes with an installed adapter (Doctor flags this until you do), or copy a ready-made profile onto it.",
				// No AdapterPreference and empty-adapter required lanes: the lanes are PRESENT (the
				// required roles exist) but UNCONFIGURED, so a fresh install is honestly not-ready
				// (Doctor flags it) instead of silently pre-wired to the fake adapter.
				Lanes: map[string]Lane{
					"author_remediator": {Execution: "host"},
					"reviewer":          {Execution: "adapter"},
				},
			},
			// Shipped-but-HIDDEN deterministic fake profile (see FakeProfile / IsHiddenProfile). It
			// carries the fake lanes the `default` profile used to have, so tests, the golden-run
			// baseline, and the ACP validation fake-only smoke stay green without shipping `default`
			// pre-wired to fake. Hidden from the normal SPA profile list + setup guidance.
			FakeProfile: {
				Description:       "Deterministic built-in fake adapter across both required lanes. Shipped but hidden (test-only): the ACP validation smoke, the golden-run baseline, and tests use it; it is not shown in the normal profile list.",
				AdapterPreference: []string{"fake"},
				Lanes: map[string]Lane{
					"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
					"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
				},
			},
		},
		ModelCatalog: map[string]CatalogEntry{
			// The shipped catalog seeds exactly ONE entry per adapter, each an ADAPTER DEFAULT
			// (adapterDefault: true) — the adapter's fallback model argument, NOT a saved-model
			// preset and NOT a claim about the provider's current catalog. The web-UI lane editor
			// projects these under "Adapter default"; genuine saved models are user/project entries
			// or discovered/manual choices the user explicitly saves. We deliberately do NOT seed
			// multiple current provider model names (those are volatile — use discovery/manual).
			"fake-model": {
				Provider:       "fake",
				Runtime:        "local",
				CanonicalModel: "fake-1",
				DisplayName:    "Fake Model 1",
				Authoritative:  boolPtr(true),
				AdapterDefault: boolPtr(true),
				Adapters:       map[string]AdapterModel{"fake": {ModelArg: "fake-1"}},
			},
			// The local Ollama model: its tag is supplied at runtime (REVIEWMESH_OLLAMA_MODEL)
			// or by a project config; an empty modelArg fails resolution with clear guidance.
			"ollama-local": {
				Provider:       "ollama",
				Runtime:        "local",
				CanonicalModel: "",
				DisplayName:    "Local Ollama model",
				Authoritative:  boolPtr(true),
				AdapterDefault: boolPtr(true),
				Adapters:       map[string]AdapterModel{"ollama": {ModelArg: ""}},
			},
			// Native provider / gateway ADAPTER DEFAULTS. ModelArg values are sensible
			// fallbacks; override per project or pick a specific model via discovery/manual.
			// The CLIs must be installed + authenticated by the user (reviewmesh never
			// authenticates them).
			"claude-code-default": {
				Provider: "anthropic", CanonicalModel: "claude-sonnet", DisplayName: "Claude Sonnet",
				Authoritative: boolPtr(true), AdapterDefault: boolPtr(true), Adapters: map[string]AdapterModel{"claude-code": {ModelArg: "sonnet"}},
			},
			"codex-cli-default": {
				Provider: "openai", CanonicalModel: "gpt-5-codex", DisplayName: "GPT-5 Codex",
				Authoritative: boolPtr(true), AdapterDefault: boolPtr(true), Adapters: map[string]AdapterModel{"codex-cli": {ModelArg: "gpt-5-codex"}},
			},
			"agy-cli-default": {
				Provider: "google", CanonicalModel: "gemini-3-pro", DisplayName: "Gemini 3 Pro",
				Authoritative: boolPtr(true), AdapterDefault: boolPtr(true), Adapters: map[string]AdapterModel{"agy-cli": {ModelArg: "gemini-3-pro"}},
			},
			"devin-cli-default": {
				Provider: "devin", CanonicalModel: "devin", DisplayName: "Devin gateway",
				// devin-cli is name-bound: `modelArg` is the Devin IDE/Cascade DISPLAY name
				// (no effort, no "Thinking"); `effort` is separate. The resolver renders the
				// final slug (e.g. "claude-opus-4-8-medium"). See RenderDevinModelArg.
				Authoritative: boolPtr(true), AdapterDefault: boolPtr(true), Adapters: map[string]AdapterModel{"devin-cli": {ModelArg: "Claude Opus 4.8", Effort: "medium"}},
			},
			// NOTE: no ACP adapters are seeded. ACP is an open protocol with no fixed CLI list, so ACP
			// adapters are USER-DEFINED instances (meshcore/config/adapterlocations `acpAdapters`),
			// synthesized into cfg.Adapters + cfg.ModelCatalog at load (see synthesizeACPInstances).
		},
		Adapters: map[string]Adapter{
			"fake": {ModelIdentity: "self_report"},
			// Real shell adapters are configured (so a profile may select them) but are
			// NEVER chosen automatically: only the `fake` adapter is in any default
			// AdapterPreference. Availability is a runtime binary check (doctor reports it).
			// The probe binary name lives with the code-owned recipe (meshcore/model/shell), not here.
			"ollama":      {ModelIdentity: "self_report"},
			"devin-cli":   {ModelIdentity: "self_report"},
			"claude-code": {ModelIdentity: "envelope"},
			"codex-cli":   {ModelIdentity: "trace"},
			"agy-cli":     {ModelIdentity: "self_report"},
			"gemini-cli":  {ModelIdentity: "envelope"},
			"cursor-cli":  {ModelIdentity: "self_report"},
			// Generic ACP adapters are NOT seeded — they are user-defined instances (see above),
			// driven by the one generic meshcore/model/acpagent adapter.
		},
		Defaults: Defaults{
			AdapterPreference: []string{"fake"},
			ProfileForAdapter: map[string]string{"*": "default"},
		},
		Surfaces: Surfaces{
			// Per-surface write-authority CEILINGS: deliberate, config-visible POLICY (the
			// capability exists on every surface; only the default exposure differs by risk).
			//
			// THESE ARE CEILINGS, NOT DEFAULTS. A run that names no mode gets `report` on every
			// surface (see ResolvePlan) — the entries here bound what a surface may do when it IS
			// asked. `cli` sits at `apply` so that `--apply` works there at all, NOT so that an
			// unadorned `aimesh review run .` writes; that inversion is exactly what was fixed.
			//
			// `acp` sits at `report`: an ACP session is driven by an external host, so a live
			// workspace write must be an explicit opt-in
			// (`surfaces: {defaultModeBySurface: {acp: apply}}`) rather than something the ceiling
			// permits by default. `mcp` sits at `report` for the same reason and one more: its
			// caller is a MODEL. Writing there additionally requires the `allowRemediate`
			// capability (`surfaces: {capabilitiesBySurface: {mcp: [allowRemediate]}}`, or
			// `aimesh review mcp --allow-remediate`), which is what RAISES the ceiling — the entry
			// here is never bypassed.
			DefaultModeBySurface: map[string]string{
				"cli": "apply", "ci": "report", "acp": "report", "mcp": "report",
			},
			DegradeWhenModeUnavailable: boolPtr(true),
		},
		Review: Review{MaxInnerIterations: intPtr(4), MaxOuterCycles: intPtr(3)},
	}
}

// WithOllamaModel returns a copy of c with the local Ollama model tag applied to
// the `ollama-local` catalog entry (canonical model + the `ollama` adapter's
// modelArg). It is how REVIEWMESH_OLLAMA_MODEL / a project config supplies the tag
// without baking a model into the shipped seed. A no-op if tag is empty or the
// entry is absent.
func WithOllamaModel(c Config, tag string) Config {
	if tag == "" {
		return c
	}
	entry, ok := c.ModelCatalog["ollama-local"]
	if !ok {
		return c
	}
	entry.CanonicalModel = tag
	// copy the adapters map key-by-key (preserving any other mappings/metadata) and
	// update only the ollama entry's modelArg — don't mutate a shared seed map.
	adapters := make(map[string]AdapterModel, len(entry.Adapters)+1)
	for k, v := range entry.Adapters {
		adapters[k] = v
	}
	am := adapters["ollama"]
	am.ModelArg = tag
	adapters["ollama"] = am
	entry.Adapters = adapters
	cat := make(map[string]CatalogEntry, len(c.ModelCatalog))
	for k, v := range c.ModelCatalog {
		cat[k] = v
	}
	cat["ollama-local"] = entry
	c.ModelCatalog = cat
	return c
}

// Load returns the effective configuration: the shipped seed overlaid with an
// optional override file (path may be ""). The format is chosen by extension —
// `.yaml`/`.yml` parse as YAML, everything else as JSON. A missing default path is
// not an error; an explicitly-requested path that is missing or invalid is a fault.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fault.Wrap(fault.Config, fmt.Sprintf("read config %q", path), err)
	}
	override, perr := parseConfigBytes(path, b)
	if perr != nil {
		return cfg, perr
	}
	return merge(cfg, override), nil
}

// parseConfigBytes parses a config layer into the typed Config. YAML is converted to JSON first
// so the single set of `json:` struct tags stays the schema source of truth (JSON is a subset of
// YAML, so a JSON file in a .yaml path also parses). It is the TYPED half of the store; the
// generic byte/format plumbing (isYAML, yamlToJSON, decodeStrictJSON) lives in rawstore.go.
func parseConfigBytes(path string, b []byte) (Config, error) {
	var override Config
	if store.IsYAML(path) {
		jb, yerr := store.YAMLToJSON(b)
		if yerr != nil {
			return override, fault.Wrap(fault.Config, fmt.Sprintf("parse YAML config %q", path), yerr)
		}
		b = jb
	}
	// Strict: reject unknown STRUCT fields (typos). Free map keys — profile, adapter,
	// model-catalog, and lane names — remain allowed (maps accept any key).
	if err := store.DecodeStrict(b, &override); err != nil {
		return override, fault.Wrap(fault.Config, fmt.Sprintf("parse config %q", path), err)
	}
	if err := validateProfileSpelling(override); err != nil {
		return override, fault.Wrap(fault.Config, fmt.Sprintf("parse config %q", path), err)
	}
	return override, nil
}

// validateProfileSpelling enforces the reviewer-panel SPELLING RULE per config LAYER: a profile
// may name `reviewers` (the panel) or `lanes.reviewer` (sugar for a one-seat panel), never both
// in the same file. Accepting both would mean silently picking a winner, which is exactly the
// class of ambiguity a fail-closed config refuses. Across layers there is no conflict to detect:
// mergeProfile makes a supplied spelling REPLACE the other, so only one survives.
func validateProfileSpelling(c Config) error {
	for _, name := range sortedProfileNames(c.Profiles) {
		p := c.Profiles[name]
		if len(p.Reviewers) == 0 {
			continue
		}
		if _, ok := p.Lanes[string(review.RoleReviewer)]; ok {
			return fault.New(fault.Config, fmt.Sprintf(
				"profile %q names both `reviewers` (the blind primary panel) and `lanes.reviewer` (its one-seat sugar) — declare exactly one; `lanes.reviewer` is equivalent to `reviewers` with a single entry",
				name)).WithReason("profile_reviewer_spelling_conflict")
		}
	}
	return nil
}

func sortedProfileNames(m map[string]Profile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateConfigBytes is the typed-schema validator injected into the generic store primitives
// (loadRawMapValidated / applyMapPatchToFile): it strict-decodes bytes into the typed Config and
// returns the parse fault, discarding the value. This is the one seam through which reviewmesh's
// schema reaches the domain-free store — so the store never learns Profile/Lane/roster shapes.
func validateConfigBytes(path string, b []byte) error {
	_, err := parseConfigBytes(path, b)
	return err
}

// ComponentName is review's subdirectory of the shared `.aimesh/` state root. Review's config used to
// live in an app-private `.reviewmesh/`, from when review was a separate application; one state root
// with a component subdirectory per domain means one `init`, one VCS exclusion, and one home override
// instead of several that had to agree.
const ComponentName = "review"

// ComponentDir is review's config directory under a base directory: <base>/.aimesh/review. The base is
// either a project root or a user home, so the same layout serves both scopes.
func ComponentDir(base string) string {
	return filepath.Join(base, localstate.HomeDirName, ComponentName)
}

// DiscoverProjectConfig returns the project config path under base, preferring YAML
// (`.aimesh/review/config.yaml`, then `.yml`) over JSON (`.aimesh/review/config.json`).
// Returns "" if none exists.
func DiscoverProjectConfig(base string) string {
	for _, name := range []string{"config.yaml", "config.yml", "config.json"} {
		p := filepath.Join(ComponentDir(base), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// HomeDir returns the base directory for user/global config — the ONE home override for the whole
// tool (localstate.HomeEnvVar), so a hermetic run sets a single variable rather than one per domain.
func HomeDir() (string, error) { return localstate.UserHomeBase() }

// UserConfigPath is the canonical user/global config write path (~/.aimesh/review/config.yaml).
func UserConfigPath() (string, error) {
	h, err := HomeDir()
	if err != nil {
		return "", err
	}
	return ProjectConfigPath(h), nil
}

// DiscoverUserConfig returns the existing user/global config path (YAML preferred,
// then .yml, then .json) under the home dir, or "" if none / home undiscoverable.
func DiscoverUserConfig() string {
	h, err := HomeDir()
	if err != nil || h == "" {
		return ""
	}
	return DiscoverProjectConfig(h)
}

// Layers records which config layers were discovered + loaded (for doctor/diagnostics).
type Layers struct {
	UserPath       string
	UserLoaded     bool
	ProjectPath    string
	ProjectLoaded  bool
	ExplicitPath   string
	ExplicitLoaded bool

	// Shared adapter-location layers (`.aimesh/adapters.yaml`) — the PATH-ONLY substrate reviewmesh
	// shares with exploremesh. SharedUser is AIMESH_HOME-anchored; SharedProject is ROOT-anchored
	// (walk-up from cwd) so a subdir run sees the repo-wide file.
	SharedUserPath       string
	SharedUserLoaded     bool
	SharedProjectPath    string
	SharedProjectLoaded  bool
	SharedProjectHasRoot bool // cwd is inside a repo (a project shared layer can exist)

	// AdapterPathSource maps each adapter whose effective binary Path is set to the layer that supplied
	// it (one of the adapterPathSource* labels). AdapterPathShadowed lists adapters whose path from one
	// layer was overridden by a higher layer (surfaced as a diagnostic so a "saved" path that no longer
	// takes effect is visible). Both are nil when no adapter paths are configured.
	AdapterPathSource   map[string]string
	AdapterPathShadowed []string

	// ACPInstances is the effective set of user-defined ACP adapter instances (from the shared layers),
	// synthesized into cfg.Adapters + cfg.ModelCatalog at load. Views surface each instance's title/args.
	ACPInstances map[string]adapterlocations.ACPInstance
}

// Adapter-path provenance labels. Adapter binary paths are sourced SOLELY from the shared
// `.aimesh/adapters.yaml` layers (config.yaml no longer carries them), in scope order user < project.
const (
	adapterPathSourceUserShared    = "user (.aimesh/adapters.yaml)"
	adapterPathSourceProjectShared = "project (.aimesh/adapters.yaml)"
)

// LoadLayered composes the effective configuration in precedence order (lowest first): shipped
// defaults ← user (~/.aimesh/review) ← project (<repo-root>/.aimesh/review) ← explicit (--config). These full config
// layers supply EVERYTHING EXCEPT adapter binary paths. Adapter binary paths are sourced SOLELY from
// the PATH-ONLY shared `.aimesh/adapters.yaml` layers — user (AIMESH_HOME) then project (repo root,
// walk-up so a subdir run sees the repo-wide file) — which are applied AFTER dropping any path a
// config layer happened to include, so `.aimesh/adapters.yaml` is the single source of truth for
// paths. All non-explicit layers are OPTIONAL (a missing one is not an error); a present-but-malformed
// layer — config or shared — IS an error, as is an explicit path that is set-but-unreadable/invalid.
// LoadLayered records per-adapter path provenance + shadow diagnostics in the returned Layers.
func LoadLayered(cwd, explicitPath string) (Config, Layers, error) {
	cfg := Default()
	var ly Layers
	prov := map[string]string{}
	shadowed := map[string]bool{}

	ly.UserPath = DiscoverUserConfig()
	if ly.UserPath != "" {
		over, err := loadOverride(ly.UserPath)
		if err != nil {
			return cfg, ly, err
		}
		cfg = merge(cfg, over)
		ly.UserLoaded = true
	}
	if cwd != "" {
		ly.ProjectPath = DiscoverProjectConfig(cwd)
	}
	if ly.ProjectPath != "" {
		over, err := loadOverride(ly.ProjectPath)
		if err != nil {
			return cfg, ly, err
		}
		cfg = merge(cfg, over)
		ly.ProjectLoaded = true
	}
	if explicitPath != "" {
		ly.ExplicitPath = explicitPath
		over, err := loadOverride(explicitPath)
		if err != nil {
			return cfg, ly, err
		}
		cfg = merge(cfg, over)
		ly.ExplicitLoaded = true
	}

	// Adapter binary paths come ONLY from the shared adapters.yaml layers: drop any path a config
	// layer (or the seed) carried, then overlay shared-user < shared-project. The same layers also carry
	// the user-defined ACP adapter instances (captured here, synthesized into cfg below).
	clearAdapterPaths(&cfg)
	var sharedLocs []adapterlocations.Locations
	if sp, err := adapterlocations.UserLocationsPath(); err == nil {
		ly.SharedUserPath = sp
		loc, err := adapterlocations.Load(sp)
		if err != nil {
			return cfg, ly, err
		}
		ly.SharedUserLoaded = len(loc.Adapters) > 0
		applySharedPaths(&cfg, loc, adapterPathSourceUserShared, prov, shadowed)
		sharedLocs = append(sharedLocs, loc)
	}
	if cwd != "" {
		if pp, ok := adapterlocations.ProjectLocationsPath(cwd); ok {
			ly.SharedProjectPath, ly.SharedProjectHasRoot = pp, true
			loc, err := adapterlocations.Load(pp)
			if err != nil {
				return cfg, ly, err
			}
			ly.SharedProjectLoaded = len(loc.Adapters) > 0
			applySharedPaths(&cfg, loc, adapterPathSourceProjectShared, prov, shadowed)
			sharedLocs = append(sharedLocs, loc)
		}
	}
	// User-defined ACP adapter instances: synthesize a typed adapter + a default model-catalog entry
	// per instance so profiles/lanes/doctor/views see them like any other adapter.
	ly.ACPInstances = adapterlocations.ACPInstances(sharedLocs...)
	synthesizeACPInstances(&cfg, ly.ACPInstances)

	if len(prov) > 0 {
		ly.AdapterPathSource = prov
	}
	if len(shadowed) > 0 {
		ly.AdapterPathShadowed = sortedKeys(shadowed)
	}
	return cfg, ly, nil
}

// clearAdapterPaths drops any adapter Path a config layer or the shipped seed carried, so the shared
// adapters.yaml overlay is the single source of truth for binary paths.
func clearAdapterPaths(cfg *Config) {
	for name, ad := range cfg.Adapters {
		if ad.Path != "" {
			ad.Path = ""
			cfg.Adapters[name] = ad
		}
	}
}

// synthesizeACPInstances turns each user-defined ACP instance into a typed adapter + a default
// model-catalog entry, so the rest of the config (profiles, doctor, views, registry paths) treats it
// like any other adapter. ModelIdentity is cli_status (an ACP session reports its active model). The
// `<name>-default` catalog entry lets a lane reference it. The instance Path (if any) also flows to
// the runnable registry via the shared-path overlay — where the PATH-lookup name is derived (app
// wiring / acpagent.Registry), which is why no probe name is stored on the typed adapter here.
func synthesizeACPInstances(cfg *Config, instances map[string]adapterlocations.ACPInstance) {
	if len(instances) == 0 {
		return
	}
	if cfg.Adapters == nil {
		cfg.Adapters = map[string]Adapter{}
	}
	if cfg.ModelCatalog == nil {
		cfg.ModelCatalog = map[string]CatalogEntry{}
	}
	for name, inst := range instances {
		ad := Adapter{ModelIdentity: "cli_status"}
		if inst.Path != "" {
			ad.Path = inst.Path
		}
		cfg.Adapters[name] = ad
		title := inst.Title
		if title == "" {
			title = "ACP: " + name
		}
		// The default catalog entry requests the model the ACP session reported at validation
		// (inst.Model) as the EXPECTED model — so identity verification confirms the agent still answers
		// as that model (cli_status), and halts only on real drift. When Model is unset (saved without a
		// successful validation), the modelArg is empty and a review using the lane will prompt to
		// configure/validate the model before use.
		cfg.ModelCatalog[name+"-default"] = CatalogEntry{
			DisplayName:    title,
			CanonicalModel: inst.Model,
			Authoritative:  boolPtr(true),
			AdapterDefault: boolPtr(true),
			Adapters:       map[string]AdapterModel{name: {ModelArg: inst.Model}},
		}
	}
}

// applySharedPaths overlays a shared adapters.yaml layer's PATH entries onto cfg. It honors the
// presence-aware clear (a non-nil empty string is an explicit "use PATH" that clears an inherited
// path); a nil path is no override. It only touches adapters reviewmesh already defines — a path-only
// entry for an unknown adapter is inert (recipes are code-owned) and skipped. A higher shared layer
// overriding a lower one's path records a shadow diagnostic.
func applySharedPaths(cfg *Config, loc adapterlocations.Locations, source string, prov map[string]string, shadowed map[string]bool) {
	for name, e := range loc.Adapters {
		if e.Path == nil {
			continue
		}
		ad, ok := cfg.Adapters[name]
		if !ok {
			continue
		}
		if ad.Path != "" {
			if prev, had := prov[name]; had && prev != source {
				shadowed[name] = true
			}
		}
		ad.Path = *e.Path // "" clears (explicit use-PATH)
		cfg.Adapters[name] = ad
		if *e.Path != "" {
			prov[name] = source
		} else {
			delete(prov, name)
		}
	}
}

// sortedKeys returns the keys of a set in stable order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LoadRawMap reads a config file into a generic key→value map AFTER strict schema
// validation, so a caller can inspect which keys are **explicitly present** (vs seed
// defaults) — used by the user→project promotion planner. Returns the schema error if the
// file does not parse strictly (so promotion never reads a malformed source). Thin typed
// wrapper over the domain-free loadRawMapValidated (rawstore.go), supplying the Config validator.
func LoadRawMap(path string) (map[string]any, error) {
	return store.LoadRawMap(path, validateConfigBytes)
}

// loadOverride reads + strict-parses one config layer file (no Default merge).
func loadOverride(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fault.Wrap(fault.Config, fmt.Sprintf("read config %q", path), err)
	}
	return parseConfigBytes(path, b)
}

// merge overlays a higher-precedence layer onto a lower one with field-granular
// deep merge of nested objects (maps merge by key; non-zero scalars/non-nil lists
// in the override win; sibling fields are preserved). This is the Batch-1 subset
// of docs/configuration.md → "Configuration resolution & merge".
func merge(base, over Config) Config {
	if over.SchemaVersion != 0 {
		base.SchemaVersion = over.SchemaVersion
	}
	if over.DefaultProfile != "" {
		base.DefaultProfile = over.DefaultProfile
	}
	for k, ov := range over.Profiles {
		if base.Profiles == nil {
			base.Profiles = map[string]Profile{}
		}
		base.Profiles[k] = mergeProfile(base.Profiles[k], ov)
	}
	for k, ov := range over.ModelCatalog {
		if base.ModelCatalog == nil {
			base.ModelCatalog = map[string]CatalogEntry{}
		}
		base.ModelCatalog[k] = mergeCatalog(base.ModelCatalog[k], ov)
	}
	for k, ov := range over.Adapters {
		if base.Adapters == nil {
			base.Adapters = map[string]Adapter{}
		}
		base.Adapters[k] = mergeAdapter(base.Adapters[k], ov)
	}
	if over.Defaults.AdapterPreference != nil {
		base.Defaults.AdapterPreference = over.Defaults.AdapterPreference
	}
	base.Defaults.ProfileForAdapter = mergeStringMap(base.Defaults.ProfileForAdapter, over.Defaults.ProfileForAdapter)
	base.Surfaces.DefaultModeBySurface = mergeStringMap(base.Surfaces.DefaultModeBySurface, over.Surfaces.DefaultModeBySurface)
	// A capability grant REPLACES the lower layer's list for that surface rather than unioning it:
	// a user or project layer must be able to REVOKE a capability by naming an empty list, and a
	// union would make revocation impossible.
	for surface, caps := range over.Surfaces.CapabilitiesBySurface {
		if base.Surfaces.CapabilitiesBySurface == nil {
			base.Surfaces.CapabilitiesBySurface = map[string][]string{}
		}
		base.Surfaces.CapabilitiesBySurface[surface] = append([]string(nil), caps...)
	}
	if over.Surfaces.DegradeWhenModeUnavailable != nil {
		base.Surfaces.DegradeWhenModeUnavailable = over.Surfaces.DegradeWhenModeUnavailable
	}
	if over.Review.MaxInnerIterations != nil {
		base.Review.MaxInnerIterations = over.Review.MaxInnerIterations
	}
	if over.Review.MaxOuterCycles != nil {
		base.Review.MaxOuterCycles = over.Review.MaxOuterCycles
	}
	if over.Review.MaxPanelRounds != nil {
		base.Review.MaxPanelRounds = over.Review.MaxPanelRounds
	}
	return base
}

func mergeProfile(b, o Profile) Profile {
	if o.Description != "" {
		b.Description = o.Description
	}
	if o.AdapterPreference != nil {
		b.AdapterPreference = o.AdapterPreference
	}
	// REVIEWER-PANEL SPELLING across layers: a layer that supplies one spelling REPLACES the
	// other, so the effective config never carries both (which validateProfileSpelling refuses
	// within a layer). Concretely: a user config declaring `reviewers: [...]` for a profile the
	// seed shipped with `lanes.reviewer` migrates that profile to a panel rather than colliding
	// with the seed's sugar — and the panel is REPLACED wholesale, never element-merged, because
	// a seat list is an ordered composition, not a bag of keyed settings.
	if len(o.Reviewers) > 0 {
		b.Reviewers = append([]Lane(nil), o.Reviewers...)
		delete(b.Lanes, string(review.RoleReviewer))
	} else if _, ok := o.Lanes[string(review.RoleReviewer)]; ok {
		b.Reviewers = nil
	}
	for role, ol := range o.Lanes {
		if b.Lanes == nil {
			b.Lanes = map[string]Lane{}
		}
		bl := b.Lanes[role]
		if ol.Execution != "" {
			bl.Execution = ol.Execution
		}
		if ol.Adapter != "" {
			bl.Adapter = ol.Adapter
		}
		if ol.Model != "" {
			bl.Model = ol.Model
		}
		b.Lanes[role] = bl
	}
	return b
}

func mergeCatalog(b, o CatalogEntry) CatalogEntry {
	if o.Provider != "" {
		b.Provider = o.Provider
	}
	if o.Runtime != "" {
		b.Runtime = o.Runtime
	}
	if o.CanonicalModel != "" {
		b.CanonicalModel = o.CanonicalModel
	}
	if o.Effort != "" {
		b.Effort = o.Effort
	}
	if o.DisplayName != "" {
		b.DisplayName = o.DisplayName
	}
	if o.Authoritative != nil {
		b.Authoritative = o.Authoritative
	}
	if o.AdapterDefault != nil {
		b.AdapterDefault = o.AdapterDefault
	}
	for name, om := range o.Adapters {
		if b.Adapters == nil {
			b.Adapters = map[string]AdapterModel{}
		}
		bm := b.Adapters[name]
		if om.ModelArg != "" {
			bm.ModelArg = om.ModelArg
		}
		if om.Effort != "" {
			bm.Effort = om.Effort
		}
		b.Adapters[name] = bm
	}
	return b
}

func mergeAdapter(b, o Adapter) Adapter {
	if o.Path != "" {
		b.Path = o.Path
	}
	if o.ModelIdentity != "" {
		b.ModelIdentity = o.ModelIdentity
	}
	if o.Enabled != nil {
		b.Enabled = o.Enabled
	}
	return b
}

// ProjectConfigPath is the canonical project-scope config location (YAML) under a
// base directory. (A `config.json` beside it is still read for compatibility via
// DiscoverProjectConfig/Load.)
func ProjectConfigPath(base string) string {
	return filepath.Join(ComponentDir(base), "config.yaml")
}

// WriteYAMLFile writes the config as YAML (camelCase keys, via the json tags), creating
// parent dirs. Like WriteJSONFile it never contains secrets.
func (c Config) WriteYAMLFile(path string) error {
	jb, err := json.Marshal(c)
	if err != nil {
		return fault.Wrap(fault.Internal, "marshal config", err)
	}
	var v any
	if err := json.Unmarshal(jb, &v); err != nil {
		return fault.Wrap(fault.Internal, "marshal config", err)
	}
	yb, err := yaml.Marshal(v)
	if err != nil {
		return fault.Wrap(fault.Internal, "marshal YAML config", err)
	}
	return store.WriteFileAtomic(path, yb)
}

// WriteJSONFile writes the config as indented JSON, creating parent dirs. It never
// contains secrets (authentication stays with each adapter's own CLI), so writing
// it is safe (the ConfigAccess write path used by the setup wizard).
func (c Config) WriteJSONFile(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Internal, "marshal config", err)
	}
	return store.WriteFileAtomic(path, append(b, '\n'))
}

// ApplyPatchToFile is the single config-mutation write path: it strictly parses the
// existing config at path (if present), applies a ConfigPatch to the decoded map (creating
// nested maps, preserving unrelated fields, erroring rather than overwriting a malformed
// shape), ensures `schemaVersion`, and writes the result only after re-validating that it
// reloads strictly. It creates the file/dir if absent and never writes secrets. An
// unparseable existing file is refused and left byte-for-byte unchanged. This is the only
// place that turns a structured `ConfigPatch` into bytes on disk.
func ApplyPatchToFile(path string, patch ConfigPatch) error {
	return store.ApplyPatchToFile(path, patch, validateConfigBytes)
}

// SetAdapterPathInFile updates ONLY `adapters.<adapter>.path`, preserving every other
// field. Thin compatibility wrapper over ApplyPatchToFile (the shared config-patch write
// path). The caller validates the adapter name and binary path first.
func SetAdapterPathInFile(path, adapter, binPath string) error {
	return ApplyPatchToFile(path, ConfigPatch{Ops: []SetOp{
		{Path: []string{"adapters", adapter, "path"}, Value: binPath},
	}})
}

func mergeStringMap(b, o map[string]string) map[string]string {
	if o == nil {
		return b
	}
	if b == nil {
		b = map[string]string{}
	}
	for k, v := range o {
		b[k] = v
	}
	return b
}
