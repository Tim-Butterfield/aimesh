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
	// Available is the set of adapter names whose binary is actually runnable now
	// (name → true). Supplied by the Manager from the adapter registry so the
	// auto-detect fallback (firstAvailableAdapter) skips configured-but-missing
	// adapters. When nil, availability is not consulted (config-only resolution).
	Available map[string]bool
	// ReviewerPanel is an AD-HOC blind-primary panel composed by the invocation (CLI
	// `--reviewer`, ACP `_meta.reviewmesh.panel`). When set it REPLACES the profile's panel
	// entirely — it never merges with it, so "what runs" is one authored list, not a diff a
	// user has to compute.
	//
	// COMPOSE-NOT-CONFIGURE applies to the ADAPTER: each seat may only name an adapter the
	// configuration defines, and an unknown one is a config error before any spend, exactly as it
	// is for a profile-authored seat. The MODEL is different — a composed seat may name a model the
	// catalog does not define, and it is handed to the adapter verbatim and recorded as
	// pass-through. The adapter is what carries trust and identity-evidence capability; requiring a
	// model STRING to be pre-registered establishes nothing the identity layer does not establish
	// independently, and it made panels the operator wanted unexpressible without editing global
	// config mid-task. A profile's own lanes stay held to the catalog: a config file declares the
	// vocabulary it is then read against.
	ReviewerPanel []review.SeatSpec
}

var modeRank = map[review.Mode]int{
	review.ModeReport: 0, review.ModePatch: 1, review.ModeApply: 2,
}

func minMode(a, b review.Mode) review.Mode {
	if modeRank[a] <= modeRank[b] {
		return a
	}
	return b
}

// maxMode returns the higher-privilege of two modes. It exists for exactly one caller —
// SurfaceCeiling, where a granted policy capability RAISES a ceiling — so that "raise" is a single
// named operation rather than an inline comparison somewhere in the write path.
func maxMode(a, b review.Mode) review.Mode {
	if modeRank[a] >= modeRank[b] {
		return a
	}
	return b
}

// Resolve turns config + request into a RunPlan, or a Config fault if a profile,
// model, or adapter cannot be resolved.
func (c Config) Resolve(req ResolveRequest) (review.RunPlan, error) {
	profName := c.selectProfile(req.Profile, req.Available)
	prof, ok := c.Profiles[profName]
	// The shipped-but-hidden fake-smoke profile is an INTERNAL test harness: without the internal
	// gate (fake.Enabled — set by tests/golden/the ACP-validation parent, never by users) it fails
	// resolution with the SAME unknown-profile error as any other unrecognized name.
	if ok && IsHiddenProfile(profName) && !fake.Enabled() {
		ok = false
	}
	if !ok {
		return review.RunPlan{}, fault.New(fault.Config, fmt.Sprintf("profile %q not found in config%s", profName, ProfileNotFoundGuidance(profName)))
	}

	plan := review.RunPlan{Surface: req.Surface, Lanes: map[review.Role]review.LaneResolution{}}

	for _, roleName := range sortedLaneNames(prof.Lanes) {
		lane := prof.Lanes[roleName]
		role := review.Role(roleName)

		// A COMPOSED PANEL REPLACES THE PROFILE'S REVIEWER LANE, so do not demand that lane
		// resolve first. It used to: `--reviewer adapter=…,model=…` against a profile whose
		// `lanes.reviewer` is unconfigured failed with `no adapter resolvable for role "reviewer"`
		// — refusing to use the panel the caller had just composed because of a lane it was about
		// to overwrite (the alias is rebuilt from seat 1 below).
		//
		// That fired on exactly the fresh-install path the panel flags exist to serve: the flag's
		// own help says a panel is composed OR selected, so a user who composes one should not also
		// need a configured profile. It was masked by alphabetical lane order — `author_remediator`
		// failed first, so the reviewer failure only appeared once that one was satisfied.
		if role == review.RoleReviewer && len(req.ReviewerPanel) > 0 {
			continue
		}

		adapter := lane.Adapter
		if ov := req.AdapterOverride[role]; ov != "" {
			adapter = ov
		}
		model := lane.Model
		if ov := req.ModelOverride[role]; ov != "" {
			model = ov
		}
		// A ROLE LANE comes from the profile, so composed is false: the config declares the
		// vocabulary it is read against, and an unknown key there is a typo worth catching.
		res, err := c.resolveSeat(prof, role, fmt.Sprintf("role %q", roleName), lane.Execution, adapter, model, "", req.Available, false)
		if err != nil {
			return review.RunPlan{}, err
		}
		plan.Lanes[role] = res
	}

	// REVIEWER-PANEL COMPATIBILITY ALIAS. `Lanes[reviewer]` is the panel's FIRST SEAT so every
	// role-shaped consumer (preflight, doctor, privacy, the ACP plan view) keeps working with a
	// panel it does not know about. It is filled here only when the panel is not already the
	// legacy `lanes.reviewer` — i.e. when the profile uses the `reviewers` spelling or the
	// invocation composed an ad-hoc panel — so a legacy profile's resolution is byte-identical
	// to a pre-panel build. The SeatID stays EMPTY on the alias: the roster (ResolvePanel) is
	// the only place a seat is named.
	if len(req.ReviewerPanel) > 0 || prof.HasPanelSpelling() {
		seats, perr := c.ResolvePanel(req)
		if perr != nil {
			return review.RunPlan{}, perr
		}
		first := seats[0]
		first.SeatID = ""
		plan.Lanes[review.RoleReviewer] = first
	}

	// effective mode = min(requested, surface ceiling). The ceiling is the surface's configured mode
	// entry, RAISED by any policy capability the config grants that surface — one evaluation point,
	// in SurfaceCeiling, so a capability can never become an exception that bypasses the ceiling
	// somewhere else.
	cap := c.SurfaceCeiling(req.Surface)

	// AN UNSPECIFIED MODE IS `report`, ON EVERY SURFACE. It used to be the CEILING, which meant the
	// CLI — whose ceiling is `apply` so that `--apply` can work at all — WROTE to the user's tree
	// when no mode flag was given. `aimesh review run .` modified files, and nothing in the command
	// the user typed says "write".
	//
	// Naming the command is consent to REVIEW; it is not consent for a model to edit your files.
	// The other two surfaces already refused that inference, and there is no reason a CLI user's
	// consent should be read more broadly than an ACP host's.
	//
	// The CEILING is deliberately unchanged. Default and ceiling were the same value here, so
	// lowering the ceiling to make the default safe would have clamped an explicit `--apply` down
	// to `report` and broken writing entirely. They are two different questions: the ceiling is the
	// most this surface may EVER do, the default is what it does when not told.
	requested := req.Mode
	if requested == "" {
		requested = review.ModeReport
	}
	plan.Mode = minMode(requested, cap)
	return plan, nil
}

// resolveSeat resolves ONE (adapter, model, effort) assignment — a role lane or a panel seat —
// into a LaneResolution. It is the single resolution rule: a panel seat and a role lane can
// never diverge in how an adapter is defaulted, a catalog entry is looked up, an empty modelArg
// is refused, or a devin-cli slug is rendered, because there is only one implementation.
// `label` names the thing being resolved in every error message (a role name, or "reviewer seat
// 2 of 3"), so a failure points at the exact slot the user has to fix.
//
// effortOverride, when non-empty, wins over the catalog defaults — that is how a panel seat
// composes the SAME model at a different reasoning effort as an independent vantage.
// nearestCatalogKey names the catalog key `model` most plausibly meant, or "" when nothing is close.
//
// It answers ONE measured confusion rather than trying to be a spell-checker. Catalog keys EMBED
// effort ("claude-opus-5-high"), while the flag documents model and effort as separate fields — so
// the documented shape names a key that does not exist, and the key that does exist is the one the
// user wrote plus a suffix. That is a PREFIX relationship, checked exactly:
//
//	model=claude-opus-5  →  claude-opus-5-high
//
// The remainder must be exactly ONE hyphen-separated segment — the shape of an omitted effort label.
// That bound is what keeps the guess honest, and it was arrived at by a test catching the two ways a
// looser rule is wrong:
//
//	"gpt"                → NOT "gpt-5-codex"            (remainder "-5-codex" is two segments: a
//	                                                     different model that merely starts the same)
//	"claude-opus-5-high" → NOTHING                      (an exact key needs no correction, even
//	                                                     though "claude-opus-5-high-ext" prefixes it)
//
// Case-insensitive, shortest match first. A confident wrong name is worse than no suggestion in a
// message whose entire value is being right.
func (c Config) nearestCatalogKey(model string) string {
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" {
		return ""
	}
	for key := range c.ModelCatalog {
		if strings.EqualFold(key, want) {
			return "" // it IS a key; nothing to suggest
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

// composed is true when the CALLER supplied this seat at the surface (`--reviewer`, an MCP `panel`)
// rather than a profile declaring it. It changes exactly one thing: an unknown model key is passed
// through to the adapter instead of refused.
func (c Config) resolveSeat(prof Profile, role review.Role, label, execution, adapter, model, effortOverride string, available map[string]bool, composed bool) (review.LaneResolution, error) {
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
	entry, ok := c.ModelCatalog[model]
	if !ok {
		// A COMPOSED seat passes the model through. See LaneResolution.ModelPassThrough for why
		// this splits on who wrote the seat rather than on what the string looks like: the adapter
		// (validated fail-closed just above) is what carries trust, and requiring a model string to
		// be pre-registered establishes nothing the identity layer does not establish independently
		// — which is why exploremesh's `--explorer` has always worked this way.
		if composed {
			return review.LaneResolution{
				Role: role, Execution: execution, Adapter: adapter,
				Model: model, ModelArg: review.ModelArg(model), Effort: effortOverride,
				ModelPassThrough: true, PassThroughHint: c.nearestCatalogKey(model),
			}, nil
		}
		hint := ""
		if near := c.nearestCatalogKey(model); near != "" {
			// The measured confusion this answers: catalog keys EMBED effort, so the documented
			// shape (model=claude-opus-5, effort=high) names a key that does not exist while
			// "claude-opus-5-high" does. Naming it turns a round trip into an edit.
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

	// Effort/thinking: an explicit per-seat effort wins, else the per-adapter model setting,
	// else the catalog entry's default. Opaque to the core; the recipe decides whether/how to
	// pass it.
	effort := effortOverride
	if effort == "" {
		effort = am.Effort
	}
	if effort == "" {
		effort = entry.Effort
	}

	// devin-cli is **name-bound**: its `--model` slug encodes the effort. Render the
	// configured display name (am.ModelArg) + effort into the final lowercase-hyphenated
	// slug here (adapter-specific resolution), so the recipe receives a ready arg and
	// emits no separate effort flag.
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

// ResolvePanel resolves the ORDERED blind-primary panel for a request: the invocation's ad-hoc
// panel when it supplied one, else the selected profile's `reviewers` (with `lanes.reviewer`
// normalized as a panel of one).
//
// It is FAIL-CLOSED and runs BEFORE any spend. Refused: an empty panel, a panel above
// review.MaxReviewerSeats, a duplicate seat identity (an identical adapter+model+effort
// triple adds no independent vantage, so counting it twice would inflate agreement), and any
// seat that does not resolve. There is no clamping and no dropping anywhere in this function:
// the count a caller asked for is the count that runs, or the run does not start.
func (c Config) ResolvePanel(req ResolveRequest) ([]review.LaneResolution, error) {
	profName := c.selectProfile(req.Profile, req.Available)
	prof, ok := c.Profiles[profName]
	if !ok && len(req.ReviewerPanel) == 0 {
		return nil, fault.New(fault.Config, fmt.Sprintf("profile %q not found in config%s", profName, ProfileNotFoundGuidance(profName))).
			WithReason("profile_not_found")
	}

	// The seat list, from the invocation when it composed one, else from the profile.
	type seatIn struct {
		adapter, model, effort, execution string
		// composed marks a seat the CALLER wrote at the surface. Only these pass an unknown model
		// through; a profile's own lanes are held to the catalog, because a config file declares
		// the vocabulary it is then read against.
		composed bool
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

	// EVERY unresolvable seat is reported, not just the first.
	//
	// Resolution is pure configuration lookup — it starts no process and spends nothing — so the
	// blockers are all knowable in one pass. Returning on the first one made an N-seat panel with N
	// bad seats cost N round trips: fix, re-run, discover the next. An operator (or a model driving
	// the CLI) should learn everything wrong with their panel from one refusal.
	out := make([]review.LaneResolution, 0, len(seats))
	var failures []error
	for i, s := range seats {
		label := fmt.Sprintf("reviewer seat %d of %d", i+1, len(seats))
		exec := s.execution
		if exec == "" {
			exec = "adapter"
		}
		// Per-lane --set overrides address the ROLE, so they apply to a one-seat panel only:
		// there is no unambiguous way to point `--set reviewer.adapter=x` at one of N seats,
		// and silently applying it to all of them would collapse the panel's independence.
		adapter, model := s.adapter, s.model
		if len(seats) == 1 && len(req.ReviewerPanel) == 0 {
			if ov := req.AdapterOverride[review.RoleReviewer]; ov != "" {
				adapter = ov
			}
			if ov := req.ModelOverride[review.RoleReviewer]; ov != "" {
				model = ov
			}
		}
		res, err := c.resolveSeat(prof, review.RoleReviewer, label, exec, adapter, model, s.effort, req.Available, s.composed)
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

	// Duplicate detection runs AFTER every seat resolved, because an identity cannot be computed for
	// a seat that did not resolve — reporting "seats 2 and 4 clash" while seat 3 is unresolvable
	// would be describing a panel that does not exist yet.
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

// unresolvableSeats folds every seat failure into one refusal.
//
// The REASON CODE is the first failure's, deliberately: it is the same code this function returns
// today for the same configuration, so anything keying on `--json` reasonCode keeps working, and a
// caller that fixes what the message lists gets past all of them at once. The enumeration lives in
// the message, where a human or a model reads it.
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

// SeatID is the stable identifier of the i-th (0-based) panel seat. Seat 1 keeps the bare role
// name so a panel of one is indistinguishable from the historical single reviewer everywhere it
// is recorded.
func SeatID(i int) string {
	if i == 0 {
		return string(review.RoleReviewer)
	}
	return fmt.Sprintf("%s-%d", review.RoleReviewer, i+1)
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

// firstAvailableAdapter returns the first adapter in pref that is configured,
// enabled, and (when available != nil) actually runnable now. This is the
// auto-detect fallback used only when a lane does not name an explicit adapter.
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
