package mcp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file builds the sanitized configuration projection behind review_list and review_doctor.
//
// Tool results are likely to reach a third party's model provider logs. Callers need adapter names,
// availability and who performs writes; binary paths, launch arguments, root paths and environment
// details would only map the operator's machine. The projection types have no path fields, so the
// type system enforces this.

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
	// IdentityEvidenceCapability is the adapter's declared evidence tier, not a live verification.
	IdentityEvidenceCapability string `json:"identityEvidenceCapability,omitempty"`
}

// ReadinessCheck is one static readiness result.
type ReadinessCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Config is the read-only configuration view this server projects. The surface, not the
// implementation, decides what a tool result may contain.
type Config interface {
	// Adapters lists the adapters this server was launched with, with their availability now.
	Adapters() []AdapterFact
	// Readiness runs the static readiness checks. It starts no process and spends nothing, so
	// review_doctor can be annotated read-only.
	Readiness() (bool, []ReadinessCheck)
}

// pathRE matches an absolute or home-relative path appearing as its own token. Readiness details come
// from meshcore and may name binary locations, so they are sanitized here.
var pathRE = regexp.MustCompile(`(^|[\s"'(\[=:,])((?:[A-Za-z]:[\\/]|~[\\/]|[\\/])[^\s"'\)\],;]{2,})`)

const maxDetailBytes = 400

// sanitizeDetail redacts filesystem paths from s and bounds its length. It is applied to every string
// composed elsewhere that leaves this surface.
func sanitizeDetail(s string) string {
	s = pathRE.ReplaceAllString(s, "$1<path>")
	if len(s) > maxDetailBytes {
		s = s[:maxDetailBytes] + "…"
	}
	return s
}

// writesNote returns the sentence shown beside writes in both read-only tools.
func (s *Server) writesNote() string {
	if s.AllowWrites {
		return "aimesh can apply accepted findings: review_remediate with output=apply and allowWrite: true writes the workspace. output=patch supplies the diff instead."
	}
	return "aimesh does not change project content on this server. review_remediate with output=patch supplies the complete diff; apply it with your own file tools."
}

// listPayload builds the review_list result.
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
		// The number of --root ceiling directories, never the paths.
		"roots": map[string]any{"ceiling": len(s.Ceiling)},
		"note":  "Logical identifiers only. Every panel is composed per call from these adapters, with model identifiers the caller supplies; each call declares its own absolute workspace. This server reports no binary paths, launch arguments, root paths or environment detail, and it cannot change any configuration.",
	}
}

// doctorPayload builds the review_doctor result: sanitized static readiness checks, who performs
// writes, and whether a --root ceiling applies. Every field is a count, enum or boolean.
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
		// How many verify commands run on each review's containment copy, never the commands.
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

// protocolEra reports which protocol revision family is serving this request. protocolMode is what
// the operator chose at launch (dual or legacy); on a dual process the era is set by how the client
// connected.
//
// SUNSET-PATH (MCP26-SUNSET): both fields go with the legacy era.
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
	// Repeated in the text rendering, since some clients show a model only this channel.
	fmt.Fprintf(&b, "Writes: %v (diff available: %v). Root ceiling: %v director(y/ies) (paths are never reported).\n",
		payload["writes"], payload["diffAvailable"], payload["rootCeiling"])
	return strings.TrimRight(b.String(), "\n")
}
