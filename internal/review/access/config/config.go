// Package config loads the layered review configuration (shipped seed, user, project, invocation
// override) and resolves it into run plans. Override files are YAML or JSON by extension and are
// parsed strictly: unknown struct fields are rejected, while map keys are free.
package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	store "github.com/Tim-Butterfield/aimesh/meshcore/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
	"gopkg.in/yaml.v3"
)

// ConfigPatch and SetOp re-export meshcore's domain-free config store types. The store implements
// patch apply, atomic write and validated load; this package supplies the typed schema and validator.
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
	// This struct is the whole accepted schema. Strict parsing rejects anything else, so a setting
	// is never accepted and ignored; add a section only together with the code that reads it.
}

// Review holds iteration and convergence caps. The fields are pointers so an explicit value,
// including 0, is distinguishable from omission during merge.
type Review struct {
	MaxInnerIterations *int `json:"maxInnerIterations,omitempty"`
	MaxOuterCycles     *int `json:"maxOuterCycles,omitempty"`
	// MaxPanelRounds caps the total blind-primary rounds one outer cycle may spend across all panel
	// seats. Unset means maxInnerIterations rounds per seat, capped in code. A value below the seat
	// count is a configuration error, because every requested seat must run at least one round.
	MaxPanelRounds *int `json:"maxPanelRounds,omitempty"`
}

// Profile is a runtime profile: a named bundle of lane assignments.
type Profile struct {
	Description       string          `json:"description,omitempty"`
	AdapterPreference []string        `json:"adapterPreference,omitempty"`
	Lanes             map[string]Lane `json:"lanes"`
	// Reviewers is the ordered blind-primary panel of 1..review.MaxReviewerSeats seats, each an
	// independent (adapter, model, effort) vantage.
	//
	// `lanes.reviewer` is accepted as a one-seat panel and normalized by ReviewerSeats. Naming both
	// spellings in one layer is a configuration error (validateProfileSpelling); across layers, the
	// higher layer's spelling replaces the other (mergeProfile). cross_check, verifier and
	// author_remediator stay in Lanes because they are single phase lanes, not seats.
	Reviewers []Lane `json:"reviewers,omitempty"`
}

// ReviewerSeats returns the profile's ordered blind-primary panel: the `reviewers` list when
// present, else `lanes.reviewer` as a panel of one, else nil. Callers use it rather than reading
// `lanes.reviewer` directly.
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

// Lane is one role's assignment within a profile. Whether a lane runs is decided by its presence:
// a profile with no `cross_check` lane skips that phase, while a missing required role fails
// resolution.
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
	// AdapterDefault marks the entry as an adapter's fallback model rather than a saved model, so
	// views list it separately. It does not affect resolution. An adapter-default entry maps a
	// single adapter.
	AdapterDefault *bool                   `json:"adapterDefault,omitempty"` // *bool: merge-safe
	Adapters       map[string]AdapterModel `json:"adapters"`
}

// IsAdapterDefault reports whether this catalog entry is an adapter's fallback model (default
// false). It does not affect resolution.
func (c CatalogEntry) IsAdapterDefault() bool { return c.AdapterDefault != nil && *c.AdapterDefault }

// AdapterModel is the per-adapter argument for a catalog entry.
type AdapterModel struct {
	ModelArg string `json:"modelArg"`
	Effort   string `json:"effort,omitempty"`
}

// Adapter is per-adapter capability metadata. The runnable half of an adapter (binary name, argv,
// identity extraction, effort handling) is owned by code in a meshcore shell recipe or the generic
// ACP adapter, so configuration carries only what it can change.
type Adapter struct {
	// Path is the adapter binary location. It comes only from the shared `.aimesh/adapters.yaml`
	// layers; LoadLayered clears any path a config layer carried.
	Path          string `json:"path,omitempty"`
	ModelIdentity string `json:"modelIdentity"`
	Enabled       *bool  `json:"enabled,omitempty"` // nil = enabled
}

// IsEnabled reports whether the adapter is enabled (default true).
func (a Adapter) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// Defaults holds resolution defaults, consulted only when `defaultProfile` is empty.
type Defaults struct {
	AdapterPreference []string          `json:"adapterPreference"`
	ProfileForAdapter map[string]string `json:"profileForAdapter"`
}

// Surfaces holds per-surface mode defaults and per-surface policy capabilities for the CLI surfaces
// (`cli`, `ci`). The MCP and ACP servers read no configuration: they gate writes with their own
// `--allow-writes` launch grant, so entries for them here have no effect.
type Surfaces struct {
	DefaultModeBySurface map[string]string `json:"defaultModeBySurface"`
	// CapabilitiesBySurface grants named policy capabilities to a surface. A capability raises the
	// surface's write ceiling (see SurfaceCeiling). The only capability is CapabilityAllowRemediate.
	CapabilitiesBySurface      map[string][]string `json:"capabilitiesBySurface,omitempty"`
	DegradeWhenModeUnavailable *bool               `json:"degradeWhenModeUnavailable,omitempty"` // *bool: merge-safe
}

// CapabilityAllowRemediate raises a surface's write ceiling to `apply`. Without it, a surface whose
// configured mode is `report` (the shipped `ci` entry) cannot reach patch or apply.
const CapabilityAllowRemediate = "allowRemediate"

// HasCapability reports whether `surface` has been granted `capability`.
func (s Surfaces) HasCapability(surface, capability string) bool {
	return slices.Contains(s.CapabilitiesBySurface[surface], capability)
}

// SurfaceCeiling returns the write ceiling for a surface: its configured mode, raised to `apply` when
// the surface holds CapabilityAllowRemediate. A surface with no entry resolves to `apply`; `ci` ships
// with an explicit `report` entry.
//
// The agent surfaces (`acp`, `mcp`) always resolve to `apply` and ignore configuration; they gate
// writes with their `--allow-writes` launch grant. Resolution, the surfaces and remediation all read
// the ceiling from here.
func (c Config) SurfaceCeiling(surface string) review.Mode {
	if IsAgentSurface(surface) {
		return review.ModeApply
	}
	ceiling := review.ModeApply
	if m, ok := c.Surfaces.DefaultModeBySurface[surface]; ok && m != "" {
		ceiling = review.Mode(m)
	}
	if c.Surfaces.HasCapability(surface, CapabilityAllowRemediate) {
		ceiling = maxMode(ceiling, review.ModeApply)
	}
	return ceiling
}

// IsAgentSurface reports whether surface is one of the agent-driven servers (`acp`, `mcp`), whose write
// authority comes from their launch arguments rather than from configuration.
func IsAgentSurface(surface string) bool { return surface == "acp" || surface == "mcp" }

// WithSurfaceCapability returns a copy of c with capability granted to surface. The capability map
// is copied so the grant cannot leak into a shared config snapshot.
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

// ExampleProfiles returns opt-in and smoke profile definitions that are not part of the shipped seed.
// A user creates one by copying or editing `default`; `fully-local-ollama` can also be seeded into a
// fresh config with `setup --profile fully-local-ollama`. Docs and tests use them as examples.
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

// ProfileNotFoundGuidance returns a trailing clause (" — …") explaining how to create a missing
// profile when name is one of the example profiles, or "" otherwise.
func ProfileNotFoundGuidance(name string) string {
	if _, ok := ExampleProfiles()[name]; !ok {
		return ""
	}
	// Only fully-local-ollama has a CLI seeding route; the others are created by copying `default`.
	if name == "fully-local-ollama" {
		return fmt.Sprintf(" — %q is an example profile, not shipped by default; create it with `aimesh review setup --profile %s`, or by copying/editing `default` in the config file", name, name)
	}
	return fmt.Sprintf(" — %q is an example profile, not shipped by default; create it by copying/editing the `default` profile in the config file", name)
}

// WithExampleProfiles returns a copy of cfg with ExampleProfiles merged in; existing profiles win.
func WithExampleProfiles(cfg Config) Config {
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	merged := map[string]Profile{}
	maps.Copy(merged, ExampleProfiles())
	maps.Copy(merged, cfg.Profiles)
	cfg.Profiles = merged
	return cfg
}

// FakeProfile is the shipped, hidden, test-only profile whose lanes use the deterministic `fake`
// adapter. Tests, the golden run and the ACP validation smoke select it by name; profile views and
// setup guidance hide it.
const FakeProfile = "fake-smoke"

// IsHiddenProfile reports whether a profile is hidden from profile views and setup guidance. Only
// FakeProfile is hidden.
func IsHiddenProfile(name string) bool { return name == FakeProfile }

// Default returns the shipped seed configuration. The `default` profile ships with its required
// lanes present but no adapters, so doctor reports a fresh install as unconfigured. FakeProfile
// provides a runnable fake-only profile for tests.
func Default() Config {
	return Config{
		SchemaVersion:  1,
		DefaultProfile: "default",
		Profiles: map[string]Profile{
			"default": {
				Description: "Delivered default profile — no adapters configured yet. Configure the author_remediator and reviewer lanes with an installed adapter (Doctor flags this until you do), or copy a ready-made profile onto it.",
				Lanes: map[string]Lane{
					"author_remediator": {Execution: "host"},
					"reviewer":          {Execution: "adapter"},
				},
			},
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
			// One adapter-default entry per adapter: a fallback model argument, not a claim about
			// the provider's current models, which change too often to ship.
			"fake-model": {
				Provider:       "fake",
				Runtime:        "local",
				CanonicalModel: "fake-1",
				DisplayName:    "Fake Model 1",
				Authoritative:  new(true),
				AdapterDefault: new(true),
				Adapters:       map[string]AdapterModel{"fake": {ModelArg: "fake-1"}},
			},
			// The local Ollama model: its tag is supplied at runtime (REVIEWMESH_OLLAMA_MODEL)
			// or by a project config; an empty modelArg fails resolution with clear guidance.
			"ollama-local": {
				Provider:       "ollama",
				Runtime:        "local",
				CanonicalModel: "",
				DisplayName:    "Local Ollama model",
				Authoritative:  new(true),
				AdapterDefault: new(true),
				Adapters:       map[string]AdapterModel{"ollama": {ModelArg: ""}},
			},
			// Provider and gateway adapter defaults. The user installs and authenticates each CLI.
			"claude-code-default": {
				Provider: "anthropic", CanonicalModel: "claude-sonnet", DisplayName: "Claude Sonnet",
				Authoritative: new(true), AdapterDefault: new(true), Adapters: map[string]AdapterModel{"claude-code": {ModelArg: "sonnet"}},
			},
			"codex-cli-default": {
				Provider: "openai", CanonicalModel: "gpt-5-codex", DisplayName: "GPT-5 Codex",
				Authoritative: new(true), AdapterDefault: new(true), Adapters: map[string]AdapterModel{"codex-cli": {ModelArg: "gpt-5-codex"}},
			},
			"agy-cli-default": {
				Provider: "google", CanonicalModel: "gemini-3-pro", DisplayName: "Gemini 3 Pro",
				Authoritative: new(true), AdapterDefault: new(true), Adapters: map[string]AdapterModel{"agy-cli": {ModelArg: "gemini-3-pro"}},
			},
			"devin-cli-default": {
				Provider: "devin", CanonicalModel: "devin", DisplayName: "Devin gateway",
				// devin-cli takes a display name plus a separate effort; the resolver renders the
				// final slug (e.g. "claude-opus-4-8-medium") with RenderDevinModelArg.
				Authoritative: new(true), AdapterDefault: new(true), Adapters: map[string]AdapterModel{"devin-cli": {ModelArg: "Claude Opus 4.8", Effort: "medium"}},
			},
			// ACP adapters are user-defined instances, added at load by synthesizeACPInstances.
		},
		Adapters: map[string]Adapter{
			"fake": {ModelIdentity: "self_report"},
			// Shell adapters a profile may select. None is in a default AdapterPreference, so none
			// is chosen automatically.
			"ollama":      {ModelIdentity: "self_report"},
			"devin-cli":   {ModelIdentity: "self_report"},
			"claude-code": {ModelIdentity: "envelope"},
			"codex-cli":   {ModelIdentity: "trace"},
			"agy-cli":     {ModelIdentity: "self_report"},
			"gemini-cli":  {ModelIdentity: "envelope"},
			"cursor-cli":  {ModelIdentity: "self_report"},
		},
		Defaults: Defaults{
			AdapterPreference: []string{"fake"},
			ProfileForAdapter: map[string]string{"*": "default"},
		},
		Surfaces: Surfaces{
			// Write ceilings for the CLI surfaces, not defaults: a run that names no mode is a report
			// on every surface (see Resolve). `ci` writes only when granted allowRemediate. The ACP
			// and MCP servers have no entry (see SurfaceCeiling).
			DefaultModeBySurface: map[string]string{
				"cli": "apply", "ci": "report",
			},
			DegradeWhenModeUnavailable: new(true),
		},
		Review: Review{MaxInnerIterations: new(4), MaxOuterCycles: new(3)},
	}
}

// WithOllamaModel returns a copy of c with tag applied to the `ollama-local` catalog entry's
// canonical model and `ollama` model argument. It is a no-op when tag is empty or the entry is
// absent.
func WithOllamaModel(c Config, tag string) Config {
	if tag == "" {
		return c
	}
	entry, ok := c.ModelCatalog["ollama-local"]
	if !ok {
		return c
	}
	entry.CanonicalModel = tag
	// Copy the maps rather than mutating the shared seed.
	adapters := make(map[string]AdapterModel, len(entry.Adapters)+1)
	maps.Copy(adapters, entry.Adapters)
	am := adapters["ollama"]
	am.ModelArg = tag
	adapters["ollama"] = am
	entry.Adapters = adapters
	cat := make(map[string]CatalogEntry, len(c.ModelCatalog))
	maps.Copy(cat, c.ModelCatalog)
	cat["ollama-local"] = entry
	c.ModelCatalog = cat
	return c
}

// Load returns the shipped seed overlaid with the override file at path, if any. `.yaml` and `.yml`
// parse as YAML, anything else as JSON. A named path that is missing or invalid is an error.
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

// parseConfigBytes strictly parses a config layer into the typed Config. YAML is converted to JSON
// first so the `json` struct tags are the single schema definition.
func parseConfigBytes(path string, b []byte) (Config, error) {
	var override Config
	if store.IsYAML(path) {
		jb, yerr := store.YAMLToJSON(b)
		if yerr != nil {
			return override, fault.Wrap(fault.Config, fmt.Sprintf("parse YAML config %q", path), yerr)
		}
		b = jb
	}
	// Unknown struct fields are rejected; map keys (profile, adapter, catalog and lane names) are free.
	if err := store.DecodeStrict(b, &override); err != nil {
		return override, fault.Wrap(fault.Config, fmt.Sprintf("parse config %q", path), err)
	}
	if err := validateProfileSpelling(override); err != nil {
		return override, fault.Wrap(fault.Config, fmt.Sprintf("parse config %q", path), err)
	}
	return override, nil
}

// validateProfileSpelling refuses a layer whose profile names both `reviewers` and
// `lanes.reviewer`, rather than silently picking one.
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

// validateConfigBytes is the schema validator passed to the domain-free store: it strictly decodes b
// into Config and returns the parse error.
func validateConfigBytes(path string, b []byte) error {
	_, err := parseConfigBytes(path, b)
	return err
}

// ComponentName is review's subdirectory of the shared `.aimesh/` state root.
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

// HomeDir returns the base directory for user config, honoring localstate.HomeEnvVar.
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

// Layers records which config layers were discovered and loaded, for doctor and diagnostics.
type Layers struct {
	UserPath       string
	UserLoaded     bool
	ProjectPath    string
	ProjectLoaded  bool
	ExplicitPath   string
	ExplicitLoaded bool

	// Shared adapter-location layers (`.aimesh/adapters.yaml`), common to review and explore.
	// SharedUser is anchored at AIMESH_HOME; SharedProject at the repository root found by walking up
	// from cwd.
	SharedUserPath       string
	SharedUserLoaded     bool
	SharedProjectPath    string
	SharedProjectLoaded  bool
	SharedProjectHasRoot bool // cwd is inside a repo (a project shared layer can exist)

	// AdapterPathSource maps each adapter with a configured binary path to the layer that supplied it.
	// AdapterPathShadowed lists adapters whose path was overridden by a higher layer. Both are nil
	// when no adapter paths are configured.
	AdapterPathSource   map[string]string
	AdapterPathShadowed []string

	// ACPInstances is the effective set of user-defined ACP adapter instances from the shared layers.
	ACPInstances map[string]adapterlocations.ACPInstance
}

// Adapter-path provenance labels, in precedence order user < project.
const (
	adapterPathSourceUserShared    = "user (.aimesh/adapters.yaml)"
	adapterPathSourceProjectShared = "project (.aimesh/adapters.yaml)"
)

// LoadLayered composes the effective configuration, lowest precedence first: shipped defaults, user
// (~/.aimesh/review), project (<repo-root>/.aimesh/review), explicit (--config).
//
// Adapter binary paths come only from the shared `.aimesh/adapters.yaml` layers (user, then project),
// applied after clearing any path a config layer carried. Missing layers are skipped; a malformed
// layer, or an unreadable explicit path, is an error. Path provenance is recorded in Layers.
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

// clearAdapterPaths drops any adapter Path a config layer carried, so the shared adapters.yaml
// layers are the only source of binary paths.
func clearAdapterPaths(cfg *Config) {
	for name, ad := range cfg.Adapters {
		if ad.Path != "" {
			ad.Path = ""
			cfg.Adapters[name] = ad
		}
	}
}

// synthesizeACPInstances adds each user-defined ACP instance as an adapter (identity cli_status,
// since an ACP session reports its active model) and a `<name>-default` catalog entry a lane can
// reference.
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
		// The expected model is the one the session reported at validation, so identity verification
		// halts only on drift. An unvalidated instance has an empty modelArg, which resolution refuses.
		cfg.ModelCatalog[name+"-default"] = CatalogEntry{
			DisplayName:    title,
			CanonicalModel: inst.Model,
			Authoritative:  new(true),
			AdapterDefault: new(true),
			Adapters:       map[string]AdapterModel{name: {ModelArg: inst.Model}},
		}
	}
}

// applySharedPaths overlays a shared adapters.yaml layer's path entries onto cfg. A nil path is no
// override; an empty path clears an inherited one (use PATH lookup). Entries for adapters cfg does
// not define are skipped, and overriding a lower layer's path records a shadow diagnostic.
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

// LoadRawMap reads a strictly validated config file into a generic map, so a caller can see which
// keys are explicitly present rather than seeded.
func LoadRawMap(path string) (map[string]any, error) {
	return store.LoadRawMap(path, validateConfigBytes)
}

// loadOverride reads and strictly parses one config layer file without merging it.
func loadOverride(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fault.Wrap(fault.Config, fmt.Sprintf("read config %q", path), err)
	}
	return parseConfigBytes(path, b)
}

// merge overlays a higher-precedence layer onto a lower one: maps merge by key, non-zero scalars and
// non-nil lists in over win, and sibling fields are preserved (see docs/configuration.md).
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
	// A capability list replaces the lower layer's list, so a higher layer can revoke a grant by
	// naming an empty list.
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
	// A layer's reviewer spelling replaces the other, so the merged profile never carries both. The
	// panel is replaced wholesale because a seat list is an ordered composition.
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

// ProjectConfigPath returns the canonical YAML config path under base. DiscoverProjectConfig also
// finds `config.yml` and `config.json`.
func ProjectConfigPath(base string) string {
	return filepath.Join(ComponentDir(base), "config.yaml")
}

// WriteYAMLFile writes the config as YAML with the json tag names, creating parent directories.
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

// WriteJSONFile writes the config as indented JSON, creating parent directories. Config holds no
// secrets; authentication stays with each adapter's CLI.
func (c Config) WriteJSONFile(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Internal, "marshal config", err)
	}
	return store.WriteFileAtomic(path, append(b, '\n'))
}

// ApplyPatchToFile applies patch to the config file at path, creating it if absent. Unrelated fields
// are preserved, and the result is written only after it re-validates strictly. An unparseable
// existing file is refused and left unchanged.
func ApplyPatchToFile(path string, patch ConfigPatch) error {
	return store.ApplyPatchToFile(path, patch, validateConfigBytes)
}

func mergeStringMap(b, o map[string]string) map[string]string {
	if o == nil {
		return b
	}
	if b == nil {
		b = map[string]string{}
	}
	maps.Copy(b, o)
	return b
}
