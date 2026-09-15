package mcp

import (
	"regexp"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file holds the sanitized configuration view behind explore_list and explore_doctor. Tool results
// are likely to reach a model provider's logs, so they carry adapter names, availability and modes but no
// binary paths, launch arguments or environment detail. The types below have no path field, and detail
// strings composed elsewhere are sanitized.

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
	// IdentityEvidenceCapability is the adapter's declared evidence tier; confirming a model's identity
	// takes a real call.
	IdentityEvidenceCapability string `json:"identityEvidenceCapability,omitempty"`
}

// ReadinessCheck is one static readiness result.
type ReadinessCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Config is the read-only configuration view the MCP server reports. It is an interface so the server can
// be tested without real CLIs.
type Config interface {
	// Adapters lists the adapters this server was launched with, with their availability now.
	Adapters() []AdapterFact
	// Readiness runs the static readiness checks. It makes no model call, so explore_doctor is read-only.
	Readiness() (bool, []ReadinessCheck)
}

// pathRE matches an absolute or home-relative path (POSIX or Windows) as a token. meshcore's readiness
// details may name where a binary was found, so they are sanitized here.
var pathRE = regexp.MustCompile(`(^|[\s"'(\[=:,])((?:[A-Za-z]:[\\/]|~[\\/]|[\\/])[^\s"'\)\],;]{2,})`)

// maxDetailBytes bounds one readiness detail string.
const maxDetailBytes = 400

// sanitizeDetail redacts filesystem paths from a detail string and bounds its length. Every string composed
// outside this package passes through it.
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

// doctorPayload builds the explore_doctor result: the static readiness checks with sanitized details, and
// the protocol posture. env is the request's protocol context, so protocolEra describes this call.
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
		// The operator's era posture and the era serving this request. They are reported here as well
		// as on stderr, because a host may discard a stdio server's stderr.
		//
		// SUNSET-PATH (MCP26-SUNSET): both go with the era.
		"protocolMode": string(s.core.Mode()),
		"protocolEra":  protocolEra(env),
		"note":         "Static readiness only: no model call is made, nothing is spent, and no path or environment detail is reported. To check that a panel's agents can do real work before a run, pass verifyReadiness: true on the explore call.",
	}
}

// protocolEra reports which protocol era serves the request. Under modern, the server sends no
// `notifications/message` frames.
func protocolEra(env *proto.RequestEnv) string {
	if env != nil && env.Era == proto.EraModern {
		return string(proto.EraModern)
	}
	return string(proto.EraLegacy)
}
