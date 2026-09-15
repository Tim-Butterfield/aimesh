// Package run owns the review-and-remediate sequence: resolve, preflight, contain, invoke the
// reviewers, verify identity, adjudicate, report/patch/apply, and record the audit trail.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/rootfile"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudicationprompt"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/remediationprompt"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/reviewprompt"
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
	"github.com/Tim-Butterfield/aimesh/internal/review/utility/doctor"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
	"github.com/Tim-Butterfield/aimesh/meshcore/verify"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// Manager orchestrates a review run.
type Manager struct {
	Cfg         config.Config
	Adapters    map[string]model.Adapter
	ArtifactDir string // base for run directories when ArtifactDirFor is unset
	// ArtifactDirFor, when set, chooses the base for run directories per run from that run's workspace.
	// A server that serves many workspaces sets it so each run's record lands beside the workspace it
	// reviewed, never beside wherever the process started.
	ArtifactDirFor func(workspace string) string
	TempBase       string // base for isolated copies ("" = OS temp)
	Now            func() time.Time
}

// Request is one review invocation.
type Request struct {
	Workspace string
	Mode      review.Mode
	Surface   string
	// RunID, when set, names this run's directory. MCP supplies it so the run id it returns at
	// admission also names the on-disk record, which lets `review_remediate {fromRun}` read an evicted
	// run's decision set without disclosing a path. It is server-generated, never caller-supplied, and
	// validated as a single path element (see validateRunID). Empty generates an id from the start time.
	RunID           string
	Profile         string
	AdapterOverride map[review.Role]string // per-lane --set role.adapter=NAME
	ModelOverride   map[review.Role]string // per-lane --set role.model=NAME
	// ReviewerPanel is a blind primary panel composed by the invocation (CLI `--reviewer`, an MCP or
	// ACP call's panel). When present it replaces the profile's panel rather than merging with it. Each
	// seat must name an available adapter, and its model string is passed through verbatim.
	ReviewerPanel []review.SeatSpec
	// ComposedRoles are the single-slot role seats an MCP or ACP call composed. When non-nil the
	// run resolves against those seats and ReviewerPanel alone, never a saved profile — see
	// config.ResolveRequest.ComposedRoles.
	ComposedRoles map[review.Role]review.SeatSpec
	// MaxParallel bounds how many seats invoke their model CLI at once; 0 runs every seat in
	// parallel. It is per invocation because the right value depends on the caller's machine and
	// provider rate limits. It bounds parallelism only: every seat still runs.
	MaxParallel int
	// Authority are the documents this review is judged against, such as requirements and specs.
	// They are context, never targets: they are embedded in prompts and never placed in the
	// containment copy, so no write can reach them. internal/review/engine/authority applies the same
	// rules for every surface.
	Authority []review.AuthorityDoc
	// TrustedRoots are the directories an agent surface's request may read from. When set, every
	// request-supplied path this run reads (today, authority `path` documents) must resolve inside
	// them, so a request can narrow but never widen them. It is empty on the CLI, where the user who
	// typed the path is the consent. It need not contain the workspace, because an agent surface may
	// review a directory it materialized (ACP `inlineWorkspace`) and validates the workspace itself.
	TrustedRoots []string
	// WorkspaceEphemeral marks a workspace this process materialized for the turn and deletes
	// afterwards (ACP `inlineWorkspace`). It is recorded on the decision set so a later fromRun write
	// is refused by name rather than failing on a vanished path.
	WorkspaceEphemeral bool
	// Select, when non-nil, narrows the write set to accepted findings whose host-computed
	// fingerprint it names; nil applies everything the adjudication authorizes, and an empty non-nil
	// slice is a refusal. Surfaces refuse it on a report run. It filters this run's own adjudication:
	// a fingerprint from an earlier run matches only if this run raises the same finding again, and
	// lands in `unmatched` otherwise. Only `review_remediate {fromRun}` replays a stored decision set.
	Select []string
	// VerifyCommands are the project's own build/test commands, supplied by the operator and run on
	// the containment copy; empty runs nothing, and nothing model-authored is ever executed (see
	// verify.go). A write run runs them before and after its edits and reports the delta; a report
	// run runs them only when VerifyBaseline is set. The result never gates anything: it does not
	// invalidate a finding, stop a commit, or change an exit code.
	VerifyCommands []string
	// VerifyTimeout bounds one command (0 means DefaultVerifyTimeout).
	VerifyTimeout time.Duration
	// VerifyBaseline runs the commands once on a report run. A write run ignores it.
	VerifyBaseline bool
	// AllowProtectedPaths waives the protected-config half of containment: it admits a workspace root
	// that is or sits inside `.git`, `.claude`, `.vscode` and similar trees, and permits writes there.
	// It never admits secrets (`.env*`, `.ssh`, `.aws`, key material) for reads or writes, and never
	// widens root confinement. It is an operator grant on every surface because files such as
	// `.git/hooks/**` execute code later.
	AllowProtectedPaths bool
	// Scope narrows which files reviewers are shown (explicit paths, a time window, or a
	// version-control baseline); the zero value shows everything. See scope.go.
	Scope Scope
	// scopeFiles is Scope resolved once by RunContext against the live workspace, so every pass sees
	// the same set; nil means no narrowing.
	scopeFiles map[string]bool
	// IncludeHostReview adds a report-only host self-review pass whose findings carry
	// source=author_self_review. It is rejected for patch and apply before any model call.
	IncludeHostReview bool
	// ValidateHostAdjudication runs one synthetic host-adjudication call when the author_remediator
	// made no natural adjudication call, proving the host adapter can run its role. It is a
	// report-only readiness check, off for normal reviews, and distinct from IncludeHostReview.
	ValidateHostAdjudication bool
	// Trace is the caller's W3C trace context when the request carried one (a `2026-07-28` `_meta`
	// convention). It is recorded on the run and never interpreted; nil records nothing.
	Trace *review.Trace
	// DryRun resolves the plan, panel, authority documents and static preflight, writes the same
	// resolved-plan.json, config-effective.json and doctor.json, then stops before the first model call
	// and returns RunOutcome.Shape. The stop sits after the last configuration error knowable without
	// spending, so a dry run returns either the shape or the refusal the real run would hit. It is not
	// a mode: Mode still says what the run would do, so a dry run can price an apply.
	DryRun bool
	// VerifyReadiness asks every configured agent to answer a bounded one-token call in a throwaway
	// directory before the first seat is dispatched (see meshcore/model.DeepProber). Available only
	// proves a binary exists, while login, folder trust and model validity fail at invocation, so this
	// avoids paying for one seat's full prompt before another seat fails. It spends one call per
	// distinct adapter/model/effort, so it is opt-in; a dry run prices it without performing it.
	VerifyReadiness bool
	// OnEvent, if set, receives each audit event as it is logged, so a surface can stream progress.
	// It must not block or panic, and must be safe for concurrent calls: panel seats emit from
	// separate goroutines.
	OnEvent func(audit.EventLine)
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Run executes the review with a background context (CLI default path).
func (m *Manager) Run(req Request) (review.RunOutcome, error) {
	return m.RunContext(context.Background(), req)
}

// RunContext executes the review. Cancelling ctx halts an in-flight run. On a halt it returns a
// *fault.Fault, and the outcome still carries the run directory and any findings.
func (m *Manager) RunContext(ctx context.Context, req Request) (review.RunOutcome, error) {
	// RunID becomes a directory name, so validate it before anything else runs.
	if rerr := validateRunID(req.RunID); rerr != nil {
		return review.RunOutcome{}, rerr
	}
	// Record each adapter's availability so resolution skips configured adapters whose binaries
	// are missing.
	available := make(map[string]bool, len(m.Adapters))
	for name, a := range m.Adapters {
		ok, _ := a.Available()
		available[name] = ok
	}
	resolveReq := config.ResolveRequest{
		Profile: req.Profile, Mode: req.Mode, Surface: req.Surface, Available: available,
		AdapterOverride: req.AdapterOverride, ModelOverride: req.ModelOverride,
		ReviewerPanel: req.ReviewerPanel, ComposedRoles: req.ComposedRoles,
	}
	plan, err := m.Cfg.Resolve(resolveReq)
	if err != nil {
		return review.RunOutcome{}, err
	}

	if _, ok := plan.Lanes[review.RoleReviewer]; !ok {
		return review.RunOutcome{}, fault.New(fault.Config, "resolved plan has no reviewer lane").
			WithReason("plan_missing_reviewer_lane")
	}
	// Resolve the blind primary panel before any spend. An unresolvable seat, an out-of-range size or
	// a duplicate identity is a config error; the panel is never trimmed (see config.ResolvePanel).
	seats, perr := m.Cfg.ResolvePanel(resolveReq)
	if perr != nil {
		return review.RunOutcome{}, perr
	}
	// includeHostReview is report-only. Surfaces check the requested mode; this check uses the
	// effective mode, which only resolution knows.
	if req.IncludeHostReview && plan.Mode != review.ModeReport {
		return review.RunOutcome{}, fault.New(fault.Config,
			fmt.Sprintf("includeHostReview is only valid in report mode (effective mode is %q)", plan.Mode)).
			WithReason("host_self_review_mode_invalid")
	}
	// Validate authority declarations on the effective mode (inline `content` is report-only).
	// Surfaces run the same check earlier; this one cannot be bypassed.
	if err := authority.Validate(req.Authority, plan.Mode); err != nil {
		return review.RunOutcome{}, err
	}
	// Preflight every lane's adapter, so a missing or unavailable binary fails before any run work.
	checked := map[string]bool{}
	preflightAdapter := func(name, label string) error {
		if checked[name] {
			return nil
		}
		checked[name] = true
		a, ok := m.Adapters[name]
		if !ok {
			return fault.New(fault.Adapter,
				fmt.Sprintf("adapter %q (%s) is not registered", name, label)).
				WithReason("adapter_not_registered")
		}
		if avail, detail := a.Available(); !avail {
			return fault.New(fault.Adapter,
				fmt.Sprintf("adapter %q (%s) is unavailable: %s", name, label, detail)).
				WithHalt("A").WithReason("adapter_unavailable")
		}
		return nil
	}
	for _, role := range sortedRoles(plan.Lanes) {
		if aerr := preflightAdapter(plan.Lanes[role].Adapter, fmt.Sprintf("role %q", role)); aerr != nil {
			return review.RunOutcome{}, aerr
		}
	}
	// Preflight every seat's adapter too, so a later seat's missing binary fails before the first
	// seat is paid for.
	for i, s := range seats {
		if aerr := preflightAdapter(s.Adapter, fmt.Sprintf("reviewer seat %d of %d", i+1, len(seats))); aerr != nil {
			return review.RunOutcome{}, aerr
		}
	}

	// Resolve scope once, before any spend, so a selector that names nothing or a baseline in a tree
	// without version control refuses for free.
	var scopeSummary *ScopeSummary
	if !req.Scope.Empty() {
		selected, serr := req.Scope.Resolve(ctx, req.Workspace)
		if serr != nil {
			return review.RunOutcome{}, serr
		}
		req.scopeFiles = selected
		available, _ := walkRelative(req.Workspace)
		scopeSummary = summarizeScope(req.Scope, selected, len(available))
	}

	startedAt := m.now()
	// An apply into a tree without version control can only be undone with the run's
	// patches/changes.patch, and that tree's run directory would otherwise default to the OS temp
	// directory. EnsureDurableRunDir gives it a durable state directory; it is a no-op for other
	// trees, and on failure the run keeps its temp-directory artifacts.
	artifactDir := m.artifactDirFor(req.Workspace)
	if durable, ok := EnsureDurableRunDir(ctx, req.Workspace, string(plan.Mode), artifactDir); ok {
		artifactDir = durable
	}
	run, err := audit.NewRun(artifactDir, req.RunID, startedAt)
	if err != nil {
		return review.RunOutcome{}, fault.Wrap(fault.Internal, "create run dir", err)
	}
	run.OnEvent = req.OnEvent
	// Trace and the scope disclosure ride the outcome from the start, so a halted run can still be
	// correlated and is not read as covering the whole tree.
	outcome := review.RunOutcome{Mode: plan.Mode, Plan: plan, RunDir: run.Dir, RunID: run.ID, Status: "running", Trace: req.Trace}
	if scopeSummary != nil {
		outcome.Scope = scopeSummary
		_ = run.WriteJSON("scope.json", scopeSummary)
		_ = run.Event(m.now(), "info", "scope_selected", fmt.Sprintf(
			"scope: %d of %d file(s) selected — reviewers see only these, and the result says nothing about the rest",
			scopeSummary.Selected, scopeSummary.Available),
			map[string]any{"selected": scopeSummary.Selected, "available": scopeSummary.Available})
	}

	_ = run.WriteJSON("resolved-plan.json", plan)
	_ = run.WriteJSON("config-effective.json", m.Cfg) // effective config — audit artifacts are JSON by design
	_ = run.Event(startedAt, "info", "run_started", "review run started", map[string]any{"mode": plan.Mode, "workspace": req.Workspace})
	_ = run.Event(m.now(), "info", "resolved_plan", "resolved role→adapter→model", planData(plan))

	// Resolve authority documents before any model call: read each root-scoped path, verify any
	// expectedHash pin, and enforce the embedding budget without truncation. The inclusion manifest is
	// recorded and rides the outcome. When TrustedRoots are set, one resolver built from them governs
	// every request-supplied path.
	var trust *scope.Resolver
	if len(req.TrustedRoots) > 0 {
		t, terr := scope.New(req.TrustedRoots...)
		if terr != nil {
			return m.halt(run, &outcome, startedAt, "",
				fault.Wrap(fault.Config, "resolve trusted roots", terr).
					WithHalt(ScopeHaltClass).WithReason(string(scope.ReasonUnresolvable)))
		}
		trust = t
	}
	auth, aerr := authority.Resolve(authority.Input{Docs: req.Authority, Mode: plan.Mode, Workspace: req.Workspace, Trust: trust})
	if aerr != nil {
		return m.halt(run, &outcome, startedAt, "", aerr)
	}
	if !auth.Empty() {
		outcome.Authority = auth.Manifest()
		_ = run.WriteJSON("authority/inclusion-manifest.json",
			map[string]any{"schemaVersion": 1, "runId": run.ID, "documents": auth.Manifest()})
		_ = run.Event(m.now(), "info", "authority_resolved", "authority/context documents embedded as quoted evidence",
			map[string]any{"documents": auth.SortedNames(), "count": len(auth.All())})
	}

	// Static preflight judges the plan resolved above, so role overrides and composed panels are
	// checked as this run will invoke them.
	_ = run.Event(m.now(), "info", "doctor_started", "static preflight", nil)
	pre := doctor.Run(doctor.Input{
		Config: m.Cfg, Adapters: m.Adapters, ArtifactDir: m.ArtifactDir,
		Workspace: req.Workspace, Profile: req.Profile,
		Resolved: &plan, ResolvedSeats: seats,
	})
	_ = run.WriteJSON("doctor.json", preflightJSON(pre))
	_ = run.Event(m.now(), "info", "doctor_completed", "static preflight complete", map[string]any{"ok": pre.OK})
	if !pre.OK {
		return m.halt(run, &outcome, startedAt, "",
			fault.New(fault.Config, "preflight failed: "+firstFailure(pre)).WithHalt("A").WithReason("preflight_failed"))
	}

	// Outer convergence cycle: applied fingerprints are fed forward so they become already_addressed,
	// and apply mode iterates until no new edits are made or the outer-cycle cap halts the run.
	maxOuter := 1
	if m.Cfg.Review.MaxOuterCycles != nil && *m.Cfg.Review.MaxOuterCycles > 0 {
		maxOuter = *m.Cfg.Review.MaxOuterCycles
	}
	// The dry-run stop: everything above was free and nothing below is. See Request.DryRun.
	if req.DryRun {
		// Workspace admissibility is a free path check, so it runs here; otherwise a dry run could
		// price a root the real run refuses. It honours the protected-path waiver for the same reason.
		if ref := workspace.AdmitRootWith(req.Workspace, req.AllowProtectedPaths); ref != nil {
			return m.halt(run, &outcome, startedAt, "",
				fault.Wrap(fault.Containment, "the workspace cannot be reviewed", ref).
					WithHalt(ScopeHaltClass).WithReason(string(ref.Reason)))
		}
		shape, serr := m.shapeOf(req, plan, seats, maxOuter)
		if serr != nil {
			return m.halt(run, &outcome, startedAt, "", serr)
		}
		outcome.Shape = &shape
		outcome.Status = "planned"
		_ = run.WriteJSON("run-shape.json", shape)
		_ = run.WriteJSON("run-state.json", runState(run, outcome, startedAt, m.now()))
		_ = run.Event(m.now(), "info", "run_planned", "dry run: plan resolved, nothing spent",
			map[string]any{"seats": len(shape.Seats), "minModelCalls": shape.MinModelCalls, "maxModelCalls": shape.MaxModelCalls})
		return outcome, nil
	}

	// Readiness probing spends, so it sits just past the dry-run stop (shapeOf prices it). It is the
	// cheapest way to learn an agent cannot work before any full prompt is sent.
	if req.VerifyReadiness {
		if rerr := m.verifyReadiness(ctx, run, probeTargets(plan, seats)); rerr != nil {
			return m.halt(run, &outcome, startedAt, "", rerr)
		}
	}

	ws := workspace.New(m.TempBase)
	// Both halves of the protected-path waiver travel together: root admission without the write
	// waiver would copy a tree nothing may edit.
	ws.AllowProtectedRoots = req.AllowProtectedPaths
	// Root confinement: every live write must land inside the workspace and avoid protected paths.
	// req.Workspace is not canonicalized, so the run record quotes the path as given.
	confine, serr := scope.NewWith(scope.Options{AllowProtectedWrites: req.AllowProtectedPaths}, req.Workspace)
	if serr != nil {
		return m.halt(run, &outcome, startedAt, "",
			fault.Wrap(fault.Config, "resolve workspace root", serr).WithHalt("M6").WithReason(string(scope.ReasonUnresolvable)))
	}
	ws.Guard = confine
	addressed := map[string]review.DecisionState{} // fingerprint → state (for Judge)
	addressedDesc := map[string]string{}           // fingerprint → readable line (for prompts)
	// The report-mode verification baseline runs before the cycles, so it describes the tree reviewers
	// are shown. A write run takes its baseline inside the write window instead.
	if plan.Mode == review.ModeReport {
		outcome.Verification = m.reportBaseline(ctx, run, ws, req)
	}
	var last adjudication.Result

	for cycle := 1; cycle <= maxOuter; cycle++ {
		callID := fmt.Sprintf("c-%04d", cycle)
		if err := ctx.Err(); err != nil { // cancelled before this cycle
			return m.halt(run, &outcome, startedAt, callID,
				fault.Wrap(fault.Policy, "review cancelled", err).WithHalt("F").WithReason("cancelled"))
		}
		adj, applied, herr := m.runCycle(ctx, run, ws, req, plan, seats, auth, addressed, addressedDesc, callID, cycle, startedAt, &outcome)
		if herr != nil {
			return outcome, herr
		}
		last = adj
		if plan.Mode == review.ModeReport || plan.Mode == review.ModePatch {
			break // single pass: nothing is committed, so there is nothing to converge
		}
		if applied == 0 {
			break // converged: no new edits this cycle
		}
		if cycle == maxOuter {
			// A one-shot config (cap == 1) intentionally does a single apply pass and
			// succeeds; the cap-halt only signals genuine non-convergence, which needs
			// at least two cycles to demonstrate (apply, then a non-empty re-apply).
			if maxOuter == 1 {
				break
			}
			return m.halt(run, &outcome, startedAt, callID,
				fault.New(fault.Policy, fmt.Sprintf("outer-cycle cap (%d) reached without convergence", maxOuter)).
					WithReason("outer_cycle_cap_reached"))
		}
	}

	// Report-only host readiness check: when the host lane made no natural adjudication call, run one
	// synthetic call. Its output is discarded; a halt propagates.
	if req.ValidateHostAdjudication && plan.Mode == review.ModeReport {
		if herr := m.ensureHostAdjudicationExercised(ctx, run, ws, req, plan, auth, startedAt, &outcome); herr != nil {
			return outcome, herr
		}
	}

	// Final self-critique re-checks the host's own dismissals and surfaces overturned ones report-only.
	// The host readiness check skips it.
	if !req.ValidateHostAdjudication {
		var scErr error
		last, scErr = m.finalSelfCritique(ctx, run, ws, req, plan, auth, last, startedAt, &outcome)
		if scErr != nil {
			return outcome, scErr
		}
	}

	// The post-cycle passes run once over the final set, after merges and id renumbering, and write
	// only labels: panel composition (composition.go), the dissent tally (dissent.go) and citation
	// grounding (grounding.go). None drops, downgrades or reorders a finding. Grounding runs after any
	// write, so on an apply run it describes the tree as it now stands.
	if outcome.Composition = composeAgreement(seats, &last); outcome.Composition != nil && outcome.Composition.Independence == IndependenceSharedModel {
		_ = run.Event(m.now(), "warn", "panel_shares_a_model", fmt.Sprintf(
			"this panel's %d seat(s) run %d distinct model(s) (%s): where seats that share a model agree, their errors correlate, so the agreement is worth less than the count suggests. No count was adjusted",
			len(outcome.Composition.Seats), outcome.Composition.DistinctModels, strings.Join(SharedModels(outcome.Composition.Seats), ", ")),
			map[string]any{
				"seats": len(outcome.Composition.Seats), "distinctModels": outcome.Composition.DistinctModels,
				"sharedModels": SharedModels(outcome.Composition.Seats),
			})
	}
	if outcome.Dissent = DissentSummaryFor(&last); outcome.Dissent != nil && outcome.Dissent.Contested > 0 {
		_ = run.Event(m.now(), "info", "panel_dissent", fmt.Sprintf(
			"%d of %d panel finding(s) are contested: fewer of the seats that ran reported them than did not. A silent seat is not a seat that disagreed, and nothing was dropped, downgraded or reordered",
			outcome.Dissent.Contested, outcome.Dissent.Panelled),
			map[string]any{
				"panelled": outcome.Dissent.Panelled, "unanimous": outcome.Dissent.Unanimous,
				"majority": outcome.Dissent.Majority, "contested": outcome.Dissent.Contested,
			})
	}
	if outcome.Grounding = groundFindings(req.Workspace, &last); outcome.Grounding != nil {
		_ = run.Event(m.now(), "info", "citations_grounded", fmt.Sprintf(
			"citation grounding: %d of %d cited finding(s) point at something that exists; %d could not be resolved (%d finding(s) cite no file). Nothing was dropped or downgraded",
			outcome.Grounding.Grounded, outcome.Grounding.Checked, outcome.Grounding.Unresolved, outcome.Grounding.NoCitation),
			map[string]any{
				"checked": outcome.Grounding.Checked, "grounded": outcome.Grounding.Grounded,
				"unresolved": outcome.Grounding.Unresolved, "noCitation": outcome.Grounding.NoCitation,
				"byStatus": outcome.Grounding.ByStatus,
			})
	}

	_ = run.WriteJSON("decisions/host-adjudication.json", hostAdjudications(last))
	outcome.Findings = last.Findings
	// Decisions are index-aligned with Findings, so surface projections use the same adjudication
	// result as run-state and the summary.
	outcome.Decisions = last.Decisions
	outcome.Status = "stable"
	_ = run.WriteJSON("run-state.json", runState(run, outcome, startedAt, m.now()))
	_ = run.WriteText("review-summary.md", summaryMarkdown(plan, last, req))
	// Record the durable decision set (report runs only) so a later call on any surface can apply
	// the set that was inspected. See decisionset.go.
	m.recordDecisionSet(run, req, plan, outcome)
	_ = run.Event(m.now(), "info", "run_completed", "review run completed", map[string]any{"status": outcome.Status})
	return outcome, nil
}

// runCycle performs one outer cycle: the blind primary panel (seats in parallel, each with its own
// bounded stabilization loop), host adjudication over the union of seat findings, the optional
// cross_check and verifier lanes, then mode handling. It returns the final adjudication and the
// number of edits applied. Host adjudication starts only after every seat has finished, which keeps
// the panel blind.
func (m *Manager) runCycle(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, plan review.RunPlan, seats []review.LaneResolution, auth authority.Set, addressed map[string]review.DecisionState, addressedDesc map[string]string, callID string, cycle int, startedAt time.Time, outcome *review.RunOutcome) (adjudication.Result, int, error) {
	// Capture the reviewed root's identity before any reviewer sees it; the write path re-verifies
	// it, so accepted edits never land in a different tree swapped in under the same path. It is per
	// cycle because a converging apply legitimately rewrites targets between cycles.
	identity, iderr := CaptureWorkspaceIdentity(req.Workspace)
	if iderr != nil {
		_, e := m.halt(run, outcome, startedAt, callID, fault.Wrap(fault.Config,
			"capture the reviewed workspace root's identity", iderr).
			WithHalt(ScopeHaltClass).WithReason(string(scope.ReasonUnresolvable)))
		return adjudication.Result{}, 0, e
	}
	maxInner := 1
	if m.Cfg.Review.MaxInnerIterations != nil && *m.Cfg.Review.MaxInnerIterations > 0 {
		maxInner = *m.Cfg.Review.MaxInnerIterations
	}
	// The round budget bounds the whole panel for this cycle, so N seats cannot multiply
	// maxInnerIterations into unbounded spend.
	budget, berr := newPanelBudget(m.Cfg.Review.MaxPanelRounds, maxInner, len(seats))
	if berr != nil {
		_, e := m.halt(run, outcome, startedAt, callID, berr)
		return adjudication.Result{}, 0, e
	}
	// The inner loop applies no edits between passes: later passes are told what was already reported
	// and asked for new issues, and findings are unioned, which raises recall even in report mode.
	// addressedLines are dispositions settled in earlier outer cycles, as readable lines.
	addressedLines := sortedValues(addressedDesc)

	// shown is the union of files shown to any applyable lane (the panel and cross_check); only
	// those files may be edited.
	shown := map[string]bool{}

	// Optional report-only host self-review: the author_remediator contributes its own findings
	// before the reviewer lanes, under the same containment and identity checks. They are reported
	// and never applied.
	var selfReviewAdj adjudication.Result
	if req.IncludeHostReview {
		if hostLane, ok := plan.Lanes[review.RoleAuthorRemediator]; ok {
			hAdapter, aerr := m.laneAdapter(hostLane)
			if aerr != nil {
				_, e := m.halt(run, outcome, startedAt, callID+"-selfreview", aerr)
				return adjudication.Result{}, 0, e
			}
			_ = run.Event(m.now(), "info", "author_self_review_started", "host self-review lane", map[string]any{"adapter": hostLane.Adapter, "model": hostLane.Model})
			sr, _, herr := m.semanticPass(ctx, run, ws, req, hostLane, hAdapter, review.RoleAuthorRemediator, review.PhaseAuthorReview, auth, addressedLines, nil, nil, callID+"-selfreview", startedAt, outcome)
			if herr != nil {
				return adjudication.Result{}, 0, herr
			}
			srFindings := withSource(sr.Findings, "author_self_review")
			selfReviewAdj, herr = m.adjudicate(ctx, run, ws, req, plan, auth, srFindings, addressed, addressedLines, callID+"-selfreview-host", startedAt, outcome)
			if herr != nil {
				return adjudication.Result{}, 0, herr
			}
			_ = run.Event(m.now(), "info", "author_self_review_completed", "host self-review complete (report-only)", map[string]any{"findings": len(sr.Findings)})
		} else {
			_ = run.Event(m.now(), "info", "lane_skipped", "no author_remediator lane configured for self-review", map[string]any{"lane": "author_remediator"})
		}
	}

	// Blind primary panel: every seat runs in parallel, blind to the others.
	panel, perr := m.runPanel(ctx, run, ws, req, seats, auth, addressedLines, callID, maxInner, budget, startedAt, outcome)
	if perr != nil {
		return adjudication.Result{}, 0, perr
	}
	reviewerFindings := panel.findings
	mergeShown(shown, panel.shown)

	// Host adjudication over the union of seat findings, after every seat has finished.
	finalAdj, herr := m.adjudicate(ctx, run, ws, req, plan, auth, reviewerFindings, addressed, addressedLines, callID+"-host", startedAt, outcome)
	if herr != nil {
		return adjudication.Result{}, 0, herr
	}
	// Host-computed provenance and the weak-identity quarantine, over the deduplicated set.
	attachPanelProvenance(&finalAdj, panel)

	// Optional cross_check lane: looks for missed or disputed issues.
	if ccLane, ok := plan.Lanes[review.RoleCrossCheck]; ok {
		ccAdapter, aerr := m.laneAdapter(ccLane)
		if aerr != nil {
			_, e := m.halt(run, outcome, startedAt, callID+"-cc", aerr)
			return adjudication.Result{}, 0, e
		}
		_ = run.Event(m.now(), "info", "cross_check_started", "cross-check lane", map[string]any{"adapter": ccLane.Adapter, "model": ccLane.Model})
		// The cross-check sees the bounded post-adjudication set, not each seat's raw output, so its
		// prompt does not scale with the panel. Truncation is stated in the prompt and recorded.
		ccFindings, ccTruncated := boundedDescriptors(finalAdj.Findings, maxInformedLaneInputs)
		ccDecisions := decisionLines(finalAdj)
		if len(ccDecisions) > maxInformedLaneInputs {
			ccDecisions, ccTruncated = ccDecisions[:maxInformedLaneInputs], true
		}
		if ccTruncated {
			ccFindings = append(ccFindings, truncationCaveat)
			_ = run.Event(m.now(), "warn", "cross_check_input_truncated",
				"the adjudicated set exceeded the cross-check input bound; the prompt states that it is partial",
				map[string]any{"bound": maxInformedLaneInputs, "findings": len(finalAdj.Findings)})
		}
		cc, ccShown, herr := m.semanticPass(ctx, run, ws, req, ccLane, ccAdapter, review.RoleCrossCheck, review.PhaseCrossCheck, auth, addressedLines, ccFindings, ccDecisions, callID+"-cc", startedAt, outcome)
		if herr != nil {
			return adjudication.Result{}, 0, herr
		}
		mergeShown(shown, ccShown)
		ccAdj, herr := m.adjudicate(ctx, run, ws, req, plan, auth, cc.Findings, addressed, addressedLines, callID+"-cc-host", startedAt, outcome)
		if herr != nil {
			return adjudication.Result{}, 0, herr
		}
		finalAdj = mergeAdjResults(finalAdj, ccAdj) // union by fingerprint; disagreements recorded
		_ = run.Event(m.now(), "info", "cross_check_completed", "cross-check complete", map[string]any{"findings": len(cc.Findings)})
	} else {
		_ = run.Event(m.now(), "info", "lane_skipped", "no cross_check lane configured", map[string]any{"lane": "cross_check"})
	}

	// Optional verifier lane: an independent report-only final check, not gated on cross_check. Its
	// findings are never applied, so an apply run cannot loop on them; regressions from applied edits
	// are caught by the next outer cycle.
	var verifierAdj adjudication.Result
	if vLane, ok := plan.Lanes[review.RoleVerifier]; ok {
		vAdapter, aerr := m.laneAdapter(vLane)
		if aerr != nil {
			_, e := m.halt(run, outcome, startedAt, callID+"-verify", aerr)
			return adjudication.Result{}, 0, e
		}
		_ = run.Event(m.now(), "info", "verifier_started", "verifier lane", map[string]any{"adapter": vLane.Adapter, "model": vLane.Model})
		// The verifier gets the same bounded post-adjudication view as the cross-check.
		vFindings, vTruncated := boundedDescriptors(finalAdj.Findings, maxInformedLaneInputs)
		vDecisions := decisionLines(finalAdj)
		if len(vDecisions) > maxInformedLaneInputs {
			vDecisions, vTruncated = vDecisions[:maxInformedLaneInputs], true
		}
		if vTruncated {
			vFindings = append(vFindings, truncationCaveat)
			_ = run.Event(m.now(), "warn", "verifier_input_truncated",
				"the adjudicated set exceeded the verifier input bound; the prompt states that it is partial",
				map[string]any{"bound": maxInformedLaneInputs, "findings": len(finalAdj.Findings)})
		}
		vres, _, herr := m.semanticPass(ctx, run, ws, req, vLane, vAdapter, review.RoleVerifier, review.PhaseVerify, auth, addressedLines, vFindings, vDecisions, callID+"-verify", startedAt, outcome)
		if herr != nil {
			return adjudication.Result{}, 0, herr
		}
		verifierAdj, herr = m.adjudicate(ctx, run, ws, req, plan, auth, vres.Findings, addressed, addressedLines, callID+"-verify-host", startedAt, outcome)
		if herr != nil {
			return adjudication.Result{}, 0, herr
		}
		_ = run.Event(m.now(), "info", "verifier_completed", "verifier complete (report-only)", map[string]any{"findings": len(vres.Findings)})
	} else {
		_ = run.Event(m.now(), "info", "lane_skipped", "no verifier lane configured", map[string]any{"lane": "verifier"})
	}

	// Mode handling over reviewer and cross_check decisions; only the host writes.
	applied, merr := m.handleMode(ctx, run, req, plan, auth, &finalAdj, shown, identity, cycle, outcome)
	if merr != nil {
		_, e := m.halt(run, outcome, startedAt, callID, merr)
		return adjudication.Result{}, 0, e
	}
	// Feed applied fixes and rejections forward as settled context for the next outer cycle, so a
	// rejected finding does not crowd out new ones. Rejections go only into the prompt lines, never
	// the addressed map the Judge reads, so a reviewer that re-raises one gets a fresh adjudication.
	for i, d := range finalAdj.Decisions {
		fp := schema.Fingerprint(finalAdj.Findings[i])
		switch d.State {
		case review.StateApplied:
			addressed[fp] = review.StateApplied
			addressedDesc[fp] = "applied (fixed): " + findingDesc(finalAdj.Findings[i])
		case review.StateInvalid, review.StateReportedInvalid:
			// prompt context only; see above
			if _, isApplied := addressed[fp]; !isApplied {
				addressedDesc[fp] = "rejected as invalid: " + findingDesc(finalAdj.Findings[i]) + reasonSuffix(d.Reasoning)
			}
		}
	}

	// Verifier and host self-review findings are appended report-only after handleMode, then every
	// id is renumbered so lane-supplied ids cannot collide.
	finalAdj = appendReportOnly(finalAdj, verifierAdj)
	finalAdj = renumberIDs(appendReportOnly(finalAdj, selfReviewAdj))
	// Re-run the idempotent write-path marking so the findings appended above are marked too.
	markApplyRefusals(auth, &finalAdj, shown)
	// A later fromRun remediation gates on the files this run showed, not on what is on disk then.
	outcome.ShownFiles = sortedSet(shown)
	return finalAdj, applied, nil
}

// markApplyRefusals marks every decision whose finding cannot be traced to the workspace copy as not
// applyable, with a machine reason. It runs in every mode, so a report run already tells a caller
// which findings an apply would refuse. It is inert when no authority was declared.
func markApplyRefusals(auth authority.Set, adj *adjudication.Result, shown map[string]bool) {
	if auth.Empty() {
		return
	}
	for i := range adj.Decisions {
		if adj.Decisions[i].ApplyRefusalReason != "" {
			continue // already marked (idempotent)
		}
		if reason := auth.ApplyRefusal(adj.Findings[i].File, shown); reason != "" {
			no := false
			adj.Decisions[i].Applyable, adj.Decisions[i].ApplyRefusalReason = &no, reason
		}
	}
}

// laneAdapter returns the registered adapter for a resolved lane; preflight has already checked
// availability.
func (m *Manager) laneAdapter(lane review.LaneResolution) (model.Adapter, error) {
	a, ok := m.Adapters[lane.Adapter]
	if !ok {
		return nil, fault.New(fault.Adapter, fmt.Sprintf("adapter %q is not registered", lane.Adapter)).
			WithHalt("A").WithReason("adapter_not_registered")
	}
	return a, nil
}

func mergeShown(dst, src map[string]bool) {
	for k := range src {
		dst[k] = true
	}
}

// maxInformedLaneInputs bounds how many adjudicated findings or decisions an informed lane
// (cross_check, verifier) is shown, so its prompt does not grow with the panel.
const maxInformedLaneInputs = 200

// truncationCaveat is appended to an informed lane's context when the adjudicated set was bounded,
// so the model knows its view is partial.
const truncationCaveat = "(NOTE: this list was TRUNCATED to the input bound — it is a PARTIAL view of the adjudicated set; do not treat an absence here as evidence that nothing was reported.)"

// boundedDescriptors renders findings as context lines, capped at limit, reporting whether the
// cap dropped anything.
func boundedDescriptors(fs []review.Finding, limit int) ([]string, bool) {
	all := descriptors(fs)
	if len(all) <= limit {
		return all, false
	}
	return all[:limit], true
}

func descriptors(fs []review.Finding) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range fs {
		d := findingDesc(f)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// decisionLines renders host decisions as readable context lines for a later lane.
func decisionLines(adj adjudication.Result) []string {
	out := make([]string, 0, len(adj.Decisions))
	for i, d := range adj.Decisions {
		verdict := "invalid"
		if d.Valid {
			verdict = "valid"
		}
		line := verdict + " (" + string(d.State) + "): " + findingDesc(adj.Findings[i])
		if d.Reasoning != "" {
			line += " — " + d.Reasoning
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}

// mergeAdjResults unions add into base by fingerprint. A finding new to base is appended with its
// decision. A finding base already decided is kept; if add disagrees on validity, a report-only
// disagreement finding is appended instead.
func mergeAdjResults(base, add adjudication.Result) adjudication.Result {
	idx := map[string]int{}
	for i := range base.Findings {
		idx[schema.Fingerprint(base.Findings[i])] = i
	}
	for j := range add.Findings {
		fp := schema.Fingerprint(add.Findings[j])
		if i, ok := idx[fp]; ok {
			if add.Decisions[j].Valid != base.Decisions[i].Valid {
				f := add.Findings[j]
				f.Kind = review.Kind("inconsistency")
				f.Title = "cross-lane disagreement: " + f.Title
				f.Source = "evidence"
				base.Findings = append(base.Findings, f)
				base.Decisions = append(base.Decisions, review.Decision{
					FindingID: f.ID, Valid: false, State: review.StateReportedInvalid,
					Reasoning: "a later lane disagreed with the host's earlier decision on this finding",
				})
			}
			continue
		}
		base.Findings = append(base.Findings, add.Findings[j])
		base.Decisions = append(base.Decisions, add.Decisions[j])
		idx[fp] = len(base.Findings) - 1
	}
	return base
}

// withSource returns a copy of fs with each finding's Source set to src.
func withSource(fs []review.Finding, src string) []review.Finding {
	out := make([]review.Finding, len(fs))
	for i, f := range fs {
		f.Source = src
		out[i] = f
	}
	return out
}

// appendReportOnly appends add's findings to base as reported_valid or reported_invalid, states
// Actionable never applies. A finding that re-raises a decided fingerprint with a different verdict
// is recorded as a report-only disagreement rather than dropped.
func appendReportOnly(base, add adjudication.Result) adjudication.Result {
	idx := map[string]int{}
	for i := range base.Findings {
		idx[schema.Fingerprint(base.Findings[i])] = i
	}
	for j := range add.Findings {
		fp := schema.Fingerprint(add.Findings[j])
		if i, ok := idx[fp]; ok {
			if add.Decisions[j].Valid != base.Decisions[i].Valid {
				f := add.Findings[j]
				f.Kind = review.Kind("inconsistency")
				f.Title = "verifier disagreement: " + f.Title
				f.Source = "evidence"
				base.Findings = append(base.Findings, f)
				base.Decisions = append(base.Decisions, review.Decision{
					FindingID: f.ID, Valid: false, State: review.StateReportedInvalid,
					Reasoning: "the verifier disagreed with the host's earlier decision on this finding",
				})
			}
			continue
		}
		d := add.Decisions[j]
		if d.Valid {
			d.State = review.StateReportedValid
		} else {
			d.State = review.StateReportedInvalid
		}
		base.Findings = append(base.Findings, add.Findings[j])
		base.Decisions = append(base.Decisions, d)
		idx[fp] = len(base.Findings) - 1
	}
	return base
}

// renumberIDs assigns final ids f1..fN to every finding and its decision, so ids repeated across
// lanes cannot collide. The fingerprint, not the id, is the stable identity.
func renumberIDs(adj adjudication.Result) adjudication.Result {
	for i := range adj.Findings {
		id := fmt.Sprintf("f%d", i+1)
		adj.Findings[i].ID = id
		if i < len(adj.Decisions) {
			adj.Decisions[i].FindingID = id
		}
	}
	return adj
}

func findingDesc(f review.Finding) string {
	loc := f.File
	if f.Location != "" {
		loc = f.File + ":" + f.Location
	}
	if loc == "" {
		return f.Title
	}
	return f.Title + " (" + loc + ")"
}

// reasonSuffix renders a host's rejection reasoning as a short single-line suffix, so a reviewer can
// judge whether to disagree. Empty reasoning yields no suffix.
func reasonSuffix(reasoning string) string {
	r := strings.TrimSpace(strings.ReplaceAll(reasoning, "\n", " "))
	if r == "" {
		return ""
	}
	if len(r) > 200 {
		r = r[:200] + "…"
	}
	return " — reason: " + r
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// passOutcome is one semantic pass's result. Shared-state side effects (the identity caveat, the
// lane failure and the halt fault) are returned rather than applied, so the same pass can run in a
// panel seat goroutine or a single-threaded lane.
type passOutcome struct {
	result schema.ReviewerResult
	shown  map[string]bool
	caveat *review.IdentityCaveat
	// identity is the accepted attempt's identity tier (verified, self_reported or unknown).
	identity string
	// withheld lists files a containment rule kept out of this pass's reviewed set, including on
	// halt paths.
	withheld []review.WithheldFile
	failure  *review.LaneFailure
	err      error
}

// semanticPass runs runSemanticPass for a single-slot lane (cross_check, verifier or host
// self-review) and commits its side effects to the outcome, halting the run on failure. The panel
// calls runSemanticPass directly and commits in seat order.
func (m *Manager) semanticPass(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, reviewerLane review.LaneResolution, adapter model.Adapter, role review.Role, phase review.Phase, auth authority.Set, addressedLines, reported, decisions []string, callID string, startedAt time.Time, outcome *review.RunOutcome) (schema.ReviewerResult, map[string]bool, error) {
	po := m.runSemanticPass(ctx, run, ws, req, reviewerLane, adapter, role, phase, auth, addressedLines, reported, decisions, callID)
	if po.failure != nil {
		outcome.Failure = po.failure
	}
	// Commit withheld files before the halt branch, so a halted run still records them.
	outcome.Withheld = appendWithheld(outcome.Withheld, po.withheld...)
	if po.err != nil {
		_, e := m.halt(run, outcome, startedAt, callID, po.err)
		return schema.ReviewerResult{}, nil, e
	}
	if po.caveat != nil {
		outcome.IdentityCaveats = appendIdentityCaveat(outcome.IdentityCaveats, *po.caveat)
	}
	return po.result, po.shown, nil
}

// runSemanticPass runs one analysis call for a review lane or panel seat, with one corrective schema
// retry, and returns the parsed result and the files shown. A non-retriable failure or a final schema
// failure is returned as a halt fault. It writes only its own call directory, never the run-level
// halt record or the outcome, so seats can run it concurrently. The lane never writes to the workspace.
func (m *Manager) runSemanticPass(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, reviewerLane review.LaneResolution, adapter model.Adapter, role review.Role, phase review.Phase, auth authority.Set, addressedLines, reported, decisions []string, callID string) passOutcome {
	callDir := run.CallDir(callID)
	status := review.CallStatus{
		SchemaVersion: 1, CallID: callID, RunID: run.ID, Role: role,
		Phase: phase, Adapter: reviewerLane.Adapter,
		RequestedModel: string(reviewerLane.ModelArg),
	}
	const maxAttempts = 2 // one corrective retry on a schema-parse failure (only)
	var rr schema.ReviewerResult
	var lastParseErr error
	parsed := false
	shown := map[string]bool{}
	// pendingCaveat is the latest attempt's identity caveat; it is returned only once that attempt
	// parses, so a malformed attempt followed by a verified retry leaves no stale caveat.
	var pendingCaveat *review.IdentityCaveat
	// withheld accumulates every attempt's containment caveats, deduplicated.
	var withheld []review.WithheldFile
	var lastCopy *workspace.Handle
	defer func() { ws.Cleanup(lastCopy) }()

	for attempt := 1; attempt <= maxAttempts && !parsed; attempt++ {
		attDir := callDir
		if attempt > 1 {
			attDir = filepath.Join(callDir, fmt.Sprintf("retry-%d", attempt-1))
		}
		copyH, err := ws.Copy(req.Workspace, true, "review")
		if err != nil {
			return passOutcome{withheld: withheld, err: fault.Wrap(fault.Config, "provide isolated copy", err).WithReason("containment_copy_failed")}
		}
		lastCopy = copyH
		_ = run.Event(m.now(), "info", "containment_copy_created", "isolated reviewer copy created", map[string]any{"root": copyH.Root, "attempt": attempt})
		// Files the copy withheld (for example, hardlinked files).
		wh := withheldFrom(stageCopy, copyH.Caveats)

		snippets, snipCaveats, cerr := workspace.CollectSnippetsWithCaveats(copyH.Root)
		if cerr != nil {
			// A containment refusal while assembling the prompt halts, so it is recorded rather
			// than looking like an empty workspace.
			m.eventWithheld(run, wh)
			return passOutcome{withheld: appendWithheld(withheld, wh...), err: collectFault(cerr)}
		}
		// Files the collector withheld. Both lists are returned for the caller to commit, because
		// this may run in a panel seat goroutine.
		wh = appendWithheld(wh, withheldFrom(stageSnippets, snipCaveats)...)
		m.eventWithheld(run, wh)
		withheld = appendWithheld(withheld, wh...)
		shown = make(map[string]bool, len(snippets))
		files := make([]reviewprompt.FileSnippet, len(snippets))
		for i, s := range snippets {
			files[i] = reviewprompt.FileSnippet{Path: s.Path, Content: s.Content}
			shown[s.Path] = true
		}
		// Analysis lanes see all authority; only host adjudication applies the provenance split.
		pin := reviewprompt.Input{Role: role, Phase: phase, Files: files, Addressed: addressedLines, Reported: reported, Decisions: decisions, RequestSelfReportIdentity: wantsSelfReport(adapter), Authority: auth.ReviewerBlock()}
		if attempt > 1 {
			pin.Corrective = true
			if lastParseErr != nil {
				pin.ParserError = lastParseErr.Error()
			}
		}
		prompt := reviewprompt.Render(pin)
		call := model.Call{
			Role: string(role), Phase: string(phase),
			Model: reviewerLane.Model, ModelArg: reviewerLane.ModelArg, Effort: reviewerLane.Effort,
			// The lane's CLI runs in the isolated copy, because agentic CLIs read relative paths from
			// their working directory. See docs/security.md for what this does not stop.
			CopyRoot: copyH.Root, WorkDir: copyH.Root, Prompt: prompt,
		}
		_ = run.WriteText(filepath.Join(attDir, "prompt.md"), prompt+"\n")
		_ = run.Event(m.now(), "info", "adapter_call_started", "invoking "+string(role), map[string]any{"adapter": reviewerLane.Adapter, "model": reviewerLane.Model, "phase": string(phase), "attempt": attempt})

		res, invErr := adapter.Invoke(ctx, call)
		_ = run.WriteText(filepath.Join(attDir, "stdout.txt"), string(res.Stdout)) // raw, always preserved
		_ = run.WriteText(filepath.Join(attDir, "stderr.txt"), string(res.Stderr))
		// body is the unwrapped envelope payload when the adapter provides one, else raw stdout.
		body := res.Stdout
		if len(res.Payload) > 0 {
			body = res.Payload
		}
		if len(body) > 0 {
			_ = run.WriteText(filepath.Join(attDir, "reviewer-result.json"), string(body))
		}
		status.Attempts, status.ActualModel, status.ExitCode, status.CompletedAt = attempt, res.ActualModel, res.ExitCode, m.now().UTC()

		pc, lf, e, ok := m.classifyAndVerify(ctx, run, ws, copyH, reviewerLane, res, prompt, invErr, callID, callDir, attDir, &status)
		if !ok {
			return passOutcome{withheld: withheld, failure: lf, err: e}
		}
		pendingCaveat = pc

		normalized, changed, nerr := schema.NormalizeReviewerOutput(body)
		status.OutputNormalized = changed
		var perr error
		if nerr != nil {
			perr = nerr
		} else {
			if changed {
				_ = run.WriteText(filepath.Join(attDir, "reviewer-result.normalized.json"), string(normalized))
			}
			rr, perr = schema.ParseReviewerResult(normalized)
		}
		if perr == nil {
			parsed = true
			break
		}
		lastParseErr = perr
		if attempt < maxAttempts {
			_ = run.Event(m.now(), "warn", "reviewer_result_invalid", "schema parse failed; retrying once with a corrective prompt", map[string]any{"attempt": attempt, "error": perr.Error()})
			continue
		}
		cls := review.HaltClass("G")
		status.HaltClass, status.ReasonCode = &cls, "reviewer_result_unparseable"
		_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), status)
		return passOutcome{withheld: withheld, err: fault.Wrap(fault.Internal, "reviewer result invalid", perr).
			WithHalt("G").WithReason(status.ReasonCode)}
	}

	// call-status attributes the call to this lane, so an echoed role or phase that does not match
	// is only a warning.
	if rr.Role != string(role) || rr.Phase != string(phase) {
		_ = run.Event(m.now(), "warn", "lane_output_role_mismatch", "lane output did not echo the requested role/phase (call-status is authoritative)",
			map[string]any{"requestedRole": string(role), "requestedPhase": string(phase), "gotRole": rr.Role, "gotPhase": rr.Phase})
	}
	status.Verdict, status.Summary, status.Findings = review.Verdict(rr.Verdict), rr.Summary, rr.Findings
	_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), status)
	_ = run.Event(m.now(), "info", "reviewer_result_parsed", "parsed reviewer result", map[string]any{"role": string(role), "phase": string(phase), "findings": len(rr.Findings), "attempts": status.Attempts})
	// The accepted attempt parsed, so its caveat and identity tier are returned for the caller to commit.
	return passOutcome{result: rr, shown: shown, caveat: pendingCaveat, identity: status.VerificationStatus, withheld: withheld}
}

// wantsSelfReport reports whether an adapter's declared identity ceiling is self-report, the only case
// in which prompts ask for the inline identity wrapper.
func wantsSelfReport(a model.Adapter) bool {
	ev, ok := a.(interface {
		Evidence() review.IdentityEvidence
	})
	return ok && ev.Evidence() == review.EvidenceSelfReport
}

// appendIdentityCaveat appends c unless an identical caveat is already present.
func appendIdentityCaveat(caveats []review.IdentityCaveat, c review.IdentityCaveat) []review.IdentityCaveat {
	if slices.Contains(caveats, c) {
		return caveats
	}
	return append(caveats, c)
}

// adjudicate turns reviewer findings into decisions, deterministically or through a model-backed
// host-adjudication call when the author_remediator lane uses a non-fake adapter and findings remain.
func (m *Manager) adjudicate(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, plan review.RunPlan, auth authority.Set, findings []review.Finding, addressed map[string]review.DecisionState, addressedLines []string, hostCallID string, startedAt time.Time, outcome *review.RunOutcome) (adjudication.Result, error) {
	base := adjudication.Judge(findings, addressed)
	hostLane, hasHost := plan.Lanes[review.RoleAuthorRemediator]

	// Only findings not already terminal (already_addressed or skipped) go to the host, each with a
	// unique id. Terminal states are preserved, so outer convergence holds.
	var live []review.Finding
	liveIdxByID := map[string]int{}
	for i := range base.Decisions {
		if st := base.Decisions[i].State; st == review.StateAlreadyAddr || st == review.StateSkipped {
			continue
		}
		id := fmt.Sprintf("f%d", i+1)
		f := base.Findings[i]
		f.ID = id
		live = append(live, f)
		liveIdxByID[id] = i
	}

	if !hasHost || hostLane.Adapter == "fake" || len(live) == 0 {
		_ = run.Event(m.now(), "info", "host_adjudication_completed", "deterministic adjudication", map[string]any{"decisions": len(base.Decisions)})
		return base, nil
	}
	hostAdapter, ok := m.Adapters[hostLane.Adapter]
	if !ok {
		return adjudication.Result{}, fault.New(fault.Adapter,
			fmt.Sprintf("host adapter %q is not registered", hostLane.Adapter)).
			WithHalt("A").WithReason("adapter_not_registered")
	}
	hr, herr := m.hostAdjudicationCall(ctx, run, ws, req, plan, hostLane, hostAdapter, auth, live, addressedLines, hostCallID, review.PhaseAdjudicate, false, startedAt, outcome)
	if herr != nil {
		return adjudication.Result{}, herr
	}
	for _, a := range hr.Adjudications {
		i, ok := liveIdxByID[a.FindingID]
		if !ok {
			continue
		}
		// State, not validity, drives Actionable, so a deferred or withheld verdict is never
		// applied even when valid.
		base.Decisions[i].Valid = a.Validity == "valid"
		base.Decisions[i].State = a.DecisionState
		if a.SeverityAdjusted != "" {
			base.Decisions[i].SeverityAdjusted = a.SeverityAdjusted
		}
		base.Decisions[i].Reasoning = a.Reasoning
	}
	_ = run.Event(m.now(), "info", "host_adjudication_completed", "model-backed host adjudication", map[string]any{"decisions": len(base.Decisions)})
	return base, nil
}

// finalSelfCritique asks the host to re-check each finding it rejected and overturns rejections that
// were wrong. Overturned findings become reported_valid and are never applied, so the pass can only
// recover findings. It is a no-op for a fake or absent host or when nothing was rejected; a host halt
// propagates.
func (m *Manager) finalSelfCritique(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, plan review.RunPlan, auth authority.Set, last adjudication.Result, startedAt time.Time, outcome *review.RunOutcome) (adjudication.Result, error) {
	hostLane, ok := plan.Lanes[review.RoleAuthorRemediator]
	if !ok || hostLane.Adapter == "fake" {
		return last, nil // deterministic/absent host: no model to re-verify a dismissal
	}
	hostAdapter, ok := m.Adapters[hostLane.Adapter]
	if !ok {
		return last, nil
	}
	// Collect rejected findings and their reasons, mapping a fresh host id to the original index.
	var rejected []review.Finding
	var reasons []string
	origIdxByID := map[string]int{}
	for i, d := range last.Decisions {
		if d.State != review.StateInvalid && d.State != review.StateReportedInvalid {
			continue
		}
		id := fmt.Sprintf("f%d", len(rejected)+1)
		f := last.Findings[i]
		f.ID = id
		rejected = append(rejected, f)
		origIdxByID[id] = i
		reasons = append(reasons, fmt.Sprintf("%s: rejected — %s", findingDesc(last.Findings[i]), oneLineReason(d.Reasoning)))
	}
	if len(rejected) == 0 {
		return last, nil // nothing was rejected — no dismissal to verify
	}
	hr, herr := m.hostAdjudicationCall(ctx, run, ws, req, plan, hostLane, hostAdapter, auth, rejected, reasons, "c-final-selfcritique", review.PhaseAdjudicate, true, startedAt, outcome)
	if herr != nil {
		return adjudication.Result{}, herr
	}
	overturned := 0
	for _, a := range hr.Adjudications {
		i, ok := origIdxByID[a.FindingID]
		if !ok || a.Validity != "valid" {
			continue
		}
		// Overturn as reported_valid, which Actionable excludes, so an apply run cannot loop on it.
		last.Decisions[i].Valid = true
		last.Decisions[i].State = review.StateReportedValid
		last.Decisions[i].Reasoning = "overturned on final self-critique: " + a.Reasoning
		overturned++
	}
	_ = run.Event(m.now(), "info", "final_self_critique_completed", "dismissal-verification of rejected findings",
		map[string]any{"rejected": len(rejected), "overturned": overturned})
	return last, nil
}

// hostAdjudicationCall runs one host-adjudication model call, with one corrective schema retry, and
// returns the parsed result. phase sets only the recorded CallStatus.Phase; the prompt and output
// schema always use PhaseAdjudicate.
func (m *Manager) hostAdjudicationCall(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, plan review.RunPlan, hostLane review.LaneResolution, hostAdapter model.Adapter, auth authority.Set, findings []review.Finding, addressedLines []string, callID string, phase review.Phase, selfCritique bool, startedAt time.Time, outcome *review.RunOutcome) (schema.HostAdjudicationResult, error) {
	callDir := run.CallDir(callID)
	status := review.CallStatus{
		SchemaVersion: 1, CallID: callID, RunID: run.ID, Role: review.RoleAuthorRemediator,
		Phase: phase, Adapter: hostLane.Adapter,
		RequestedModel: string(hostLane.ModelArg),
	}
	findingIDs := make([]string, len(findings))
	for i, f := range findings {
		findingIDs[i] = f.ID
	}
	const maxAttempts = 2
	var hr schema.HostAdjudicationResult
	var lastParseErr error
	parsed := false
	var pendingCaveat *review.IdentityCaveat // committed only after the accepted attempt parses
	var lastCopy *workspace.Handle
	defer func() { ws.Cleanup(lastCopy) }()

	for attempt := 1; attempt <= maxAttempts && !parsed; attempt++ {
		attDir := callDir
		if attempt > 1 {
			attDir = filepath.Join(callDir, fmt.Sprintf("retry-%d", attempt-1))
		}
		copyH, err := ws.Copy(req.Workspace, true, "adjudicate")
		if err != nil {
			_, e := m.halt(run, outcome, startedAt, callID,
				fault.Wrap(fault.Config, "provide isolated copy", err).WithReason("containment_copy_failed"))
			return schema.HostAdjudicationResult{}, e
		}
		lastCopy = copyH
		wh := withheldFrom(stageCopy, copyH.Caveats)
		snippets, snipCaveats, cerr := workspace.CollectSnippetsWithCaveats(copyH.Root)
		wh = appendWithheld(wh, withheldFrom(stageSnippets, snipCaveats)...)
		// This runs single-threaded, so withheld files are committed here, before the halt branch.
		m.eventWithheld(run, wh)
		outcome.Withheld = appendWithheld(outcome.Withheld, wh...)
		if cerr != nil {
			_, e := m.halt(run, outcome, startedAt, callID, collectFault(cerr))
			return schema.HostAdjudicationResult{}, e
		}
		files := make([]adjudicationprompt.FileSnippet, len(snippets))
		for i, s := range snippets {
			files[i] = adjudicationprompt.FileSnippet{Path: s.Path, Content: s.Content}
		}
		// Provenance split: the adjudicator sees path authority only, so a client model cannot
		// supply the intent the host then judges findings against.
		pin := adjudicationprompt.Input{Mode: plan.Mode, Findings: findings, Files: files, Addressed: addressedLines, RequestSelfReportIdentity: wantsSelfReport(hostAdapter), SelfCritique: selfCritique, Authority: auth.AdjudicatorBlock()}
		if attempt > 1 {
			pin.Corrective = true
			if lastParseErr != nil {
				pin.ParserError = lastParseErr.Error()
			}
		}
		prompt := adjudicationprompt.Render(pin)
		call := model.Call{
			Role: string(review.RoleAuthorRemediator), Phase: string(review.PhaseAdjudicate),
			Model: hostLane.Model, ModelArg: hostLane.ModelArg, Effort: hostLane.Effort,
			// The adjudicator CLI runs in the isolated copy.
			CopyRoot: copyH.Root, WorkDir: copyH.Root, Prompt: prompt,
		}
		_ = run.WriteText(filepath.Join(attDir, "prompt.md"), prompt+"\n")
		_ = run.Event(m.now(), "info", "adapter_call_started", "invoking host adjudicator", map[string]any{"adapter": hostLane.Adapter, "model": hostLane.Model, "attempt": attempt})

		res, invErr := hostAdapter.Invoke(ctx, call)
		_ = run.WriteText(filepath.Join(attDir, "stdout.txt"), string(res.Stdout)) // raw, always preserved
		_ = run.WriteText(filepath.Join(attDir, "stderr.txt"), string(res.Stderr))
		body := res.Stdout
		if len(res.Payload) > 0 {
			body = res.Payload // unwrapped envelope payload (e.g. claude-code)
		}
		if len(body) > 0 {
			_ = run.WriteText(filepath.Join(attDir, "host-adjudication-result.json"), string(body))
		}
		status.Attempts, status.ActualModel, status.ExitCode, status.CompletedAt = attempt, res.ActualModel, res.ExitCode, m.now().UTC()

		pc, lf, e, ok := m.classifyAndVerify(ctx, run, ws, copyH, hostLane, res, prompt, invErr, callID, callDir, attDir, &status)
		if !ok {
			if lf != nil {
				outcome.Failure = lf
			}
			_, he := m.halt(run, outcome, startedAt, callID, e)
			return schema.HostAdjudicationResult{}, he
		}
		pendingCaveat = pc

		normalized, changed, nerr := schema.NormalizeReviewerOutput(body)
		status.OutputNormalized = changed
		var perr error
		if nerr != nil {
			perr = nerr
		} else {
			if changed {
				_ = run.WriteText(filepath.Join(attDir, "host-adjudication-result.normalized.json"), string(normalized))
			}
			hr, perr = schema.ParseHostAdjudication(normalized, findingIDs)
		}
		if perr == nil {
			parsed = true
			break
		}
		lastParseErr = perr
		if attempt < maxAttempts {
			_ = run.Event(m.now(), "warn", "host_adjudication_invalid", "schema parse failed; retrying once with a corrective prompt", map[string]any{"attempt": attempt, "error": perr.Error()})
			continue
		}
		cls := review.HaltClass("G")
		status.HaltClass, status.ReasonCode = &cls, "host_adjudication_unparseable"
		_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), status)
		_, e = m.halt(run, outcome, startedAt, callID,
			fault.Wrap(fault.Internal, "host adjudication invalid", perr).WithHalt("G").WithReason(status.ReasonCode))
		return schema.HostAdjudicationResult{}, e
	}

	status.HostAdjudications = hr.Adjudications
	_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), status)
	_ = run.Event(m.now(), "info", "host_adjudication_parsed", "parsed host adjudication", map[string]any{"adjudications": len(hr.Adjudications), "attempts": status.Attempts})
	if pendingCaveat != nil {
		outcome.IdentityCaveats = appendIdentityCaveat(outcome.IdentityCaveats, *pendingCaveat)
	}
	return hr, nil
}

// ensureHostAdjudicationExercised runs one synthetic host-adjudication call, through the same adapter,
// containment, schema and identity checks as a natural one, when a non-fake host lane made no natural
// adjudication call this run. Its output is discarded and a halt propagates. It is a no-op for a fake
// or unconfigured host, or one already exercised.
func (m *Manager) ensureHostAdjudicationExercised(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, plan review.RunPlan, auth authority.Set, startedAt time.Time, outcome *review.RunOutcome) error {
	hostLane, ok := plan.Lanes[review.RoleAuthorRemediator]
	if !ok || hostLane.Adapter == "fake" {
		return nil // not configured, or a deterministic fake host (no model call exists to validate)
	}
	if m.hostAdjudicationOccurred(outcome.RunDir) {
		return nil // a natural host adjudication already exercised the lane this run
	}
	hostAdapter, ok := m.Adapters[hostLane.Adapter]
	if !ok {
		_, e := m.halt(run, outcome, startedAt, "c-0001-hostvalidate",
			fault.New(fault.Adapter, fmt.Sprintf("host adapter %q is not registered", hostLane.Adapter)).
				WithHalt("A").WithReason("adapter_not_registered"))
		return e
	}
	// A minimal, benign controlled input for the host to adjudicate — never a real review finding.
	synthetic := []review.Finding{{
		ID: "f1", Title: "host adjudication readiness probe",
		Detail: "Synthetic controlled input used only to validate that the configured author_remediator adapter/model can run its adjudication role during ACP validation. Not a real review finding.",
		Kind:   review.KindPass, Severity: review.SeverityInfo, Source: "evidence", Primitive: "P1",
	}}
	_ = run.Event(m.now(), "info", "host_adjudication_validation_started",
		"synthetic adjudication readiness probe (no natural findings to adjudicate)",
		map[string]any{"adapter": hostLane.Adapter, "model": hostLane.Model})
	// Output discarded: only the call-status + identity side effects (and any halt) matter.
	_, herr := m.hostAdjudicationCall(ctx, run, ws, req, plan, hostLane, hostAdapter, auth, synthetic, nil, "c-0001-hostvalidate", review.PhaseValidateAdjudicate, false, startedAt, outcome)
	return herr
}

// hostAdjudicationOccurred reports whether a natural host adjudication call ran this run: a
// schemaVersion 1 call-status.json with role author_remediator and phase semantic_adjudicate. A
// self-review call does not count.
func (m *Manager) hostAdjudicationOccurred(runDir string) bool {
	matches, _ := filepath.Glob(filepath.Join(runDir, "calls", "*", "call-status.json"))
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cs review.CallStatus
		if json.Unmarshal(b, &cs) == nil && cs.SchemaVersion == 1 &&
			cs.Role == review.RoleAuthorRemediator && cs.Phase == review.PhaseAdjudicate {
			return true
		}
	}
	return false
}

// classifyAndVerify runs one model call's non-retriable halt checks (copy mutation, cancellation,
// invocation failure, non-zero exit) and classifies the model identity. On a halt it writes the
// call-status and returns the fault with ok false. Identity never halts: any result short of strong
// verification yields a pending caveat for the caller to commit once the attempt parses.
//
// It never touches the outcome or the run-level halt record, because it runs in panel seat goroutines;
// the caller commits results in seat order. prompt is the text sent, so laneFailure can subtract its echo.
func (m *Manager) classifyAndVerify(ctx context.Context, run *audit.Run, ws *workspace.Access, copyH *workspace.Handle, lane review.LaneResolution, res model.Result, prompt string, invErr error, callID, callDir, attDir string, status *review.CallStatus) (*review.IdentityCaveat, *review.LaneFailure, error, bool) {
	// status is reused across attempts, so reset per-attempt fields before classifying.
	status.HaltClass, status.ReasonCode, status.Signal = nil, "", ""
	mutDiff, m5, _ := ws.Discard(copyH)
	if m5 != nil {
		_ = run.WriteText("containment/mutation-diff.patch", mutDiff)
		_ = run.Event(m.now(), "error", "containment_mutation_detected", "model mutated the isolated copy", nil)
		status.HaltClass, status.ReasonCode, status.VerificationStatus = m5, "containment_mutation_detected", "unverified"
		_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), *status)
		return nil, nil, fault.New(fault.Containment, "containment breach: model mutated the isolated copy").
			WithHalt("M5").WithReason("containment_mutation_detected"), false
	}
	if ctx.Err() != nil || errors.Is(invErr, context.Canceled) {
		cls := review.HaltClass("F")
		status.HaltClass, status.ReasonCode, status.VerificationStatus = &cls, "cancelled", "unverified"
		_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), *status)
		return nil, nil, fault.Wrap(fault.Policy, "review cancelled", context.Canceled).
			WithHalt("F").WithReason("cancelled"), false
	}
	if invErr != nil {
		cls := review.HaltClass("F")
		f := laneFailure(lane, res, prompt, "F", "adapter_invocation_failed")
		status.HaltClass, status.ReasonCode, status.VerificationStatus = &cls, "adapter_invocation_failed", "unverified"
		status.Signal = f.Signal
		_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), *status)
		return nil, f, fault.Wrap(fault.Internal, "adapter invocation failed", invErr).
			WithHalt("F").WithReason("adapter_invocation_failed").WithSignal(f.Signal), false
	}
	if res.ExitCode != 0 {
		cls := review.HaltClass("A")
		f := laneFailure(lane, res, prompt, "A", "adapter_exited_nonzero")
		status.HaltClass, status.ReasonCode, status.VerificationStatus = &cls, "adapter_exited_nonzero", "unverified"
		status.Signal = f.Signal
		_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), *status)
		return nil, f, fault.New(fault.Adapter, fmt.Sprintf("adapter %q exited %d", lane.Adapter, res.ExitCode)).
			WithHalt("A").WithReason("adapter_exited_nonzero").WithSignal(f.Signal), false
	}
	// Cap evidence at the adapter's declared ceiling so a weak adapter cannot be classified as verified.
	// An adapter without an Evidence method is treated as EvidenceNone.
	ceiling := review.EvidenceNone
	if a, ok := m.Adapters[lane.Adapter]; ok {
		if ev, ok2 := a.(interface {
			Evidence() review.IdentityEvidence
		}); ok2 {
			ceiling = ev.Evidence()
		}
	}
	produced := verify.CapEvidence(ceiling, res.Evidence)
	vstatus, ok := verify.ClassifyIdentity(produced, string(lane.ModelArg), res.ActualModel, lane.Adapter)
	status.VerificationStatus, status.IdentityEvidence = vstatus, produced
	_ = m.writeModelVerification(run, attDir, lane, res.ActualModel, produced, vstatus)
	_ = run.Event(m.now(), "info", "model_verification_completed", "classified model identity",
		map[string]any{"verificationStatus": vstatus, "evidence": string(produced)})
	// Any identity short of a strong match, including a mismatch, is a caveat that keeps the reported
	// model. Identity never gates a finding: a lane's valid output is used whatever its identity says.
	// See docs/model-identity.md.
	var caveat *review.IdentityCaveat
	if !ok {
		caveat = &review.IdentityCaveat{
			Role: string(lane.Role), Adapter: lane.Adapter, RequestedModel: string(lane.ModelArg),
			Evidence: produced, Status: vstatus, ReportedModel: res.ActualModel,
		}
		status.ReasonCode = "model_identity_" + vstatus
		_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), *status)
		_ = run.Event(m.now(), "info", "model_identity_caveat",
			"adapter ran and returned output; model identity recorded as "+vstatus+" (the output is used regardless)",
			map[string]any{"role": string(lane.Role), "adapter": lane.Adapter, "reported": res.ActualModel, "status": vstatus})
	} else if vstatus == review.VerifSelfReported {
		// ok=true but weaker than verified: still worth telling the reader.
		caveat = &review.IdentityCaveat{
			Role: string(lane.Role), Adapter: lane.Adapter, RequestedModel: string(lane.ModelArg),
			Evidence: produced, Status: vstatus, ReportedModel: res.ActualModel,
		}
	}
	return caveat, nil, nil, true
}

// laneFailure captures a failing lane's detail (role, adapter, model, exit code, raw output) for
// RunOutcome.Failure; surfaces sanitize and cap the output. The clihint signal is classified once here
// so the audit record and the user-facing hint agree. prompt is subtracted before classifying, because
// some CLIs echo the prompt to stderr and it embeds reviewed source.
func laneFailure(lane review.LaneResolution, res model.Result, prompt, halt, reason string) *review.LaneFailure {
	return &review.LaneFailure{
		Role: string(lane.Role), Adapter: lane.Adapter, Model: lane.Model,
		ModelArg: string(lane.ModelArg), ExitCode: res.ExitCode,
		HaltClass: halt, ReasonCode: reason,
		Signal: string(clihint.ForFailure(clihint.Failure{
			Stderr: string(res.Stderr), Stdout: string(res.Stdout),
			Prompt: prompt, ExitCode: res.ExitCode, Adapter: lane.Adapter,
		})),
		StderrExcerpt: string(res.Stderr), StdoutExcerpt: string(res.Stdout),
	}
}

func evidenceOrNone(e review.IdentityEvidence) review.IdentityEvidence {
	if e == "" {
		return review.EvidenceNone
	}
	return e
}

// handleMode performs the report, patch or apply behavior, finalizes decision states, and returns how
// many findings were committed to the live workspace. Patch and apply delegate every write to
// governedWrite (writepath.go), so there is one write path; this function owns only the decision
// states before and after that write.
func (m *Manager) handleMode(ctx context.Context, run *audit.Run, req Request, plan review.RunPlan, auth authority.Set, adj *adjudication.Result, shown map[string]bool, identity WorkspaceIdentity, cycle int, outcome *review.RunOutcome) (int, error) {
	// Mark write-path refusals before branching, so report mode records them too.
	markApplyRefusals(auth, adj, shown)
	if plan.Mode == review.ModeReport {
		for i := range adj.Decisions {
			if adjudication.Actionable(adj.Decisions[i]) {
				adj.Decisions[i].State = review.StateReportedValid
			}
		}
		return 0, nil
	}

	// The write path binds decisions to findings by id, and lane-supplied ids can repeat, so make
	// them unique; ids that already are keep their value.
	uniqueFindingIDs(adj.Findings, adj.Decisions)

	// A finding supported only by authority text is reported, never applied. Naming a declared
	// document is not an escape attempt, so it degrades to report-only rather than halting.
	for i := range adj.Decisions {
		if !adjudication.Actionable(adj.Decisions[i]) {
			continue
		}
		if reason := adj.Decisions[i].ApplyRefusalReason; reason != "" {
			f := adj.Findings[i]
			adj.Decisions[i].State = review.StateReportedValid
			_ = run.Event(m.now(), "warn", "apply_refused_untraceable",
				"finding cannot be traced to the workspace copy; reported but not applied",
				map[string]any{"finding": f.ID, "file": f.File, "reason": reason})
		}
	}

	// Each outer cycle's write window gets its own artifact directory so no journal is overwritten;
	// the first keeps the documented remediation/ path.
	artifacts := ""
	if cycle > 1 {
		artifacts = defaultWriteArtifacts + "/" + fmt.Sprintf("cycle-%d", cycle)
	}
	_ = run.Event(m.now(), "info", "remediation_started", "remediation started", map[string]any{
		"mode": string(plan.Mode), "findings": len(adj.Findings),
	})

	res, werr := m.governedWrite(ctx, run, writeRequest{
		Workspace: req.Workspace, Identity: identity,
		Mode: plan.Mode, Plan: plan, Artifacts: artifacts,
		Findings: adj.Findings, Decisions: adj.Decisions,
		Accept: func(d review.Decision) bool {
			return adjudication.Actionable(d) && d.ApplyRefusalReason == ""
		},
		// The selection narrows every cycle, but only the first is measured against the caller's
		// list: by a later cycle a selected finding may be absent because it was already applied.
		Select: req.Select, SelectPrimary: cycle <= 1,
		Shown: shown,
		// A full cycle records pins from the copy it just took. See writepath.go.
		PinsFromCopy: true,
		// On a converging apply the last window's verification wins, since it measured the final tree.
		VerifyCommands: req.VerifyCommands, VerifyTimeout: req.VerifyTimeout,
		AllowProtectedPaths: req.AllowProtectedPaths,
	})
	if res.Selection != nil {
		outcome.Selection = res.Selection
	}
	if res.Verification != nil {
		outcome.Verification = res.Verification
	}
	outcome.Withheld = appendWithheld(outcome.Withheld, res.Withheld...)
	// Protected-path refusals accumulate across cycles on the outcome, deduplicated (see appendRefusals).
	outcome.Refusals = appendRefusals(outcome.Refusals, res.Refusals...)
	if werr != nil {
		return 0, werr
	}

	// Derive terminal states only from what the write path reported. applied, the convergence signal,
	// counts only findings that reached the live tree.
	byID := make(map[string]findingWrite, len(res.Findings))
	for _, fw := range res.Findings {
		byID[fw.FindingID] = fw
	}
	applied := 0
	for i := range adj.Decisions {
		fw, ok := byID[adj.Findings[i].ID]
		if !ok {
			continue // not offered to the write path (not actionable, or refused above)
		}
		switch {
		case fw.Refused:
			// Record the refusal with the same fields as other write-path refusals; the finding
			// stays reported_valid because only the write was refused.
			no := false
			adj.Decisions[i].State = review.StateReportedValid
			adj.Decisions[i].Applyable, adj.Decisions[i].ApplyRefusalReason = &no, fw.Reason
		case fw.Skipped:
			adj.Decisions[i].State = review.StateSkipped
		case fw.Applied && plan.Mode == review.ModeApply:
			adj.Decisions[i].State = review.StateApplied
			applied++
		default:
			// patch mode, or apply with a failed edit → not "applied"
			adj.Decisions[i].State = review.StateReportedValid
		}
	}
	// outcome.Applied is the reported total and includes patch mode, unlike the convergence signal.
	outcome.Applied += len(res.Receipt.Applied)
	return applied, nil
}

// ScopeHaltClass is the halt class for a path-confinement refusal, a mechanical containment check
// that maps to the containment exit code.
const ScopeHaltClass = "M6"

// authorizeTarget checks one workspace-relative target against the run's write guard and turns a
// refusal into a halt fault wrapping the *scope.Denial, so callers can tell which rule refused. It is
// a no-op without a guard or for an empty path.
//
// A non-Denial error is returned as is, because scopeFault's typed nil would be a non-nil error.
func authorizeTarget(ws *workspace.Access, h *workspace.Handle, rel string) error {
	if ws == nil || ws.Guard == nil || h == nil || rel == "" {
		return nil
	}
	if _, err := ws.Guard.ResolveWrite(filepath.Join(h.Live, rel)); err != nil {
		if sf := scopeFault(err); sf != nil {
			return sf
		}
		return err
	}
	return nil
}

// scopeFault maps a path-confinement refusal to a containment halt (class M6) carrying the
// resolver's reason code. It returns nil for any other error.
func scopeFault(err error) *fault.Fault {
	d, ok := scope.AsDenial(err)
	if !ok {
		return nil
	}
	return fault.Wrap(fault.Containment, "refused by workspace scope policy", err).
		WithHalt(ScopeHaltClass).WithReason(string(d.Reason))
}

// collectFault classifies a failure to collect workspace snippets. A containment refusal from
// meshcore/workspace becomes an M6 containment halt with its reason code; anything else is an
// internal failure.
func collectFault(err error) *fault.Fault {
	if r, ok := workspace.AsRefusal(err); ok {
		return fault.Wrap(fault.Containment, "workspace snippets refused by containment policy", err).
			WithHalt(ScopeHaltClass).WithReason(string(r.Reason))
	}
	return fault.Wrap(fault.Internal, "collect workspace snippets", err).
		WithReason("workspace_collect_failed")
}

// Withheld files: meshcore withholds some files (such as hardlinked files) rather than halting.
// That is safe only if the omission is recorded, so these helpers carry the caveats onto the outcome,
// the audit log and the projection.

// stageCopy and stageSnippets name where a file was withheld: from the isolated copy, or from the
// prompt.
const (
	stageCopy     = "copy"
	stageSnippets = "snippets"
)

// withheldFrom converts meshcore's typed caveats into the outcome's shape.
func withheldFrom(stage string, cs []workspace.Caveat) []review.WithheldFile {
	if len(cs) == 0 {
		return nil
	}
	out := make([]review.WithheldFile, 0, len(cs))
	for _, c := range cs {
		out = append(out, review.WithheldFile{
			Path: c.Path, Reason: string(c.Reason), Rule: c.Rule, Detail: c.Detail, Stage: stage,
		})
	}
	return out
}

// appendWithheld merges withheld records, deduplicated by stage, path and reason, since the same file
// is withheld again on every retry, seat and cycle.
func appendWithheld(dst []review.WithheldFile, add ...review.WithheldFile) []review.WithheldFile {
	for _, w := range add {
		dup := false
		for _, e := range dst {
			if e.Stage == w.Stage && e.Path == w.Path && e.Reason == w.Reason {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, w)
		}
	}
	return dst
}

// appendRefusals accumulates protected-path refusals across outer cycles, deduplicated by fingerprint.
// A refused finding is re-raised every cycle, so without deduplication one finding would be counted
// once per cycle. Each cycle's receipt still records its own window.
func appendRefusals(dst []review.ApplyRefusal, add ...review.ApplyRefusal) []review.ApplyRefusal {
	for _, r := range add {
		dup := false
		for _, e := range dst {
			if e.Fingerprint == r.Fingerprint {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, r)
		}
	}
	return dst
}

// eventWithheld logs a warning for each withheld file, naming the path and the rule.
func (m *Manager) eventWithheld(run *audit.Run, w []review.WithheldFile) {
	for _, e := range w {
		_ = run.Event(m.now(), "warn", "workspace_file_withheld",
			"a containment rule withheld this file from the reviewed set; it was NOT shown to any reviewer",
			map[string]any{"file": e.Path, "reasonCode": e.Reason, "rule": e.Rule, "detail": e.Detail, "stage": e.Stage})
	}
}

// remediationMaxBytes bounds the file content shown to the remediator; the model is told to decline
// when a fix needs the omitted part.
const remediationMaxBytes = 60000

// readBounded reads one file of the remediation copy, capped at remediationMaxBytes, and reports
// whether it was truncated. It reads through rootfile rather than a joined path, so a symlink swapped
// into the copy cannot redirect the read outside it.
func readBounded(copyRoot, rel string) (content string, truncated bool, err error) {
	b, err := rootfile.ReadUnder(copyRoot, rel)
	if err != nil {
		return "", false, err
	}
	if len(b) > remediationMaxBytes {
		return string(b[:remediationMaxBytes]), true, nil
	}
	return string(b), false, nil
}

// hostRemediationCall asks the author_remediator for anchored edits that fix one finding in
// fileContent. It is best effort: any failure or decline returns nil and the caller falls back to the
// deterministic marker. The call runs in a disposable copy of sourceRoot, and every returned edit is
// re-validated and applied to the copy before diffing. It makes one corrective retry.
func (m *Manager) hostRemediationCall(ctx context.Context, run *audit.Run, ws *workspace.Access, sourceRoot string, hostLane review.LaneResolution, hostAdapter model.Adapter, f review.Finding, fileContent string, truncated bool, callID string) []review.Edit {
	callDir := run.CallDir(callID)
	const maxAttempts = 2
	var lastParseErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attDir := callDir
		if attempt > 1 {
			attDir = filepath.Join(callDir, fmt.Sprintf("retry-%d", attempt-1))
		}
		copyH, err := ws.Copy(sourceRoot, true, "remediate")
		if err != nil {
			_ = run.Event(m.now(), "warn", "remediation_completed", "remediation copy failed; marker fallback", map[string]any{"finding": f.ID, "error": err.Error()})
			return nil
		}
		pin := remediationprompt.Input{
			Finding: f, FilePath: f.File, FileContent: fileContent, Truncated: truncated,
			RequestSelfReportIdentity: wantsSelfReport(hostAdapter),
		}
		if attempt > 1 {
			pin.Corrective = true
			if lastParseErr != nil {
				pin.ParserError = lastParseErr.Error()
			}
		}
		prompt := remediationprompt.Render(pin)
		call := model.Call{
			Role: string(review.RoleAuthorRemediator), Phase: string(review.PhaseRemediate),
			Model: hostLane.Model, ModelArg: hostLane.ModelArg, Effort: hostLane.Effort,
			// The remediator CLI runs in the isolated copy.
			CopyRoot: copyH.Root, WorkDir: copyH.Root, Prompt: prompt,
		}
		_ = run.WriteText(filepath.Join(attDir, "prompt.md"), prompt+"\n")
		_ = run.Event(m.now(), "info", "adapter_call_started", "invoking remediator", map[string]any{"adapter": hostLane.Adapter, "model": hostLane.Model, "finding": f.ID, "attempt": attempt})
		res, invErr := hostAdapter.Invoke(ctx, call)
		_ = run.WriteText(filepath.Join(attDir, "stdout.txt"), string(res.Stdout))
		_ = run.WriteText(filepath.Join(attDir, "stderr.txt"), string(res.Stderr))
		ws.Cleanup(copyH) // read-only call; nothing to keep — discard the model's copy
		if invErr != nil || res.ExitCode != 0 {
			_ = run.Event(m.now(), "warn", "remediation_completed", "remediation call failed; marker fallback", map[string]any{"finding": f.ID, "exit": res.ExitCode})
			return nil
		}
		body := res.Stdout
		if len(res.Payload) > 0 {
			body = res.Payload // unwrapped envelope payload (e.g. claude-code)
		}
		if len(body) > 0 {
			_ = run.WriteText(filepath.Join(attDir, "remediation-result.json"), string(body))
		}
		normalized, _, nerr := schema.NormalizeReviewerOutput(body)
		if nerr == nil {
			var rr schema.RemediationResult
			rr, lastParseErr = schema.ParseRemediationResult(normalized, f.File)
			if lastParseErr == nil {
				if rr.NoEdit || len(rr.Edits) == 0 {
					_ = run.Event(m.now(), "info", "remediation_completed", "remediator declined a safe fix; marker fallback", map[string]any{"finding": f.ID, "reason": oneLineReason(rr.Reason)})
					return nil
				}
				_ = run.Event(m.now(), "info", "remediation_completed", "model-driven remediation produced edits", map[string]any{"finding": f.ID, "edits": len(rr.Edits)})
				return rr.Edits
			}
		} else {
			lastParseErr = nerr
		}
		if attempt < maxAttempts {
			_ = run.Event(m.now(), "warn", "remediation_invalid", "remediation output unparseable; retrying once", map[string]any{"finding": f.ID, "error": lastParseErr.Error()})
		}
	}
	_ = run.Event(m.now(), "warn", "remediation_completed", "remediation unparseable after retry; marker fallback", map[string]any{"finding": f.ID})
	return nil
}

// oneLineReason flattens a short decline reason for a log field.
func oneLineReason(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func (m *Manager) halt(run *audit.Run, outcome *review.RunOutcome, startedAt time.Time, callID string, f error) (review.RunOutcome, error) {
	outcome.Status = "halted"
	cls := ""
	if fp, ok := f.(*fault.Fault); ok {
		cls = fp.Halt
		if cls != "" {
			hc := review.HaltClass(cls)
			outcome.Halt = &hc
		}
	}
	// The reason and signal ride the outcome, so surfaces need not re-read the run directory.
	outcome.HaltReason, outcome.HaltSignal = fault.ReasonOf(f), fault.SignalOf(f)
	rel := "halt-record.json"
	if callID != "" {
		rel = filepath.Join(run.CallDir(callID), "halt-record.json")
	}
	_ = run.WriteJSON(rel, haltRecord(run, cls, callID, f, m.now()))
	_ = run.WriteJSON("run-state.json", runState(run, *outcome, startedAt, m.now()))
	_ = run.Event(m.now(), "error", "run_halted", "review run halted",
		map[string]any{"halt": cls, "reasonCode": fault.ReasonOf(f)})
	return *outcome, f
}

func (m *Manager) writeModelVerification(run *audit.Run, callDir string, lane review.LaneResolution, actual string, evidence review.IdentityEvidence, status string) error {
	return run.WriteJSON(filepath.Join(callDir, "model-verification.json"), map[string]any{
		"schemaVersion": 1, "role": string(lane.Role), "adapter": lane.Adapter,
		"requestedModel": string(lane.ModelArg), "actualModel": actual,
		"identityEvidence": string(evidenceOrNone(evidence)), "verificationStatus": status,
		// success means strongly verified; self_reported, unknown and mismatch are all false, and
		// verificationStatus carries the precise disposition.
		"success":         status == review.VerifVerified,
		"rawIdentityText": actual, // sanitized: only the model string, never secrets
		"verifiedAt":      m.now().UTC().Format(time.RFC3339),
	})
}

func sortedRoles(lanes map[review.Role]review.LaneResolution) []review.Role {
	roles := make([]review.Role, 0, len(lanes))
	for r := range lanes {
		roles = append(roles, r)
	}
	slices.Sort(roles)
	return roles
}

func planData(plan review.RunPlan) map[string]any {
	lanes := map[string]any{}
	for role, l := range plan.Lanes {
		lanes[string(role)] = map[string]string{"adapter": l.Adapter, "model": l.Model}
	}
	return map[string]any{"mode": string(plan.Mode), "surface": plan.Surface, "lanes": lanes}
}

func runState(run *audit.Run, o review.RunOutcome, startedAt, completedAt time.Time) map[string]any {
	roles := map[string]any{}
	for role, l := range o.Plan.Lanes {
		roles[string(role)] = map[string]string{"adapter": l.Adapter, "requestedModel": string(l.ModelArg)}
	}
	st := map[string]any{
		"schemaVersion": 1,
		"runId":         run.ID,
		"outputMode":    string(o.Mode),
		"surface":       o.Plan.Surface,
		"status":        o.Status,
		"roles":         roles,
		"startedAt":     startedAt.UTC().Format(time.RFC3339),
		"completedAt":   completedAt.UTC().Format(time.RFC3339),
	}
	// Optional blocks are omitted when empty: the authority inclusion manifest, withheld files, the
	// caller's trace context and the halt summary.
	if len(o.Authority) > 0 {
		st["authority"] = o.Authority
	}
	if len(o.Withheld) > 0 {
		withheld := make([]map[string]any, 0, len(o.Withheld))
		for _, w := range o.Withheld {
			e := map[string]any{"path": w.Path, "reasonCode": w.Reason, "stage": w.Stage}
			if w.Rule != "" {
				e["rule"] = w.Rule
			}
			if w.Detail != "" {
				e["detail"] = w.Detail
			}
			withheld = append(withheld, e)
		}
		st["withheld"] = withheld
	}
	if o.Trace != nil {
		st["trace"] = o.Trace
	}
	if o.Halt != nil {
		// A halt summary; the full record is halt-record.json.
		h := map[string]any{"haltClass": string(*o.Halt)}
		if o.HaltReason != "" {
			h["reasonCode"] = o.HaltReason
		}
		if o.HaltSignal != "" {
			h["signal"] = o.HaltSignal
		}
		st["halt"] = h
	}
	return st
}

// haltRecord builds the machine-readable halt record. reasonCode is a stable lower_snake code, never a
// sentence; detail carries the human message, and signal carries any classified hint such as
// login_required or model_invalid.
func haltRecord(run *audit.Run, class, callID string, err error, now time.Time) map[string]any {
	rec := map[string]any{
		"schemaVersion": 1, "runId": run.ID, "occurredAt": now.UTC().Format(time.RFC3339),
	}
	if callID != "" {
		rec["callId"] = callID
	}
	if class != "" {
		rec["haltClass"] = class
	}
	if err != nil {
		// ReasonOf yields a code-shaped reason even for an untyped error.
		rec["reasonCode"] = fault.ReasonOf(err)
		rec["exitCode"] = int(fault.CodeOf(err))
		rec["detail"] = err.Error() // the human rendering
		if sig := fault.SignalOf(err); sig != "" {
			rec["signal"] = sig
		}
	}
	return rec
}

func hostAdjudications(adj adjudication.Result) []review.HostAdjudication {
	out := make([]review.HostAdjudication, 0, len(adj.Decisions))
	for _, d := range adj.Decisions {
		validity := "valid"
		if !d.Valid {
			validity = "invalid"
		}
		reason := d.Reasoning
		if reason == "" {
			reason = "host adjudication (deterministic, Batch 1)"
		}
		out = append(out, review.HostAdjudication{
			FindingID: d.FindingID, Validity: validity, DecisionState: d.State,
			SeverityAdjusted: d.SeverityAdjusted, Reasoning: reason,
		})
	}
	return out
}

func preflightJSON(rep doctor.Report) map[string]any {
	checks := make([]map[string]any, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		checks = append(checks, map[string]any{"name": c.Name, "ok": c.OK, "detail": c.Detail})
	}
	return map[string]any{"ok": rep.OK, "checks": checks}
}

func firstFailure(rep doctor.Report) string {
	for _, c := range rep.Checks {
		if !c.OK {
			return c.Name + " (" + c.Detail + ")"
		}
	}
	return "unknown"
}

func patchSummary(mode review.Mode, changes []workspace.FileChange) map[string]any {
	files := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		files = append(files, map[string]any{"file": c.File, "editCount": c.EditCount, "applied": mode == review.ModeApply})
	}
	return map[string]any{"schemaVersion": 1, "mode": string(mode), "patchArtifact": "patches/changes.patch", "files": files}
}

func summaryMarkdown(plan review.RunPlan, adj adjudication.Result, req Request) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# reviewmesh review summary\n\n")
	fmt.Fprintf(&sb, "- Mode: `%s`\n- Surface: `%s`\n- Workspace: `%s`\n- Findings: %d\n\n", plan.Mode, plan.Surface, req.Workspace, len(adj.Findings))
	if len(adj.Findings) == 0 {
		sb.WriteString("No findings.\n")
		return sb.String()
	}
	sb.WriteString("## Findings\n\n")
	panelled := false
	for i, f := range adj.Findings {
		fmt.Fprintf(&sb, "- **%s** [%s/%s] %s (`%s`) — %s", f.ID, f.Kind, f.Severity, f.Title, f.File, adj.Decisions[i].State)
		// Show the panel's consensus on the finding's own line.
		if phrase := ConsensusPhrase(len(adj.Decisions[i].SupportingSeats), len(adj.Decisions[i].DissentingSeats), adj.Decisions[i].Consensus); phrase != "" {
			fmt.Fprintf(&sb, " · %s", phrase)
			panelled = true
		}
		sb.WriteString("\n")
	}
	if panelled {
		// The note explains that "contested" does not mean the other seats disagreed.
		fmt.Fprintf(&sb, "\n> %s\n", DissentNote)
	}
	return sb.String()
}
