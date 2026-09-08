package mcp

import (
	"fmt"
	"slices"
	"strings"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// This file resolves WHICH explorers a call runs. It is the compose-not-configure boundary, and every
// refusal here happens BEFORE any spend.
//
// The rule the whole file exists to enforce: a call may SELECT a configured profile, or COMPOSE a panel
// from the adapter set this server bound at STARTUP. It can never introduce an adapter, a binary path or
// a launch argument. That is enforced here rather than left to the JSON Schema, because a schema is
// advisory to a client — a server that trusted it would be one malformed call away from running whatever
// it was handed.
//
// Failures here are JSON-RPC protocol errors (-32602), not `isError` results: nothing has been spent and
// no run exists, so this is the malformed-request case the taxonomy reserves protocol errors for. Every
// message names the field and the accepted values, so the corrected call is derivable rather than
// guessable.

// resolvePanel turns the optional `panel` argument into the panel that will execute.
func (s *Server) resolvePanel(p *panelArg) (panelPick, error) {
	if p == nil {
		return s.defaultPanel()
	}
	named := strings.TrimSpace(p.Profile) != "" || p.Count != nil
	adhoc := len(p.Explorers) > 0 || p.Collator != nil
	switch {
	case named && adhoc:
		return panelPick{}, proto.InvalidParams("invalid params: `panel` must be EITHER {profile, count} or {explorers, collator} — you supplied both, which asks for two different panels")
	case adhoc:
		return s.adHocPanel(p)
	case named:
		return s.profilePanel(p)
	default:
		return s.defaultPanel()
	}
}

// defaultPanel runs this server's bound default panel unchanged.
func (s *Server) defaultPanel() (panelPick, error) {
	if len(s.Plan.Explorers) < MinPanelExplorers {
		return panelPick{}, proto.InvalidParams(
			"invalid params: this server has no configured default panel (it needs at least %d explorers and a collator). Call `explore_list` for the configured profiles and name one in `panel.profile`, or compose `panel.explorers` + `panel.collator`.",
			MinPanelExplorers)
	}
	n := len(s.Plan.Explorers)
	return panelPick{source: "default", plan: s.Plan, selected: n, configured: n}, nil
}

// profilePanel selects a named profile and applies `count` (top-N by the profile's PREFERENCE order).
// The count is never clamped: requested equals executed, or the call fails.
func (s *Server) profilePanel(p *panelArg) (panelPick, error) {
	name := strings.TrimSpace(p.Profile)
	var src roster.Roster
	resolved := ""
	if len(s.Profiles.Profiles) == 0 {
		if name != "" {
			return panelPick{}, proto.InvalidParams(
				"invalid params: unknown profile %q — this server is bound to a single roster with no named profiles. Omit `panel.profile`, or compose `panel.explorers` + `panel.collator`.", name)
		}
		// Canonicalizers travel with the roster: a `count` re-selects the EXPLORERS, and must not quietly
		// discard the governance identities the bound roster names.
		src = roster.Roster{Explorers: s.Plan.Explorers, Collator: s.Plan.Collator, Canonicalizers: s.Plan.Canonicalizers}
	} else {
		if name == "" {
			name = s.Profiles.DefaultProfile
		}
		prof, ok := s.Profiles.Get(name)
		if !ok {
			return panelPick{}, proto.InvalidParams(
				"invalid params: unknown profile %q (configured profiles: %s — call `explore_list` to see their panels)", name, strings.Join(s.Profiles.Names(), ", "))
		}
		src, resolved = prof.Roster(), name
	}
	full := len(src.Explorers)
	n := full
	requested := 0
	if p.Count != nil {
		n, requested = *p.Count, *p.Count
	}
	plan, err := src.SelectTopN(n)
	if err != nil {
		// Out of range or below the floor — never clamped (design §7: requested = executed).
		return panelPick{}, proto.InvalidParams("invalid params: panel.count: %v", err)
	}
	return panelPick{source: "profile", profile: resolved, count: requested, plan: plan, selected: n, configured: full}, nil
}

// adHocPanel composes a panel from identifiers, fail-closed against the startup-bound adapter set.
func (s *Server) adHocPanel(p *panelArg) (panelPick, error) {
	if len(p.Explorers) < MinPanelExplorers {
		return panelPick{}, proto.InvalidParams(
			"invalid params: panel.explorers needs at least %d entries (got %d) — a panel with fewer than %d explorers has nothing to be blind about",
			MinPanelExplorers, len(p.Explorers), MinPanelExplorers)
	}
	if len(p.Explorers) > MaxPanelExplorers {
		return panelPick{}, proto.InvalidParams(
			"invalid params: panel.explorers has %d entries, exceeding the fan-out cap of %d. The cap is a spend control and is never clamped — send at most %d.",
			len(p.Explorers), MaxPanelExplorers, MaxPanelExplorers)
	}
	if p.Collator == nil {
		return panelPick{}, proto.InvalidParams("invalid params: panel.collator is required for an ad-hoc panel — a composed panel must name the seat that collates it")
	}
	var r roster.Roster
	for i, e := range p.Explorers {
		if err := s.checkConfigured(fmt.Sprintf("panel.explorers[%d]", i), e); err != nil {
			return panelPick{}, err
		}
		r.Explorers = append(r.Explorers, roster.Explorer{Adapter: strings.TrimSpace(e.Adapter), Model: strings.TrimSpace(e.Model), Effort: strings.TrimSpace(e.Effort)})
	}
	if err := s.checkConfigured("panel.collator", *p.Collator); err != nil {
		return panelPick{}, err
	}
	r.Collator = roster.Collator{Adapter: strings.TrimSpace(p.Collator.Adapter), Model: strings.TrimSpace(p.Collator.Model), Effort: strings.TrimSpace(p.Collator.Effort)}
	plan, err := r.Plan() // enforces the floor, unique triples, and non-empty adapter/model
	if err != nil {
		return panelPick{}, proto.InvalidParams("invalid params: panel: %v", err)
	}
	n := len(plan.Explorers)
	return panelPick{source: "adhoc", reqSeats: p.Explorers, reqCollate: p.Collator, plan: plan, selected: n, configured: n}, nil
}

// resolveCanonicalizers turns the optional `canonicalizers` argument into the two explicit canonicalizer
// identities (design §4). Every seat goes through the SAME compose-not-configure check an ad-hoc panel seat
// gets — a call may select and order configured identities, never introduce one — and the 0-or-2 /
// distinct-identities rule is roster.ValidateCanonicalizers', shared with the CLI, the ACP surface and the
// profile schema so all four state the rule in the same words.
func (s *Server) resolveCanonicalizers(specs []slot) ([]roster.Explorer, error) {
	out := make([]roster.Explorer, 0, len(specs))
	for i, c := range specs {
		if err := s.checkConfigured(fmt.Sprintf("canonicalizers[%d]", i), c); err != nil {
			return nil, err
		}
		out = append(out, roster.Explorer{Adapter: strings.TrimSpace(c.Adapter), Model: strings.TrimSpace(c.Model), Effort: strings.TrimSpace(c.Effort)})
	}
	if err := roster.ValidateCanonicalizers(out); err != nil {
		return nil, proto.InvalidParams("invalid params: %v", err)
	}
	return out, nil
}

// checkConfigured enforces compose-not-configure for one seat. The refusal NAMES the configured set, so
// the corrected call follows from the error.
func (s *Server) checkConfigured(field string, seat slot) error {
	adapter, model := strings.TrimSpace(seat.Adapter), strings.TrimSpace(seat.Model)
	if adapter == "" || model == "" {
		return proto.InvalidParams("invalid params: %s needs a non-empty adapter and model (got adapter=%q model=%q)", field, adapter, model)
	}
	if len(s.Adapters) == 0 {
		return proto.InvalidParams("invalid params: %s cannot be resolved — this server bound no configured adapter set, so ad-hoc panel composition is refused", field)
	}
	if !slices.Contains(s.Adapters, adapter) {
		return proto.InvalidParams(
			"invalid params: %s names adapter %q, which is not configured on this server — configured adapters are: %s. A call selects from the configured set; it never introduces an adapter, a path or a launch argument.",
			field, adapter, strings.Join(sortedNames(s.Adapters), ", "))
	}
	return nil
}
