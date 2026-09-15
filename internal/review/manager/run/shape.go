package run

import (
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// shapeOf computes what this run would do, for a dry run to disclose (see review.RunShape). It uses
// the already-resolved plan and panel. It fails on the same round-budget error and containment refusal
// the real run would hit.
func (m *Manager) shapeOf(req Request, plan review.RunPlan, seats []review.LaneResolution, maxOuter int) (review.RunShape, error) {
	maxInner := 1
	if m.Cfg.Review.MaxInnerIterations != nil && *m.Cfg.Review.MaxInnerIterations > 0 {
		maxInner = *m.Cfg.Review.MaxInnerIterations
	}
	// The same round budget the real cycle builds.
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
	// RoleReviewer aliases the panel's first seat in the role map (see review.RunPlan), so skip it to
	// avoid listing seat 1 twice.
	for _, role := range sortedRoles(plan.Lanes) {
		if role == review.RoleReviewer {
			continue
		}
		l := plan.Lanes[role]
		shape.Lanes = append(shape.Lanes, review.ShapeLane{
			Role: role, Execution: l.Execution, Adapter: l.Adapter, Model: l.Model, Effort: l.Effort,
		})
	}

	shape.SkippedSteps = review.SkippedOptionalSteps(plan)
	shape.Egress = shapeEgress(shape.Seats, shape.Lanes)

	// Host adjudication costs a call only with a non-fake host lane; otherwise adjudicate decides in
	// process.
	hostLane, hasHost := plan.Lanes[review.RoleAuthorRemediator]
	hostCall := 0
	if hasHost && hostLane.Adapter != "fake" {
		hostCall = 1
	}
	shape.DeterministicHost = hostCall == 0
	_, hasCC := plan.Lanes[review.RoleCrossCheck]
	_, hasVerifier := plan.Lanes[review.RoleVerifier]

	// Floor: every seat runs one round, every informed lane runs once, and no adjudication call is
	// needed.
	cycleMin := len(seats)
	// Ceiling: the panel spends its whole round budget and every lane's output needs a host
	// adjudication.
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

	// Run-level calls outside the cycle: the final self-critique (only when something was dismissed),
	// or the host readiness check when ValidateHostAdjudication is set.
	runLevelMax := 0
	if req.ValidateHostAdjudication {
		if plan.Mode == review.ModeReport {
			runLevelMax += hostCall
		}
	} else {
		runLevelMax += hostCall
	}

	// Readiness probes always run when enabled, so they count in both bounds.
	if req.VerifyReadiness {
		shape.ReadinessProbes = len(probeTargets(plan, seats))
	}
	shape.MinModelCalls = cycleMin + shape.ReadinessProbes
	shape.MaxModelCalls = shape.OuterCycles*cycleMax + runLevelMax + shape.ReadinessProbes

	// Preview the payload the calls would carry by walking the live tree under the collector's rules,
	// copying nothing. The protected-path waiver is passed so the preview matches the real run.
	pay, perr := workspace.PreviewPayloadWith(req.Workspace, req.AllowProtectedPaths)
	if perr != nil {
		return review.RunShape{}, collectFault(perr)
	}
	shape.Payload = payloadOf(pay)
	return shape, nil
}

// payloadOf projects the workspace preview into the shape, with paths sorted so dry runs are
// comparable.
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
