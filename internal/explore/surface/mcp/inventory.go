package mcp

import (
	"regexp"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
)

// This file is the SANITIZED configuration projection behind `explore_list` and `explore_doctor`.
//
// The sanitization is the point, not an afterthought. A tool result is inference input for a third party:
// whatever these tools return is very likely to end up in someone else's model provider's logs. Adapter
// IDENTIFIERS, selectable models, profile shapes and readiness booleans are what a caller needs in order
// to compose a valid run. Binary paths, launch arguments and environment detail are what an attacker (or
// an inattentive log retention policy) needs in order to map the operator's machine — and they buy the
// caller nothing, because it cannot configure anything anyway.
//
// The projection types below carry NO path field at all. That is deliberate: a rule enforced by the type
// system cannot be forgotten by the next person to add a field to the response.

// AdapterFact is one configured adapter, projected to logical facts only.
type AdapterFact struct {
	Name string `json:"name"`
	// DisplayName is the readable product name; Kind distinguishes a code-owned CLI recipe from a
	// user-defined ACP instance (and, under the internal test gate only, the hidden fake).
	DisplayName string `json:"displayName,omitempty"`
	Kind        string `json:"kind"`
	Configured  bool   `json:"configured"`
	// IdentityEvidenceCapability is the adapter's DECLARED evidence tier, not a live verdict — proving a
	// model's identity still takes a real call.
	IdentityEvidenceCapability string `json:"identityEvidenceCapability,omitempty"`
	SpecOnly                   bool   `json:"specOnly,omitempty"`
}

// ReadinessCheck is one static readiness result.
type ReadinessCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Config is the read-only configuration view the MCP server projects. It is an interface so the server
// stays testable without a real home directory, and so the surface — not the caller — owns what a tool
// result is allowed to contain.
type Config interface {
	// Adapters lists the configured adapters.
	Adapters() []AdapterFact
	// ProfileSet is the bound profile set (the panels a call may select by name).
	ProfileSet() profile.Set
	// Readiness runs the STATIC readiness checks. It starts no process and spends nothing — which is why
	// `explore_doctor` can honestly carry readOnlyHint. The live `--probe` pass stays CLI-only.
	Readiness() (bool, []ReadinessCheck)
}

// pathRE matches an absolute or home-relative filesystem path (POSIX or Windows) appearing as its own
// token. Readiness details are composed by meshcore, which legitimately names where it found a binary —
// so the check text is sanitized on the way out rather than meshcore being made to guess who is reading.
var pathRE = regexp.MustCompile(`(^|[\s"'(\[=:,])((?:[A-Za-z]:[\\/]|~[\\/]|[\\/])[^\s"'\)\],;]{2,})`)

// maxDetailBytes bounds one readiness detail string.
const maxDetailBytes = 400

// sanitizeDetail redacts filesystem paths from a human detail string and bounds its length. It is applied
// to EVERY string that leaves this surface having been composed elsewhere.
func sanitizeDetail(s string) string {
	s = pathRE.ReplaceAllString(s, "$1<path>")
	if len(s) > maxDetailBytes {
		s = s[:maxDetailBytes] + "…"
	}
	return s
}

// listPayload builds the `explore_list` result: adapters, profiles, modes and the admission limits in force.
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
	set := s.Config.ProfileSet()
	profiles := make([]map[string]any, 0, len(set.Profiles))
	for _, name := range set.Names() {
		p := set.Profiles[name]
		seats := make([]map[string]any, 0, len(p.Explorers))
		for _, e := range p.Explorers {
			seats = append(seats, map[string]any{"adapter": e.Adapter, "model": e.Model, "effort": e.Effort})
		}
		canon := make([]map[string]any, 0, len(p.Canonicalizers))
		for _, c := range p.Canonicalizers {
			canon = append(canon, map[string]any{"adapter": c.Adapter, "model": c.Model, "effort": c.Effort})
		}
		canonSource := "derived"
		if len(canon) > 0 {
			canonSource = "explicit"
		}
		row := map[string]any{
			"name":      name,
			"isDefault": name == set.DefaultProfile,
			// The AUTHORED order is the preference order a `count` selects the top-N from — reordering it
			// changes WHICH explorers a subset picks, so it is reported as-authored, never sorted.
			"explorers": seats,
			"collator":  map[string]any{"adapter": p.Collator.Adapter, "model": p.Collator.Model, "effort": p.Collator.Effort},
			// The canonicalizer identities this profile names, and — stated rather than inferred from an
			// empty array — whether it names them at all. A caller composing a governed run needs to know
			// that "no canonicalizers listed" means "the host will derive them", not "there are none".
			"canonicalizers":      canon,
			"canonicalizerSource": canonSource,
		}
		if p.DefaultMode != "" {
			row["defaultMode"] = p.DefaultMode
		}
		profiles = append(profiles, row)
	}
	active, started := s.runs.counts()
	return map[string]any{
		"adapters": adapters,
		"profiles": map[string]any{"defaultProfile": set.DefaultProfile, "profiles": profiles},
		"modes":    s.modeNames(),
		"limits": map[string]any{
			"runsInFlight":       active,
			"runsStarted":        started,
			"minPanelExplorers":  MinPanelExplorers,
			"maxPanelExplorers":  MaxPanelExplorers,
			"defaultWaitSeconds": s.waitDefault(),
			"maxWaitSeconds":     MaxWaitSeconds,
			"maxArgumentBytes":   maxArgumentBytes,
		},
		"note": "Logical identifiers only. This server deliberately reports no binary paths, launch arguments or environment detail, and it cannot change any configuration.",
	}
}

// doctorPayload builds the `explore_doctor` result from the STATIC readiness checks, with every detail string
// sanitized, PLUS the protocol-era disclosure.
//
// `env` is the request's own protocol context, which is what lets `protocolEra` describe the revision
// serving THIS call rather than a process-global guess.
func (s *Server) doctorPayload(env *proto.RequestEnv) map[string]any {
	ok, checks := s.Config.Readiness()
	rows := make([]map[string]any, 0, len(checks))
	for _, c := range checks {
		row := map[string]any{"name": c.Name, "ok": c.OK}
		if d := sanitizeDetail(c.Detail); d != "" {
			row["detail"] = d
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"ok":     ok,
		"checks": rows,
		// The era posture the OPERATOR chose, and the era actually serving this request. Reported
		// here and not only on stderr at launch, because a host launches its servers from a config
		// file and MAY discard stderr entirely — the stdio transport says so in as many words.
		//
		// SUNSET-PATH (MCP26-SUNSET): both go with the era.
		"protocolMode": string(s.core.Mode()),
		"protocolEra":  protocolEra(env),
		"note":         "Static readiness only: no process is started, nothing is spent, and no path or environment detail is reported. The live adapter probe is available on the exploremesh CLI (`aimesh explore doctor --probe`).",
	}
}

// protocolEra reports which revision family is serving THIS request, and it is the disclosure of the
// one user-visible difference between them on this server: under `modern` there are no
// `notifications/message` frames at all.
func protocolEra(env *proto.RequestEnv) string {
	if env != nil && env.Era == proto.EraModern {
		return string(proto.EraModern)
	}
	return string(proto.EraLegacy)
}
