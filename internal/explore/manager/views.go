package manager

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"

	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// This file exposes PURE read projections of the current roster + resolved adapter config as plain DTOs
// (no roster/config types leak out), so a Client surface (CLI, web UI) can render config/roster/adapter/
// privacy state through the Manager. These are read-only; all mutations go through the write seams.

// LayerDTO is one config layer's path + whether it is loaded (the file exists on disk).
type LayerDTO struct {
	Label  string `json:"label"`
	Path   string `json:"path"`
	Loaded bool   `json:"loaded"`
}

// OverviewDTO is the Overview projection: where the roster + shared adapters live, how many adapters are
// configured, the current config generation, and the config layers.
type OverviewDTO struct {
	RosterPath         string     `json:"rosterPath"`
	RosterScope        string     `json:"rosterScope"` // "project" (cwd inside a repo) | "user"
	HomePath           string     `json:"homePath"`
	SharedAdaptersPath string     `json:"sharedAdaptersPath"`
	SharedAdaptersNote string     `json:"sharedAdaptersNote"`
	AdapterCount       int        `json:"adapterCount"`
	ConfigGeneration   int        `json:"configGeneration"`
	Layers             []LayerDTO `json:"layers"`
}

// ExplorerViewDTO projects one explorer slot for the roster editor.
type ExplorerViewDTO struct {
	Index          int    `json:"index"`
	Adapter        string `json:"adapter"`
	AdapterDisplay string `json:"adapterDisplay"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
}

// CollatorViewDTO projects the single collator slot.
type CollatorViewDTO struct {
	Adapter        string `json:"adapter"`
	AdapterDisplay string `json:"adapterDisplay"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
}

// RosterViewDTO projects the full roster (ordered explorers + the collator + any explicit canonicalizers).
type RosterViewDTO struct {
	Explorers []ExplorerViewDTO `json:"explorers"`
	Collator  CollatorViewDTO   `json:"collator"`
	// Canonicalizers is EITHER empty (the host derives them at run time) or exactly two. It is projected
	// because canonicalization is a distinct governed role: which identities hold it decides which merges
	// hold versus contest, and therefore every corroboration count.
	Canonicalizers []ExplorerViewDTO `json:"canonicalizers"`
}

// AdapterUseDTO names one roster slot that references an adapter — ref is the stable slot key
// (e.g. "explorer-0", "collator"), role is the readable slot label (e.g. "explorer 1", "collator").
type AdapterUseDTO struct {
	Ref  string `json:"ref"`
	Role string `json:"role"`
}

// AdapterViewDTO mirrors reviewmesh's AdapterView json shape EXACTLY (field-for-field json tags) so the
// shared web/shared AdapterCard renders an exploremesh adapter with no per-app branching. The fields'
// meaning is adapted to exploremesh's roster grammar (usedBy is over roster slots, not profile lanes).
type AdapterViewDTO struct {
	Name            string          `json:"name"`        // stable technical key (config/testid/API)
	DisplayName     string          `json:"displayName"` // readable product name (primary UI label)
	Provider        string          `json:"provider,omitempty"`
	Implemented     bool            `json:"implemented"` // a known shell recipe, a configured ACP instance, or `fake`
	Configured      bool            `json:"configured"`  // `fake`, or a path is recorded, or it's an ACP instance
	Used            bool            `json:"used"`        // referenced by at least one roster slot
	UsedBy          []AdapterUseDTO `json:"usedBy"`
	Path            string          `json:"path,omitempty"`
	ModelIdentity   string          `json:"modelIdentity,omitempty"`
	SpecOnly        bool            `json:"specOnly"` // untested/spec-only (e.g. gemini-cli)
	RemovableConfig bool            `json:"removableConfig"`
	IsACP           bool            `json:"isAcp"`
	ACPArgs         []string        `json:"acpArgs,omitempty"`
}

// TrustDTO projects one slot's privacy/trust facts.
type TrustDTO struct {
	Adapter        string `json:"adapter"`
	AdapterDisplay string `json:"adapterDisplay"`
	Provider       string `json:"provider"`
	Local          bool   `json:"local"`
	Note           string `json:"note,omitempty"`
}

// PrivacyDTO projects the roster's privacy posture: whether every slot runs locally, plus per-slot trust.
type PrivacyDTO struct {
	FullyLocal bool       `json:"fullyLocal"`
	Explorers  []TrustDTO `json:"explorers"`
	Collator   TrustDTO   `json:"collator"`
}

// adapterDisplayNames maps stable adapter KEYS to readable product DISPLAY names. Keys stay canonical
// everywhere (config, API payloads, testids); this is the primary UI label. An unknown key falls back to
// a Title-Cased rendering (see adapterDisplay).
var adapterDisplayNames = map[string]string{
	"codex-cli":          "Codex",
	"claude-code":        "Claude Code",
	"agy-cli":            "Antigravity",
	"devin-cli":          "Devin",
	"gemini-cli":         "Gemini",
	"cursor-cli":         "Cursor",
	"ollama":             "Ollama",
	registry.FakeAdapter: "Fake",
}

// adapterProviders is a best-effort provider label per shell recipe (honest; empty where unclear). It is
// display metadata only — the privacy classification below owns the local/cloud decision.
var adapterProviders = map[string]string{
	"codex-cli":   "OpenAI",
	"claude-code": "Anthropic",
	"gemini-cli":  "Google",
	"devin-cli":   "Cognition",
	"cursor-cli":  "Cursor",
}

// localAdapters are the adapters that keep an exploration fully on-machine (no cloud provider call):
// the deterministic `fake` and the local `ollama` runtime. Every CLI adapter reaches a cloud provider.
var localAdapters = map[string]bool{registry.FakeAdapter: true, "ollama": true}

// specOnlyAdapters are shell recipes that have never been RUN against the real CLI, so nothing about
// their behavior is known and they must not be selected. This is a stronger claim than "identity
// uncaptured": an EvidenceNone adapter (agy-cli, cursor-cli, devin-cli, gemini-cli) runs fine and
// passes with an identity caveat, whereas a spec-only adapter is refused outright.
//
// It is EMPTY today: every shipped recipe has been verified running against its real CLI (per-recipe
// status: docs/adapters.md). gemini-cli was the last member and graduated once the `--skip-trust`
// fix made it succeed — keeping the marker would have refused a working adapter. The mechanism is
// retained for the next recipe added from spec alone; add its key here until it is verified.
var specOnlyAdapters = map[string]bool{}

// adapterDisplay returns the readable label for an adapter key: a configured ACP instance's title (or
// "ACP: <name>"), else the display-name map, else a deterministic Title-Cased fallback.
func (m *Manager) adapterDisplay(name string) string {
	if _, ok := m.acpInstances[name]; ok {
		// The resolved launch Instance carries no title; read the friendly title from config.
		if t := m.acpTitle(name); t != "" {
			return t
		}
		return "ACP: " + name
	}
	if d, ok := adapterDisplayNames[name]; ok {
		return d
	}
	return titleCaseKey(name)
}

// acpTitle reads a configured ACP instance's friendly title from the shared adapters.yaml (the launch
// Instance the registry resolves does not carry it). Best-effort ("" when absent/unreadable).
func (m *Manager) acpTitle(name string) string {
	loc, err := adapterlocations.Load(m.sharedPath)
	if err != nil {
		return ""
	}
	if inst, ok := loc.ACPAdapters[name]; ok {
		return strings.TrimSpace(inst.Title)
	}
	return ""
}

// titleCaseKey renders an unknown adapter key as a readable label (hyphens/underscores → spaces, each
// word capitalized) — never a blind "text before the hyphen" rule.
//
// A trailing `cli` segment is DROPPED: the `-cli` in keys like `cursor-cli`/`codex-cli` distinguishes the
// terminal binary from the vendor's desktop app, so it is an INTERNAL disambiguator and never part of the
// product name — it must not reach a user-visible label ("Opencode Cli"). Only `cli` is dropped; `code` is
// a real product word (Claude Code). The only segment is never stripped.
func titleCaseKey(key string) string {
	if key == "" {
		return ""
	}
	words := strings.FieldsFunc(key, func(r rune) bool { return r == '-' || r == '_' })
	if len(words) > 1 && strings.EqualFold(words[len(words)-1], "cli") {
		words = words[:len(words)-1]
	}
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// Overview projects where the roster + shared adapters live, the configured-adapter count, the config
// generation, and the config layers (roster file + user/project shared adapters.yaml).
func (m *Manager) Overview() OverviewDTO {
	m.mu.Lock()
	defer m.mu.Unlock()

	scope := "user"
	if _, ok := roster.ProjectComponentDir(m.cwd); ok {
		scope = "project"
	}
	homePath := ""
	if dir, err := roster.UserComponentDir(); err == nil {
		homePath = dir
	}

	layers := []LayerDTO{{Label: "roster", Path: m.savePath, Loaded: fileExists(m.savePath)}}
	if up, err := adapterlocations.UserLocationsPath(); err == nil {
		layers = append(layers, LayerDTO{Label: "user shared adapters", Path: up, Loaded: fileExists(up)})
	}
	if pp, ok := adapterlocations.ProjectLocationsPath(m.cwd); ok {
		layers = append(layers, LayerDTO{Label: "project shared adapters", Path: pp, Loaded: fileExists(pp)})
	} else {
		layers = append(layers, LayerDTO{Label: "project shared adapters", Path: "", Loaded: false})
	}

	return OverviewDTO{
		RosterPath:         m.savePath,
		RosterScope:        scope,
		HomePath:           homePath,
		SharedAdaptersPath: m.sharedPath,
		SharedAdaptersNote: "Adapter binary paths + ACP instances are shared across aimesh apps (written to the user-scope .aimesh/adapters.yaml).",
		AdapterCount:       m.configuredAdapterCountLocked(),
		ConfigGeneration:   m.generation,
		Layers:             layers,
	}
}

// configuredAdapterCountLocked is the Overview's adapter count: the adapters the user has actually SET
// UP. A shell recipe exploremesh knows how to drive but that has no recorded path is available, not
// configured, and is not counted; the built-in `fake` is not counted either, since it is a deterministic
// test fixture rather than a provider the user configured (this matches reviewmesh, which also omits it).
//
// Caller must hold m.mu (Overview does).
func (m *Manager) configuredAdapterCountLocked() int {
	n := 0
	for name := range m.adapterPaths {
		if name != registry.FakeAdapter {
			n++
		}
	}
	for name := range m.acpInstances {
		if _, alsoShell := m.adapterPaths[name]; !alsoShell {
			n++ // an ACP instance is configured by definition; never double-count one
		}
	}
	return n
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// RosterView projects the ordered explorers + the collator for the roster editor.
func (m *Manager) RosterView() RosterViewDTO {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := RosterViewDTO{
		Explorers:      make([]ExplorerViewDTO, 0, len(m.roster.Explorers)),
		Canonicalizers: make([]ExplorerViewDTO, 0, len(m.roster.Canonicalizers)),
	}
	for i, e := range m.roster.Explorers {
		out.Explorers = append(out.Explorers, ExplorerViewDTO{
			Index: i, Adapter: e.Adapter, AdapterDisplay: m.adapterDisplay(e.Adapter), Model: e.Model, Effort: e.Effort,
		})
	}
	for i, cz := range m.roster.Canonicalizers {
		out.Canonicalizers = append(out.Canonicalizers, ExplorerViewDTO{
			Index: i, Adapter: cz.Adapter, AdapterDisplay: m.adapterDisplay(cz.Adapter), Model: cz.Model, Effort: cz.Effort,
		})
	}
	c := m.roster.Collator
	out.Collator = CollatorViewDTO{
		Adapter: c.Adapter, AdapterDisplay: m.adapterDisplay(c.Adapter), Model: c.Model, Effort: c.Effort,
	}
	return out
}

// adapterUsageLocked maps each adapter name → the roster slots that reference it (explorers first, in
// order, then the collator). Caller holds the mutex.
func (m *Manager) adapterUsageLocked() map[string][]AdapterUseDTO {
	uses := map[string][]AdapterUseDTO{}
	for i, e := range m.roster.Explorers {
		if e.Adapter != "" {
			uses[e.Adapter] = append(uses[e.Adapter], AdapterUseDTO{Ref: fmt.Sprintf("explorer-%d", i), Role: fmt.Sprintf("explorer %d", i+1)})
		}
	}
	if c := m.roster.Collator.Adapter; c != "" {
		uses[c] = append(uses[c], AdapterUseDTO{Ref: "collator", Role: "collator"})
	}
	// A canonicalizer is a real seat that makes real calls, so clearing its adapter's path must be refused
	// exactly as it is for an explorer or the collator — otherwise an adapter used ONLY to canonicalize
	// looks unused and can be removed out from under a governance rule.
	for i, cz := range m.roster.Canonicalizers {
		if cz.Adapter != "" {
			uses[cz.Adapter] = append(uses[cz.Adapter], AdapterUseDTO{
				Ref: fmt.Sprintf("canonicalizer-%d", i), Role: fmt.Sprintf("canonicalizer %s", string(rune('a'+i))),
			})
		}
	}
	return uses
}

// userLayerAdaptersLocked reports which shell adapters have a saved path in the USER shared adapters.yaml
// (the layer this workbench edits) — so removableConfig reflects the only case where Remove has anything
// to clear. Best-effort: an unreadable/absent file contributes nothing.
func (m *Manager) userLayerAdaptersLocked() map[string]bool {
	out := map[string]bool{}
	if loc, err := adapterlocations.Load(m.sharedPath); err == nil {
		for n, e := range loc.Adapters {
			if e.Path != nil {
				out[n] = true
			}
		}
	}
	return out
}

// AdapterViews projects every adapter the WORKBENCH configures — the shell recipes exploremesh
// supports and the configured ACP instances — each with the implemented/configured/used distinction
// and the shared AdapterCard's exact field shape. The list is sorted by name.
//
// The built-in `fake` adapter is deliberately ABSENT: it is a deterministic test fixture, not a
// provider a user configures, so it never appears in the UI (no tile, no profile-editor choice) —
// matching reviewmesh. It still resolves BY NAME for a profile written outside the UI (tests, the
// golden run), and a roster that names it still renders honestly in the Profiles/Privacy read views.
func (m *Manager) AdapterViews() []AdapterViewDTO {
	m.mu.Lock()
	defer m.mu.Unlock()

	usage := m.adapterUsageLocked()
	userLayer := m.userLayerAdaptersLocked()
	recipes := shell.Recipes()

	// Collect the union of names: shell recipes + configured ACP instances.
	nameSet := map[string]bool{}
	for n := range recipes {
		nameSet[n] = true
	}
	for n := range m.acpInstances {
		nameSet[n] = true
	}
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]AdapterViewDTO, 0, len(names))
	for _, name := range names {
		inst, isACP := m.acpInstances[name]
		recipe, isRecipe := recipes[name]
		isFake := name == registry.FakeAdapter

		v := AdapterViewDTO{
			Name:            name,
			DisplayName:     m.adapterDisplay(name),
			Provider:        adapterProviders[name],
			Implemented:     isRecipe || isACP || isFake,
			Used:            len(usage[name]) > 0,
			UsedBy:          usage[name],
			SpecOnly:        specOnlyAdapters[name],
			IsACP:           isACP,
			RemovableConfig: !isFake && (userLayer[name] || isACP),
		}
		switch {
		case isFake:
			v.Configured = true
		case isACP:
			v.Configured = true
			v.Path = inst.Path
			v.ACPArgs = inst.Args
			v.ModelIdentity = string(acpEvidence)
		case isRecipe:
			p, hasPath := m.adapterPaths[name]
			v.Configured = hasPath
			v.Path = p
			v.ModelIdentity = string(recipe.Identity)
		}
		out = append(out, v)
	}
	return out
}

// acpEvidence is the identity-evidence ceiling an ACP session reaches (it can authoritatively report its
// active model) — surfaced as an ACP adapter's modelIdentity tier.
const acpEvidence = "cli_status"

// PrivacyView projects the roster's privacy posture: whether every slot runs locally, plus per-slot
// trust facts. The classification is deliberately simple + honest: `fake`/`ollama` are local; every CLI
// adapter reaches a cloud provider and is not local.
func (m *Manager) PrivacyView() PrivacyDTO {
	m.mu.Lock()
	defer m.mu.Unlock()

	fullyLocal := true
	trust := func(adapter, note string) TrustDTO {
		local := localAdapters[adapter]
		if !local {
			fullyLocal = false
		}
		prov := adapterProviders[adapter]
		if local && prov == "" {
			prov = "local"
		}
		return TrustDTO{Adapter: adapter, AdapterDisplay: m.adapterDisplay(adapter), Provider: prov, Local: local, Note: note}
	}

	out := PrivacyDTO{Explorers: make([]TrustDTO, 0, len(m.roster.Explorers))}
	for _, e := range m.roster.Explorers {
		out.Explorers = append(out.Explorers, trust(e.Adapter, ""))
	}
	out.Collator = trust(m.roster.Collator.Adapter, "The collator bookends the run — it formulates the panel task and synthesizes every explorer response.")
	out.FullyLocal = fullyLocal
	return out
}
