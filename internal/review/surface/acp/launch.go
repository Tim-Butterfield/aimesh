package acp

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// ReasonWritesNotGranted refuses an apply turn on an agent launched without --allow-writes.
const ReasonWritesNotGranted = "writes_not_granted"

// Reason codes for a refused turn panel.
const (
	ReasonPanelRequired          = "panel_required"
	ReasonPanelReviewersEmpty    = "panel_reviewers_empty"
	ReasonPanelTooLarge          = "panel_too_large"
	ReasonPanelSeatIncomplete    = "panel_seat_incomplete"
	ReasonPanelAdjudicatorNeeded = "panel_author_remediator_required"
	ReasonPanelRoleEffort        = "panel_role_effort"
	ReasonPanelNoAdapters        = "panel_no_adapters"
	ReasonPanelAdapterNotNamed   = "panel_adapter_not_launched"
	ReasonPanelAdapterDown       = "panel_adapter_unavailable"
)

// panelMeta is `_meta.reviewmesh.panel`: the seats one review turn composes. reviewers and
// author_remediator are required; cross_check and verifier run only when named. Each seat names an
// adapter this agent was launched with and the exact model identifier that adapter's CLI accepts.
//
// It has no field for a binary path, launch arguments or an adapter definition, and the decoder
// refuses unknown fields, so a turn can use the adapters named at launch but never introduce one.
type panelMeta struct {
	Reviewers        []review.SeatSpec `json:"reviewers"`
	AuthorRemediator *review.SeatSpec  `json:"author_remediator,omitempty"`
	CrossCheck       *review.SeatSpec  `json:"cross_check,omitempty"`
	Verifier         *review.SeatSpec  `json:"verifier,omitempty"`
}

const panelExample = `{"panel": {"reviewers": [{"adapter": "<adapter>", "model": "<model>"}], "author_remediator": {"adapter": "<adapter>", "model": "<model>"}}}`

// roleSeat is one single-slot role seat and the field that names it.
type roleSeat struct {
	role  review.Role
	field string
	seat  *review.SeatSpec
}

func (p *panelMeta) roleSeats() []roleSeat {
	return []roleSeat{
		{review.RoleAuthorRemediator, "author_remediator", p.AuthorRemediator},
		{review.RoleCrossCheck, "cross_check", p.CrossCheck},
		{review.RoleVerifier, "verifier", p.Verifier},
	}
}

// validate checks the panel's shape before anything is spent: a bounded reviewer list, complete seats,
// a named adjudicator, and no effort on a single-slot role seat. It does not consult the launch set.
func (p *panelMeta) validate() (string, error) {
	if len(p.Reviewers) == 0 {
		return ReasonPanelReviewersEmpty, fmt.Errorf("_meta.reviewmesh.panel.reviewers needs at least 1 seat — it is the blind primary panel. Example: %s", panelExample)
	}
	if len(p.Reviewers) > review.MaxReviewerSeats {
		return ReasonPanelTooLarge, fmt.Errorf("_meta.reviewmesh.panel.reviewers has %d seats, exceeding the cap of %d — each seat is a real model CLI, and the count is never trimmed for you", len(p.Reviewers), review.MaxReviewerSeats)
	}
	for i, s := range p.Reviewers {
		if strings.TrimSpace(s.Adapter) == "" || strings.TrimSpace(s.Model) == "" {
			return ReasonPanelSeatIncomplete, fmt.Errorf("_meta.reviewmesh.panel.reviewers[%d] needs a non-empty `adapter` and `model`", i)
		}
	}
	if p.AuthorRemediator == nil {
		return ReasonPanelAdjudicatorNeeded, fmt.Errorf("_meta.reviewmesh.panel.author_remediator is required — it is the host-adjudication seat whose judgment becomes the accepted set, and it is never defaulted. Example: %s", panelExample)
	}
	for _, rs := range p.roleSeats() {
		if rs.seat == nil {
			continue
		}
		if strings.TrimSpace(rs.seat.Adapter) == "" || strings.TrimSpace(rs.seat.Model) == "" {
			return ReasonPanelSeatIncomplete, fmt.Errorf("_meta.reviewmesh.panel.%s needs a non-empty `adapter` and `model`", rs.field)
		}
		if strings.TrimSpace(rs.seat.Effort) != "" {
			return ReasonPanelRoleEffort, fmt.Errorf("_meta.reviewmesh.panel.%s.effort is not accepted — reasoning effort is per-seat only on `reviewers`; put an effort-bearing identifier in %s.model instead", rs.field, rs.field)
		}
	}
	return "", nil
}

// resolvePanel turns a turn's panel into the requested reviewer seats and the role seats the Manager
// resolves them from. A panel is required, and every seat must name an adapter this agent was launched
// with whose CLI can be started now.
func (s *Server) resolvePanel(p *panelMeta) ([]review.SeatSpec, map[review.Role]review.SeatSpec, string, error) {
	if p == nil {
		return nil, nil, ReasonPanelRequired, fmt.Errorf("_meta.reviewmesh.panel is required — every review turn names its own seats. Example: %s", panelExample)
	}
	if reason, err := p.validate(); err != nil {
		return nil, nil, reason, err
	}
	check := func(field string, seat review.SeatSpec) (string, error) {
		name := strings.TrimSpace(seat.Adapter)
		if s.Adapters.Empty() {
			return ReasonPanelNoAdapters, fmt.Errorf("_meta.reviewmesh.panel.%s cannot be resolved — this agent was launched with no adapter; the operator must add --adapter <name> (or %s) to the command that starts it", field, launchflags.EnvVar)
		}
		if !s.Adapters.Has(name) {
			return ReasonPanelAdapterNotNamed, fmt.Errorf("_meta.reviewmesh.panel.%s names adapter %q, which this agent was not launched with — available adapters are: %s", field, name, strings.Join(s.Adapters.Names(), ", "))
		}
		if ok, why := s.Adapters.Available(name); !ok {
			return ReasonPanelAdapterDown, fmt.Errorf("_meta.reviewmesh.panel.%s names adapter %q, whose CLI cannot be started right now: %s", field, name, why)
		}
		return "", nil
	}
	reviewers := make([]review.SeatSpec, 0, len(p.Reviewers))
	for i, seat := range p.Reviewers {
		if reason, err := check(fmt.Sprintf("reviewers[%d]", i), seat); err != nil {
			return nil, nil, reason, err
		}
		reviewers = append(reviewers, trimSeat(seat))
	}
	roles := map[review.Role]review.SeatSpec{}
	for _, rs := range p.roleSeats() {
		if rs.seat == nil {
			continue
		}
		if reason, err := check(rs.field, *rs.seat); err != nil {
			return nil, nil, reason, err
		}
		roles[rs.role] = trimSeat(*rs.seat)
	}
	return reviewers, roles, "", nil
}

func trimSeat(s review.SeatSpec) review.SeatSpec {
	return review.SeatSpec{Adapter: strings.TrimSpace(s.Adapter), Model: strings.TrimSpace(s.Model), Effort: strings.TrimSpace(s.Effort)}
}

// turnScope builds the confinement for one turn from the paths that turn declares: its workspace (unless
// it is an inline workspace this process materialized) and its extra `_meta.reviewmesh.roots`. Each path
// must be absolute, must not be the filesystem root, a home directory, a system tree or a protected
// directory, and must lie inside the operator's --root ceiling when one was set. A turn that declares no
// path gets the zero resolver, which refuses every filesystem path.
func (s *Server) turnScope(workspace string, owned bool, extra []string) (*scope.Resolver, string, error) {
	var paths []string
	if !owned && strings.TrimSpace(workspace) != "" {
		paths = append(paths, workspace)
	}
	for _, r := range extra {
		if strings.TrimSpace(r) == "" {
			return nil, ReasonCallPathRelative, fmt.Errorf("_meta.reviewmesh.roots entries must be non-empty absolute directory paths")
		}
		paths = append(paths, r)
	}
	if len(paths) == 0 {
		return &scope.Resolver{}, "", nil
	}
	r, err := CallScope(paths, s.Ceiling, "")
	if err != nil {
		return nil, fault.ReasonOf(err), err
	}
	return r, "", nil
}

// scopeHint appends the remedy to a turn-scope refusal.
func scopeHint(reason string) string {
	switch reason {
	case ReasonCallPathRelative:
		return " — name the absolute workspace directory"
	case ReasonOutsideCeiling:
		return " — this agent was launched with a --root ceiling; name a directory inside it"
	}
	return ""
}

// writesValue names who performs writes on this agent: aimesh when the operator launched it with
// --allow-writes, else the agent that sent the prompt.
func (s *Server) writesValue() string {
	if s.AllowWrites {
		return "aimesh"
	}
	return "agent"
}

// withWrites adds the write disclosure to a `_meta.reviewmesh` payload.
func (s *Server) withWrites(rm map[string]any) map[string]any {
	rm["writes"] = s.writesValue()
	rm["diffAvailable"] = true
	return rm
}
