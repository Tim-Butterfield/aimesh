package manager

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"

	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
)

// This file holds read-only views of the roster and adapter configuration, returned as plain DTOs.

// ExplorerViewDTO describes one explorer slot.
type ExplorerViewDTO struct {
	Index          int    `json:"index"`
	Adapter        string `json:"adapter"`
	AdapterDisplay string `json:"adapterDisplay"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
}

// CollatorViewDTO describes the collator slot.
type CollatorViewDTO struct {
	Adapter        string `json:"adapter"`
	AdapterDisplay string `json:"adapterDisplay"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
}

// RosterViewDTO describes the default profile's roster.
type RosterViewDTO struct {
	Explorers []ExplorerViewDTO `json:"explorers"`
	Collator  CollatorViewDTO   `json:"collator"`
	// Canonicalizers is empty, meaning the host derives them, or exactly two.
	Canonicalizers []ExplorerViewDTO `json:"canonicalizers"`
}

// AdapterUseDTO names a roster slot that uses an adapter: Ref is the slot key (such as "explorer-0") and
// Role its label (such as "explorer 1").
type AdapterUseDTO struct {
	Ref  string `json:"ref"`
	Role string `json:"role"`
}

// AdapterViewDTO describes one adapter. Its JSON shape matches the review domain's adapter view.
type AdapterViewDTO struct {
	Name            string          `json:"name"`        // stable key
	DisplayName     string          `json:"displayName"` // product name for display
	Provider        string          `json:"provider,omitempty"`
	Implemented     bool            `json:"implemented"` // a shell recipe, a configured ACP instance, or `fake`
	Configured      bool            `json:"configured"`  // `fake`, a recorded path, or an ACP instance
	Used            bool            `json:"used"`        // referenced by at least one roster slot
	UsedBy          []AdapterUseDTO `json:"usedBy"`
	Path            string          `json:"path,omitempty"`
	ModelIdentity   string          `json:"modelIdentity,omitempty"`
	SpecOnly        bool            `json:"specOnly"` // never verified against the real CLI
	RemovableConfig bool            `json:"removableConfig"`
	IsACP           bool            `json:"isAcp"`
	ACPArgs         []string        `json:"acpArgs,omitempty"`
}

// adapterDisplayNames maps adapter keys to product display names.
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

// adapterProviders maps shell recipes to provider names, for display only.
var adapterProviders = map[string]string{
	"codex-cli":   "OpenAI",
	"claude-code": "Anthropic",
	"gemini-cli":  "Google",
	"devin-cli":   "Cognition",
	"cursor-cli":  "Cursor",
}

// specOnlyAdapters lists shell recipes not yet verified against their real CLI; they are refused. Every
// shipped recipe is verified (see docs/adapters.md), so the set is empty. Add a new unverified recipe here.
var specOnlyAdapters = map[string]bool{}

// adapterDisplay returns an adapter's label: an ACP instance's configured title (or "ACP: <name>"), a
// known display name, or a title-cased form of the key.
func (m *Manager) adapterDisplay(name string) string {
	if _, ok := m.acpInstances[name]; ok {
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

// acpTitle returns an ACP instance's title from the shared adapters.yaml, or "" if unavailable.
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

// titleCaseKey turns an adapter key into a label: hyphens and underscores become spaces and each word is
// capitalized. A trailing `cli` word is dropped unless it is the only word, since it is not part of the
// product name.
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

// RosterView returns the default profile's roster.
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

// adapterUsageLocked maps each adapter name to the roster slots that use it: explorers in order, the
// collator, then canonicalizers. The caller holds m.mu.
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
	// Canonicalizers count as uses, so an adapter used only for canonicalization cannot be removed.
	for i, cz := range m.roster.Canonicalizers {
		if cz.Adapter != "" {
			uses[cz.Adapter] = append(uses[cz.Adapter], AdapterUseDTO{
				Ref: fmt.Sprintf("canonicalizer-%d", i), Role: fmt.Sprintf("canonicalizer %s", string(rune('a'+i))),
			})
		}
	}
	return uses
}

// userLayerAdaptersLocked returns the adapters with a saved path in the user-scope adapters.yaml, the only
// layer removal can clear. An unreadable file yields none.
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

// AdapterViews returns every shell recipe and configured ACP instance, sorted by name. The `fake` test
// adapter is not listed.
func (m *Manager) AdapterViews() []AdapterViewDTO {
	m.mu.Lock()
	defer m.mu.Unlock()

	usage := m.adapterUsageLocked()
	userLayer := m.userLayerAdaptersLocked()
	recipes := shell.Recipes()

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

// acpEvidence is the identity-evidence tier reported for ACP adapters, which can report their active
// model.
const acpEvidence = "cli_status"
