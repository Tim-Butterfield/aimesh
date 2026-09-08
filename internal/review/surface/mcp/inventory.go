package mcp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the SANITIZED configuration projection behind `review_list` and `review_doctor`.
//
// The sanitization is the point, not an afterthought. A tool result is inference input for a third
// party: whatever these tools return is very likely to end up in someone else's model provider's
// logs. Adapter IDENTIFIERS, selectable models, profile shapes and readiness booleans are what a
// caller needs in order to compose a valid run. Binary paths, launch arguments, trusted-root paths
// and environment detail are what an attacker (or an inattentive log retention policy) needs in
// order to map the operator's machine — and they buy the caller nothing, because it cannot
// configure anything anyway.
//
// The projection types below carry NO path field at all. reviewmesh's own CLI `list --json` DOES
// report adapter paths (a human on the machine asked); this surface deliberately re-projects rather
// than reusing that view, because a rule enforced by the type system cannot be forgotten by the
// next person who adds a field.

// AdapterFact is one configured adapter, projected to logical facts only.
type AdapterFact struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// Kind distinguishes a code-owned CLI recipe from a user-defined ACP instance (and, under the
	// internal test gate only, the hidden fake).
	Kind       string `json:"kind"`
	Configured bool   `json:"configured"`
	// IdentityEvidenceCapability is the adapter's DECLARED evidence tier, not a live verdict —
	// proving a model's identity still takes a real call.
	IdentityEvidenceCapability string `json:"identityEvidenceCapability,omitempty"`
	SpecOnly                   bool   `json:"specOnly,omitempty"`
}

// SeatFact is one configured seat of a profile's blind panel (or one of its single-slot lanes).
type SeatFact struct {
	Role    string `json:"role,omitempty"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// ProfileFact is one configured profile: the ordered blind panel plus the single-slot lanes. It is
// what a caller compares against when deciding whether to select a profile or compose a panel.
type ProfileFact struct {
	Name      string     `json:"name"`
	IsDefault bool       `json:"isDefault"`
	Reviewers []SeatFact `json:"reviewers"`
	Lanes     []SeatFact `json:"lanes"`
}

// CatalogFact is one `modelCatalog` key: the exact token a caller may put in `panel[].model`, and
// what it resolves to.
//
// It is projected here for the same reason the profiles are: a caller composing a panel needs the
// vocabulary the composition will be validated against, and without it the only way to learn a valid
// value is to read the operator's config file — which a peer process cannot and should not do. Every
// field is a logical identifier the operator chose; there is no path, argument or environment detail
// in any of them.
type CatalogFact struct {
	Key            string        `json:"key"`
	Provider       string        `json:"provider,omitempty"`
	CanonicalModel string        `json:"canonicalModel,omitempty"`
	Adapters       []CatalogBind `json:"adapters"`
	AdapterDefault bool          `json:"adapterDefault,omitempty"`
}

// CatalogBind is one adapter a catalog key is reachable through. `modelArg` is what that adapter's
// CLI is actually given, which is frequently not the key.
type CatalogBind struct {
	Adapter  string `json:"adapter"`
	ModelArg string `json:"modelArg,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

// ReadinessCheck is one static readiness result.
type ReadinessCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Config is the read-only configuration view this server projects. It is an interface so the server
// stays testable without a real home directory, and so the SURFACE — not the caller — owns what a
// tool result is allowed to contain.
type Config interface {
	// Adapters lists the configured adapters.
	Adapters() []AdapterFact
	// Profiles lists the configured profiles; DefaultProfile names the one a call with no
	// selection runs.
	Profiles() []ProfileFact
	DefaultProfile() string
	// Catalog lists the `modelCatalog` keys a composed panel may name.
	Catalog() []CatalogFact
	// Readiness runs the STATIC readiness checks. It starts no process and spends nothing — which
	// is why `review_doctor` can honestly carry readOnlyHint. The live `--probe` pass stays CLI-only.
	Readiness() (bool, []ReadinessCheck)
}

// pathRE matches an absolute or home-relative filesystem path appearing as its own token. Readiness
// details are composed by meshcore, which legitimately names where it found a binary — so the check
// text is sanitized on the way out rather than meshcore being made to guess who is reading.
var pathRE = regexp.MustCompile(`(^|[\s"'(\[=:,])((?:[A-Za-z]:[\\/]|~[\\/]|[\\/])[^\s"'\)\],;]{2,})`)

const maxDetailBytes = 400

// sanitizeDetail redacts filesystem paths from a human detail string and bounds its length. It is
// applied to EVERY string that leaves this surface having been composed elsewhere.
func sanitizeDetail(s string) string {
	s = pathRE.ReplaceAllString(s, "$1<path>")
	if len(s) > maxDetailBytes {
		s = s[:maxDetailBytes] + "…"
	}
	return s
}

// listPayload builds the `review_list` result.
func (s *Server) listPayload() map[string]any {
	adapters := make([]map[string]any, 0)
	for _, a := range s.Config.Adapters() {
		row := map[string]any{"name": a.Name, "kind": a.Kind, "configured": a.Configured}
		if a.DisplayName != "" {
			row["displayName"] = a.DisplayName
		}
		if a.IdentityEvidenceCapability != "" {
			row["identityEvidenceCapability"] = a.IdentityEvidenceCapability
		}
		if a.SpecOnly {
			row["specOnly"] = true
		}
		adapters = append(adapters, row)
	}
	profiles := make([]map[string]any, 0)
	for _, p := range s.Config.Profiles() {
		profiles = append(profiles, map[string]any{
			"name": p.Name, "isDefault": p.IsDefault,
			// The AUTHORED order is the panel's order — reordering it would change which seat is
			// seat 1, so it is reported as authored, never sorted.
			"reviewers": seatFactMaps(p.Reviewers),
			"lanes":     seatFactMaps(p.Lanes),
		})
	}
	active, started := s.runs.counts()
	remediation := map[string]any{
		"allowed":    s.allowRemediate(),
		"ceiling":    string(s.remediationCeiling()),
		"capability": capabilityName,
	}
	if s.allowRemediate() {
		remediation["note"] = "review_remediate is available. Every call must additionally pass allowWrite: true, and the fromRun form applies an accepted set that already exists as an inspectable artifact."
	} else {
		remediation["note"] = "This server cannot write. review_remediate is not listed; the operator would have to relaunch with --allow-remediate (or grant the capability in config)."
	}
	catalog := make([]map[string]any, 0)
	for _, c := range s.Config.Catalog() {
		binds := make([]map[string]any, 0, len(c.Adapters))
		for _, b := range c.Adapters {
			bind := map[string]any{"adapter": b.Adapter}
			if b.ModelArg != "" {
				bind["modelArg"] = b.ModelArg
			}
			if b.Effort != "" {
				bind["effort"] = b.Effort
			}
			binds = append(binds, bind)
		}
		row := map[string]any{"key": c.Key, "adapters": binds}
		if c.Provider != "" {
			row["provider"] = c.Provider
		}
		if c.CanonicalModel != "" {
			row["canonicalModel"] = c.CanonicalModel
		}
		if c.AdapterDefault {
			row["adapterDefault"] = true
		}
		catalog = append(catalog, row)
	}
	return map[string]any{
		"adapters": adapters,
		// The vocabulary a composed panel is validated against. Without it a caller can see WHICH
		// adapters exist but not which model tokens they accept, and has to guess.
		"modelCatalog": catalog,
		"profiles":     map[string]any{"defaultProfile": s.Config.DefaultProfile(), "profiles": profiles},
		"modes":        []string{string(review.ModeReport), string(review.ModePatch), string(review.ModeApply)},
		"limits": map[string]any{
			"runsInFlight":       active,
			"runsStarted":        started,
			"maxReviewerSeats":   review.MaxReviewerSeats,
			"maxAuthorityDocs":   MaxAuthorityDocs,
			"defaultWaitSeconds": s.waitDefault(),
			"maxWaitSeconds":     MaxWaitSeconds,
			"maxArgumentBytes":   maxArgumentBytes,
			"turnTimeoutSeconds": int(s.turnBudget().Seconds()),
		},
		"remediation": remediation,
		// The COUNT of trusted roots, never the paths: a caller needs to know whether this server
		// can read files at all, and nothing more.
		"roots": map[string]any{"count": len(s.Roots)},
		"note":  "Logical identifiers only. This server deliberately reports no binary paths, launch arguments, trusted-root paths or environment detail, and it cannot change any configuration.",
	}
}

func seatFactMaps(seats []SeatFact) []map[string]any {
	out := make([]map[string]any, 0, len(seats))
	for _, s := range seats {
		row := map[string]any{"adapter": s.Adapter, "model": s.Model}
		if s.Role != "" {
			row["role"] = s.Role
		}
		if s.Effort != "" {
			row["effort"] = s.Effort
		}
		out = append(out, row)
	}
	return out
}

// RootConfinementCheck is the NAME of the readiness entry that discloses this server's root
// provenance and the inferred-root waiver. It is exported so a test can name the row it asserts
// rather than matching a string that could drift.
const RootConfinementCheck = "root-confinement"

// doctorPayload builds the `review_doctor` result from the STATIC readiness checks, with every detail
// string sanitized, PLUS the root-confinement disclosure.
//
// The disclosure is a first-class readiness entry and not merely a field, because that is what makes
// it flow to every consumer of the readiness projection rather than to this one tool. And it is
// CATEGORICAL — a count and two enums, no path — for the same reason everything else on this surface
// is: a tool result is inference input for a third party, and a waiver check that named the waived
// directory would violate the rule it exists to disclose.
//
// `env` is the request's own protocol context, so `rootNarrowing` and `rootCount` describe the roots
// in force FOR THIS REQUEST rather than a process-global snapshot that a client's `roots/list_changed`
// may already have replaced.
func (s *Server) doctorPayload(env *proto.RequestEnv) map[string]any {
	ok, checks := s.Config.Readiness()
	rows := make([]map[string]any, 0, len(checks)+1)
	for _, c := range checks {
		row := map[string]any{"name": c.Name, "ok": c.OK}
		if d := sanitizeDetail(c.Detail); d != "" {
			row["detail"] = d
		}
		rows = append(rows, row)
	}
	waived := s.RootSource == acp.RootsInferredCwd && s.AllowInferredRoot
	rows = append(rows, map[string]any{
		"name": RootConfinementCheck,
		// It is not a FAILING check — a waiver is a documented operator choice, not a fault, and a
		// doctor that failed on it would train operators to ignore a failing doctor. It is a check so
		// that it is READ.
		"ok":     true,
		"detail": rootConfinementDetail(s.RootSource, waived, env),
	})
	return map[string]any{
		"ok":                 ok,
		"checks":             rows,
		"rootNarrowing":      rootNarrowing(env),
		"rootCount":          len(env.Roots),
		"inferredRootWaived": waived,
		"protocolMode":       string(s.core.Mode()),
		"protocolEra":        protocolEra(env),
		"note":               "Static readiness only: no process is started, nothing is spent, and no path or environment detail is reported. The live adapter probe is available on the reviewmesh CLI (`aimesh review doctor --probe`).",
	}
}

// protocolEra reports WHICH REVISION FAMILY is answering this very call.
//
// It is separate from `protocolMode` because the two answer different questions and conflating them
// would hide the one that matters. `protocolMode` is what the OPERATOR chose at launch (`dual` or
// `legacy`); `protocolEra` is what this REQUEST is being served under, and on a `dual` process that is
// decided by the client, once, by how it opened.
//
// It also carries the log-channel disclosure, because that is the one user-visible difference between
// the two eras on this server: under `modern` there are no `notifications/message` frames at all.
//
// SUNSET-PATH (MCP26-SUNSET): both fields go with the era.
func protocolEra(env *proto.RequestEnv) string {
	if env != nil && env.Era == proto.EraModern {
		return string(proto.EraModern)
	}
	return string(proto.EraLegacy)
}

// rootNarrowing reports WHICH channels narrowed this request's scope — categorically, never a path.
//
// THREE VALUES, NOT FOUR, and the missing one is deliberate. The migration design's §10.6 lists a
// fourth, `startup+call`, for "a `roots` argument narrowed this run". `review_doctor` accepts no arguments
// and therefore no `roots`, so this projection can never observe that case: the narrowing a call
// performs is scoped to that call, and `review_doctor` is a different call. Declaring an enum value in a
// published schema that the server cannot emit would be a promise nothing keeps, so it is not
// declared. If `review_doctor` is ever given a `roots` argument, the value arrives with it.
func rootNarrowing(env *proto.RequestEnv) string {
	switch {
	case env == nil || len(env.Roots) == 0:
		return "none"
	case env.Narrowed:
		return "startup+client"
	default:
		return "startup-only"
	}
}

// rootConfinementDetail is the human sentence beside the readiness row. NO PATHS.
func rootConfinementDetail(src acp.RootSource, waived bool, env *proto.RequestEnv) string {
	count := 0
	if env != nil {
		count = len(env.Roots)
	}
	switch {
	case src == acp.RootsInferredCwd && waived:
		return fmt.Sprintf("%d trusted root(s), INFERRED from this server's launch directory, and --allow-inferred-root is SET: they are honoured on every protocol revision, including those that removed the client's ability to narrow this server. Paths are not reported.", count)
	case src == acp.RootsInferredCwd:
		return fmt.Sprintf("%d trusted root(s), INFERRED from this server's launch directory. On protocol revisions that removed the client's ability to narrow this server, an inferred root is NOT trusted and every filesystem path is refused; relaunch with `--root <project-dir>` (preferred) or `--allow-inferred-root`. Paths are not reported.", count)
	case src == acp.RootsExplicit:
		return fmt.Sprintf("%d trusted root(s), named EXPLICITLY by the operator (`--root`). A call may narrow them and can never widen them. Paths are not reported.", count)
	default:
		return fmt.Sprintf("%d trusted root(s); provenance was not recorded by this launch. A call may narrow them and can never widen them. Paths are not reported.", count)
	}
}

// --- human renderings for the read-only tools ---

func renderList(payload map[string]any) string {
	var b strings.Builder
	adapters, _ := payload["adapters"].([]map[string]any)
	b.WriteString("Configured adapters (identifiers only — no paths are reported):\n")
	for _, a := range adapters {
		fmt.Fprintf(&b, "  %v [%v] configured=%v", a["name"], a["kind"], a["configured"])
		if ev, ok := a["identityEvidenceCapability"]; ok {
			fmt.Fprintf(&b, " identityEvidence=%v", ev)
		}
		b.WriteString("\n")
	}
	profiles, _ := payload["profiles"].(map[string]any)
	rows, _ := profiles["profiles"].([]map[string]any)
	fmt.Fprintf(&b, "Profiles (default: %v):\n", profiles["defaultProfile"])
	for _, p := range rows {
		seats, _ := p["reviewers"].([]map[string]any)
		lanes, _ := p["lanes"].([]map[string]any)
		fmt.Fprintf(&b, "  %v — %d blind reviewer seat(s), %d single-slot lane(s)\n", p["name"], len(seats), len(lanes))
	}
	rem, _ := payload["remediation"].(map[string]any)
	fmt.Fprintf(&b, "Remediation: allowed=%v (ceiling %v). %v\n", rem["allowed"], rem["ceiling"], rem["note"])
	b.WriteString("Full detail, including the admission limits, is in structuredContent.")
	return b.String()
}

func renderDoctor(payload map[string]any) string {
	var b strings.Builder
	if ok, _ := payload["ok"].(bool); ok {
		b.WriteString("Readiness: OK (static checks only — no process was started and nothing was spent).\n")
	} else {
		b.WriteString("Readiness: FAILING (static checks only — no process was started and nothing was spent).\n")
	}
	checks, _ := payload["checks"].([]map[string]any)
	for _, c := range checks {
		state := "ok  "
		if v, _ := c["ok"].(bool); !v {
			state = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %v", state, c["name"])
		if d, has := c["detail"]; has {
			fmt.Fprintf(&b, " — %v", d)
		}
		b.WriteString("\n")
	}
	// The confinement facts are repeated in the human rendering, because some clients show a model
	// ONLY this channel — and a disclosure that exists only in structuredContent is, for those
	// clients, no disclosure.
	fmt.Fprintf(&b, "Root confinement: %v root(s), narrowing=%v, inferredRootWaived=%v (paths are never reported).\n",
		payload["rootCount"], payload["rootNarrowing"], payload["inferredRootWaived"])
	return strings.TrimRight(b.String(), "\n")
}
