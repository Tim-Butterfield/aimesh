package mcp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the SANITIZED configuration projection behind `review_list` and `review_doctor`.
//
// The sanitization is the point, not an afterthought. A tool result is inference input for a third
// party: whatever these tools return is very likely to end up in someone else's model provider's
// logs. Adapter IDENTIFIERS, their availability and who performs writes are what a caller needs in
// order to compose a valid run. Binary paths, launch arguments, root paths and environment detail are
// what an attacker (or an inattentive log retention policy) needs in order to map the operator's
// machine — and they buy the caller nothing, because it cannot configure anything anyway.
//
// The projection types below carry NO path field at all, so the rule is enforced by the type system
// rather than remembered by the next person who adds a field.

// AdapterFact is one adapter this server was launched with, projected to logical facts only.
type AdapterFact struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// Kind is "shell" for a built-in CLI recipe, or "fake" for the internal test adapter.
	Kind string `json:"kind"`
	// Available reports whether the adapter's CLI can be started right now; Reason says why not.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// Source is where the operator named the adapter: "flag" or "env".
	Source string `json:"source,omitempty"`
	// IdentityEvidenceCapability is the adapter's DECLARED evidence tier, not a live verdict —
	// proving a model's identity still takes a real call.
	IdentityEvidenceCapability string `json:"identityEvidenceCapability,omitempty"`
}

// ReadinessCheck is one static readiness result.
type ReadinessCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Config is the read-only configuration view this server projects. It is an interface so the server
// stays testable without real CLIs, and so the SURFACE — not the caller — owns what a tool result is
// allowed to contain.
type Config interface {
	// Adapters lists the adapters this server was launched with, with their availability now.
	Adapters() []AdapterFact
	// Readiness runs the STATIC readiness checks. It starts no process and spends nothing — which
	// is why `review_doctor` can honestly carry readOnlyHint.
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

// writesNote is the sentence beside `writes`, for both read-only tools.
func (s *Server) writesNote() string {
	if s.AllowWrites {
		return "aimesh can apply accepted findings: review_remediate with output=apply and allowWrite: true writes the workspace. output=patch supplies the diff instead."
	}
	return "aimesh does not change project content on this server. review_remediate with output=patch supplies the complete diff; apply it with your own file tools."
}

// listPayload builds the `review_list` result.
func (s *Server) listPayload() map[string]any {
	adapters := make([]map[string]any, 0)
	for _, a := range s.Config.Adapters() {
		row := map[string]any{"name": a.Name, "kind": a.Kind, "available": a.Available}
		if a.DisplayName != "" {
			row["displayName"] = a.DisplayName
		}
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
		"modes":    []string{string(review.ModeReport), string(review.ModePatch), string(review.ModeApply)},
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
		"remediation": map[string]any{
			"writes":        s.writesValue(),
			"diffAvailable": true,
			"note":          s.writesNote(),
		},
		// The COUNT of --root ceiling directories, never the paths: a caller needs to know whether its
		// declared paths are bounded, and nothing more.
		"roots": map[string]any{"ceiling": len(s.Ceiling)},
		"note":  "Logical identifiers only. Every panel is composed per call from these adapters, with model identifiers the caller supplies; each call declares its own absolute workspace. This server reports no binary paths, launch arguments, root paths or environment detail, and it cannot change any configuration.",
	}
}

// doctorPayload builds the `review_doctor` result from the STATIC readiness checks, with every detail
// string sanitized, plus who performs writes and whether a --root ceiling bounds calls. Every field is a
// count, an enum or a boolean — never a path.
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
		"ok":            ok,
		"checks":        rows,
		"writes":        s.writesValue(),
		"diffAvailable": true,
		"rootCeiling":   len(s.Ceiling),
		// The operator's bounded-execution grant: how many project commands run on each review's
		// containment copy, never the commands themselves.
		"verify": map[string]any{
			"commands":       len(s.VerifyCommands),
			"baseline":       s.VerifyBaseline,
			"timeoutSeconds": int(s.VerifyTimeout.Seconds()),
		},
		"protocolMode": string(s.core.Mode()),
		"protocolEra":  protocolEra(env),
		"note":         "Static readiness only: no process is started, nothing is spent, and no path or environment detail is reported. " + s.writesNote(),
	}
}

// protocolEra reports WHICH REVISION FAMILY is answering this very call.
//
// It is separate from `protocolMode` because the two answer different questions: `protocolMode` is what
// the OPERATOR chose at launch (`dual` or `legacy`); `protocolEra` is what this REQUEST is being served
// under, and on a `dual` process that is decided by the client, once, by how it opened.
//
// SUNSET-PATH (MCP26-SUNSET): both fields go with the era.
func protocolEra(env *proto.RequestEnv) string {
	if env != nil && env.Era == proto.EraModern {
		return string(proto.EraModern)
	}
	return string(proto.EraLegacy)
}

// --- human renderings for the read-only tools ---

func renderList(payload map[string]any) string {
	var b strings.Builder
	adapters, _ := payload["adapters"].([]map[string]any)
	b.WriteString("Adapters this server was launched with (identifiers only — no paths are reported):\n")
	if len(adapters) == 0 {
		b.WriteString("  none — the operator must add --adapter <name> to the host configuration\n")
	}
	for _, a := range adapters {
		fmt.Fprintf(&b, "  %v [%v] available=%v", a["name"], a["kind"], a["available"])
		if r, ok := a["reason"]; ok {
			fmt.Fprintf(&b, " (%v)", r)
		}
		if ev, ok := a["identityEvidenceCapability"]; ok {
			fmt.Fprintf(&b, " identityEvidence=%v", ev)
		}
		b.WriteString("\n")
	}
	rem, _ := payload["remediation"].(map[string]any)
	fmt.Fprintf(&b, "Writes: %v. %v\n", rem["writes"], rem["note"])
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
	// Repeated in the human rendering, because some clients show a model ONLY this channel.
	fmt.Fprintf(&b, "Writes: %v (diff available: %v). Root ceiling: %v director(y/ies) (paths are never reported).\n",
		payload["writes"], payload["diffAvailable"], payload["rootCeiling"])
	return strings.TrimRight(b.String(), "\n")
}
