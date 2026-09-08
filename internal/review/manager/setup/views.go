package setup

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
)

// This file exposes PURE read projections of the resolved config as plain DTOs (no config
// types leak out), so a Client surface (CLI, web UI) can render config/profile/adapter/privacy
// state through the SetupManager (a Manager) without importing ResourceAccess (`internal/access/*`).
// These are read-only; all *mutations* still go through the ConfigPatch write path.

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
	// DefaultAdapters are the adapters the profile named `default` uses (the workbench's one
	// fixed default profile), so the UI can say "profile `default` uses adapter: fake" without
	// a separate, conflatable "Default Profile" concept.
	DefaultAdapters []string `json:"defaultAdapters"`
	// DefaultAdapterDisplays are the readable product names for DefaultAdapters (same order), so
	// the Overview shows "Fake" rather than the raw key as the primary label.
	DefaultAdapterDisplays []string `json:"defaultAdapterDisplays"`
	// LegacyDefaultPointer + LegacyDefaultNote are set when the SAVED merged config's
	// `defaultProfile` names something other than `default` (e.g. a stale `fake` pointer
	// written by an older build). The workbench never repoints defaultProfile as a feature
	// ("no set as default") — this is surfaced honestly as a legacy/stale posture with a
	// user-confirmed repair (set it back to `default`), never silently rewritten.
	LegacyDefaultPointer string      `json:"legacyDefaultPointer,omitempty"`
	LegacyDefaultNote    string      `json:"legacyDefaultNote,omitempty"`
	SchemaVersion        int         `json:"schemaVersion"`
	ProfileCount         int         `json:"profileCount"`
	AdapterCount         int         `json:"adapterCount"`
	Paths                ConfigPaths `json:"paths"`
	// EditableScopes are the config LAYERS governed mutations may target (Go-owned). "user" is always
	// present; "project" is added when a project config is loaded for this folder. DefaultEditScope is
	// the layer the workbench edits by default (project when a project config exists, else user). The
	// SPA holds the *selected* scope as input state, initialized from DefaultEditScope, and submits it
	// with each write — Go validates/resolves it (a project scope with no project config is blocked).
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
	ModelLabel     string `json:"modelLabel"`     // readable model label (primary UI text)
}

// ProfileView projects a profile (name, description, default flag, lanes).
type ProfileView struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"` // readable name (primary UI label); Name stays the identifier
	Description string `json:"description"` // RAW user description (the value for any edit form / API)
	// DescriptionDisplay is the description with raw `-cli`/`-code` adapter keys mapped to readable
	// names (display-only). Never reuse it as editable config — Description is the source of truth.
	DescriptionDisplay string `json:"descriptionDisplay"`
	IsDefault          bool   `json:"isDefault"`
	// FakeOnly reports that every lane uses the built-in `fake` adapter (no real provider/model
	// call possible). Retained as a governed projection — used by the test-only `/api/acp/validate`
	// protocol check and privacy posture; it is NOT a Profiles-list column (the normal ACP flow
	// runs any saved profile, gated by Doctor + confirmation).
	FakeOnly bool `json:"fakeOnly"`
	// ACPBlockedBy names the lanes that make a profile non-fake-only, as "role=adapter" strings
	// (empty for fake-only profiles). Retained for the test-only fake-only protocol check; the
	// user-facing ACP validation flow does not use it (it accepts any profile).
	ACPBlockedBy []string `json:"acpBlockedBy,omitempty"`
	// Sources lists which config layers define this profile, in merge order (delivered default,
	// user, project, explicit) — a governed projection for layer-aware decisions (e.g. a user key
	// shadowing a lower-layer one). It is NOT rendered as a Profiles-list column.
	Sources  []string   `json:"sources,omitempty"`
	Adapters []string   `json:"adapters"`
	Lanes    []LaneView `json:"lanes"`
	// Reviewers is the ORDERED blind primary panel. It is ALWAYS the panel projection, whichever
	// spelling the config uses: a legacy `lanes.reviewer` is normalized to a single seat here, so
	// the editor renders one control for both and a variable count is the only shape it knows.
	// `lanes` still carries the reviewer role for the single-slot lane views that predate the
	// panel (and for the cross_check / verifier / author_remediator slots, which are roles, not
	// seats) — the panel is the authority for the primary stage.
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
	ProfileDisplay string `json:"profileDisplay"` // readable profile name (primary UI label)
	Role           string `json:"role"`
}

// AdapterView projects an adapter with the implemented/configured/used distinction.
type AdapterView struct {
	Name          string       `json:"name"`        // stable technical key (config/testid/API)
	DisplayName   string       `json:"displayName"` // readable product name (primary UI label)
	Provider      string       `json:"provider,omitempty"`
	Implemented   bool         `json:"implemented"` // reviewmesh knows how to drive it (registered)
	Configured    bool         `json:"configured"`  // fake, or a binary path is recorded
	Used          bool         `json:"used"`        // referenced by at least one profile/lane
	UsedBy        []AdapterUse `json:"usedBy"`
	Path          string       `json:"path,omitempty"`
	ModelIdentity string       `json:"modelIdentity,omitempty"`
	SpecOnly      bool         `json:"specOnly"` // untested/spec-only (e.g. gemini-cli), never auto-selected
	// RemovableConfig reports whether adapters.<name> exists in the USER config layer — the
	// only layer this surface edits, so the only case where Remove has anything to remove.
	// A shipped-only adapter has no user configuration to remove (Remove is disabled with
	// that explanation, instead of a "removal" that visibly changes nothing).
	RemovableConfig bool `json:"removableConfig"`
	// IsACP marks a USER-DEFINED generic ACP adapter instance (vs a code-owned shell recipe). ACPArgs is
	// the launch args that start its ACP server (editable in the UI). The UI renders these instances
	// with an editable path+args and a "Remove ACP" action rather than the shell "Clear saved path".
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
	ProfileDisplay string        `json:"profileDisplay"` // readable profile name (primary UI label)
	Exists         bool          `json:"exists"`
	FullyLocal     bool          `json:"fullyLocal"`
	Providers      []string      `json:"providers"`
	Lanes          []PrivacyLane `json:"lanes"`
	AuditDir       string        `json:"auditDir"`
	WriteNote      string        `json:"writeNote"` // live-write capability is surface/policy-gated at run time
}

// SmokeLane is one lane in the ACP validation plan — projected in the fixed editor role order for ALL
// four roles (configured or not) so the user can match it against the profile editor.
type SmokeLane struct {
	Role           string `json:"role"`
	RolePurpose    string `json:"rolePurpose"` // short Go-owned description of the role
	Configured     bool   `json:"configured"`  // the profile defines this lane
	Status         string `json:"status"`      // configured | not configured (optional) | required — missing | unresolved
	Adapter        string `json:"adapter,omitempty"`
	AdapterDisplay string `json:"adapterDisplay,omitempty"`
	Model          string `json:"model,omitempty"`        // the modelCatalog key the lane references
	ModelLabel     string `json:"modelLabel,omitempty"`   // readable model label (primary UI text)
	Source         string `json:"source,omitempty"`       // "adapter default" | "saved model" | "unresolved"
	EffectiveArg   string `json:"effectiveArg,omitempty"` // what the CLI receives (devin: the rendered slug)
	Local          bool   `json:"local"`
	Provider       string `json:"provider,omitempty"`
	// Invocation is the lane's HONEST role in the ACP validation run: "invoked" (a spawned adapter,
	// identity-verified), "host adjudication" (author_remediator — model-backed only when non-fake and
	// there are findings; self-review is NOT requested for ACP validation), or "skipped".
	Invocation string `json:"invocation"`
	// Identity evidence (Go-owned): the tier this lane's adapter can produce, plus a plain-language note
	// about what that means. Both are DESCRIPTIVE — identity never decides whether a lane may run or
	// whether its findings are used (see ../../../../docs/model-identity.md).
	IdentityEvidence string `json:"identityEvidence,omitempty"`
	IdentityNote     string `json:"identityNote,omitempty"`
	// BlocksValidation is true when this configured lane is genuinely NOT RUNNABLE — a spec-only adapter
	// or an unresolved lane — so ACP validation must NOT run (it would spend then fail). Identity
	// evidence never sets it.
	BlocksValidation bool `json:"blocksValidation,omitempty"`
}

// SmokePlanView is the Go-composed summary the SPA renders in the real-profile ACP smoke
// confirmation modal: the exact profile, its lanes → adapter → model/source/effective argument,
// and an honest spend/compute warning. It performs NO model call and writes nothing.
type SmokePlanView struct {
	Profile        string `json:"profile"`
	ProfileDisplay string `json:"profileDisplay"` // readable profile name (primary UI label)
	Exists         bool   `json:"exists"`
	FullyLocal     bool   `json:"fullyLocal"`
	// RequiresSpendConsent is the BEHAVIOR flag the SPA branches on (Go decides what fullyLocal
	// MEANS for consent, keeping that rule out of Svelte): true → the UI must show the spend
	// confirmation modal before running; false → a fully-local plan may run after the explicit
	// Run click. It fails CLOSED — anything not definitively fully-local requires consent.
	RequiresSpendConsent bool        `json:"requiresSpendConsent"`
	Lanes                []SmokeLane `json:"lanes"`
	SpendWarning         string      `json:"spendWarning"`
	// Ready is the SERVER-AUTHORITATIVE readiness gate: ACP validation may run only when Ready (no lane
	// blocks identity verification, no required lane missing, the plan resolves). The SPA disables Run
	// when !Ready AND the smoke POST handler refuses a blocked plan — a known-unready adapter can never
	// spend tokens before failing.
	Ready         bool     `json:"ready"`
	Blocked       bool     `json:"blocked"`
	BlockingLanes []string `json:"blockingLanes,omitempty"` // roles that block validation
	// BlockMessage is the SPECIFIC, identity-focused reason (never generic install/auth wording).
	BlockMessage string `json:"blockMessage,omitempty"`
}

// SmokePlanView projects a saved profile's lanes for the real-profile ACP smoke confirmation.
// acpLaneOrder is the fixed editor role order the ACP validation plan mirrors (docs/architecture.md),
// so the user can match the plan against the profile editor.
var acpLaneOrder = []review.Role{review.RoleAuthorRemediator, review.RoleReviewer, review.RoleCrossCheck, review.RoleVerifier}

var acpRolePurpose = map[review.Role]string{
	review.RoleAuthorRemediator: "Host / adjudication authority — judges each finding and is the only writer of your workspace.",
	review.RoleReviewer:         "Primary reviewer — iterates to a stable finding set (spawned first).",
	review.RoleCrossCheck:       "Optional second opinion, ideally a different provider; its findings can be applied.",
	review.RoleVerifier:         "Optional final pass; its findings are report-only (never auto-applied).",
}

func acpRoleRequired(r review.Role) bool {
	return r == review.RoleAuthorRemediator || r == review.RoleReviewer
}

// acpInvocation is the lane's HONEST role in an ACP validation run. Self-review is NOT requested for
// ACP validation (the ACP surface builds review.Request without IncludeHostReview). A NON-fake host is
// ALWAYS exercised during ACP validation — by a natural adjudication when the reviewer has findings, or
// a synthetic adjudication readiness probe otherwise — so its adapter/model is proven runnable. A fake
// host adjudicates deterministically (no model call).
func acpInvocation(r review.Role, adapter string) string {
	switch r {
	case review.RoleAuthorRemediator:
		if adapter == "fake" {
			return "host adjudication (deterministic — fake host, not a model call)"
		}
		return "host adjudication (always exercised during ACP validation — natural, or a synthetic readiness probe when the reviewer has no findings)"
	case review.RoleReviewer:
		return "invoked (primary reviewer, spawned first)"
	case review.RoleCrossCheck:
		return "invoked (second opinion)"
	case review.RoleVerifier:
		return "invoked (findings are report-only)"
	}
	return "invoked"
}

// identityEvidenceNote describes, for the reader, what a lane's adapter can prove about which model
// answered. It is purely informational: no note here blocks validation, and no lane's findings are
// weighted by it. Note that cli_status/invocation_tag are captured, verifiable-within-the-local-adapter-
// trust-model evidence — they guard ReviewMesh↔adapter selection drift, not provider attestations.
func identityEvidenceNote(ev review.IdentityEvidence, adapterDisplay string) string {
	switch ev {
	case review.EvidenceEnvelope, review.EvidenceTrace, review.EvidenceCLIStatus, review.EvidenceInvocationTag:
		return ""
	case review.EvidenceSelfReport:
		return fmt.Sprintf("%s can only self-report which model answered — recorded as a weak identity, and its findings are used the same as any other lane's.", adapterDisplay)
	default:
		return fmt.Sprintf("%s does not report which model answered — recorded as an unknown identity, and its findings are used the same as any other lane's.", adapterDisplay)
	}
}

// adapterEvidence reads an adapter's PRODUCED evidence tier via the optional Evidence() interface,
// failing CLOSED (EvidenceNone → blocked) for any adapter that does not report it. It is never derived
// from the config `modelIdentity` metadata (the intended mechanism, not the captured tier).
func (m *Manager) adapterEvidence(name string) review.IdentityEvidence {
	if a, ok := m.Adapters[name]; ok {
		if er, ok := a.(interface {
			Evidence() review.IdentityEvidence
		}); ok {
			return er.Evidence()
		}
	}
	return review.EvidenceNone
}

// specOnlyAdapters are shell recipes that have never been RUN against the real CLI, so nothing about
// their behavior is known and they must not be selected. Unlike an EvidenceNone adapter — which CAN
// run and passes with an identity caveat — a spec-only adapter genuinely blocks validation.
//
// It is EMPTY today: every shipped recipe has been verified running against its real CLI (per-recipe
// status: docs/adapters.md). gemini-cli was the last member and graduated once the `--skip-trust`
// fix made it succeed — keeping the marker would have refused a working adapter. The mechanism is
// retained for the next recipe added from spec alone; add its key here until it is verified.
// (A recipe/adapter `Runnable=false` capability is a recorded future refinement.)
var specOnlyAdapters = map[string]bool{}

// acpSpecOnly reports whether an adapter is spec-only / not-runnable for ACP validation.
func acpSpecOnly(adapter string) bool { return specOnlyAdapters[adapter] }

// SmokePlanView projects the ACP VALIDATION PLAN + readiness for a saved profile: all four roles in
// the editor order, each with its resolved adapter/model/effective-arg, identity-evidence readiness,
// and honest invocation role — plus a SERVER-AUTHORITATIVE block when an invoked lane cannot be
// model-identity verified. It resolves the plan exactly as RunContext does (so readiness reflects the
// actually-invoked lanes) and makes NO model call and writes nothing.
func (m *Manager) SmokePlanView(profile string) (SmokePlanView, bool) {
	p, ok := m.Cfg.Profiles[profile]
	if !ok {
		return SmokePlanView{Profile: profile, ProfileDisplay: ProfileDisplayName(profile), Exists: false}, false
	}
	available := map[string]bool{}
	for name, a := range m.Adapters {
		okA, _ := a.Available()
		available[name] = okA
	}
	resolvedLanes := map[review.Role]review.LaneResolution{}
	if plan, err := m.Cfg.Resolve(config.ResolveRequest{Profile: profile, Mode: review.ModeReport, Surface: "acp", Available: available}); err == nil {
		resolvedLanes = plan.Lanes
	}

	fullyLocal := true
	lanes := make([]SmokeLane, 0, len(acpLaneOrder))
	var blockingLanes []string
	blockMsg := ""
	for _, role := range acpLaneOrder {
		sl := SmokeLane{Role: string(role), RolePurpose: acpRolePurpose[role]}
		rl, inResolved := resolvedLanes[role]
		_, inRaw := p.Lanes[string(role)]
		switch {
		case inResolved:
			adapter := rl.Adapter
			sl.Configured, sl.Status = true, "configured"
			sl.Adapter, sl.AdapterDisplay = adapter, AdapterDisplayName(adapter)
			sl.Model, sl.ModelLabel = rl.Model, m.ModelDisplayLabel(rl.Model)
			sl.EffectiveArg = string(rl.ModelArg) // already resolved (incl. Devin rendering)
			sl.Local = localAdapters[adapter]
			sl.Provider = m.adapterProvider(adapter)
			sl.Invocation = acpInvocation(role, adapter)
			sl.Source = "unresolved"
			if entry, ok := m.Cfg.ModelCatalog[rl.Model]; ok {
				if entry.IsAdapterDefault() {
					sl.Source = "adapter default"
				} else {
					sl.Source = "saved model"
				}
			}
			if !sl.Local {
				fullyLocal = false
			}
			if adapter == "fake" {
				sl.IdentityEvidence = string(review.EvidenceInvocationTag)
			} else if acpSpecOnly(adapter) {
				// A spec-only adapter is NOT runnable → this genuinely blocks validation (it is a
				// runnability blocker, NOT a mere identity caveat).
				sl.IdentityEvidence = string(m.adapterEvidence(adapter))
				sl.BlocksValidation = true
				sl.IdentityNote = fmt.Sprintf("%s is spec-only and not runnable for ACP validation yet.", sl.AdapterDisplay)
				blockingLanes = append(blockingLanes, string(role))
				if blockMsg == "" {
					blockMsg = fmt.Sprintf("%s is spec-only and cannot be run for ACP validation.", sl.AdapterDisplay)
				}
			} else {
				ev := m.adapterEvidence(adapter)
				sl.IdentityEvidence = string(ev)
				sl.IdentityNote = identityEvidenceNote(ev, sl.AdapterDisplay)
			}
		case inRaw:
			raw := p.Lanes[string(role)]
			sl.Configured, sl.Status = true, "unresolved"
			sl.Adapter, sl.AdapterDisplay = raw.Adapter, AdapterDisplayName(raw.Adapter)
			sl.Invocation = acpInvocation(role, raw.Adapter)
			sl.BlocksValidation = true
			blockingLanes = append(blockingLanes, string(role))
			if blockMsg == "" {
				blockMsg = fmt.Sprintf("The %s lane could not be resolved for this profile (adapter unavailable or model unresolved); fix it with `aimesh review doctor --fix` or `aimesh review setup`, then re-check.", role)
			}
		default:
			if acpRoleRequired(role) {
				sl.Status, sl.Invocation = "required — missing", "not configured"
				blockingLanes = append(blockingLanes, string(role))
				if blockMsg == "" {
					blockMsg = fmt.Sprintf("The required %s lane is not configured for this profile.", role)
				}
			} else {
				sl.Status, sl.Invocation = "not configured (optional lane)", "skipped"
			}
		}
		lanes = append(lanes, sl)
	}

	blocked := len(blockingLanes) > 0
	warning := "This runs the profile's real adapters through ACP and may spend tokens/credits."
	if fullyLocal {
		warning = "This runs the profile's real adapters through ACP using local compute (no cloud provider call)."
	}
	return SmokePlanView{
		Profile: profile, ProfileDisplay: ProfileDisplayName(profile), Exists: true, FullyLocal: fullyLocal,
		RequiresSpendConsent: !fullyLocal, // fail-closed: only a definitively-local plan skips consent
		Lanes:                lanes, SpendWarning: warning,
		Ready: !blocked, Blocked: blocked, BlockingLanes: blockingLanes, BlockMessage: blockMsg,
	}, true
}

// localAdapters are adapters that keep review fully on-machine.
var localAdapters = map[string]bool{"fake": true, "ollama": true}

// IsFakeOnlyProfile reports whether every lane of the named profile uses the built-in `fake`
// adapter. Such a profile is deterministic and triggers NO real provider/model call — the web-UI
// ACP validation gates on this (stricter than "fully local", which also admits e.g. Ollama).
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

// visibleProfileCount and configuredAdapterCount are the Overview's counts. They are DERIVED from the
// same projections that build the visible lists, not from raw config maps, so the Overview can never
// disagree with the Profiles/Adapters tabs — a raw len() counted the hidden `fake-smoke` profile and
// the hidden `fake` adapter, so the Overview claimed "2 profiles / 10 adapters" for a config showing
// one profile and nine adapters.
//
// The counts answer "how much have I set up", so an adapter reviewmesh merely KNOWS HOW to drive but
// that has no path recorded is not counted — it is available, not configured.
func (m *Manager) visibleProfileCount() int {
	n := 0
	for name := range m.Cfg.Profiles {
		if !config.IsHiddenProfile(name) {
			n++
		}
	}
	return n
}

func (m *Manager) configuredAdapterCount() int {
	n := 0
	for _, av := range m.AdapterViews() { // AdapterViews already omits the hidden `fake` adapter
		if av.Configured {
			n++
		}
	}
	return n
}

// ConfigOverview projects the resolved config summary.
func (m *Manager) ConfigOverview() ConfigOverview {
	out := ConfigOverview{
		DefaultProfile:        m.Cfg.DefaultProfile,
		DefaultProfileDisplay: ProfileDisplayName(m.Cfg.DefaultProfile),
		SchemaVersion:         m.Cfg.SchemaVersion,
		ProfileCount:          m.visibleProfileCount(),
		AdapterCount:          m.configuredAdapterCount(),
		Paths: ConfigPaths{
			User:     LayerInfo{Path: m.Layers.UserPath, Loaded: m.Layers.UserLoaded},
			Project:  LayerInfo{Path: m.Layers.ProjectPath, Loaded: m.Layers.ProjectLoaded},
			Explicit: LayerInfo{Path: m.Layers.ExplicitPath, Loaded: m.Layers.ExplicitLoaded},
		},
	}
	// Editing scope: user is always editable; project is editable (and the default) when a project
	// config is loaded for this folder. Go owns this decision; the SPA renders/selects.
	out.EditableScopes = []string{writeScopeUser}
	out.DefaultEditScope = writeScopeUser
	if m.Layers.ProjectLoaded && m.Layers.ProjectPath != "" {
		out.EditableScopes = []string{writeScopeProject, writeScopeUser}
		out.DefaultEditScope = writeScopeProject
	}
	// Always non-nil (JSON `[]`, never `null`) so the SPA can treat these as arrays — the shipped
	// `default` profile now ships UNCONFIGURED, so this list is legitimately empty for a fresh install.
	out.DefaultAdapters = []string{}
	out.DefaultAdapterDisplays = []string{}
	if p, ok := m.Cfg.Profiles["default"]; ok {
		set := map[string]bool{}
		for _, l := range p.Lanes {
			if l.Adapter != "" {
				set[l.Adapter] = true
			}
		}
		for a := range set {
			out.DefaultAdapters = append(out.DefaultAdapters, a)
		}
		sort.Strings(out.DefaultAdapters)
		for _, a := range out.DefaultAdapters {
			out.DefaultAdapterDisplays = append(out.DefaultAdapterDisplays, AdapterDisplayName(a))
		}
	}
	if m.Cfg.DefaultProfile != "default" {
		out.LegacyDefaultPointer = m.Cfg.DefaultProfile
		out.LegacyDefaultNote = "Your saved config's `defaultProfile` points at \"" + m.Cfg.DefaultProfile +
			"\" (typically a leftover from an older build). This workbench's one default profile is the profile named `default` — " +
			"runtime commands will keep using \"" + m.Cfg.DefaultProfile + "\" until this is repaired. " +
			"The repair sets `defaultProfile: default` in your user config (confirmed, never automatic); no profile is modified or deleted."
	}
	return out
}

// profileSources maps each merged profile name → the config layers that define it, in
// merge order. Best-effort raw-layer reads (an unreadable layer is simply omitted).
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

// ACPBlockersFor lists the non-fake lanes ("role=adapter") of the named profile (empty when
// fake-only or the profile is absent) — used to name the blocking lanes in the preflight-block
// response of the TEST-ONLY fake-only protocol check (`/api/acp/validate`). The user-facing ACP
// flow accepts any profile and does not use this.
func (m *Manager) ACPBlockersFor(name string) []string {
	p, ok := m.Cfg.Profiles[name]
	if !ok || m.IsFakeOnlyProfile(name) {
		return nil
	}
	return m.acpBlockedBy(p)
}

// acpBlockedBy lists the non-fake lanes ("role=adapter") of a profile (empty for fake-only
// profiles) — consumed by the test-only fake-only protocol check, not the user-facing ACP flow.
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

// CatalogEntryView is one `modelCatalog` key as an inventory reports it: the KEY a caller names, and
// what that key actually resolves to.
//
// The key is the point. It is the exact vocabulary `--reviewer model=…` (and a profile lane) requires,
// and it is not derivable from the adapter list or the profile list — a caller who cannot see it has
// to read the config file by hand to find out what it may say.
type CatalogEntryView struct {
	// Key is what a caller writes. Effort is EMBEDDED in it by convention
	// ("claude-code-opus-medium"), which is why the resolved effort is reported beside it rather than
	// left to be inferred from the string.
	Key string `json:"key"`
	// Provider and CanonicalModel are the entry's own description of what it names.
	Provider       string `json:"provider,omitempty"`
	CanonicalModel string `json:"canonicalModel,omitempty"`
	// Adapters are the adapters this key is reachable through, sorted, each with the MODEL ARGUMENT
	// actually passed to that adapter and the effort that applies. A key bound to one adapter is the
	// ordinary case; the shape is a list because the config permits more.
	Adapters []CatalogAdapterView `json:"adapters"`
	// AdapterDefault marks the entry an adapter falls back to when a lane names no model.
	AdapterDefault bool `json:"adapterDefault,omitempty"`
}

// CatalogAdapterView is one adapter binding of a catalog key.
type CatalogAdapterView struct {
	Adapter string `json:"adapter"`
	// ModelArg is what is actually handed to that adapter's CLI — frequently NOT the key.
	ModelArg string `json:"modelArg,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

// CatalogViews projects the whole `modelCatalog` (sorted by key), resolving each entry's per-adapter
// model argument and effort the same way the lane resolver does. It re-derives nothing a run would
// decide differently.
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
		// The shipped-but-HIDDEN fake profile (config.FakeProfile) is test-only — it never appears in
		// the normal profile list (mirroring how the `fake` ADAPTER is skipped in AdapterViews). It
		// stays selectable by name for tests/golden/the ACP fake-only smoke.
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
		// The BLIND REVIEWER PANEL, in its authored order, with the legacy `lanes.reviewer`
		// normalized to a panel of one — so the editor always renders a panel and never has to
		// know which spelling the file uses.
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

// adapterUsage maps each adapter name → the profiles/roles that reference it.
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

// adapterDisplayNames is the Go-owned map from stable adapter KEYS to readable product DISPLAY
// names. Keys stay canonical everywhere (config, API payloads, testids, secondary UI text); the
// SPA renders these display names as the primary label (it never derives a name from the key).
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

// viaSuffixPattern strips a trailing "(via <adapter-key>)" from a catalog DisplayName so a shipped
// or user DisplayName can never leak an adapter key into the primary UI label.
var viaSuffixPattern = regexp.MustCompile(`\s*\(via [^)]*\)\s*$`)

// ModelDisplayLabel returns a readable label for a model catalog KEY, for the SPA to show instead
// of the raw key (which for a generated entry embeds the adapter key, e.g. "codex-cli-gpt-5.5-high").
// It formats ALREADY-CONFIGURED data only (no invented provider catalog): the catalog entry's
// DisplayName (with any "(via …)" suffix stripped) if set, else CanonicalModel (+ effort); for a
// key absent from the catalog that begins with an implemented adapter key, it strips that prefix so
// no "-cli" remains; otherwise it returns the key unchanged. The raw key stays available separately.
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

// descriptionDisplay replaces a raw `-cli`/`-code` adapter KEY that appears as a STANDALONE TOKEN in
// a profile description with its readable product name (display-only; the raw description stays the
// editable/API value). Token boundaries (`(^|[^\w-]) … ($|[^\w-])`) mean a key embedded in a longer
// identifier (e.g. "codex-cli-compatible") is left untouched — only "Single-adapter smoke for
// codex-cli." → "…for Codex."
// The key set is DERIVED from adapterDisplayNames rather than restated, so a newly shipped adapter
// cannot be silently omitted (cursor-cli was, before this was derived). Only HYPHENATED keys
// participate: a bare-word key (`fake`, `ollama`) is an ordinary English word in prose, and rewriting
// every occurrence of "fake" would edit sentences rather than resolve an adapter reference.
var descKeyRe = buildDescKeyRe()

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

func descriptionDisplay(desc string) string {
	return descKeyRe.ReplaceAllStringFunc(desc, func(m string) string {
		sub := descKeyRe.FindStringSubmatch(m)
		return sub[1] + AdapterDisplayName(strings.ToLower(sub[2])) + sub[3]
	})
}

// AdapterDisplayName returns the readable product name for an adapter key. Unknown keys get a
// deterministic Title-Cased fallback (hyphens/underscores → spaces, each word capitalized) — never
// a blind "text before the hyphen" rule (which would yield e.g. "claude" for claude-code).
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

// dropKeySuffix removes a trailing `cli` segment from a split adapter KEY. The `-cli` in keys like
// `cursor-cli`/`codex-cli` distinguishes the terminal binary from the vendor's desktop app — it is an
// INTERNAL disambiguator and never part of the product name, so it must not reach a user-visible label
// ("Opencode Cli"). Only `cli` is dropped: `code` is a real product word (Claude Code). Never strips the
// only segment, so a hypothetical key of exactly "cli" still renders something.
func dropKeySuffix(words []string) []string {
	if len(words) > 1 && strings.EqualFold(words[len(words)-1], "cli") {
		return words[:len(words)-1]
	}
	return words
}

// adapterDisplayLabel is the ACP-aware readable label for an adapter key: a user-defined ACP instance's
// editable Title (falling back to "ACP: <name>"), else the static AdapterDisplayName. BOTH the Adapters
// tiles and the lane-editor adapter dropdown resolve their label through this, so an ACP adapter reads
// identically in both places — the dropdown never shows a Title-Cased key ("Acp Gemini") while the tile
// shows the instance title ("ACP: Gemini").
func (m *Manager) adapterDisplayLabel(name string) string {
	if inst, isACP := m.Layers.ACPInstances[name]; isACP {
		if t := strings.TrimSpace(inst.Title); t != "" {
			return t
		}
		return "ACP: " + name
	}
	return AdapterDisplayName(name)
}

// exampleProfileDisplayNames maps the shipped EXAMPLE/smoke profile identifiers (whose stable keys
// embed raw adapter keys) to readable product names, so the UI/guidance/docs never show a raw
// adapter key in a profile name. The keys remain stable config/API identifiers.
var exampleProfileDisplayNames = map[string]string{
	"default":                      "Default",          // the built-in profile shows as "Default" in normal UI
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

// ProfileDisplayName returns the readable name for a profile identifier: an explicit mapping for the
// known example/smoke profiles that embed adapter keys, otherwise the name unchanged (a user's own
// profile name is its own display). Never derives a label in the SPA.
func ProfileDisplayName(name string) string {
	if d, ok := exampleProfileDisplayNames[name]; ok {
		return d
	}
	return name
}

// scopeSharedPath is the write-scope's SHARED adapters.yaml path for READ-ONLY projections (no
// blocking): user → AIMESH_HOME's file; project → the root-anchored file when the folder is in a repo,
// else "". Unlike sharedWriteTarget it never errors — a missing scope just yields "".
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

// userLayerAdapters reports which adapter names have a SAVED PATH at the WRITE scope's shared
// adapters.yaml — adapter binary paths live only there, and "Clear saved path" clears exactly that.
// Best-effort: an unreadable/absent layer contributes nothing. It follows the selected write scope so
// the "clear saved path" affordance reflects the layer being edited.
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
		// `fake` is the deterministic test/demo adapter — it is never a user-configurable provider, so
		// it is not shown in the adapters UI (it remains available to profiles/tests by name).
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

// HasProfile reports whether the named profile exists in the resolved config.
func (m *Manager) HasProfile(name string) bool {
	_, ok := m.Cfg.Profiles[name]
	return ok
}

// RoleOption is one review role a lane can be assigned for, with its Go-owned purpose text
// (the SPA renders these; it never hardcodes role semantics).
type RoleOption struct {
	Role    string `json:"role"`
	Purpose string `json:"purpose"`
	// Required marks a role a new profile MUST define (author_remediator + reviewer). Go-owned so
	// the New-profile dialog projects the rule rather than hardcoding role names (it mirrors
	// CreateProfile's validation, the single source of truth).
	Required bool `json:"required"`
}

// AdapterOption is one selectable lane adapter plus its adapter-owned model/effort/discovery
// configuration projection (`Config`), which carries the four distinct model sources
// (adapter default, saved models, discovery, manual entry) — there is no flat "models" list,
// so an adapter default can never be presented as a saved-model preset.
type AdapterOption struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Configured  bool   `json:"configured"`
	SpecOnly    bool   `json:"specOnly"`
	// IdentityTier is the adapter's declared identity-evidence mechanism (envelope | trace |
	// self_report …) — descriptive metadata surfaced so a lane choice can weigh trust.
	IdentityTier string          `json:"identityTier,omitempty"`
	Config       LaneModelConfig `json:"config"`
}

// laneRolePurposes is the canonical role vocabulary (docs/architecture.md) the lane editor
// offers, in pipeline order.
var laneRolePurposes = []RoleOption{
	{Role: "author_remediator", Required: true, Purpose: "The host/adjudicator: judges every finding's validity and is the only writer of the live workspace."},
	{Role: "reviewer", Required: true, Purpose: "The primary reviewer: iterates to stabilization (bounded) and produces the main finding set."},
	{Role: "cross_check", Purpose: "Optional second-opinion pass, once per outer cycle, ideally from a different provider; findings are applyable."},
	{Role: "verifier", Purpose: "Final verification pass after cross-check; its findings are report-only (never applied)."},
}

// LaneOptionsView is the Go-owned projection the lane editor renders: valid roles, valid
// adapters, and per-adapter valid models from the model catalog. The SPA submits choices
// from this view; it never hardcodes roles/adapters/models.
type LaneOptionsView struct {
	Roles    []RoleOption    `json:"roles"`
	Adapters []AdapterOption `json:"adapters"`
}

// LaneOptions projects the valid lane choices: every implemented, enabled adapter, each with
// the catalog models that declare support for it and its adapter-owned model/effort
// configuration (discoveries carries the host's cached on-demand discovery results, keyed
// by adapter; nil entries mean not run). Pure read; no decision beyond projection.
func (m *Manager) LaneOptions(discoveries map[string]*DiscoveryResult) LaneOptionsView {
	names := make([]string, 0, len(m.Cfg.Adapters))
	for n, a := range m.Cfg.Adapters {
		if _, implemented := m.Adapters[n]; !implemented || !a.IsEnabled() {
			continue
		}
		// The built-in `fake` adapter never appears in the UI — it is a deterministic test fixture,
		// not a provider a user assigns to a lane. It stays resolvable BY NAME for the hidden
		// `fake-smoke` profile and tests; a saved profile that names it still renders honestly.
		if n == "fake" {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	adapters := make([]AdapterOption, 0, len(names))
	for _, name := range names {
		a := m.Cfg.Adapters[name]
		adapters = append(adapters, AdapterOption{
			Name: name, DisplayName: m.adapterDisplayLabel(name), Configured: a.Path != "",
			SpecOnly: specOnlyAdapters[name], IdentityTier: a.ModelIdentity,
			Config: m.laneModelConfig(name, discoveries[name]),
		})
	}
	return LaneOptionsView{Roles: append([]RoleOption(nil), laneRolePurposes...), Adapters: adapters}
}

// PrivacyView projects a saved profile's privacy/trust posture (ok=false if the profile is absent).
func (m *Manager) PrivacyView(profile string) (PrivacyView, bool) {
	p, ok := m.Cfg.Profiles[profile]
	if !ok {
		return PrivacyView{Profile: profile, ProfileDisplay: ProfileDisplayName(profile), Exists: false}, false
	}
	roles := make([]string, 0, len(p.Lanes))
	for r := range p.Lanes {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	fullyLocal := true
	provSet := map[string]bool{}
	lanes := make([]PrivacyLane, 0, len(roles))
	for _, r := range roles {
		adapter := p.Lanes[r].Adapter
		local := localAdapters[adapter]
		if !local {
			fullyLocal = false
		}
		prov := m.adapterProvider(adapter)
		if prov != "" && !local {
			provSet[prov] = true
		}
		tier := ""
		if a, ok := m.Cfg.Adapters[adapter]; ok {
			tier = a.ModelIdentity
		}
		lanes = append(lanes, PrivacyLane{Role: r, Adapter: adapter, AdapterDisplay: AdapterDisplayName(adapter), Provider: prov, Local: local, IdentityTier: tier})
	}
	providers := make([]string, 0, len(provSet))
	for pr := range provSet {
		providers = append(providers, pr)
	}
	sort.Strings(providers)
	auditDir := m.ArtifactDir
	if auditDir == "" {
		auditDir = "tmp/reviewmesh"
	}
	return PrivacyView{
		Profile: profile, ProfileDisplay: ProfileDisplayName(profile), Exists: true, FullyLocal: fullyLocal, Providers: providers, Lanes: lanes,
		AuditDir: auditDir,
		WriteNote: "Live writes (apply mode) are gated by the surface capability + policy at run time; " +
			"reviewers stay read-only and only the host writes.",
	}, true
}
