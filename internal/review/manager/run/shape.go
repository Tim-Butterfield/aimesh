package run

import (
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// shapeOf computes what this run WOULD do, for a dry run to disclose. See review.RunShape for
// what the numbers mean and why the call count is a range.
//
// It runs at the dry-run stop, so it is handed the ALREADY-RESOLVED plan and panel: it invents
// nothing and re-resolves nothing. It has two remaining failures, and both are things a dry run
// exists to hand over for free: the panel round budget (a budget below the seat count would
// silently drop a seat) and a containment refusal while previewing the payload — the same refusal
// the real run's collector would hit, at the same rules, before rather than after the copy.
func (m *Manager) shapeOf(req Request, plan review.RunPlan, seats []review.LaneResolution, maxOuter int) (review.RunShape, error) {
	maxInner := 1
	if m.Cfg.Review.MaxInnerIterations != nil && *m.Cfg.Review.MaxInnerIterations > 0 {
		maxInner = *m.Cfg.Review.MaxInnerIterations
	}
	// The same budget the real cycle builds, computed here for the same reason the real cycle
	// builds it before spending: a panel that cannot give every seat one round is a config error,
	// and a dry run exists to hand those over for free.
	budget, berr := newPanelBudget(m.Cfg.Review.MaxPanelRounds, maxInner, len(seats))
	if berr != nil {
		return review.RunShape{}, berr
	}

	shape := review.RunShape{
		Mode:        plan.Mode,
		Surface:     plan.Surface,
		MaxParallel: req.MaxParallel,
		PanelRounds: budget.total,
		Writes:      plan.Mode == review.ModePatch || plan.Mode == review.ModeApply,
	}
	for _, s := range seats {
		shape.Seats = append(shape.Seats, review.ShapeSeat{
			SeatID: s.SeatID, Adapter: s.Adapter, Model: s.Model, Effort: s.Effort,
		})
	}
	// RoleReviewer is the panel's FIRST SEAT kept in the role map as a compatibility alias (see
	// review.RunPlan), so listing it here would report seat 1 twice — once as a seat and once as
	// a lane — and invite a reader to add it into the total.
	for _, role := range sortedRoles(plan.Lanes) {
		if role == review.RoleReviewer {
			continue
		}
		l := plan.Lanes[role]
		shape.Lanes = append(shape.Lanes, review.ShapeLane{
			Role: role, Execution: l.Execution, Adapter: l.Adapter, Model: l.Model, Effort: l.Effort,
		})
	}

	shape.Egress = shapeEgress(shape.Seats, shape.Lanes)

	// A host adjudication costs a call only when there is a host lane AND it is not the
	// deterministic `fake` adapter — adjudicate() returns the in-process judgement without
	// invoking anything in either other case, so counting one there would overstate every
	// default-configured run.
	hostLane, hasHost := plan.Lanes[review.RoleAuthorRemediator]
	hostCall := 0
	if hasHost && hostLane.Adapter != "fake" {
		hostCall = 1
	}
	shape.DeterministicHost = hostCall == 0
	_, hasCC := plan.Lanes[review.RoleCrossCheck]
	_, hasVerifier := plan.Lanes[review.RoleVerifier]

	// FLOOR: every seat runs exactly one round, every informed lane runs once, and every
	// adjudication is deterministic because nothing contested was found. This is a run that
	// converges immediately, which is the ordinary run.
	cycleMin := len(seats)
	// CEILING: the panel spends its whole shared round budget, and each informed lane's output is
	// contested enough to cost its own host adjudication.
	cycleMax := budget.total + hostCall
	if req.IncludeHostReview && hasHost {
		cycleMin++
		cycleMax += 1 + hostCall
	}
	if hasCC {
		cycleMin++
		cycleMax += 1 + hostCall
	}
	if hasVerifier {
		cycleMin++
		cycleMax += 1 + hostCall
	}

	// Report and patch commit nothing, so the outer cycle breaks after one pass whatever
	// maxOuterCycles says. Only apply can iterate.
	shape.OuterCycles = 1
	if plan.Mode == review.ModeApply {
		shape.OuterCycles = maxOuter
	}

	// Run-level calls, outside the cycle: the final self-critique re-checks the host's own
	// dismissals (nothing dismissed → no call), and the ACP validation probe fires only for the
	// validator that asks for it.
	runLevelMax := 0
	if req.ValidateHostAdjudication {
		if plan.Mode == review.ModeReport {
			runLevelMax += hostCall
		}
	} else {
		runLevelMax += hostCall
	}

	// READINESS PROBES are model calls, so they are priced in BOTH bounds: unlike everything else in
	// the ceiling they are not contingent on anything, so a run that enables them cannot make fewer.
	// A dry run counts them and performs none of them (they spend; see Request.VerifyReadiness).
	if req.VerifyReadiness {
		shape.ReadinessProbes = len(probeTargets(plan, seats))
	}
	shape.MinModelCalls = cycleMin + shape.ReadinessProbes
	shape.MaxModelCalls = shape.OuterCycles*cycleMax + runLevelMax + shape.ReadinessProbes

	// WHAT THOSE CALLS WOULD CARRY. Priced separately from the calls themselves because it varies
	// independently of them: the same panel over a monorepo and over one package costs the same
	// and shows the reviewers something entirely different. The preview walks the LIVE tree under
	// the collector's own rules and copies nothing — see workspace.PreviewPayload.
	// The waiver is passed through so the preview describes the tree the RUN would be allowed to
	// copy. Without it a dry run of a protected root would refuse while the real run succeeded —
	// the exact inequivalence PreviewPayload exists to rule out.
	pay, perr := workspace.PreviewPayloadWith(req.Workspace, req.AllowProtectedPaths)
	if perr != nil {
		return review.RunShape{}, collectFault(perr)
	}
	shape.Payload = payloadOf(pay)
	return shape, nil
}

// payloadOf projects the workspace preview into the shape's disclosure. Paths are sorted here (the
// preview keeps walk order), so the artifact is deterministic and two dry runs of the same
// workspace are comparable.
func payloadOf(p workspace.Payload) review.ShapePayload {
	out := review.ShapePayload{Files: len(p.Files), Bytes: p.TotalBytes, Paths: []string{}}
	for _, f := range p.SortedFiles() {
		out.Paths = append(out.Paths, f.Path)
	}
	for _, c := range p.Withheld {
		out.Withheld = append(out.Withheld, c.String())
	}
	return out
}
