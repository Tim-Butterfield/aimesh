package setup

import (
	"regexp"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
)

// This file holds read-only projections of the resolved config as plain DTOs, so callers can render
// config, profile, adapter and privacy state without importing the config access layer. Mutations go
// through the ConfigPatch write path.

// LayerInfo is one config layer's path + whether it was loaded.
type LayerInfo struct {
	Path   string `json:"path"`
	Loaded bool   `json:"loaded"`
}

// ConfigPaths projects the discovered/loaded config layers.
type ConfigPaths struct {
	User     LayerInfo `json:"user"`
	Project  LayerInfo `json:"project"`
	Explicit LayerInfo `json:"explicit"`
}

// ConfigOverview is the Overview projection.
type ConfigOverview struct {
	DefaultProfile        string `json:"defaultProfile"`
	DefaultProfileDisplay string `json:"defaultProfileDisplay"` // readable name (e.g. "Default")
	// DefaultAdapters are the adapters the profile named `default` uses.
	DefaultAdapters []string `json:"defaultAdapters"`
	// DefaultAdapterDisplays are the readable names for DefaultAdapters, in the same order.
	DefaultAdapterDisplays []string `json:"defaultAdapterDisplays"`
	// LegacyDefaultPointer and LegacyDefaultNote are set when the saved defaultProfile names
	// something other than `default`. Setup never repoints defaultProfile; the note offers a
	// confirmed repair back to `default`.
	LegacyDefaultPointer string      `json:"legacyDefaultPointer,omitempty"`
	LegacyDefaultNote    string      `json:"legacyDefaultNote,omitempty"`
	SchemaVersion        int         `json:"schemaVersion"`
	ProfileCount         int         `json:"profileCount"`
	AdapterCount         int         `json:"adapterCount"`
	Paths                ConfigPaths `json:"paths"`
	// EditableScopes are the config layers mutations may target: always "user", plus "project" when
	// a project config is loaded. DefaultEditScope is project when available, else user. A caller
	// submits its selected scope with each write; a project scope without a project config is blocked.
	EditableScopes   []string `json:"editableScopes"`
	DefaultEditScope string   `json:"defaultEditScope"`
}

// LaneView is one role's lane assignment.
type LaneView struct {
	Role           string `json:"role"`
	Execution      string `json:"execution"`
	Adapter        string `json:"adapter"`        // stable key
	AdapterDisplay string `json:"adapterDisplay"` // readable product name
	Model          string `json:"model"`          // raw catalog key
	ModelLabel     string `json:"modelLabel"`     // readable model label
}

// ProfileView projects a profile (name, description, default flag, lanes).
type ProfileView struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"` // readable name; Name stays the identifier
	Description string `json:"description"` // raw description, the editable value
	// DescriptionDisplay is the description with hyphenated adapter keys shown as readable names. It
	// is display-only; Description is the source of truth.
	DescriptionDisplay string `json:"descriptionDisplay"`
	IsDefault          bool   `json:"isDefault"`
	// FakeOnly reports that every lane uses the built-in `fake` adapter, so no real model call is
	// possible.
	FakeOnly bool `json:"fakeOnly"`
	// ACPBlockedBy names the non-fake lanes as "role=adapter" strings; empty for fake-only profiles.
	ACPBlockedBy []string `json:"acpBlockedBy,omitempty"`
	// Sources lists the config layers that define this profile, in merge order.
	Sources  []string   `json:"sources,omitempty"`
	Adapters []string   `json:"adapters"`
	Lanes    []LaneView `json:"lanes"`
	// Reviewers is the ordered blind primary panel. A single `lanes.reviewer` is normalized to a
	// one-seat panel, so callers handle one shape; Lanes still carries the single-slot roles.
	Reviewers []SeatView `json:"reviewers"`
}

// SeatView is one blind-primary panel seat in a read projection.
type SeatView struct {
	SeatID         string `json:"seatId"`
	Index          int    `json:"index"` // 1-based position in the ordered panel
	Adapter        string `json:"adapter"`
	AdapterDisplay string `json:"adapterDisplay"`
	Model          string `json:"model"`
	ModelLabel     string `json:"modelLabel"`
}

// AdapterUse names a profile+role that references an adapter.
type AdapterUse struct {
	Profile        string `json:"profile"`
	ProfileDisplay string `json:"profileDisplay"` // readable profile name
	Role           string `json:"role"`
}

// AdapterView projects an adapter with the implemented/configured/used distinction.
type AdapterView struct {
	Name          string       `json:"name"`        // stable adapter key
	DisplayName   string       `json:"displayName"` // readable product name
	Provider      string       `json:"provider,omitempty"`
	Implemented   bool         `json:"implemented"` // reviewmesh knows how to drive it (registered)
	Configured    bool         `json:"configured"`  // fake, or a binary path is recorded
	Used          bool         `json:"used"`        // referenced by at least one profile/lane
	UsedBy        []AdapterUse `json:"usedBy"`
	Path          string       `json:"path,omitempty"`
	ModelIdentity string       `json:"modelIdentity,omitempty"`
	SpecOnly      bool         `json:"specOnly"` // untested/spec-only (e.g. gemini-cli), never auto-selected
	// RemovableConfig reports whether the write scope's shared adapters file records a path for this
	// adapter, the only case where removing it changes anything.
	RemovableConfig bool `json:"removableConfig"`
	// IsACP marks a user-defined generic ACP adapter rather than a built-in shell recipe; ACPArgs are
	// the launch args that start its ACP server.
	IsACP   bool     `json:"isAcp"`
	ACPArgs []string `json:"acpArgs,omitempty"`
}

// PrivacyLane projects one lane's trust facts.
type PrivacyLane struct {
	Role           string `json:"role"`
	Adapter        string `json:"adapter"`
	AdapterDisplay string `json:"adapterDisplay"`
	Provider       string `json:"provider"`
	Local          bool   `json:"local"`
	IdentityTier   string `json:"identityTier"`
}

// PrivacyView projects a saved profile's privacy/trust posture (a pure projection).
type PrivacyView struct {
	Profile        string        `json:"profile"`
	ProfileDisplay string        `json:"profileDisplay"` // readable profile name
	Exists         bool          `json:"exists"`
	FullyLocal     bool          `json:"fullyLocal"`
	Providers      []string      `json:"providers"`
	Lanes          []PrivacyLane `json:"lanes"`
	AuditDir       string        `json:"auditDir"`
	WriteNote      string        `json:"writeNote"` // live-write capability is surface/policy-gated at run time
}

// SmokeLane is one lane in the ACP validation plan. All four roles are projected, configured or not,
// in a fixed role order.
type SmokeLane struct {
	Role           string `json:"role"`
	RolePurpose    string `json:"rolePurpose"` // short description of the role
	Configured     bool   `json:"configured"`  // the profile defines this lane
	Status         string `json:"status"`      // configured | not configured (optional) | required — missing | unresolved
	Adapter        string `json:"adapter,omitempty"`
	AdapterDisplay string `json:"adapterDisplay,omitempty"`
	Model          string `json:"model,omitempty"`        // the modelCatalog key the lane references
	ModelLabel     string `json:"modelLabel,omitempty"`   // readable model label
	Source         string `json:"source,omitempty"`       // "adapter default" | "saved model" | "unresolved"
	EffectiveArg   string `json:"effectiveArg,omitempty"` // what the CLI receives (devin: the rendered slug)
	Local          bool   `json:"local"`
	Provider       string `json:"provider,omitempty"`
	// Invocation is the lane's role in the validation run: "invoked", host adjudication for
	// author_remediator, or "skipped".
	Invocation string `json:"invocation"`
	// IdentityEvidence is the tier the lane's adapter can produce, and IdentityNote explains it. Both
	// are descriptive: identity never decides whether a lane runs or its findings are used (see
	// docs/model-identity.md).
	IdentityEvidence string `json:"identityEvidence,omitempty"`
	IdentityNote     string `json:"identityNote,omitempty"`
	// BlocksValidation is true when the lane cannot run (a spec-only adapter or an unresolved lane),
	// so validation must not start. Identity evidence never sets it.
	BlocksValidation bool `json:"blocksValidation,omitempty"`
}

// SmokePlanView summarizes a saved profile's ACP validation plan: each lane's adapter, model, source
// and effective argument, a spend warning, and readiness. It makes no model call and writes nothing.
type SmokePlanView struct {
	Profile        string `json:"profile"`
	ProfileDisplay string `json:"profileDisplay"` // readable profile name
	Exists         bool   `json:"exists"`
	FullyLocal     bool   `json:"fullyLocal"`
	// RequiresSpendConsent is true unless the plan is fully local; callers must obtain spend consent
	// before running.
	RequiresSpendConsent bool        `json:"requiresSpendConsent"`
	Lanes                []SmokeLane `json:"lanes"`
	SpendWarning         string      `json:"spendWarning"`
	// Ready reports whether validation may run: no lane blocks, no required lane is missing, and the
	// plan resolves. Callers must refuse a plan that is not ready.
	Ready         bool     `json:"ready"`
	Blocked       bool     `json:"blocked"`
	BlockingLanes []string `json:"blockingLanes,omitempty"` // roles that block validation
	// BlockMessage is the specific reason validation is blocked.
	BlockMessage string `json:"blockMessage,omitempty"`
}

// specOnlyAdapters lists shell recipes that have never run against their real CLI; such an adapter
// blocks validation. Every shipped recipe is verified (see docs/adapters.md), so it is empty; add a
// new recipe's key here until it is verified.
var specOnlyAdapters = map[string]bool{}

// IsFakeOnlyProfile reports whether every lane of the named profile uses the built-in `fake` adapter,
// so it makes no real model call. This is stricter than fully local, which also admits Ollama.
func (m *Manager) IsFakeOnlyProfile(name string) bool {
	p, ok := m.Cfg.Profiles[name]
	if !ok || len(p.Lanes) == 0 {
		return false
	}
	for _, l := range p.Lanes {
		if l.Adapter != "fake" {
			return false
		}
	}
	return true
}

// profileSources maps each merged profile name to the config layers that define it, in merge order.
// An unreadable layer is omitted.
func (m *Manager) profileSources() map[string][]string {
	out := map[string][]string{}
	add := func(layer string, names map[string]bool) {
		for n := range names {
			out[n] = append(out[n], layer)
		}
	}
	shipped := map[string]bool{}
	for n := range config.Default().Profiles {
		shipped[n] = true
	}
	add("shipped", shipped)
	rawNames := func(path string) map[string]bool {
		names := map[string]bool{}
		if path == "" {
			return names
		}
		raw, err := config.LoadRawMap(path)
		if err != nil {
			return names
		}
		if profs, ok := raw["profiles"].(map[string]any); ok {
			for n := range profs {
				names[n] = true
			}
		}
		return names
	}
	if p, err := m.userConfigTarget(); err == nil {
		add("user", rawNames(p))
	}
	if m.Layers.ProjectLoaded {
		add("project", rawNames(m.Layers.ProjectPath))
	}
	if m.Layers.ExplicitLoaded {
		add("explicit", rawNames(m.Layers.ExplicitPath))
	}
	return out
}

// acpBlockedBy lists a profile's non-fake lanes as "role=adapter", sorted by role.
func (m *Manager) acpBlockedBy(p config.Profile) []string {
	roles := make([]string, 0, len(p.Lanes))
	for r := range p.Lanes {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	var out []string
	for _, r := range roles {
		if a := p.Lanes[r].Adapter; a != "fake" {
			out = append(out, r+"="+a)
		}
	}
	return out
}

// CatalogEntryView is one modelCatalog key and what it resolves to. The key is what `--reviewer
// model=…` and profile lanes name.
type CatalogEntryView struct {
	// Key is what a caller writes. Effort is often embedded in it ("claude-code-opus-medium"), so the
	// resolved effort is reported separately.
	Key string `json:"key"`
	// Provider and CanonicalModel are the entry's own description of what it names.
	Provider       string `json:"provider,omitempty"`
	CanonicalModel string `json:"canonicalModel,omitempty"`
	// Adapters are the adapters the key is reachable through, sorted, each with the model argument and
	// effort actually used.
	Adapters []CatalogAdapterView `json:"adapters"`
	// AdapterDefault marks the entry an adapter falls back to when a lane names no model.
	AdapterDefault bool `json:"adapterDefault,omitempty"`
}

// CatalogAdapterView is one adapter binding of a catalog key.
type CatalogAdapterView struct {
	Adapter string `json:"adapter"`
	// ModelArg is what the adapter's CLI receives, which often differs from the key.
	ModelArg string `json:"modelArg,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

// CatalogViews projects the whole modelCatalog, sorted by key, resolving each entry's per-adapter model
// argument and effort as the lane resolver does.
func (m *Manager) CatalogViews() []CatalogEntryView {
	keys := make([]string, 0, len(m.Cfg.ModelCatalog))
	for k := range m.Cfg.ModelCatalog {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]CatalogEntryView, 0, len(keys))
	for _, k := range keys {
		e := m.Cfg.ModelCatalog[k]
		adapters := make([]string, 0, len(e.Adapters))
		for a := range e.Adapters {
			adapters = append(adapters, a)
		}
		sort.Strings(adapters)
		binds := make([]CatalogAdapterView, 0, len(adapters))
		for _, a := range adapters {
			am := e.Adapters[a]
			effort := am.Effort
			if effort == "" {
				// The entry-level effort is the fallback, exactly as laneModelSources resolves it.
				effort = e.Effort
			}
			binds = append(binds, CatalogAdapterView{Adapter: a, ModelArg: am.ModelArg, Effort: effort})
		}
		out = append(out, CatalogEntryView{
			Key: k, Provider: e.Provider, CanonicalModel: e.CanonicalModel,
			Adapters: binds, AdapterDefault: e.IsAdapterDefault(),
		})
	}
	return out
}

// ProfileViews projects all profiles (sorted by name).
func (m *Manager) ProfileViews() []ProfileView {
	names := make([]string, 0, len(m.Cfg.Profiles))
	for n := range m.Cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	sources := m.profileSources()
	out := make([]ProfileView, 0, len(names))
	for _, name := range names {
		// Hidden test profiles are omitted, as the `fake` adapter is in AdapterViews; they remain
		// selectable by name.
		if config.IsHiddenProfile(name) {
			continue
		}
		p := m.Cfg.Profiles[name]
		roles := make([]string, 0, len(p.Lanes))
		for r := range p.Lanes {
			roles = append(roles, r)
		}
		sort.Strings(roles)
		lanes := make([]LaneView, 0, len(roles))
		for _, r := range roles {
			l := p.Lanes[r]
			lanes = append(lanes, LaneView{Role: r, Execution: l.Execution, Adapter: l.Adapter, AdapterDisplay: AdapterDisplayName(l.Adapter), Model: l.Model, ModelLabel: m.ModelDisplayLabel(l.Model)})
		}
		fakeOnly := m.IsFakeOnlyProfile(name)
		var blocked []string
		if !fakeOnly {
			blocked = m.acpBlockedBy(p)
		}
		// The reviewer panel in authored order, with a single `lanes.reviewer` normalized to one seat.
		seats := p.ReviewerSeats()
		reviewers := make([]SeatView, 0, len(seats))
		for i, l := range seats {
			reviewers = append(reviewers, SeatView{
				SeatID: config.SeatID(i), Index: i + 1,
				Adapter: l.Adapter, AdapterDisplay: AdapterDisplayName(l.Adapter),
				Model: l.Model, ModelLabel: m.ModelDisplayLabel(l.Model),
			})
		}
		out = append(out, ProfileView{
			Name: name, DisplayName: ProfileDisplayName(name), Description: p.Description, DescriptionDisplay: descriptionDisplay(p.Description), IsDefault: name == m.Cfg.DefaultProfile,
			FakeOnly: fakeOnly, ACPBlockedBy: blocked, Sources: sources[name],
			Adapters: append([]string(nil), p.AdapterPreference...), Lanes: lanes,
			Reviewers: reviewers,
		})
	}
	return out
}

// adapterUsage maps each adapter name to the profile roles that reference it.
func (m *Manager) adapterUsage() map[string][]AdapterUse {
	uses := map[string][]AdapterUse{}
	pnames := make([]string, 0, len(m.Cfg.Profiles))
	for n := range m.Cfg.Profiles {
		pnames = append(pnames, n)
	}
	sort.Strings(pnames)
	for _, pn := range pnames {
		p := m.Cfg.Profiles[pn]
		roles := make([]string, 0, len(p.Lanes))
		for r := range p.Lanes {
			roles = append(roles, r)
		}
		sort.Strings(roles)
		for _, r := range roles {
			if a := p.Lanes[r].Adapter; a != "" {
				uses[a] = append(uses[a], AdapterUse{Profile: pn, ProfileDisplay: ProfileDisplayName(pn), Role: r})
			}
		}
	}
	return uses
}

// adapterProvider returns a best-effort provider for an adapter from the model catalog.
func (m *Manager) adapterProvider(name string) string {
	for _, e := range m.Cfg.ModelCatalog {
		if _, ok := e.Adapters[name]; ok {
			return e.Provider
		}
	}
	return ""
}

// adapterDisplayNames maps adapter keys to readable product names. Keys remain the identifiers
// everywhere; these names are labels.
var adapterDisplayNames = map[string]string{
	"codex-cli":   "Codex",
	"claude-code": "Claude Code",
	"agy-cli":     "Antigravity",
	"devin-cli":   "Devin",
	"gemini-cli":  "Gemini",
	"cursor-cli":  "Cursor",
	"ollama":      "Ollama",
	"fake":        "Fake",
}

// viaSuffixPattern matches a trailing "(via <adapter-key>)" in a catalog DisplayName, so no adapter
// key reaches a label.
var viaSuffixPattern = regexp.MustCompile(`\s*\(via [^)]*\)\s*$`)

// ModelDisplayLabel returns a readable label for a catalog key: the entry's DisplayName without any
// "(via …)" suffix, else its CanonicalModel plus effort. For an unknown key that starts with an
// adapter key it strips that prefix; otherwise it returns the key.
func (m *Manager) ModelDisplayLabel(catalogKey string) string {
	if catalogKey == "" {
		return ""
	}
	if e, ok := m.Cfg.ModelCatalog[catalogKey]; ok {
		if d := strings.TrimSpace(viaSuffixPattern.ReplaceAllString(e.DisplayName, "")); d != "" {
			return normalizeModelLabel(d)
		}
		if e.CanonicalModel != "" {
			if e.Effort != "" {
				return normalizeModelLabel(e.CanonicalModel + " " + e.Effort)
			}
			return normalizeModelLabel(e.CanonicalModel)
		}
	}
	// Orphaned/unknown key: drop a leading implemented-adapter-key prefix so no "-cli" is shown.
	// Adapter keys don't overlap as prefixes, so at most one matches.
	for name := range m.Adapters {
		if rest, ok := strings.CutPrefix(catalogKey, name+"-"); ok {
			return normalizeModelLabel(rest)
		}
	}
	return catalogKey
}

var modelEffortWords = map[string]string{"high": "High", "medium": "Medium", "low": "Low", "xhigh": "XHigh", "max": "Max"}

// normalizeModelLabel renders a model label in a consistent product style: drop `( … )`, capitalize
// the effort word, uppercase the `gpt` acronym, and title-case a bare-lowercase leading word — while
// preserving version numbers and internal punctuation (e.g. `gpt-5.5 (high)` → `GPT-5.5 High`,
// `Opus 4.8 (high)` → `Opus 4.8 High`). Display-only formatting; never changes config.
func normalizeModelLabel(s string) string {
	s = strings.NewReplacer("(", "", ")", "").Replace(s)
	out := make([]string, 0, 4)
	for w := range strings.FieldsSeq(s) {
		lw := strings.ToLower(w)
		switch {
		case modelEffortWords[lw] != "":
			out = append(out, modelEffortWords[lw])
		case lw == "gpt":
			out = append(out, "GPT")
		case strings.HasPrefix(lw, "gpt-"):
			out = append(out, "GPT"+w[3:])
		default:
			r := []rune(w)
			if len(r) > 0 && r[0] >= 'a' && r[0] <= 'z' {
				out = append(out, strings.ToUpper(string(r[0]))+string(r[1:]))
			} else {
				out = append(out, w)
			}
		}
	}
	return strings.Join(out, " ")
}

// descKeyRe matches a hyphenated adapter key appearing as a standalone token in a profile description
// ("smoke for codex-cli." but not "codex-cli-compatible"). Keys come from adapterDisplayNames;
// bare-word keys such as `fake` are excluded because they are ordinary words.
var descKeyRe = buildDescKeyRe()

// buildDescKeyRe builds descKeyRe from the hyphenated keys of adapterDisplayNames.
func buildDescKeyRe() *regexp.Regexp {
	alts := make([]string, 0, len(adapterDisplayNames))
	for key := range adapterDisplayNames {
		if strings.Contains(key, "-") {
			alts = append(alts, regexp.QuoteMeta(key))
		}
	}
	sort.Strings(alts) // deterministic pattern
	return regexp.MustCompile(`(?i)(^|[^\w-])(` + strings.Join(alts, "|") + `)($|[^\w-])`)
}

// descriptionDisplay replaces standalone adapter keys in a profile description with product names.
func descriptionDisplay(desc string) string {
	return descKeyRe.ReplaceAllStringFunc(desc, func(m string) string {
		sub := descKeyRe.FindStringSubmatch(m)
		return sub[1] + AdapterDisplayName(strings.ToLower(sub[2])) + sub[3]
	})
}

// AdapterDisplayName returns the readable product name for an adapter key. An unknown key is split on
// hyphens and underscores, a trailing `cli` segment is dropped, and each word is capitalized.
func AdapterDisplayName(key string) string {
	if d, ok := adapterDisplayNames[key]; ok {
		return d
	}
	if key == "" {
		return ""
	}
	words := strings.FieldsFunc(key, func(r rune) bool { return r == '-' || r == '_' })
	words = dropKeySuffix(words)
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// dropKeySuffix removes a trailing `cli` segment from a split adapter key; it distinguishes a terminal
// binary from a desktop app and is not part of the product name. It never removes the only segment.
func dropKeySuffix(words []string) []string {
	if len(words) > 1 && strings.EqualFold(words[len(words)-1], "cli") {
		return words[:len(words)-1]
	}
	return words
}

// adapterDisplayLabel returns an adapter's label: a user-defined ACP instance's title (or
// "ACP: <name>"), else AdapterDisplayName. AdapterViews and LaneOptions both use it, so labels agree.
func (m *Manager) adapterDisplayLabel(name string) string {
	if inst, isACP := m.Layers.ACPInstances[name]; isACP {
		if t := strings.TrimSpace(inst.Title); t != "" {
			return t
		}
		return "ACP: " + name
	}
	return AdapterDisplayName(name)
}

// exampleProfileDisplayNames maps shipped example profile identifiers, which embed adapter keys, to
// readable names.
var exampleProfileDisplayNames = map[string]string{
	"default":                      "Default",          // the built-in profile
	"fake-smoke":                   "Fake (test only)", // the shipped-but-hidden fake profile (config.FakeProfile)
	"codex-cli-smoke":              "Codex smoke",
	"claude-code-smoke":            "Claude Code smoke",
	"agy-cli-smoke":                "Antigravity smoke",
	"devin-cli-smoke":              "Devin smoke",
	"gemini-cli-smoke":             "Gemini smoke",
	"native-three-provider":        "Three-provider",
	"devin-gateway-three-provider": "Devin gateway",
	"fully-local-ollama":           "Fully local (Ollama)",
}

// ProfileDisplayName returns the readable name for a profile identifier: the mapped name for a shipped
// example profile, otherwise the name unchanged.
func ProfileDisplayName(name string) string {
	if d, ok := exampleProfileDisplayNames[name]; ok {
		return d
	}
	return name
}

// scopeSharedPath returns the write scope's shared adapters file for read-only projections: the
// AIMESH_HOME file for user scope, the repository-root file for project scope, or "" when there is
// none. Unlike sharedWriteTarget it never errors.
func (m *Manager) scopeSharedPath() string {
	if m.scope() == writeScopeProject {
		if m.Layers.SharedProjectHasRoot {
			return m.Layers.SharedProjectPath
		}
		return ""
	}
	if m.Layers.SharedUserPath != "" {
		return m.Layers.SharedUserPath
	}
	p, _ := config.SharedUserLocationsPath()
	return p
}

// userLayerAdapters reports which adapters have a saved path in the write scope's shared adapters
// file. An unreadable or absent file contributes nothing.
func (m *Manager) userLayerAdapters() map[string]bool {
	out := map[string]bool{}
	if sp := m.scopeSharedPath(); sp != "" {
		if loc, err := adapterlocations.Load(sp); err == nil {
			for n, e := range loc.Adapters {
				if e.Path != nil {
					out[n] = true
				}
			}
		}
	}
	return out
}

// AdapterViews projects all registered adapters with implemented/configured/used status.
func (m *Manager) AdapterViews() []AdapterView {
	usage := m.adapterUsage()
	userLayer := m.userLayerAdapters()
	names := make([]string, 0, len(m.Cfg.Adapters))
	for n := range m.Cfg.Adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]AdapterView, 0, len(names))
	for _, name := range names {
		// `fake` is a test adapter, so it is not listed; profiles and tests can still name it.
		if name == "fake" {
			continue
		}
		a := m.Cfg.Adapters[name]
		inst, isACP := m.Layers.ACPInstances[name]
		// A user-defined ACP instance is by definition configured; its display name is the instance
		// title and it carries the editable launch args.
		configured := a.Path != "" || isACP
		display := m.adapterDisplayLabel(name)
		_, implemented := m.Adapters[name] // reviewmesh actually knows how to drive it (live registry)
		out = append(out, AdapterView{
			Name: name, DisplayName: display, Provider: m.adapterProvider(name),
			Implemented: implemented, Configured: configured,
			Used: len(usage[name]) > 0, UsedBy: usage[name],
			Path: a.Path, ModelIdentity: a.ModelIdentity,
			SpecOnly:        specOnlyAdapters[name],
			RemovableConfig: name != "fake" && userLayer[name],
			IsACP:           isACP,
			ACPArgs:         inst.Args,
		})
	}
	return out
}

// RoleOption is one review role a lane can be assigned, with its purpose text.
type RoleOption struct {
	Role    string `json:"role"`
	Purpose string `json:"purpose"`
	// Required marks a role a new profile must define; it mirrors CreateProfile's validation.
	Required bool `json:"required"`
}

// AdapterOption is one selectable lane adapter with its model configuration. Config keeps the model
// sources (adapter default, saved models, discovery, manual entry) separate, so an adapter default is
// never presented as a saved model.
type AdapterOption struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Configured  bool   `json:"configured"`
	SpecOnly    bool   `json:"specOnly"`
	// IdentityTier is the adapter's declared identity-evidence mechanism (envelope, trace,
	// self_report, …); it is descriptive only.
	IdentityTier string          `json:"identityTier,omitempty"`
	Config       LaneModelConfig `json:"config"`
}

// LaneOptionsView lists the valid lane roles and adapters, each adapter with its model configuration.
type LaneOptionsView struct {
	Roles    []RoleOption    `json:"roles"`
	Adapters []AdapterOption `json:"adapters"`
}
