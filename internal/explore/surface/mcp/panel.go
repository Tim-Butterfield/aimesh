package mcp

import (
	"fmt"
	"strings"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
)

// This file resolves which explorers a call runs, before any model call. Each call composes its panel from
// the adapters named at launch and cannot add an adapter, binary path or launch argument; this is checked
// here because clients may ignore the JSON Schema. Failures are JSON-RPC invalid-params errors, since no
// run exists yet, and each message names the field and the accepted values.

const panelExample = `{"panel": {"explorers": [{"adapter": "<adapter>", "model": "<model>"}, {"adapter": "<adapter>", "model": "<model>"}], "collator": {"adapter": "<adapter>", "model": "<model>"}}}`

// resolvePanel turns the required `panel` argument into the panel that will execute.
func (s *Server) resolvePanel(p *panelArg) (panelPick, error) {
	if p == nil {
		return panelPick{}, proto.InvalidParams("invalid params: `panel` is required — every exploration names its own seats. Corrected call: %s", panelExample)
	}
	if len(p.Explorers) < MinPanelExplorers {
		return panelPick{}, proto.InvalidParams(
			"invalid params: panel.explorers needs at least %d entries (got %d) — a panel with fewer than %d explorers has nothing to be blind about. Corrected call: %s",
			MinPanelExplorers, len(p.Explorers), MinPanelExplorers, panelExample)
	}
	if len(p.Explorers) > MaxPanelExplorers {
		return panelPick{}, proto.InvalidParams(
			"invalid params: panel.explorers has %d entries, exceeding the fan-out cap of %d. The cap is a spend control and is never clamped — send at most %d.",
			len(p.Explorers), MaxPanelExplorers, MaxPanelExplorers)
	}
	if p.Collator == nil {
		return panelPick{}, proto.InvalidParams("invalid params: panel.collator is required — a panel must name the seat that collates it. Corrected call: %s", panelExample)
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

// resolveCanonicalizers turns the optional `canonicalizers` argument into explicit canonicalizer
// identities. Each seat gets the same launch-set check as a panel seat, and roster.ValidateCanonicalizers
// applies the rule shared with the CLI and ACP.
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

// checkConfigured enforces that a seat names an adapter this server was launched with, and that the
// adapter's CLI can be started now. The refusal names the launched set, so the corrected call follows
// from the error.
func (s *Server) checkConfigured(field string, seat slot) error {
	adapter, model := strings.TrimSpace(seat.Adapter), strings.TrimSpace(seat.Model)
	if adapter == "" || model == "" {
		return proto.InvalidParams("invalid params: %s needs a non-empty adapter and model (got adapter=%q model=%q)", field, adapter, model)
	}
	if s.Adapters.Empty() {
		return proto.InvalidParams("invalid params: %s cannot be resolved — this server was launched with no adapter. The operator must add --adapter <name> (or %s) to the host configuration that starts it.", field, launchflags.EnvVar)
	}
	if !s.Adapters.Has(adapter) {
		return proto.InvalidParams(
			"invalid params: %s names adapter %q, which this server was not launched with — available adapters are: %s. A call uses the adapters named at launch; it never introduces one.",
			field, adapter, strings.Join(s.Adapters.Names(), ", "))
	}
	if ok, why := s.Adapters.Available(adapter); !ok {
		return proto.InvalidParams("invalid params: %s names adapter %q, whose CLI cannot be started right now: %s", field, adapter, sanitizeDetail(why))
	}
	return nil
}
