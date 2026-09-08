// Package review is the ReviewManager: it owns the review-and-remediate sequence
// (CUC-1) for the Batch-1 fake workflow — resolve → preflight → contain → invoke
// reviewer → verify identity → parse/adjudicate → report/patch/apply → audit.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	ArtifactDir string // base for run directories (e.g. tmp/reviewmesh)
	TempBase    string // base for isolated copies ("" = OS temp)
	Now         func() time.Time
}

// Request is one review invocation.
type Request struct {
	Workspace string
	Mode      review.Mode
	Surface   string
	// RunID, when set, NAMES this run's directory — supplied by a surface that has to hand its
	// caller a DURABLE HANDLE before the run exists.
	//
	// MCP is the one such surface, and the reason is its job shape: it answers with a run id the
	// instant a run is ADMITTED, long before this function creates any directory, and that id is the
	// only handle the caller ever receives (host paths are deliberately withheld on that wire). Two
	// independently minted identifiers — one for the client, one for the directory — is precisely
	// what left `review_remediate {fromRun}` unable to read its own on-disk decision set: the handle
	// a client held named nothing on disk, so an evicted or pre-restart run could only be refused.
	// One identifier closes that, and closes it without disclosing anything: the id stays opaque and
	// the directory it names is still never spelled on the wire.
	//
	// It is SERVER-GENERATED, never caller-supplied, and it is validated as a single path element
	// before it is joined to the artifact directory — see validateRunID. Empty (the CLI, ACP, and
	// every test) generates one from the run's start time, exactly as before.
	RunID           string
	Profile         string
	AdapterOverride map[review.Role]string // per-lane --set role.adapter=NAME
	ModelOverride   map[review.Role]string // per-lane --set role.model=NAME
	// ReviewerPanel is an AD-HOC blind primary panel composed by the invocation (CLI
	// `--reviewer`, ACP `_meta.reviewmesh.panel`). When present it REPLACES the selected
	// profile's panel — it never merges with it. COMPOSE-NOT-CONFIGURE: every seat must name an
	// adapter and model the CONFIGURATION already defines, so a request can select and order
	// configured identities but can never introduce an adapter, a binary path, or a launch
	// argument. An unresolvable seat is a config error before any spend.
	ReviewerPanel []review.SeatSpec
	// MaxParallel bounds how many seats invoke their model CLI AT ONCE. 0 (the default) runs every
	// seat in parallel; the panel size is then the only bound, which is the caller's own choice.
	//
	// It is stated PER INVOCATION rather than configured once, because the number it should be is a
	// fact about the machine the CLIs run on — resident memory and process count for a cloud CLI,
	// loaded weights for a local model — and about the caller's provider rate limits. A launch-time
	// or compiled-in constant would be aimesh guessing on behalf of hardware it cannot see.
	//
	// It bounds PARALLELISM only, never membership: every requested seat still runs, so lowering it
	// slows a review down without changing who reviewed or what was found.
	MaxParallel int
	// Authority are the AUTHORITY / CONTEXT documents this review is judged AGAINST —
	// requirements, design docs, specs. They are CONTEXT, never targets: never reviewed,
	// never patched, never applied, and never placed in the containment copy (they are
	// prompt-embedded, so the write layer cannot reach them by construction). Every surface
	// supplies the same declaration and internal/engine/authority enforces the identical
	// rules for all of them — root-scoped path reads, hash pinning, a fail-closed embedding
	// budget (no silent truncation), the report-mode-only inline provenance split, and the
	// quoted-evidence rendering that reaches every judging phase.
	Authority []review.AuthorityDoc
	// TrustedRoots are the OUT-OF-BAND trusted roots of a surface whose caller is NOT a
	// human: the ACP surface's `--root` directories (or its launch cwd), established before
	// any request arrived. When set, every REQUEST-supplied path this run reads — today the
	// authority `path` documents — must resolve INSIDE them, so a request can only narrow
	// the trusted set and never widen it.
	//
	// It is EMPTY on the CLI, where the human who typed the path is the consent, and it is
	// deliberately not required to contain the workspace: an agent surface may review a
	// directory IT materialized (ACP `inlineWorkspace`), which no human root covers. The
	// surface that accepted the request validates the workspace itself, before any spend.
	TrustedRoots []string
	// WorkspaceEphemeral marks a workspace THIS PROCESS materialized for the turn and deletes
	// when the turn ends (the ACP `inlineWorkspace`). It is recorded on the run's decision set so
	// that a later from-run write is refused by NAME — "that run reviewed content supplied over
	// the wire, into a directory that no longer exists" — rather than failing obscurely on a
	// vanished path. Only the surface that materialized the directory knows this, so only it can
	// say so; a review is otherwise unaffected by it.
	WorkspaceEphemeral bool
	// Select, when non-nil, NARROWS this run's write set to the accepted findings whose
	// HOST-COMPUTED fingerprint it names (D8-A, design §13.3). nil applies everything the run's own
	// adjudication authorizes; an EMPTY non-nil slice is a refusal, never "apply everything".
	//
	// It is meaningful only in a write mode. A `report` run writes nothing, so a selection on one
	// names a filter over an empty set — every surface refuses it before any spend rather than
	// accepting a parameter that could not have had an effect.
	//
	// WHAT IT MEANS ON A FULL CYCLE, precisely, because the answer differs from the MCP from-run
	// form and the difference matters: this run performs its OWN adjudication and the selection
	// filters THAT. A fingerprint is stable across runs (it is derived from the finding's kind,
	// file and normalized location), so a selector taken from an earlier report run will match the
	// same finding here IF this run's panel raises it again — and will land in `unmatched` if it
	// does not. This is a narrowing of what this run decided; it is not a replay of what an
	// earlier run decided. Only the MCP `review_remediate {fromRun}` form replays a stored set.
	Select []string
	// VerifyCommands are the PROJECT'S OWN build/test commands, run on the containment copy and
	// RECORDED. Empty (the default) means nothing is executed at all.
	//
	// They come from the OPERATOR — never from a model, never from a finding — and they run in a copy
	// of the operator's own project, so the threat model is exactly what it was: these are commands
	// the user already runs on that tree. Nothing model-authored is ever executed; see verify.go for
	// why the alternative (running seat-proposed reproducers) is refused pending a real sandbox.
	//
	// On a WRITE run (patch/apply) they run twice — before any edit and after every edit, both inside
	// the copy and both before the commit window opens — and the report's `delta` is the answer. On a
	// REPORT run there is no "after", so they run only when VerifyBaseline is set: a baseline alone is
	// a real wall-clock cost for one fact, which is a choice an operator should make rather than
	// inherit.
	//
	// THE RESULT NEVER GATES ANYTHING. A red suite does not invalidate a finding, does not stop a
	// commit, and does not change an exit code.
	VerifyCommands []string
	// VerifyTimeout bounds ONE command (0 → DefaultVerifyTimeout). Per command rather than per pass,
	// so the last command's budget does not depend on how long the first one took.
	VerifyTimeout time.Duration
	// VerifyBaseline opts a REPORT run into running the commands once. Ignored on a write run, which
	// runs them because there is an "after" to compare against.
	VerifyBaseline bool
	// AllowProtectedPaths waives the PROTECTED-CONFIG half of containment: it admits a workspace
	// root that is, or sits inside, `.git`/`.claude`/`.vscode`/`.aimesh`/… and permits writes
	// there. It exists because those are ordinary directories people own and legitimately ask a
	// review to fix — `~/.claude/hooks` is someone's actual source tree — and refusing them is the
	// tool overriding an explicit instruction about the user's own machine.
	//
	// IT DOES NOT TOUCH THE SECRET FAMILY. `.env*`, `.ssh`, `.aws`, key material and friends stay
	// refused for reads and writes alike, because a read is what puts the bytes in a prompt and a
	// prompt reaches a vendor: unlike an unwanted edit, that disclosure cannot be taken back. Root
	// confinement is untouched too — this widens WHICH names are allowed inside a root, never the
	// roots themselves.
	//
	// Operator act on every surface, for the reason above it: `.git/hooks/**` and `.vscode/mcp.json`
	// execute code on someone's next command, so a caller that could grant itself this could arrange
	// to run arbitrary code later.
	AllowProtectedPaths bool
	// Scope narrows what reviewers are SHOWN — explicit paths, a time window, or a version-control
	// baseline. The zero value shows everything, which is what every run did before scope existed.
	// See scope.go, in particular why the VCS baselines are one resolver among several rather than
	// the organising idea.
	Scope Scope
	// scopeFiles is Scope RESOLVED — the workspace-relative paths a reviewer may be shown. It is
	// unexported and filled in by RunContext, once, against the live workspace: resolving it per
	// pass would re-walk the tree for every seat and every attempt, and worse, a resolver that ran
	// twice could disagree with itself if a file changed between them. nil means no narrowing.
	scopeFiles map[string]bool
	// IncludeHostReview enables an optional report-only host self-review pass (the
	// author_remediator contributes its own findings, source=author_self_review). Valid
	// ONLY when the effective mode is report; rejected for patch/apply before any model call.
	IncludeHostReview bool
	// ValidateHostAdjudication enables an ACP-VALIDATION-ONLY readiness probe: after the normal cycle,
	// if the configured author_remediator host lane made NO natural adjudication model call (the
	// reviewer produced zero findings), run one synthetic host-adjudication call so ACP Validation
	// proves the host adapter/model can run its role. Report-mode only; OFF for normal CLI/agent
	// reviews (a real ACP client never sets it). NOT self-review (that is IncludeHostReview).
	ValidateHostAdjudication bool
	// Trace is the caller's W3C trace context, when the surface that accepted the request carried
	// one. nil (the zero value) records nothing, which is every CLI run and every legacy-era MCP
	// request — the three keys are a `2026-07-28` `_meta` convention. It is CARRIED to the run
	// record and never interpreted; see review.Trace.
	Trace *review.Trace
	// DryRun resolves everything and spends nothing: the plan, the panel, the authority
	// documents and the static preflight all run exactly as they would on a real review, the
	// same resolved-plan.json / config-effective.json / doctor.json are written, and the run
	// then STOPS before the first model call and returns review.RunOutcome.Shape.
	//
	// THE STOP POINT IS THE POINT. It sits after the last configuration error that is knowable
	// without spending — an unresolvable seat, a duplicate panel identity, a missing binary, an
	// oversized authority document, a failed preflight — and before the containment copy is
	// made or any adapter is invoked. So a dry run either hands back a shape or hands back the
	// exact refusal the real run would have hit, at zero cost, which is what makes it worth
	// typing before an expensive panel rather than a ceremony to skip.
	//
	// It is not a mode. Mode still governs what the run WOULD do (RunShape.Writes reports it),
	// because "what would --apply cost" is precisely the question worth asking for free, and a
	// dry run that silently became a report run could not answer it.
	DryRun bool
	// VerifyReadiness asks EVERY configured agent, before the first seat is dispatched, whether it can
	// actually do real work — a bounded one-token invocation in a throwaway directory, through the same
	// path a run takes (see meshcore/model.DeepProber and verifyReadiness).
	//
	// It exists because `Available()` proves only that a binary exists. Login, folder trust and model
	// validity all fail at INVOCATION time, so without this a panel pays for seat 1's full prompt and
	// then discovers at seat 2 that a second CLI was blocked all along. Probing first turns that
	// expensive discovery into a one-token one.
	//
	// IT SPENDS — one call per distinct adapter/model/effort — so it is opt-in, and a dry run PRICES it
	// without performing it.
	VerifyReadiness bool
	// OnEvent, if set, receives each audit event as it is logged — an optional, in-process
	// progress sink a surface can use to stream progress. The Manager stays surface-agnostic
	// (it only logs events); it never knows what the sink does. Must not block or panic, and
	// must be SAFE FOR CONCURRENT CALLS: the blind reviewer panel emits from one goroutine per
	// seat (the ACP sink serializes its frame writes for exactly this reason).
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

// RunContext executes the review and returns the outcome. The context cancels an
// in-flight run (CLI signal or ACP `cancel`) → a Class-cancellation halt. On a halt
// it returns a *fault.Fault (whose code the CLI maps to an exit code); the outcome
// still carries the run directory and any findings for the caller to render.
func (m *Manager) RunContext(ctx context.Context, req Request) (review.RunOutcome, error) {
	// The one request field that becomes a FILESYSTEM NAME is checked before anything else happens
	// — before the plan resolves, before an adapter is preflighted, and certainly before a directory
	// is created from it. See validateRunID.
	if rerr := validateRunID(req.RunID); rerr != nil {
		return review.RunOutcome{}, rerr
	}
	// availability of every registered adapter, so the auto-detect fallback skips
	// configured-but-missing adapters (it does not auto-select cloud adapters — only
	// `fake` is in any default preference).
	available := make(map[string]bool, len(m.Adapters))
	for name, a := range m.Adapters {
		ok, _ := a.Available()
		available[name] = ok
	}
	resolveReq := config.ResolveRequest{
		Profile: req.Profile, Mode: req.Mode, Surface: req.Surface, Available: available,
		AdapterOverride: req.AdapterOverride, ModelOverride: req.ModelOverride,
		ReviewerPanel: req.ReviewerPanel,
	}
	plan, err := m.Cfg.Resolve(resolveReq)
	if err != nil {
		return review.RunOutcome{}, err
	}

	if _, ok := plan.Lanes[review.RoleReviewer]; !ok {
		return review.RunOutcome{}, fault.New(fault.Config, "resolved plan has no reviewer lane").
			WithReason("plan_missing_reviewer_lane")
	}
	// THE BLIND PRIMARY PANEL, resolved BEFORE any spend. Every seat must resolve, the panel
	// must be 1..MaxReviewerSeats, and no two seats may share an identity — all config errors
	// here, never a trimmed panel later (see config.ResolvePanel). A panel of one is the
	// ordinary case and is exactly the historical single-reviewer run.
	seats, perr := m.Cfg.ResolvePanel(resolveReq)
	if perr != nil {
		return review.RunOutcome{}, perr
	}
	// includeHostReview is report-only: reject it for patch/apply on the EFFECTIVE mode,
	// before any audit/model work begins (a usage/config error, exit 3). The CLI also
	// rejects it early on the requested mode; this is the authoritative server-side guard
	// (a surface/config-set mode can only be caught here).
	if req.IncludeHostReview && plan.Mode != review.ModeReport {
		return review.RunOutcome{}, fault.New(fault.Config,
			fmt.Sprintf("includeHostReview is only valid in report mode (effective mode is %q)", plan.Mode)).
			WithReason("host_self_review_mode_invalid")
	}
	// Authority DECLARATION checks (pure, no I/O) run before any run/audit work, on the
	// EFFECTIVE mode — this is the authoritative server-side guard for the provenance split
	// (inline `content` authority is report-mode only). Surfaces run the same check to fail
	// fast in their own error carrier; this one cannot be bypassed by any of them.
	if err := authority.Validate(req.Authority, plan.Mode); err != nil {
		return review.RunOutcome{}, err
	}
	// Preflight EVERY lane's adapter: a resolved plan that selects any adapter that is
	// not registered or whose binary is unavailable must fail clearly BEFORE any
	// run/audit work begins (Class A / exit 4) — not just the reviewer lane.
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
	// EVERY seat's adapter is preflighted too — a panel whose second seat names a missing binary
	// must fail before any spend, not after the first seat has already been paid for.
	for i, s := range seats {
		if aerr := preflightAdapter(s.Adapter, fmt.Sprintf("reviewer seat %d of %d", i+1, len(seats))); aerr != nil {
			return review.RunOutcome{}, aerr
		}
	}

	// SCOPE, resolved ONCE and BEFORE ANY SPEND. A selector that names nothing, or a version-control
	// baseline in a tree that has none, refuses here — where it costs nothing — rather than after a
	// panel has been paid for to review either everything or nothing.
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
	// A DURABLE HOME FOR THE ONLY UNDO THIS TREE WILL HAVE.
	//
	// An apply into a tree under no version control has exactly one way back: reverse-applying the
	// run's own `patches/changes.patch`. And that tree is precisely the one whose run directory
	// defaults to the OS TEMP directory, because localstate.RunDir falls back there when no `.aimesh/`
	// exists. The least recoverable tree would otherwise keep its only recovery artifact in the most
	// disposable place — so the state directory is created for it, before the run, and the whole
	// record lands somewhere that survives a reboot.
	//
	// It is a no-op for every other tree, and a failure is not fatal: the run proceeds with the
	// artifacts it would have had before this existed.
	artifactDir := m.ArtifactDir
	if durable, ok := EnsureDurableRunDir(ctx, req.Workspace, string(plan.Mode), m.ArtifactDir); ok {
		artifactDir = durable
	}
	// req.RunID names the directory when a surface supplied one (MCP; see Request.RunID); empty
	// generates the start-time id every other surface uses.
	run, err := audit.NewRun(artifactDir, req.RunID, startedAt)
	if err != nil {
		return review.RunOutcome{}, fault.Wrap(fault.Internal, "create run dir", err)
	}
	run.OnEvent = req.OnEvent // optional in-process progress sink (surface-set; nil otherwise)
	// Trace rides the outcome from the START, before anything can halt: a halted run is the one most
	// worth correlating with the caller's own trace, and m.halt has no request in scope.
	outcome := review.RunOutcome{Mode: plan.Mode, Plan: plan, RunDir: run.Dir, RunID: run.ID, Status: "running", Trace: req.Trace}
	// The scope disclosure rides the outcome from the START, before anything can halt — a halted
	// narrowed run is exactly one someone would otherwise read as covering the whole tree.
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

	// Resolve the authority/context documents BEFORE any model call: read each root-scoped
	// path through meshcore/scope, verify any expectedHash pin, and enforce the embedding
	// budget FAIL-CLOSED (an oversized requireFull document halts naming its sizes — it is
	// never silently truncated). The inclusion manifest that comes back is written to the run
	// record and rides the outcome, so "which intent was this judged against" is a recorded
	// fact rather than an implication.
	//
	// A surface whose caller is not a human supplies TrustedRoots; the ONE resolver built
	// from them governs every request-supplied path this run reads, so a peer-declared
	// authority path can only narrow the trusted set, never authorize itself.
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

	// preflight (CUC-2 static checks) before any review work begins
	_ = run.Event(m.now(), "info", "doctor_started", "static preflight", nil)
	// The plan RESOLVED ABOVE is handed over, so readiness is judged against what this run will
	// actually invoke. Re-deriving it from the profile made preflight refuse valid runs: a `--set`
	// role override or a composed `--reviewer` panel is invisible to a bare config resolve, so a
	// plan that had just resolved was halted for a lane the run was never going to use.
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

	// Outer convergence cycle. Addressed-context feeds applied fingerprints forward
	// so already-applied findings become already_addressed (not actionable); apply
	// mode iterates until stable (no new edits) or the outer-cycle cap (halt, exit 7).
	// (The inner per-reviewer stabilization loop is a documented hook for a later
	// batch; Batch 2 runs one reviewer call per outer cycle.)
	maxOuter := 1
	if m.Cfg.Review.MaxOuterCycles != nil && *m.Cfg.Review.MaxOuterCycles > 0 {
		maxOuter = *m.Cfg.Review.MaxOuterCycles
	}
	// THE DRY-RUN STOP. Everything above resolved; nothing below is free. See Request.DryRun for
	// why the boundary is here and not earlier (a shape nobody could act on) or later (a shape
	// that already cost what it was meant to price).
	if req.DryRun {
		// WORKSPACE ADMISSIBILITY, judged before the shape is reported. It is a pure path check
		// (no copy, no read), so it costs nothing and belongs on the free side of the stop — and
		// without it a dry run answers "here is your plan, 2..10 calls" for a root the very next
		// step refuses outright, which is precisely the promise this flag makes and would break.
		// The operator's waiver applies HERE TOO, or the dry run refuses a root the real run would
		// accept — inverting the equivalence this check exists to hold. It was missed when the
		// waiver landed: AdmitRootWith was added and this, its only caller, kept the strict form,
		// so `--dry-run --allow-protected-paths` was refused while the same run without --dry-run
		// went ahead. Driving the real binary is what caught it; no test paired the two flags.
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

	// READINESS, before anything expensive. It sits just past the dry-run stop because it SPENDS: a
	// dry run prices these calls (shapeOf counts one per target) and performs none of them. Everything
	// above this line was free; this is the first thing that is not, and it is deliberately the
	// cheapest possible way to find out that an agent cannot work — a one-token question asked of
	// every configured agent at once, rather than a 900 KB prompt discovering it one seat at a time.
	if req.VerifyReadiness {
		if rerr := m.verifyReadiness(ctx, run, probeTargets(plan, seats)); rerr != nil {
			return m.halt(run, &outcome, startedAt, "", rerr)
		}
	}

	ws := workspace.New(m.TempBase)
	// The two halves of the operator's protected-path waiver travel together: admitting the
	// root without the write waiver would copy a tree nothing may edit, and the write waiver
	// without root admission would never reach a file to edit.
	ws.AllowProtectedRoots = req.AllowProtectedPaths
	// Root confinement for THIS run. The workspace path the caller was given by a human
	// (a CLI argument, an agent-host session cwd) is the consent boundary, so it is the
	// allowed root: every live write this run performs must land inside it and must not
	// hit a protected path. One resolver, built here, is shared by every surface — the
	// confinement rule cannot diverge per surface because no surface owns it.
	//
	// req.Workspace itself is deliberately NOT rewritten to its canonical form: the run
	// record should quote the path the human actually gave.
	confine, serr := scope.NewWith(scope.Options{AllowProtectedWrites: req.AllowProtectedPaths}, req.Workspace)
	if serr != nil {
		return m.halt(run, &outcome, startedAt, "",
			fault.Wrap(fault.Config, "resolve workspace root", serr).WithHalt("M6").WithReason(string(scope.ReasonUnresolvable)))
	}
	ws.Guard = confine
	addressed := map[string]review.DecisionState{} // fingerprint → state (for Judge)
	addressedDesc := map[string]string{}           // fingerprint → readable line (for prompts)
	// CROSS-RUN DISPOSITION MEMORY (opt-in; nil and inert by default — see dispositions.go).
	// It is handed `addressedDesc` and NOT `addressed`: a remembered disposition becomes prompt
	// text a reviewer may argue with, never an input to the suppression map the Judge reads.
	// The REPORT-mode verification baseline, when the operator asked for one. It runs here — before
	// the cycles — so it describes the tree the reviewers are about to be shown rather than whatever
	// it looks like once the run is over. A write run does NOT reach this: its baseline is taken
	// inside the write window, on the very copy the edits are derived from.
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

	// ACP-VALIDATION readiness probe (report-mode only): if the configured, real (non-fake) host lane
	// was never exercised by a NATURAL adjudication (zero reviewer findings → deterministic), run one
	// synthetic host-adjudication call so validation proves the host adapter/model can run. Its output
	// is discarded (never merged into `last`), so the real review result below is unaffected; a halt
	// propagates. OFF for normal reviews; the flag is set only by the ACP validator.
	if req.ValidateHostAdjudication && plan.Mode == review.ModeReport {
		if herr := m.ensureHostAdjudicationExercised(ctx, run, ws, req, plan, auth, startedAt, &outcome); herr != nil {
			return outcome, herr
		}
	}

	// Final self-critique (dismissal-verification): re-check the host's own rejections and overturn any
	// that were wrong, surfacing them report-only. Skipped for the ACP-validation probe (kept minimal).
	if !req.ValidateHostAdjudication {
		var scErr error
		last, scErr = m.finalSelfCritique(ctx, run, ws, req, plan, auth, last, startedAt, &outcome)
		if scErr != nil {
			return outcome, scErr
		}
	}

	// THE HOST ANNOTATION, applied once, over the FINAL set — after the merges, the report-only
	// appends and the id renumbering, so every finding that reaches a caller is labelled and none
	// is labelled twice.
	// THE CITATION GROUNDING PASS, in the same place and for the same reason: once, over the final set,
	// after the merges and the id renumbering. It executes nothing — it reads files this run already
	// read — and it writes Decision.Grounding and nothing else. A finding whose citation did not
	// resolve is labelled, never dropped or downgraded; see grounding.go for why that is not a
	// softening but the only claim the host is entitled to make.
	//
	// It runs AFTER any write this cycle performed, so on an apply run the check describes the tree as
	// it now stands. That is the honest ordering: a caller reading the result is looking at the tree
	// they have, not the one that was reviewed, and the write path's own base-hash pins are what tie a
	// decision back to what it judged.
	// PANEL COMPOSITION, in the same place and on the same rule: once, over the final set, writing
	// only labels. It qualifies what each agreement count was WORTH without changing a single count —
	// see composition.go for why an adjusted figure would need a taxonomy the host cannot verify.
	if outcome.Composition = composeAgreement(seats, &last); outcome.Composition != nil && outcome.Composition.Independence == IndependenceSharedModel {
		_ = run.Event(m.now(), "warn", "panel_shares_a_model", fmt.Sprintf(
			"this panel's %d seat(s) run %d distinct model(s) (%s): where seats that share a model agree, their errors correlate, so the agreement is worth less than the count suggests. No count was adjusted",
			len(outcome.Composition.Seats), outcome.Composition.DistinctModels, strings.Join(SharedModels(outcome.Composition.Seats), ", ")),
			map[string]any{
				"seats": len(outcome.Composition.Seats), "distinctModels": outcome.Composition.DistinctModels,
				"sharedModels": SharedModels(outcome.Composition.Seats),
			})
	}
	// THE DISSENT TALLY, over labels the panel already wrote onto each decision. It is a count, not a
	// pass: nothing is checked here, and nothing can be changed by it. It exists so a surface can say
	// how much of this result the panel agreed on without walking every decision, and so a reader
	// always gets the denominator.
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
	// Decisions ride the outcome index-aligned with Findings, so a surface projection is
	// built from the SAME adjudication result run-state and the summary are written from
	// (no second source of truth for a finding's disposition).
	outcome.Decisions = last.Decisions
	outcome.Status = "stable"
	_ = run.WriteJSON("run-state.json", runState(run, outcome, startedAt, m.now()))
	_ = run.WriteText("review-summary.md", summaryMarkdown(plan, last, req))
	// THE DURABLE DECISION SET (report runs only). It is what makes "apply the set you inspected"
	// expressible by a LATER, separate call on any surface, without an in-memory registry that dies
	// with the process. See decisionset.go.
	m.recordDecisionSet(run, req, plan, outcome)
	_ = run.Event(m.now(), "info", "run_completed", "review run completed", map[string]any{"status": outcome.Status})
	return outcome, nil
}

// runCycle performs one outer cycle: the BLIND PRIMARY PANEL (every seat in parallel, each with
// its own bounded stabilization loop) → host adjudication over the union of seat findings →
// optional cross_check → optional verifier → mode handling. Returns the final adjudication +
// edits applied; on a halt it writes halt artifacts and returns the error.
//
// Host adjudication does not begin until every seat has completed or halted — that ordering is
// the blindness guarantee at cycle scale, mirroring the per-seat guarantee inside runSeat.
func (m *Manager) runCycle(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, plan review.RunPlan, seats []review.LaneResolution, auth authority.Set, addressed map[string]review.DecisionState, addressedDesc map[string]string, callID string, cycle int, startedAt time.Time, outcome *review.RunOutcome) (adjudication.Result, int, error) {
	// THE REVIEWED ROOT'S IDENTITY, captured HERE — before any reviewer sees anything and before
	// any model call — and re-verified by the write path at the end of this cycle. A pathname is
	// not a repository: without this, the same string can be made to name a different tree during
	// the minutes a panel takes, and the accepted set would be written into a tree nobody reviewed.
	// It is captured per CYCLE, not per run, because a converging apply legitimately rewrites its
	// targets between cycles (which changes a single-file target's inode) and the question this
	// answers is "is this still the tree THIS cycle judged".
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
	// The run-level round budget for THIS cycle's panel. It bounds the whole panel, not each
	// seat, so N seats cannot multiply maxInnerIterations into an unbounded spend; the outer
	// cycle cap bounds how many times a cycle's budget can be spent. A panel of one gets
	// exactly maxInner rounds, i.e. today's inner loop unchanged.
	budget, berr := newPanelBudget(m.Cfg.Review.MaxPanelRounds, maxInner, len(seats))
	if berr != nil {
		_, e := m.halt(run, outcome, startedAt, callID, berr)
		return adjudication.Result{}, 0, e
	}
	// NOTE (intentional divergence from the methodology's "report = single-pass" rule): reviewmesh's
	// inner loop does NOT apply edits between passes (edits happen once, in handleMode, after the loop).
	// It is a union-of-findings ACCUMULATION loop — later passes carry a "previously reported this
	// cycle, focus on NEW issues" context and surface ADDITIONAL findings, which are unioned. Running
	// it in report mode therefore RAISES recall rather than wasting work, so report mode keeps the
	// multi-pass loop on purpose. (The outer cycle is still single-pass for report — nothing commits.)
	// SETTLED prior dispositions (applied in earlier outer cycles), as readable lines
	// so the model can correlate them with the findings it sees (not opaque hashes).
	addressedLines := sortedValues(addressedDesc)

	// `shown` is the union of files shown to ANY applyable lane (the panel + cross_check),
	// gating apply-safety so the host only edits files a reviewer actually saw.
	shown := map[string]bool{}

	// ---- HOST SELF-REVIEW pass (optional, REPORT-ONLY): the author_remediator contributes
	// its OWN independent findings (source=author_self_review) BEFORE the reviewer lanes. It
	// runs read-only under the same containment/identity verification as any reviewer lane;
	// its findings are adjudicated and surfaced in the report but NEVER applied (appended
	// report-only after handleMode). Gated to report mode in RunContext.
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

	// ---- BLIND PRIMARY PANEL: every seat, in parallel, each blind to the others ----
	panel, perr := m.runPanel(ctx, run, ws, req, seats, auth, addressedLines, callID, maxInner, budget, startedAt, outcome)
	if perr != nil {
		return adjudication.Result{}, 0, perr
	}
	reviewerFindings := panel.findings
	mergeShown(shown, panel.shown)

	// Host adjudication over the UNION of seat findings (deterministic for the fake/default
	// path; a single model-backed host call otherwise). It starts only now — after every seat
	// has completed or halted — so no seat could have been influenced by it.
	finalAdj, herr := m.adjudicate(ctx, run, ws, req, plan, auth, reviewerFindings, addressed, addressedLines, callID+"-host", startedAt, outcome)
	if herr != nil {
		return adjudication.Result{}, 0, herr
	}
	// HOST-COMPUTED provenance + the weak-identity quarantine, over the DEDUPED set.
	attachPanelProvenance(&finalAdj, panel)

	// ---- CROSS-CHECK lane (optional): find missed/disputed issues ----
	if ccLane, ok := plan.Lanes[review.RoleCrossCheck]; ok {
		ccAdapter, aerr := m.laneAdapter(ccLane)
		if aerr != nil {
			_, e := m.halt(run, outcome, startedAt, callID+"-cc", aerr)
			return adjudication.Result{}, 0, e
		}
		_ = run.Event(m.now(), "info", "cross_check_started", "cross-check lane", map[string]any{"adapter": ccLane.Adapter, "model": ccLane.Model})
		// CROSS-CHECK INPUT WITH A PANEL (design §Reviewer panel): the informed lane sees the
		// POST-ADJUDICATION decision set — one deduped, host-decided view — NOT N raw seat
		// streams. Feeding it every seat's raw output would scale its prompt with the panel and
		// re-expose the raw streams the seats were kept blind to. The set is bounded, and a
		// truncation is surfaced as a caveat line in the prompt plus a recorded event: never
		// silent.
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

	// ---- VERIFIER lane (optional): REPORT-ONLY this batch ----
	// NOTE (intentional divergence): reviewmesh's verifier is an INDEPENDENT final-check lane, NOT the
	// methodology's "iterating reviewer re-runs to catch cross-check regressions" (which is coupled to
	// cross-check). So it is not gated on cross-check — a standalone verifier is a valid config.
	// Regressions from applied edits are caught by reviewmesh's OUTER-cycle re-review (the primary
	// reviewer re-running on the edited workspace); the verifier is an extra report-only opinion. Its
	// findings are adjudicated and REPORTED but never applied/patched, so an apply run cannot loop on
	// newly-introduced verifier edits.
	var verifierAdj adjudication.Result
	if vLane, ok := plan.Lanes[review.RoleVerifier]; ok {
		vAdapter, aerr := m.laneAdapter(vLane)
		if aerr != nil {
			_, e := m.halt(run, outcome, startedAt, callID+"-verify", aerr)
			return adjudication.Result{}, 0, e
		}
		_ = run.Event(m.now(), "info", "verifier_started", "verifier lane", map[string]any{"adapter": vLane.Adapter, "model": vLane.Model})
		// Same bounded, post-adjudication view the cross-check gets (see above): the verifier is
		// an informed lane too, so a panel must not scale its prompt either, and a truncated
		// view is stated in the prompt rather than implied.
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

	// ---- MODE handling over reviewer + cross_check decisions (host writes only) ----
	applied, merr := m.handleMode(ctx, run, req, plan, auth, &finalAdj, shown, identity, cycle, outcome)
	if merr != nil {
		_, e := m.halt(run, outcome, startedAt, callID, merr)
		return adjudication.Result{}, 0, e
	}
	// Feed prior dispositions forward so the next outer cycle treats them as settled context
	// (the reviewer prompt lists these under a disagreement-gated PRIOR DISPOSITIONS section).
	// Carry not just APPLIED fixes but also INVALID rejections — otherwise a rejected finding is
	// re-raised every cycle, crowding the output budget and starving NEW findings (the exact
	// problem addressed-context exists to solve; methodology § Addressed-context construction).
	// Rejections go into the prompt desc ONLY, never the `addressed` suppression map the Judge
	// reads (adjudication.go): a reviewer that DISAGREES and re-raises must be re-adjudicated
	// fresh, not auto-marked already-addressed.
	for i, d := range finalAdj.Decisions {
		fp := schema.Fingerprint(finalAdj.Findings[i])
		switch d.State {
		case review.StateApplied:
			addressed[fp] = review.StateApplied
			addressedDesc[fp] = "applied (fixed): " + findingDesc(finalAdj.Findings[i])
		case review.StateInvalid, review.StateReportedInvalid:
			// prompt context only — do NOT add to `addressed` (see note above)
			if _, isApplied := addressed[fp]; !isApplied {
				addressedDesc[fp] = "rejected as invalid: " + findingDesc(finalAdj.Findings[i]) + reasonSuffix(d.Reasoning)
			}
		}
	}

	// Append verifier findings as REPORT-ONLY to the final result (after handleMode, so
	// they are surfaced in the report/run-state but never applied), then assign unique
	// final ids so lane-supplied ids can't collide in the output/audit.
	finalAdj = appendReportOnly(finalAdj, verifierAdj)
	// Host self-review findings are surfaced report-only too (never applied), then all ids
	// are renumbered so lane-supplied ids can't collide in the output/audit.
	finalAdj = renumberIDs(appendReportOnly(finalAdj, selfReviewAdj))
	// Re-run the write-path marking over the FINAL merged set: the verifier and host
	// self-review findings are appended after handleMode, so without this pass an
	// authority-only finding from one of those lanes would reach the projection unmarked.
	// The marking is idempotent.
	markApplyRefusals(auth, &finalAdj, shown)
	// The apply-safety gate, carried on the outcome: a LATER remediation of this decision set (the
	// MCP two-phase `review_remediate --fromRun`) must gate on the files THIS run showed, not on a
	// set re-derived from whatever is on disk when it runs.
	outcome.ShownFiles = sortedSet(shown)
	return finalAdj, applied, nil
}

// markApplyRefusals applies the WRITE-PATH RULE to every decision: a finding whose support
// cannot be traced to the workspace copy is marked NOT APPLYABLE, with a machine reason.
//
// It runs in EVERY mode — including report — because the marking is a governance FACT about
// the finding, not a consequence of what this particular run was allowed to write: a report
// run's projection must already tell a caller which findings a later apply run would refuse.
// It is inert when no authority was declared, so a review without authority is unchanged.
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

// laneAdapter returns the registered adapter for a resolved lane (preflight already
// verified availability; this is the registry lookup).
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

// maxInformedLaneInputs bounds how many adjudicated findings/decisions an INFORMED lane
// (cross_check, verifier) is shown. It exists because a panel scales the primary stage's output
// with its seat count, and an unbounded prompt would grow with it. Truncation is never silent:
// truncationCaveat goes into the prompt and a warn event goes into the audit record.
const maxInformedLaneInputs = 200

// truncationCaveat is the line appended to an informed lane's prompt context when the
// adjudicated set was bounded — so the model is told its view is partial rather than assuming
// it is complete.
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

// mergeAdjResults unions `add` into `base` by fingerprint. A finding new to base is
// appended with its decision. A finding base already decided is NOT silently
// overwritten; if `add` disagrees on validity, a report-only disagreement finding is
// appended so the dispute is auditable rather than mutating history.
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

// appendReportOnly appends verifier findings/decisions to the result as report-only
// (states forced to reported_valid/reported_invalid, which Actionable never applies),
// so they appear in the run result without ever being written. A verifier finding that
// re-raises an already-decided fingerprint is NOT dropped: if it disagrees with the
// host's earlier decision, a report-only disagreement finding is recorded (governance:
// a later lane's objection is auditable, never silently lost or overwritten).
// withSource returns a copy of fs with each finding's Source set to src (used to attribute
// host self-review findings as author_self_review).
func withSource(fs []review.Finding, src string) []review.Finding {
	out := make([]review.Finding, len(fs))
	for i, f := range fs {
		f.Source = src
		out[i] = f
	}
	return out
}

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

// renumberIDs assigns unique, aligned final ids (f1..fN) to every finding/decision in
// the merged result, so lane-supplied ids that repeat across lanes can't collide in the
// run output or audit. Fingerprint (not id) remains the stable identity.
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

// reasonSuffix renders a host's rejection reasoning as a short, single-line suffix so the
// reviewer can see WHY a finding was rejected and decide whether it has grounds to disagree
// (the disagreement gate is meaningless without the reason). Empty reasoning → no suffix.
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

// passOutcome is one semantic pass's complete result, with every shared-state side effect
// DEFERRED to the caller: the identity caveat, the failing-lane detail, and the halt-carrying
// fault are returned rather than applied. That is what lets an identical pass run inside a
// blind panel seat goroutine and inside the single-threaded cross_check/verifier lanes without
// two implementations.
type passOutcome struct {
	result schema.ReviewerResult
	shown  map[string]bool
	caveat *review.IdentityCaveat
	// identity is the identity TIER of the accepted attempt (verified | self_reported |
	// unknown) — the provenance ledger's per-seat evidence strength.
	identity string
	// withheld lists the files a containment rule kept out of THIS pass's reviewed set. It is
	// returned on the halt paths too: a file that was never shown is a fact about the run
	// whether or not the run then succeeded.
	withheld []review.WithheldFile
	failure  *review.LaneFailure
	err      error
}

// semanticPass runs one analysis call for a single-slot lane (cross_check / verifier / the host
// self-review) and commits its side effects to the outcome, halting the run on failure. It is
// the thin committing wrapper over runSemanticPass — the panel path calls runSemanticPass
// directly and commits in seat order instead.
func (m *Manager) semanticPass(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, reviewerLane review.LaneResolution, adapter model.Adapter, role review.Role, phase review.Phase, auth authority.Set, addressedLines, reported, decisions []string, callID string, startedAt time.Time, outcome *review.RunOutcome) (schema.ReviewerResult, map[string]bool, error) {
	po := m.runSemanticPass(ctx, run, ws, req, reviewerLane, adapter, role, phase, auth, addressedLines, reported, decisions, callID)
	if po.failure != nil {
		outcome.Failure = po.failure
	}
	// Committed BEFORE the halt branch: a halted run's record must still name the files that
	// were withheld from the reviewers, not lose them because the run ended badly.
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

// runSemanticPass runs one analysis call for any review lane or panel seat (reviewer /
// cross_check / verifier) with one corrective schema retry, and returns the parsed result plus
// the files shown to that reviewer. A non-retriable failure (M5/cancel/F/A/E) or a final schema
// failure (G) comes back as a halt-carrying fault in the returned passOutcome — this function
// writes only its OWN call directory, never the run-level halt record or the RunOutcome, so N
// seats can run it concurrently and the host still decides the halt deterministically. The lane
// only analyzes — it never writes; the host remains the sole adjudicator/writer.
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
	// pendingCaveat holds the accepted attempt's identity caveat (unknown/self-reported). It is
	// committed to outcome.IdentityCaveats ONLY after that attempt parses schema-valid output, so a
	// caveat always corresponds to a real, valid run (a malformed attempt then a verified retry leaves
	// no stale caveat).
	var pendingCaveat *review.IdentityCaveat
	// withheld accumulates the containment caveats of every attempt (deduped), so a file that
	// was never shown to this lane is a stated fact on the returned passOutcome rather than a
	// gap only a file-list diff would reveal.
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
		// Files the COPY withheld (a hardlinked file is never copied). Recorded before the
		// prompt is built, so the audit log says what was left out even if this attempt then
		// halts.
		wh := withheldFrom(stageCopy, copyH.Caveats)

		snippets, snipCaveats, cerr := workspace.CollectSnippetsWithCaveats(copyH.Root)
		if cerr != nil {
			// A containment refusal while assembling the prompt HALTS. The legacy collector
			// failed closed by returning an empty set, which is indistinguishable in the audit
			// record from "the workspace had nothing to show" — a refusal must be a recorded
			// fact, not an absence.
			m.eventWithheld(run, wh)
			return passOutcome{withheld: appendWithheld(withheld, wh...), err: collectFault(cerr)}
		}
		// ...and files the COLLECTOR withheld. Both lists ride the passOutcome rather than
		// being written to shared state here: this function runs inside a blind panel seat
		// goroutine, so every outcome mutation is the caller's to commit in seat order.
		wh = appendWithheld(wh, withheldFrom(stageSnippets, snipCaveats)...)
		m.eventWithheld(run, wh)
		withheld = appendWithheld(withheld, wh...)
		shown = make(map[string]bool, len(snippets))
		files := make([]reviewprompt.FileSnippet, len(snippets))
		for i, s := range snippets {
			files[i] = reviewprompt.FileSnippet{Path: s.Path, Content: s.Content}
			shown[s.Path] = true
		}
		// The analysis lanes see ALL authority (path + inline); the provenance split applies
		// only to host adjudication, which uses auth.AdjudicatorBlock().
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
			// CONTAINMENT: the lane's CLI runs IN the isolated copy (WorkDir → cmd.Dir), not in
			// the orchestrator's cwd. An agentic reviewer CLI reads relative paths and "the
			// current project" from wherever it was started, so leaving WorkDir empty pointed
			// every reviewer at the real tree that invoked reviewmesh, regardless of what the
			// prompt showed it. See docs/security.md for what this does NOT stop (absolute paths).
			CopyRoot: copyH.Root, WorkDir: copyH.Root, Prompt: prompt,
		}
		_ = run.WriteText(filepath.Join(attDir, "prompt.md"), prompt+"\n")
		_ = run.Event(m.now(), "info", "adapter_call_started", "invoking "+string(role), map[string]any{"adapter": reviewerLane.Adapter, "model": reviewerLane.Model, "phase": string(phase), "attempt": attempt})

		res, invErr := adapter.Invoke(ctx, call)
		_ = run.WriteText(filepath.Join(attDir, "stdout.txt"), string(res.Stdout)) // raw, always preserved
		_ = run.WriteText(filepath.Join(attDir, "stderr.txt"), string(res.Stderr))
		// `body` is the semantic content to parse: the unwrapped envelope payload when the
		// adapter provided one (e.g. claude-code), else raw stdout. The raw stdout stays
		// in stdout.txt for audit; reviewer-result.json holds the (unwrapped) body.
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

	// The lane (role/phase in call-status) is authoritative for the audit trail. If the
	// model's echoed role/phase doesn't match the lane (common with small models that
	// don't restate it), record a non-fatal warning rather than failing — the contract
	// is compatible-by-shape, and call-status already attributes the call to this lane.
	if rr.Role != string(role) || rr.Phase != string(phase) {
		_ = run.Event(m.now(), "warn", "lane_output_role_mismatch", "lane output did not echo the requested role/phase (call-status is authoritative)",
			map[string]any{"requestedRole": string(role), "requestedPhase": string(phase), "gotRole": rr.Role, "gotPhase": rr.Phase})
	}
	status.Verdict, status.Summary, status.Findings = review.Verdict(rr.Verdict), rr.Summary, rr.Findings
	_ = run.WriteJSON(filepath.Join(callDir, "call-status.json"), status)
	_ = run.Event(m.now(), "info", "reviewer_result_parsed", "parsed reviewer result", map[string]any{"role": string(role), "phase": string(phase), "findings": len(rr.Findings), "attempts": status.Attempts})
	// The identity caveat is now valid (the accepted attempt parsed schema-valid output); the
	// caller commits it. status.VerificationStatus is the accepted attempt's tier — the panel's
	// provenance ledger records it per seat.
	return passOutcome{result: rr, shown: shown, caveat: pendingCaveat, identity: status.VerificationStatus, withheld: withheld}
}

// wantsSelfReport reports whether an adapter's declared identity CEILING is a WEAK self-report — the
// only case in which the review/adjudication prompt asks for the inline identity wrapper. Strong-
// evidence adapters (codex/claude/ollama) and none adapters never receive it, so their prompts and
// verified paths are untouched. Reuses the same optional Evidence() ceiling the classifier caps at.
func wantsSelfReport(a model.Adapter) bool {
	ev, ok := a.(interface {
		Evidence() review.IdentityEvidence
	})
	return ok && ev.Evidence() == review.EvidenceSelfReport
}

// appendIdentityCaveat appends a caveat, deduped by role+adapter+model+evidence+status (a lane's
// schema retries would otherwise record the same caveat more than once).
func appendIdentityCaveat(caveats []review.IdentityCaveat, c review.IdentityCaveat) []review.IdentityCaveat {
	for _, existing := range caveats {
		if existing == c {
			return caveats
		}
	}
	return append(caveats, c)
}

// adjudicate turns reviewer findings into decisions: deterministic (the safe
// fake/default path) or via a model-backed host-adjudication call when the
// author_remediator lane resolves to a non-fake adapter and there are findings.
func (m *Manager) adjudicate(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, plan review.RunPlan, auth authority.Set, findings []review.Finding, addressed map[string]review.DecisionState, addressedLines []string, hostCallID string, startedAt time.Time, outcome *review.RunOutcome) (adjudication.Result, error) {
	base := adjudication.Judge(findings, addressed)
	hostLane, hasHost := plan.Lanes[review.RoleAuthorRemediator]

	// Collect the findings still "live" — i.e. NOT already in a deterministic terminal
	// state (already_addressed from a prior outer cycle, or skipped). Only these go to
	// the host. Each gets a UNIQUE host id (f1..fN) so cross-pass ID reuse can't cause
	// a duplicate-id parse failure. Terminal deterministic states are preserved (the
	// host can never re-activate an already_addressed finding → outer convergence holds).
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
		// Validity sets Valid; the decision State drives Actionable, which excludes
		// every non-apply state (invalid/skipped/already_addressed/reported_invalid/
		// upstream_conflict_deferred/withheld_class_e) — so a deferred/withheld host
		// verdict is never applied even when validity is "valid".
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

// finalSelfCritique is the methodology's dismissal-verification pass (§ Final self-critique): after
// the cycle settles, the host RE-CHECKS each finding it rejected as invalid against the files and
// OVERTURNS any rejection that was clearly wrong. An overturned finding is surfaced REPORT-ONLY
// (state reported_valid) — never silently applied after the cycle — so this can only RAISE recall
// (recover a wrongly-dropped finding); it can neither destabilize an apply run nor remove a finding.
// A fake/absent host, no rejected findings, or the ACP-validation probe is a no-op. A host halt here
// propagates (the same host just adjudicated successfully, so a failure signals a real problem).
func (m *Manager) finalSelfCritique(ctx context.Context, run *audit.Run, ws *workspace.Access, req Request, plan review.RunPlan, auth authority.Set, last adjudication.Result, startedAt time.Time, outcome *review.RunOutcome) (adjudication.Result, error) {
	hostLane, ok := plan.Lanes[review.RoleAuthorRemediator]
	if !ok || hostLane.Adapter == "fake" {
		return last, nil // deterministic/absent host: no model to re-verify a dismissal
	}
	hostAdapter, ok := m.Adapters[hostLane.Adapter]
	if !ok {
		return last, nil
	}
	// Collect the rejected findings + their original reasons; map a fresh host id → original index.
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
		// Overturn: surface it as a valid finding the user should act on, REPORT-ONLY (never applied
		// post-cycle). Actionable() excludes reported_valid, so an apply run cannot loop on this.
		last.Decisions[i].Valid = true
		last.Decisions[i].State = review.StateReportedValid
		last.Decisions[i].Reasoning = "overturned on final self-critique: " + a.Reasoning
		overturned++
	}
	_ = run.Event(m.now(), "info", "final_self_critique_completed", "dismissal-verification of rejected findings",
		map[string]any{"rejected": len(rejected), "overturned": overturned})
	return last, nil
}

// hostAdjudicationCall runs one host-adjudication model call (with one corrective
// schema retry) and returns the parsed result. Halts mirror reviewerPass.
// The `phase` argument sets the PERSISTED CallStatus.Phase label only (semantic_adjudicate for a
// natural adjudication, validate_adjudicate for the ACP readiness probe); the model prompt + output
// schema stay PhaseAdjudicate so ParseHostAdjudication is unaffected.
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
		// Committed here (this runs single-threaded, unlike a panel seat) and BEFORE the halt
		// branch, so a file withheld from the adjudicator is recorded either way.
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
		// PROVENANCE SPLIT: the host adjudicator sees PATH authority only. Inline caller-supplied
		// authority is deliberately withheld here — otherwise a client model could supply the
		// intent, its peers could find "deviations" from it, and the host would decide on the
		// strength of an artifact no human ever wrote.
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
			// CONTAINMENT: the adjudicator CLI runs in the isolated copy, never the orchestrator cwd.
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

// ensureHostAdjudicationExercised is the ACP-validation readiness probe. When the configured host lane
// is a REAL (non-fake) adapter that made NO natural adjudication call this run (zero reviewer findings),
// it runs ONE synthetic host-adjudication call — through the same model-adapter boundary, containment,
// schema, and identity classification as a natural one — so ACP Validation proves the host can run. Its
// adjudication OUTPUT is discarded (only the invocation + identity + call-status matter); a halt (Class
// A/E/G) propagates → the host lane is shown failed. A fake or unconfigured host, or a host already
// exercised by a natural adjudication, is a no-op.
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

// hostAdjudicationOccurred reports whether a NATURAL host adjudication model call ran this run — a
// per-call call-status.json under run.Dir with role=author_remediator AND phase=semantic_adjudicate.
// (A semantic_author_review self-review call does NOT count as adjudication readiness.) Fail-closed:
// only schemaVersion-1 records count.
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

// classifyAndVerify runs the no-retry halt checks (M5 from discarding copyH, cancel, invocation
// failure, non-zero exit) for one model call. On a halt it writes the per-call call-status and returns
// the halt-carrying fault with ok=false; ok=true ⇒ proceed. It returns a PENDING identity caveat (nil
// only when the identity is strongly verified) that the caller commits to outcome.IdentityCaveats ONLY
// after the accepted attempt parses schema-valid output — so a caveat always corresponds to a real,
// valid run. NO identity classification is a halt on any surface: unknown, self-reported and a proven
// mismatch alike are recorded and the lane's output is used.
//
// It deliberately does NOT touch the RunOutcome and does NOT write the run-level halt record: it
// runs inside a blind panel SEAT goroutine, where mutating shared run state would both race and
// make the halt-vs-halt ordering depend on wall clock. It reports (caveat, laneFailure, fault) to
// the caller, which commits them single-threaded in seat order (first halt by INDEX wins).
// `prompt` is the exact text this call SENT. It exists solely so the clihint classification below can
// subtract the adapter's echo of it; see laneFailure.
func (m *Manager) classifyAndVerify(ctx context.Context, run *audit.Run, ws *workspace.Access, copyH *workspace.Handle, lane review.LaneResolution, res model.Result, prompt string, invErr error, callID, callDir, attDir string, status *review.CallStatus) (*review.IdentityCaveat, *review.LaneFailure, error, bool) {
	// This runs once PER attempt against a REUSED CallStatus. Reset the per-attempt halt/reason fields
	// so a rejected earlier attempt's outcome (e.g. attempt 1 set model_identity_unknown) can never be
	// audited on a later ACCEPTED attempt (e.g. attempt 2 verified) — each attempt classifies fresh.
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
	// Cap the produced evidence at the adapter's declared CEILING (the registered recipe is the
	// authority): a self_report/none adapter can never be classified on a strong tier and faked as
	// `verified`. FAIL CLOSED — an adapter that exposes no `Evidence()` ceiling is treated as
	// EvidenceNone (→ unknown), so a future/custom adapter cannot bypass the cap by omitting it.
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
	// Any identity short of a strong-evidence match is a PENDING caveat — including a proven MISMATCH;
	// the caller commits it only after the accepted attempt parses schema-valid output. ReportedModel
	// preserves what the adapter said so the surfaced caveat never hides a suspicious reported value.
	//
	// IDENTITY NEVER GATES A FINDING. A lane that ran and produced schema-valid output is kept whatever
	// its identity says: what a reviewer FOUND is what decides whether the finding is worth anything, and
	// a label about which model produced it cannot make a real defect false. This replaced two Class-E
	// halts (an unconfirmed strong-evidence adapter, and a mismatch) that between them could discard a
	// whole review over provenance. See ../../../../docs/model-identity.md.
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

// laneFailure captures the failing lane's actionable detail (role/adapter/model/exit + raw stderr)
// for RunOutcome.Failure, so a surface can present the underlying adapter failure rather than only
// the top-level "adapter X exited N". The stderr is raw here; surfaces sanitize + cap it.
//
// The clihint classification happens HERE, once, and rides the failure: the persisted audit
// signal and the next-step a human is shown are then the same fact by construction, instead of
// two independent re-derivations that can drift.
// `prompt` is what this lane was SENT. It is passed so clihint can subtract the echo before
// classifying: several provider CLIs print the whole prompt to stderr, and our prompts embed the
// reviewed source — so without it the classifier reads OUR vocabulary and reports a signal about it.
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

// handleMode performs the report/patch/apply behavior, finalizes decision states,
// and returns how many edits were applied (committed to the live workspace).
//
// Report mode is entirely this function's business: nothing is written, so there is nothing to
// govern. Patch and apply DELEGATE — every byte this run puts in the live workspace goes through
// governedWrite (writepath.go), the same function `Manager.Remediate` calls for the MCP surface.
// That delegation is the point: a branch carrying its own copy/edit/commit sequence and committing
// with an unpinned `ws.Commit` would leave the CLI and ACP surfaces silently without the journal,
// the content pins, the cancel/commit gate, the receipt and the edit-target authorization the MCP
// surface has. There is one write path and one commit call site.
//
// What this function still owns is the DECISION STATE: which findings authorize a write (the
// write-path rule's untraceable refusals are marked here, before anything is offered to the write
// path), and what each finding's terminal state becomes given what the write path reports.
func (m *Manager) handleMode(ctx context.Context, run *audit.Run, req Request, plan review.RunPlan, auth authority.Set, adj *adjudication.Result, shown map[string]bool, identity WorkspaceIdentity, cycle int, outcome *review.RunOutcome) (int, error) {
	// WRITE-PATH RULE, marked before any mode branch so report mode records it too.
	markApplyRefusals(auth, adj, shown)
	if plan.Mode == review.ModeReport {
		for i := range adj.Decisions {
			if adjudication.Actionable(adj.Decisions[i]) {
				adj.Decisions[i].State = review.StateReportedValid
			}
		}
		return 0, nil
	}

	// The ids are about to AUTHORIZE writes, and the write path binds a decision to a finding by
	// id. Lane-supplied ids can repeat across lanes (the merge dedupes by fingerprint, not by id),
	// so a collision is resolved here rather than tolerated by a weaker binding. Ids that are
	// already unique are untouched — the remediation marker quotes them.
	uniqueFindingIDs(adj.Findings, adj.Decisions)

	// WRITE-PATH RULE — the injection backstop. An applied hunk must trace to evidence in the
	// workspace COPY, so a finding supported only by authority text is REPORTED and never applied.
	// It is decided here, before the write path is offered anything, because naming an authority
	// document is not an attempted escape (the human declared that document): it must degrade to a
	// report-only disposition rather than halt the run, which is the opposite of what the write
	// path does with an unauthorized target.
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

	// A run that converges over several outer cycles opens a write window per cycle, and each one
	// gets its own journal/receipt directory: overwriting cycle 1's journal with cycle 2's would
	// destroy the record of a write that already happened. The FIRST window keeps the documented
	// `remediation/` path, so a single-cycle run (every MCP remediation, and the ordinary CLI
	// apply) is spelled identically on every surface.
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
		// A full cycle writes what its own adjudication left actionable and un-refused. The
		// refusal marking above has already turned every untraceable finding into a report-only
		// decision, so this predicate never sees one.
		Accept: func(d review.Decision) bool {
			return adjudication.Actionable(d) && d.ApplyRefusalReason == ""
		},
		// THE NARROWING SELECTION (D8-A). It narrows EVERY cycle of a converging apply — a filter
		// that stopped applying after cycle 1 would write findings the caller excluded — but only
		// the first cycle is MEASURED against the caller's list (SelectPrimary), because by cycle 2
		// a selected finding may be absent precisely because cycle 1 applied it.
		Select: req.Select, SelectPrimary: cycle <= 1,
		Shown: shown,
		// The pins are RECORDED from the isolated copy this write is derived from, not carried
		// from an earlier run: a full cycle decides against the copy it just took. See
		// writepath.go.
		PinsFromCopy: true,
		// The operator's own build/test commands. On a CONVERGING apply this window is one of
		// several, and each records its own before/after — so the LAST one wins on the outcome
		// below, which is the right answer: it is the only pass taken against the tree the run
		// finally produced.
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
	// Protected-path refusals ride the outcome so the CLI and the ACP surface report the SAME
	// fact the receipt records. They accumulate across the run's outer cycles, deduped by
	// fingerprint — see appendRefusals for why the dedup is not cosmetic.
	outcome.Refusals = appendRefusals(outcome.Refusals, res.Refusals...)
	if werr != nil {
		return 0, werr
	}

	// The terminal decision state of every finding the write path handled, derived ONLY from what
	// it reported. `applied` is the convergence signal the outer cycle reads, so it counts findings
	// whose hunks actually reached the live tree — never findings that merely reached the copy.
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
			// A protected-path refusal is recorded on the DECISION with the same two fields the
			// write-path rule's other refusals use, so every projection that already renders
			// `applyable: false` + a reason renders this one too, without knowing it exists.
			// The state stays `reported_valid`: the finding is real and was reported; what was
			// refused is writing it.
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
	// `applied` is the convergence signal (live-tree writes only); `outcome.Applied` is the
	// REPORTED total and counts patch mode too, because a patch run that refused one finding must
	// still be able to say "7 applied, 1 refused" rather than "0 applied".
	outcome.Applied += len(res.Receipt.Applied)
	return applied, nil
}

// ScopeHaltClass is the halt-taxonomy class for a path-confinement refusal: a MECHANICAL
// containment check (like M5), not a call-result class — the host was asked to write
// somewhere it is not permitted to write. It maps to the existing containment exit code;
// no new exit code is introduced.
const ScopeHaltClass = "M6"

// authorizeTarget runs the run's write guard against one workspace-relative target and
// turns a refusal into a halt-carrying fault. It is a no-op when no guard is configured
// (a caller that constructed the Access itself) and for an empty path (the caller's own
// skip rules handle that).
//
// The returned error WRAPS the resolver's typed *scope.Denial, so a caller can ask which
// rule refused — governedWrite does exactly that, because a protected-path denial is now a
// recorded refusal while every other denial is still a halt. A non-Denial error is returned
// unwrapped rather than through scopeFault: scopeFault returns a typed nil for one, and a
// typed nil in an `error` is non-nil to the caller and answers no question correctly.
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

// scopeFault maps a path-confinement refusal to a typed halt: exit 6 (containment), halt
// class M6, and the resolver's own MACHINE reason code — so the audit record names the
// exact rule that refused rather than a generic "internal error". It returns nil for any
// error that is not a confinement refusal, so the caller can classify it normally.
func scopeFault(err error) *fault.Fault {
	d, ok := scope.AsDenial(err)
	if !ok {
		return nil
	}
	return fault.Wrap(fault.Containment, "refused by workspace scope policy", err).
		WithHalt(ScopeHaltClass).WithReason(string(d.Reason))
}

// collectFault classifies a failure to assemble the prompt's workspace snippets. A typed
// containment REFUSAL from meshcore/workspace (a protected copy root, a hardlinked file that
// may be a second name for a secret) gets exactly the treatment a scope denial gets — exit 6,
// halt class M6, the refusal's own MACHINE reason code — because it is the same kind of event:
// the host was asked to read something it is not permitted to read. Anything else is an
// ordinary internal failure. Either way the run STOPS: the legacy collector's empty-set return
// made a refusal look like an empty workspace in the audit record.
func collectFault(err error) *fault.Fault {
	if r, ok := workspace.AsRefusal(err); ok {
		return fault.Wrap(fault.Containment, "workspace snippets refused by containment policy", err).
			WithHalt(ScopeHaltClass).WithReason(string(r.Reason))
	}
	return fault.Wrap(fault.Internal, "collect workspace snippets", err).
		WithReason("workspace_collect_failed")
}

// --- withheld files (containment caveats) ---
//
// meshcore withholds a file rather than halting when withholding is the proportionate answer
// — today: a hardlinked regular file, whose innocuous in-tree name may be a second name for
// protected material, but which is also what an ordinary `cp -al` tree or dedup store looks
// like. Halting on one would hand any writer inside a trusted tree an availability switch.
//
// The whole reason that is SAFE is that the omission is stated. A file nobody was shown is a
// file no reviewer can object to, and a reviewer's silence reads as approval — so an
// unrecorded withhold converts a containment rule into a blind spot. These helpers carry
// meshcore's caveats onto the outcome, into the audit log, and out through the projection.

// stageCopy / stageSnippets name WHERE a file was withheld: never placed in the isolated copy
// at all, or present in the copy but never rendered into a prompt.
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

// appendWithheld merges withheld records, deduped by (stage, path, reason). The same file is
// withheld again on every retry, every seat and every cycle — the run record should say
// "these files were not shown", once each, not repeat the list N times.
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

// appendRefusals accumulates protected-path refusals across a run's outer cycles, DEDUPED BY
// FINGERPRINT.
//
// The dedup is load-bearing rather than tidy. A converging apply opens a write window per cycle,
// and a refused finding is refused again every time it is offered — it never reaches `applied`,
// so nothing marks it as addressed and the next cycle re-raises it. Without dedup a two-cycle run
// would report `refused: 2` for ONE finding, which is exactly the kind of recounted number the
// host-computes-the-counts rule exists to prevent. Each cycle's receipt still records its own
// window truthfully; this is the run-level roll-up.
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

// eventWithheld records the withholding in the audit log at the moment it happens, naming the
// path and the rule. It is a warn, not an info: a file dropping out of the reviewed set is
// something a human should see, not something to find later by diffing file lists.
func (m *Manager) eventWithheld(run *audit.Run, w []review.WithheldFile) {
	for _, e := range w {
		_ = run.Event(m.now(), "warn", "workspace_file_withheld",
			"a containment rule withheld this file from the reviewed set; it was NOT shown to any reviewer",
			map[string]any{"file": e.Path, "reasonCode": e.Reason, "rule": e.Rule, "detail": e.Detail, "stage": e.Stage})
	}
}

// remediationMaxBytes bounds the file content shown to the remediator (a huge file is truncated
// and the model is told to decline if the fix needs the omitted part — never a blind edit).
const remediationMaxBytes = 60000

// readBounded reads one file OF THE REMEDIATION COPY and caps it to remediationMaxBytes,
// reporting whether it was truncated.
//
// It takes the copy ROOT and a workspace-RELATIVE path rather than a joined path string,
// because the bytes it returns are shown to the remediation model. Joining and then calling
// os.ReadFile reopens the path by NAME, so an entry swapped for a symlink inside the copy
// between the shown-file checks and this read would be followed straight out of the copy.
// rootfile reads it through an identity-bound handle on the copy root instead: every
// component is resolved inside that root, the final component is opened no-follow, and what
// is validated is the OPENED descriptor.
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

// hostRemediationCall asks the host author_remediator to produce anchored edits that FIX one
// finding in `fileContent` (the target file's CURRENT content in the remediation copy). It is
// BEST-EFFORT: any problem — adapter error, non-zero exit, unparseable output, or a safe decline —
// returns nil so the caller falls back to the deterministic review marker. A remediation miss must
// never abort an otherwise-good apply run. The call runs read-only in a disposable copy of the
// CURRENT remediation state (`sourceRoot`, so the model sees edits already applied this run); the
// host lane's identity was verified by this cycle's adjudication, and every returned edit is
// re-validated (ParseRemediationResult) and applied to a copy then diffed, so a wrong edit cannot
// escape the safety envelope. One corrective retry mirrors the reviewer/adjudication calls.
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
			// CONTAINMENT: the remediator CLI runs in the isolated copy, never the orchestrator cwd.
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
	// The halt's machine reason/signal ride the outcome too, so a surface can project the
	// same codes the run directory records without re-reading it.
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
		// success = the identity was STRONGLY verified. Only VerifVerified qualifies: a weak
		// self_reported identity is ACCEPTED (the run proceeds) but is NOT strong verification, so it is
		// success=false with verificationStatus=self_reported — never conflated with verified. unknown
		// and mismatch are also success=false. verificationStatus carries the precise disposition.
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
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
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
	// The inclusion manifest rides run-state so the audit record answers "which intent was
	// this judged against" without opening a second file. The key is ABSENT when no authority
	// was declared, so a run that uses none is byte-identical to before.
	if len(o.Authority) > 0 {
		st["authority"] = o.Authority
	}
	// The files a containment rule kept OUT of the reviewed set, with the rule that did it.
	// Same shape of promise as `authority`: absent when there is nothing to say, so a run that
	// withheld nothing is byte-identical to before, and never a silent omission when there is.
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
	// The caller's W3C trace context, carried verbatim and never interpreted. ABSENT when the caller
	// sent none — which is every CLI run and every legacy-era request — so a run without one is
	// byte-identical to a run written before the field existed. `measures` inside the block says what
	// it does and does not account for: correlation, never token or cost figures, which no run in this
	// repo records. See review.Trace.
	if o.Trace != nil {
		st["trace"] = o.Trace
	}
	// CROSS-RUN DISPOSITION MEMORY's disclosure. Same shape of promise as `authority` and `trace`:
	// ABSENT when the run did not enable it (so the overwhelmingly common run is byte-identical to
	// one written before the feature existed), and PRESENT whenever it was — because "this blind
	// reviewer's prompt carried context from an earlier run" is a property of the review, not an
	// implementation detail a reader should have to reconstruct.
	if o.Halt != nil {
		// A machine-readable halt SUMMARY (the full record lives in halt-record.json):
		// class + stable reason code + any actionability signal.
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

// haltRecord writes the machine-readable halt record.
//
// `reasonCode` is a STABLE MACHINE CODE (lower_snake, e.g. "adapter_exited_nonzero",
// "scope_write_denied") — never a sentence. Carrying the fault's human message here would
// make the field unusable for automation: the text moves whenever the wording does.
// The human rendering lives in `detail`, and `signal` carries the classified
// actionability hint (login_required / folder_trust / model_invalid / update_prompt /
// timeout) when the failure produced one, so an automated consumer can react to WHY a run
// halted and WHAT to do about it without parsing prose.
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
		// ReasonOf/CodeOf/SignalOf are total: even an error that is not a typed fault
		// yields a code-shaped reason, so this record never carries a sentence where a
		// machine consumer expects a code.
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
		// WHAT THE PANEL DID WITH IT, on the same line as the finding, because a reader deciding
		// which finding to open next is deciding it here. A finding one seat of five reported and a
		// finding all five reported are different things to read, and until this they looked
		// identical in the artifact a person actually opens.
		if phrase := ConsensusPhrase(len(adj.Decisions[i].SupportingSeats), len(adj.Decisions[i].DissentingSeats), adj.Decisions[i].Consensus); phrase != "" {
			fmt.Fprintf(&sb, " · %s", phrase)
			panelled = true
		}
		sb.WriteString("\n")
	}
	if panelled {
		// The caveat travels WITH the labels, in the same document. A reader who meets the word
		// "contested" without it will supply their own meaning, and the meaning they supply — that
		// the other seats voted against this — is the one thing it does not mean.
		fmt.Fprintf(&sb, "\n> %s\n", DissentNote)
	}
	return sb.String()
}
