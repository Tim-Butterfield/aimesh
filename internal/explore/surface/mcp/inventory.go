package mcp

import (
	"regexp"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the SANITIZED configuration projection behind `explore_list` and `explore_doctor`.
//
// The sanitization is the point, not an afterthought. A tool result is inference input for a third party:
// whatever these tools return is very likely to end up in someone else's model provider's logs. Adapter
// IDENTIFIERS, their availability and the modes are what a caller needs in order to compose a valid run.
// Binary paths, launch arguments and environment detail are what an attacker (or an inattentive log
// retention policy) needs in order to map the operator's machine — and they buy the caller nothing.
//
// The projection types below carry NO path field at all, so the rule is enforced by the type system.

// AdapterFact is one adapter this server was launched with, projected to logical facts only.
type AdapterFact struct {
	Name string `json:"name"`
	// Kind is "shell" for a built-in CLI recipe, or "fake" for the internal test adapter.
	Kind string `json:"kind"`
	// Available reports whether the adapter's CLI can be started right now; Reason says why not.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// Source is where the operator named the adapter: "flag" or "env".
	Source string `json:"source,omitempty"`
	// IdentityEvidenceCapability is the adapter's DECLARED evidence tier, not a live verdict — proving a
	// model's identity still takes a real call.
	IdentityEvidenceCapability string `json:"identityEvidenceCapability,omitempty"`
}

// ReadinessCheck is one static readiness result.
type ReadinessCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Config is the read-only configuration view the MCP server projects. It is an interface so the server
// stays testable without real CLIs, and so the surface — not the caller — owns what a tool result is
// allowed to contain.
type Config interface {
	// Adapters lists the adapters this server was launched with, with their availability now.
	Adapters() []AdapterFact
	// Readiness runs the STATIC readiness checks. It starts no model call and spends nothing — which is
	// why `explore_doctor` can honestly carry readOnlyHint.
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

// listPayload builds the `explore_list` result: the launched adapters, the modes and the admission limits.
func (s *Server) listPayload() map[string]any {
	adapters := make([]map[string]any, 0)
	for _, a := range s.Config.Adapters() {
		row := map[string]any{"name": a.Name, "kind": a.Kind, "available": a.Available}
		if a.Reason != "" {
			row["reason"] = sanitizeDetail(a.Reason)
		}
		if a.Source != "" {
			row["source"] = a.Source
		}
		if a.IdentityEvidenceCapability != "" {
			row["identityEvidenceCapability"] = a.IdentityEvidenceCapability
		}
		adapters = append(adapters, row)
	}
	active, started := s.runs.counts()
	return map[string]any{
		"adapters": adapters,
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
		"note": "Logical identifiers only. Every panel is composed per call from these adapters, with model identifiers the caller supplies. This server reports no binary paths, launch arguments or environment detail, and it cannot change any configuration.",
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
		"note":         "Static readiness only: no model call is made, nothing is spent, and no path or environment detail is reported. To check that a panel's agents can do real work before a run, pass verifyReadiness: true on the explore call.",
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
