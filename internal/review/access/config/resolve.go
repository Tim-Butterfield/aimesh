package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	adapterpkg "github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// ResolveRequest is the input to resolution (the parts a run/invocation supplies).
type ResolveRequest struct {
	Profile         string      // --profile (optional)
	Mode            review.Mode // requested mode (optional; "" lets the surface default apply)
	Surface         string      // "cli" | "ci" | ...
	AdapterOverride map[review.Role]string
	ModelOverride   map[review.Role]string
	// Available is the set of adapters runnable now. The adapter fallback skips any not in it;
	// nil means availability is not consulted.
	Available map[string]bool
	// ReviewerPanel is a panel composed by the invocation (CLI `--reviewer`, an MCP or ACP panel).
	// It replaces the profile's panel entirely. Each seat must name a configured adapter; its model
	// may be any string, which is passed to the adapter verbatim and recorded as pass-through.
	ReviewerPanel []review.SeatSpec
	// ComposedRoles are the single-role seats (author_remediator, cross_check, verifier) a call
	// composed on a surface that reads no saved configuration (MCP, ACP). When non-nil, resolution
	// uses only these seats and ReviewerPanel: author_remediator is required, an absent role is a
	// step the run skips, and every model passes through verbatim. The CLI's `--set` overrides use
	// AdapterOverride and ModelOverride instead and stay held to the catalog.
	ComposedRoles map[review.Role]review.SeatSpec
}

// Reason codes for a call-composed panel that cannot be resolved.
const (
	ReasonCallPanelMissingHost = "call_panel_missing_author_remediator"
	ReasonCallPanelRoleInvalid = "call_panel_role_invalid"
)

var modeRank = map[review.Mode]int{
	review.ModeReport: 0, review.ModePatch: 1, review.ModeApply: 2,
}

func minMode(a, b review.Mode) review.Mode {
	if modeRank[a] <= modeRank[b] {
		return a
	}
	return b
}

// maxMode returns the higher-privilege of two modes.
func maxMode(a, b review.Mode) review.Mode {
	if modeRank[a] >= modeRank[b] {
		return a
	}
	return b
}

// Resolve turns the configuration and request into a RunPlan, or returns a Config fault when a
// profile, model or adapter cannot be resolved.
func (c Config) Resolve(req ResolveRequest) (review.RunPlan, error) {
	prof, _, err := c.requestProfile(req)
	if err != nil {
		return review.RunPlan{}, err
	}
	composed := req.ComposedRoles != nil

	plan := review.RunPlan{Surface: req.Surface, Lanes: map[review.Role]review.LaneResolution{}}

	for _, roleName := range sortedLaneNames(prof.Lanes) {
		lane := prof.Lanes[roleName]
		role := review.Role(roleName)

		// A composed panel replaces the reviewer lane; the alias below is rebuilt from seat 1.
		if role == review.RoleReviewer && len(req.ReviewerPanel) > 0 {
			continue
		}

		adapter, model, effort := lane.Adapter, lane.Model, ""
		if composed {
			effort = req.ComposedRoles[role].Effort
		} else {
			// A saved lane and its `--set` override are held to the catalog.
			if ov := req.AdapterOverride[role]; ov != "" {
				adapter = ov
			}
			if ov := req.ModelOverride[role]; ov != "" {
				model = ov
			}
		}
		res, err := c.resolveSeat(prof, role, fmt.Sprintf("role %q", roleName), lane.Execution, adapter, model, effort, req.Available, composed, composed)
		if err != nil {
			return review.RunPlan{}, err
		}
		plan.Lanes[role] = res
	}

	// Lanes[reviewer] is the panel's first seat, for role-shaped consumers (preflight, doctor,
	// privacy, the ACP plan view). It is set here when the panel comes from `reviewers` or the
	// invocation; its SeatID stays empty because only ResolvePanel names seats.
	if len(req.ReviewerPanel) > 0 || prof.HasPanelSpelling() {
		seats, perr := c.ResolvePanel(req)
		if perr != nil {
			return review.RunPlan{}, perr
		}
		first := seats[0]
		first.SeatID = ""
		plan.Lanes[review.RoleReviewer] = first
	}

	// The effective mode is the requested mode capped at the surface ceiling. An unspecified mode
	// is `report` on every surface: asking for a review is not consent to edit files.
	cap := c.SurfaceCeiling(req.Surface)
	requested := req.Mode
	if requested == "" {
		requested = review.ModeReport
	}
	plan.Mode = minMode(requested, cap)
	return plan, nil
}

// nearestCatalogKey returns the catalog key model most plausibly meant, or "" when nothing is close.
//
// Catalog keys embed effort ("claude-opus-5-high") while callers often pass model and effort
// separately, so the match is a key that extends model by exactly one hyphen-separated segment:
//
//	"claude-opus-5"      → "claude-opus-5-high"
//	"gpt"                → ""  ("-5-codex" is two segments: a different model)
//	"claude-opus-5-high" → ""  (already a key)
//
// Matching is case-insensitive, and the shortest key wins.
func (c Config) nearestCatalogKey(model string) string {
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" {
		return ""
	}
	for key := range c.ModelCatalog {
		if strings.EqualFold(key, want) {
			return "" // already a key
		}
	}
	best := ""
	for key := range c.ModelCatalog {
		rest, ok := strings.CutPrefix(strings.ToLower(key), want)
		if !ok || len(rest) < 2 || rest[0] != '-' || strings.Contains(rest[1:], "-") {
			continue
		}
		if best == "" || len(key) < len(best) || (len(key) == len(best) && key < best) {
			best = key
		}
	}
	return best
}

// resolveSeat resolves one (adapter, model, effort) assignment, a role lane or a panel seat, into a
// LaneResolution. label names the slot in error messages (a role, or "reviewer seat 2 of 3"), and a
// non-empty effortOverride wins over catalog defaults.
//
// composed means the caller supplied the seat, so a model that is not a catalog key passes through
// instead of being refused. verbatim (MCP, ACP) passes the model through even when it matches a
// catalog key.
func (c Config) resolveSeat(prof Profile, role review.Role, label, execution, adapter, model, effortOverride string, available map[string]bool, composed, verbatim bool) (review.LaneResolution, error) {
	if adapter == "" {
		adapter = firstAvailableAdapter(c, prof.AdapterPreference, available)
	}
	if adapter == "" {
		return review.LaneResolution{}, fault.New(fault.Config, fmt.Sprintf("no adapter resolvable for %s", label)).
			WithReason("lane_adapter_unresolvable")
	}
	if a, ok := c.Adapters[adapter]; !ok || !a.IsEnabled() {
		return review.LaneResolution{}, fault.New(fault.Config, fmt.Sprintf("adapter %q (%s) is not configured/enabled", adapter, label)).
			WithReason("lane_adapter_unconfigured")
	}
	if composed && verbatim {
		// devin-cli encodes reasoning effort in its model identifier, so a separate effort cannot be
		// applied without rewriting the caller's identifier. It is refused rather than dropped.
		if adapter == "devin-cli" && effortOverride != "" {
			return review.LaneResolution{}, fault.New(fault.Config, fmt.Sprintf(
				"%s: devin-cli encodes effort in the model identifier; pass the full identifier (including its effort) as model and omit effort", label)).
				WithReason("lane_effort_not_separable")
		}
		return review.LaneResolution{
			Role: role, Execution: execution, Adapter: adapter,
			Model: model, ModelArg: review.ModelArg(model), Effort: effortOverride,
			ModelPassThrough: true,
		}, nil
	}
	entry, ok := c.ModelCatalog[model]
	if !ok {
		if composed {
			return review.LaneResolution{
				Role: role, Execution: execution, Adapter: adapter,
				Model: model, ModelArg: review.ModelArg(model), Effort: effortOverride,
				ModelPassThrough: true, PassThroughHint: c.nearestCatalogKey(model),
			}, nil
		}
		hint := ""
		if near := c.nearestCatalogKey(model); near != "" {
			hint = fmt.Sprintf(" — did you mean %q?", near)
		}
		return review.LaneResolution{}, fault.New(fault.Config, fmt.Sprintf("model %q (%s) not in modelCatalog%s", model, label, hint)).
			WithReason("lane_model_unknown")
	}
	am, ok := entry.Adapters[adapter]
	if !ok {
		return review.LaneResolution{}, fault.New(fault.Config, fmt.Sprintf("model %q has no modelArg for adapter %q (%s)", model, adapter, label)).
			WithReason("lane_model_adapter_unmapped")
	}
	if am.ModelArg == "" {
		return review.LaneResolution{}, fault.New(fault.Config, fmt.Sprintf("model %q has an empty modelArg for adapter %q (%s); configure the model tag (e.g. set REVIEWMESH_OLLAMA_MODEL for the ollama adapter)", model, adapter, label)).
			WithReason("lane_model_arg_empty")
	}

	// Effort precedence: the seat's own, then the per-adapter setting, then the catalog default.
	effort := effortOverride
	if effort == "" {
		effort = am.Effort
	}
	if effort == "" {
		effort = entry.Effort
	}

	// devin-cli's model slug encodes the effort, so render the display name and effort into it here.
	modelArg := am.ModelArg
	if adapter == "devin-cli" {
		rendered, rerr := adapterpkg.RenderDevinModelArg(am.ModelArg, effort)
		if rerr != nil {
			return review.LaneResolution{}, fmt.Errorf("model %q (%s): %w", model, label, rerr)
		}
		modelArg = rendered
	}
	return review.LaneResolution{
		Role:      role,
		Execution: execution,
		Adapter:   adapter,
		Model:     model,
		ModelArg:  review.ModelArg(modelArg),
		Effort:    effort,
	}, nil
}

// ResolvePanel resolves the ordered blind-primary panel: the invocation's panel when it supplied
// one, else the selected profile's ReviewerSeats.
//
// It refuses, before any spend, an empty panel, one above review.MaxReviewerSeats, a duplicate seat
// identity (which would inflate agreement), and any seat that does not resolve. Seats are never
// dropped or clamped.
func (c Config) ResolvePanel(req ResolveRequest) ([]review.LaneResolution, error) {
	prof, profName, perr := c.requestProfile(req)
	if perr != nil {
		// A composed panel needs no saved profile, unless the call's composed roles are invalid.
		if req.ComposedRoles != nil || len(req.ReviewerPanel) == 0 {
			return nil, perr
		}
		prof = Profile{}
	}

	type seatIn struct {
		adapter, model, effort, execution string
		composed                          bool // written by the caller, so an unknown model passes through
	}
	var seats []seatIn
	if len(req.ReviewerPanel) > 0 {
		for _, s := range req.ReviewerPanel {
			seats = append(seats, seatIn{adapter: s.Adapter, model: s.Model, effort: s.Effort, execution: "adapter", composed: true})
		}
	} else {
		for _, l := range prof.ReviewerSeats() {
			seats = append(seats, seatIn{adapter: l.Adapter, model: l.Model, execution: l.Execution})
		}
	}
	if len(seats) == 0 {
		return nil, fault.New(fault.Config, fmt.Sprintf("profile %q configures no blind primary reviewer: add a `reviewers` panel (1..%d seats) or a `lanes.reviewer`", profName, review.MaxReviewerSeats)).
			WithReason("panel_empty")
	}
	if len(seats) > review.MaxReviewerSeats {
		return nil, fault.New(fault.Config, fmt.Sprintf("reviewer panel has %d seats, exceeding the cap of %d — each seat is a real model CLI, so an unbounded panel is a spend hazard; remove seats rather than expecting the panel to be trimmed (the count is never clamped)", len(seats), review.MaxReviewerSeats)).
			WithReason("panel_too_large")
	}

	// Every unresolvable seat is reported in one refusal; resolution spends nothing.
	out := make([]review.LaneResolution, 0, len(seats))
	var failures []error
	for i, s := range seats {
		label := fmt.Sprintf("reviewer seat %d of %d", i+1, len(seats))
		exec := s.execution
		if exec == "" {
			exec = "adapter"
		}
		// `--set reviewer.*` overrides apply only to a one-seat profile panel; applying them to every
		// seat would collapse the panel's independence.
		adapter, model := s.adapter, s.model
		if len(seats) == 1 && len(req.ReviewerPanel) == 0 {
			if ov := req.AdapterOverride[review.RoleReviewer]; ov != "" {
				adapter = ov
			}
			if ov := req.ModelOverride[review.RoleReviewer]; ov != "" {
				model = ov
			}
		}
		res, err := c.resolveSeat(prof, review.RoleReviewer, label, exec, adapter, model, s.effort, req.Available, s.composed, req.ComposedRoles != nil)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		res.SeatID = SeatID(i)
		out = append(out, res)
	}
	if len(failures) > 0 {
		return nil, unresolvableSeats(failures, len(seats))
	}

	// Duplicates are checked after resolution, since identity is computed from resolved seats.
	seen := map[string]int{}
	for i, res := range out {
		identity := review.SeatSpec{Adapter: res.Adapter, Model: res.Model, Effort: res.Effort}.Identity()
		if prev, dup := seen[identity]; dup {
			return nil, fault.New(fault.Config, fmt.Sprintf(
				"reviewer seats %d and %d are the same identity (adapter=%q model=%q effort=%q) — an identical seat adds no independent vantage and would double-count as agreement; vary the adapter, model, or effort",
				prev+1, i+1, res.Adapter, res.Model, res.Effort)).WithReason("panel_duplicate_seat")
		}
		seen[identity] = i
	}
	return out, nil
}

// unresolvableSeats folds every seat failure into one refusal whose message lists them all and
// whose reason code is the first failure's.
func unresolvableSeats(failures []error, total int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d reviewer seat(s) cannot be resolved. Every blocker is listed so one pass fixes them all:", len(failures), total)
	for _, err := range failures {
		fmt.Fprintf(&b, "\n  - %s", err.Error())
	}
	out := fault.New(fault.Config, b.String())
	if r := fault.ReasonOf(failures[0]); r != "" {
		out = out.WithReason(r)
	}
	return out
}

// SeatID returns the identifier of the i-th (0-based) panel seat: "reviewer" for the first, then
// "reviewer-2", "reviewer-3", ….
func SeatID(i int) string {
	if i == 0 {
		return string(review.RoleReviewer)
	}
	return fmt.Sprintf("%s-%d", review.RoleReviewer, i+1)
}

// requestProfile returns the profile a request resolves against, and its name.
//
// A call-composed request (ComposedRoles non-nil) gets a profile built from its own seats and no
// name: author_remediator runs as the host lane, cross_check and verifier run only when named, and
// nothing is taken from any saved or shipped profile. Every other request selects a saved profile.
func (c Config) requestProfile(req ResolveRequest) (Profile, string, error) {
	if req.ComposedRoles != nil {
		if _, ok := req.ComposedRoles[review.RoleAuthorRemediator]; !ok {
			return Profile{}, "", fault.New(fault.Config, "a composed panel must name author_remediator: it is the host-adjudication seat whose judgment becomes the accepted set, and it is never defaulted").
				WithReason(ReasonCallPanelMissingHost)
		}
		p := Profile{Lanes: map[string]Lane{}}
		for role, seat := range req.ComposedRoles {
			execution := "adapter"
			switch role {
			case review.RoleAuthorRemediator:
				execution = "host"
			case review.RoleCrossCheck, review.RoleVerifier:
			default:
				return Profile{}, "", fault.New(fault.Config, fmt.Sprintf("a composed panel cannot name role %q as a single seat; reviewers are the panel itself, and the single seats are author_remediator, cross_check and verifier", role)).
					WithReason(ReasonCallPanelRoleInvalid)
			}
			p.Lanes[string(role)] = Lane{Execution: execution, Adapter: seat.Adapter, Model: seat.Model}
		}
		return p, "", nil
	}
	name := c.selectProfile(req.Profile, req.Available)
	prof, ok := c.Profiles[name]
	// The hidden fake profile resolves only under the internal test gate; otherwise it is unknown.
	if ok && IsHiddenProfile(name) && !fake.Enabled() {
		ok = false
	}
	if !ok {
		return Profile{}, name, fault.New(fault.Config, fmt.Sprintf("profile %q not found in config%s", name, ProfileNotFoundGuidance(name))).
			WithReason("profile_not_found")
	}
	return prof, name, nil
}

func (c Config) selectProfile(invocation string, available map[string]bool) string {
	if invocation != "" {
		return invocation
	}
	if c.DefaultProfile != "" {
		return c.DefaultProfile
	}
	adapter := firstAvailableAdapter(c, c.Defaults.AdapterPreference, available)
	if p, ok := c.Defaults.ProfileForAdapter[adapter]; ok {
		return p
	}
	return c.Defaults.ProfileForAdapter["*"]
}

// firstAvailableAdapter returns the first adapter in pref that is configured, enabled and, when
// available is non-nil, runnable. It is used only when a lane names no adapter.
func firstAvailableAdapter(c Config, pref []string, available map[string]bool) string {
	for _, name := range pref {
		a, ok := c.Adapters[name]
		if !ok || !a.IsEnabled() {
			continue
		}
		if available != nil && !available[name] {
			continue
		}
		return name
	}
	return ""
}

func sortedLaneNames(lanes map[string]Lane) []string {
	names := make([]string, 0, len(lanes))
	for n := range lanes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
