// Package mcp serves reviewmesh as a local MCP (Model Context Protocol) stdio server, so an agent can
// run a governed, blind, multi-model review and, when the operator allows it, apply the accepted
// findings. The protocol lives in meshcore/mcp; this package owns the tools, schemas, run registry and
// write window. It mirrors the exploremesh MCP surface where the two overlap.
//
// One tool writes to the user's filesystem on a model's request, which shapes the design:
//
//  1. The server reads no configuration. Adapters, the write grant and an optional root ceiling come
//     from launch arguments, and every call composes its own panel.
//  2. Scope is declared per call. Each call names an absolute workspace and any extra roots, judged
//     inside the operator's --root ceiling when set. Nothing is inferred from the process's cwd.
//  3. Writing is gated twice. output "apply" requires the --allow-writes launch grant and
//     allowWrite: true on the call; output "patch" supplies the diff on every server.
//  4. The gates and tool annotations coordinate with the host's permission layer but are not an
//     authorization boundary against a hostile client. Scope confinement, the write denylist, the
//     write-path evidence rule and fromRun's stored decision set hold regardless.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
	// The per-call scope builder is shared with the ACP surface, so both enforce one confinement model.
	"github.com/Tim-Butterfield/aimesh/internal/agentguide"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/review/version"
)

// ServerName identifies this server in the MCP handshake.
const ServerName = "reviewmesh"

// Tool names, as they appear on the wire.
const (
	toolReport    = "review_report"
	toolRemediate = "review_remediate"
	toolList      = "review_list"
	toolDoctor    = "review_doctor"
	toolRunStatus = "review_run_status"
	toolRunResult = "review_run_result"
)

// maxArgumentBytes bounds one tool call's arguments before decoding.
const maxArgumentBytes = 1 << 20 // 1 MiB

// Inline-workspace bounds. Inline content is model-authored and written to disk, so it is bounded per
// entry, in total and in count.
const (
	maxInlineEntries    = 200
	maxInlineEntryBytes = 256 << 10
	maxInlineTotalBytes = 768 << 10
)

// defaultTurnTimeout is the wall-clock budget for one run when none is configured, matching
// exploremesh.
const defaultTurnTimeout = 10 * time.Minute

// Reviewer is the manager capability this surface routes to: the review entry point shared with ACP
// plus the remediation entry points. Tests substitute a fake.
type Reviewer interface {
	RunContext(ctx context.Context, r run.Request) (review.RunOutcome, error)
	// ReadDecisionSetByID resolves a run id for a run over workspace to the decision set that run
	// recorded, verifying the handle against the workspace's run-record locations rather than trusting it
	// as a path. It returns the set and the canonical run directory.
	ReadDecisionSetByID(workspace, runID string) (*run.StoredDecisionSet, string, error)
	Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error)
}

// Server is the reviewmesh MCP server.
type Server struct {
	// Manager runs reviews; Config supplies the sanitized review_list and review_doctor projections.
	Manager Reviewer
	Config  Config

	// Adapters are the adapters this server was launched with (--adapter). A call's seats may name only
	// these; empty means every run is refused.
	Adapters launchflags.Set

	// Ceiling is the operator's optional --root set: every path a call declares must lie inside it. Empty
	// means no ceiling.
	Ceiling []string
	// ClientRoots, when non-empty, are the roots a legacy MCP client declared through roots/list. They only
	// narrow: every declared path must also lie inside them.
	ClientRoots []string

	// AllowWrites is the operator's --allow-writes grant. Without it aimesh never changes project content;
	// review_remediate still supplies the diff with output "patch".
	AllowWrites bool
	// VerifyCommands, VerifyTimeout and VerifyBaseline configure bounded execution: the project's own build
	// and test commands, run on the containment copy and recorded. Empty means nothing runs.
	//
	// They are launch settings only. A caller that could name a command would have arbitrary code execution
	// on the operator's machine.
	VerifyCommands []string
	VerifyTimeout  time.Duration
	VerifyBaseline bool
	// AllowProtectedPaths admits a workspace under a protected configuration path (.git, .claude, .vscode,
	// …) and permits writes there. Secrets stay refused. It is a launch setting because those paths execute
	// code on a later command.
	AllowProtectedPaths bool

	// WaitSeconds overrides the inline wait default. TurnTimeout bounds one run's wall clock (0 means 10
	// minutes). There are no admission bounds; callers state concurrency per call with maxParallel.
	WaitSeconds int
	TurnTimeout time.Duration

	// StrictSchema enables an opt-in check that every structuredContent satisfies its tool's outputSchema
	// before sending. See proto.Server.StrictSchema.
	StrictSchema bool

	Framing string
	// Protocol is the launch era posture (--protocol dual|legacy; "" means dual). review_doctor reports it,
	// since a host may discard stderr.
	//
	// SUNSET-PATH (MCP26-SUNSET): removed with the legacy era.
	Protocol proto.ProtocolMode
	// Diagnostics is the server's own log sink. It must never be the protocol stream.
	Diagnostics io.Writer

	runs *registry
	core *proto.Server

	// attached is set when this server registers into an external core (the combined aimesh mcp). It skips
	// the shared tools the composer registers once, such as agents_md.
	attached bool

	// artifacts publishes finished runs' artifacts as MCP resources, which is how a patch-mode
	// remediation's patch is fetched.
	artifacts proto.RunStore

	// clientMu guards ClientRoots, which a legacy client's roots/list answer replaces mid-session.
	clientMu sync.RWMutex
}

// clientRoots returns the roots a legacy client declared, or nil when it declared none.
func (s *Server) clientRoots() []string {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return append([]string(nil), s.ClientRoots...)
}

// applyClientRoots records a legacy roots/list answer. The roots only narrow (see callScope). A client
// that lists no roots narrows nothing, and non-file URIs are ignored.
func (s *Server) applyClientRoots(roots []proto.Root) {
	var paths []string
	skipped := 0
	for _, r := range roots {
		p, ok := r.Path()
		if !ok {
			skipped++
			continue
		}
		paths = append(paths, p)
	}
	s.clientMu.Lock()
	s.ClientRoots = paths
	s.clientMu.Unlock()
	s.core.Diagnosticf("aimesh review mcp: client declared %d root(s) (%d not a local file:// path, ignored); every path a call declares must lie inside them",
		len(roots), skipped)
}

// Serve builds the tool set and serves MCP on in and out until EOF. On return every in-flight run is
// cancelled, so a disconnected client leaves no model CLIs running.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	s.build()
	defer s.runs.cancelAll()
	return s.core.Serve(in, out)
}

// Core returns the underlying protocol server, for tests that drive it over an explicit framer.
func (s *Server) Core() *proto.Server {
	s.build()
	return s.core
}

func (s *Server) build() { s.buildInto(nil) }

// Attach registers this domain's tools into an external protocol core, as the combined aimesh mcp
// server does, and returns the resource and task providers the composer must merge.
//
// It also sets review's OnRoots handler on the shared core. Explore reads no paths, so client roots
// affect only review calls.
func (s *Server) Attach(core *proto.Server) (proto.ResourceProvider, proto.TaskProvider) {
	s.buildInto(core)
	return &s.artifacts, taskProvider{s}
}

// buildInto wires this server. With a nil core it creates its own (standalone aimesh review mcp);
// otherwise it registers into core.
func (s *Server) buildInto(core *proto.Server) {
	if s.core != nil {
		return
	}
	s.runs = newRegistry()
	s.runs.onEvict = s.artifacts.Forget
	if core != nil {
		s.attached = true
		s.core = core
		// On a shared core, set only review-specific fields; Info, Instructions, Framing and Protocol belong to
		// the composed server.
		s.core.OnRoots = s.applyClientRoots
	} else {
		s.core = &proto.Server{
			Info:         proto.Implementation{Name: ServerName, Title: "reviewmesh", Version: version.Get().Version},
			Instructions: instructions,
			Framing:      s.Framing,
			Protocol:     s.Protocol,
			Diagnostics:  s.Diagnostics,
			StrictSchema: s.StrictSchema,
			// The roots/list round trip runs only when the client declared the capability, and only narrows.
			OnRoots: s.applyClientRoots,
			// resources/*, so a governed write's patch can be collected. The store publishes no host path.
			Resources: &s.artifacts,
			// The tasks extension is a projection over the run registry (taskId == runId; see tasks.go). It is
			// opt-in per client and modern-era only.
			Tasks: taskProvider{s},
		}
	}
	s.registerTools()
}

func (s *Server) waitDefault() int {
	if s.WaitSeconds > 0 {
		return s.WaitSeconds
	}
	return DefaultWaitSeconds
}

func (s *Server) turnBudget() time.Duration {
	if s.TurnTimeout > 0 {
		return s.TurnTimeout
	}
	return defaultTurnTimeout
}

// writesValue returns who performs writes on this server: aimesh with --allow-writes, otherwise the
// agent.
func (s *Server) writesValue() string {
	if s.AllowWrites {
		return "aimesh"
	}
	return "agent"
}

// traceOf copies the caller's W3C trace context from the request into the run record's shape. It
// returns nil when the caller sent none, which includes every legacy-era request.
//
// Values are carried verbatim and never validated: the spec says implementations must not make
// assumptions about reserved _meta values, and nothing here branches on them.
func traceOf(env *proto.RequestEnv) *review.Trace {
	if env == nil {
		return nil
	}
	return review.NewTrace(env.Trace.TraceParent, env.Trace.TraceState, env.Trace.Baggage)
}

// --- annotations ---

// spendAnnotations returns annotations for a tool that starts a run: not read-only, open-world and not
// idempotent. A host uses readOnlyHint to decide whether to ask the user, so these must never claim
// read-only.
func spendAnnotations(title string, destructive bool) *proto.ToolAnnotations {
	return &proto.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: new(destructive),
		IdempotentHint:  false,
		OpenWorldHint:   new(true),
	}
}

// readOnlyAnnotations returns annotations for a tool that only reports: review_list, review_doctor,
// review_run_status and review_run_result.
func readOnlyAnnotations(title string) *proto.ToolAnnotations {
	return &proto.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: new(false),
		IdempotentHint:  true,
		OpenWorldHint:   new(false),
	}
}

func (s *Server) registerTools() {
	s.core.Register(proto.Tool{
		Name:  toolReport,
		Title: "Review an artifact with a blind multi-model panel",
		Description: "Runs the governed review cycle — a blind panel of independent reviewer seats, then HOST adjudication — and returns the adjudicated findings with their per-seat provenance. " +
			"It makes NO CHANGES to the workspace: to apply the accepted fixes afterwards, call review_remediate with this run's runId. " +
			"SPENDS MONEY: it launches the model CLIs the panel names. The workspace is an absolute path the call declares, and it is the call's root. " +
			"Job-shaped: if the run outlives waitSeconds you get a runId to poll with review_run_status.",
		InputSchema:  raw(reportInputSchema),
		OutputSchema: raw(reportResultSchema),
		// A user reads the title in a permission prompt, so it must agree with the hints: the tool spawns model
		// CLIs and writes a run directory but changes no workspace file.
		Annotations: spendAnnotations("Review (reports findings; writes no workspace changes)", false),
	}, s.reportHandler)

	// review_remediate is listed on every server: patch output changes no project content. Apply output
	// needs allowWrite on the call and the --allow-writes grant; see remediationMode.
	s.core.Register(proto.Tool{
		Name:  toolRemediate,
		Title: "Produce or apply accepted review findings",
		Description: "Produces or applies the ALREADY-ADJUDICATED accepted findings of a prior review_report run (`fromRun`, with the `workspace` that run reviewed) — nothing is re-reviewed and nothing is re-judged. " +
			"output=patch returns the complete diff and changes no project content; it works on every server, so you can apply the change with your own file tools. " +
			"output=apply WRITES THE WORKSPACE, needs allowWrite: true, and works only when the operator launched this server with --allow-writes (review_doctor reports `writes`). " +
			"NEITHER a retry NOR a restart can make it apply twice, but they report differently: while this server is running, a retry returns the ORIGINAL receipt; after a restart the run is re-read from its record, the first apply has already changed the files the decision set pins, and the second call HALTS (reasonCode stale_decision_set) having written nothing. " +
			"If any targeted file has changed since the review, the call HALTS rather than acting on a stale decision.",
		InputSchema:  raw(remediateInputSchema),
		OutputSchema: raw(remediateResultSchema),
		Annotations:  spendAnnotations("Remediate (patch, or apply with --allow-writes)", true),
	}, s.remediateHandler)

	s.core.Register(proto.Tool{
		Name:         toolList,
		Title:        "Report the launched adapters and write posture",
		Description:  "Reports what this server can run: the adapters it was launched with and whether each can be started now, the review modes, who performs writes (writes: aimesh|agent, diffAvailable), how many --root ceiling directories bound calls, and the admission limits in force. Read-only. Reports no binary paths, launch arguments, root paths or environment detail, and cannot change any configuration. Call this before composing a panel.",
		InputSchema:  raw(emptyInputSchema),
		OutputSchema: raw(listOutputSchema),
		Annotations:  readOnlyAnnotations("List configuration"),
	}, func(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
		payload := s.listPayload()
		return proto.Result(renderList(payload), payload), nil
	})

	s.core.Register(proto.Tool{
		Name:         toolDoctor,
		Title:        "Report adapter readiness",
		Description:  "Static readiness of the adapters this server would run: no process is started, nothing is spent, and no path or environment detail is reported. An unrecognized adapter is a failing check — it is never silently substituted.",
		InputSchema:  raw(emptyInputSchema),
		OutputSchema: raw(doctorOutputSchema),
		Annotations:  readOnlyAnnotations("Check readiness"),
	}, func(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
		payload := s.doctorPayload(c.Env())
		return proto.Result(renderDoctor(payload), payload), nil
	})

	// The agent guide, identical to `aimesh agents-md`. When attached, the composed server registers it
	// once for both domains.
	if !s.attached {
		s.core.Register(proto.Tool{
			Name:         agentguide.ToolName,
			Title:        agentguide.ToolTitle,
			Description:  agentguide.ToolDescription,
			InputSchema:  raw(agentguide.InputSchema),
			OutputSchema: raw(agentguide.OutputSchema),
			Annotations:  readOnlyAnnotations(agentguide.ToolAnnotationTitle),
		}, func(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
			text, payload, err := agentguide.ToolPayload()
			if err != nil {
				// An unreadable AGENTS_MD override is an error rather than a silent fallback to the embedded guide.
				return proto.ErrorResult(err.Error(), nil), nil
			}
			return proto.Result(text, payload), nil
		})
	}

	s.core.Register(proto.Tool{
		Name:         toolRunStatus,
		Title:        "Poll a run",
		Description:  "Reports whether a run started by review_report or review_remediate is still running. Read-only; it never starts, re-starts or re-applies anything. When the state is no longer `running`, fetch the result with review_run_result.",
		InputSchema:  raw(runIDInputSchema),
		OutputSchema: raw(runStatusSchema),
		Annotations:  readOnlyAnnotations("Run status"),
	}, s.runStatusHandler)

	s.core.Register(proto.Tool{
		Name:         toolRunResult,
		Title:        "Fetch a run's result",
		Description:  "Returns the full result of a run — the same payload the starting call would have returned had it finished in time, governance block, authority manifest and panel echo included; for a remediation, the receipt. Its output schema is therefore the UNION of the two: branch on `tool`. Read-only and free: re-reading a run costs nothing, never re-runs it, and never re-applies it.",
		InputSchema:  raw(runIDInputSchema),
		OutputSchema: raw(runResultSchema),
		Annotations:  readOnlyAnnotations("Run result"),
	}, s.runResultHandler)
}

// --- arguments ---

type seatArg struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
}

func (s seatArg) spec() review.SeatSpec {
	return review.SeatSpec{
		Adapter: strings.TrimSpace(s.Adapter), Model: strings.TrimSpace(s.Model), Effort: strings.TrimSpace(s.Effort),
	}
}

type panelArg struct {
	Reviewers        []seatArg `json:"reviewers"`
	CrossCheck       *seatArg  `json:"cross_check"`
	Verifier         *seatArg  `json:"verifier"`
	AuthorRemediator *seatArg  `json:"author_remediator"`
}

// runArgs are the run-forming parameters both review tools take.
type runArgs struct {
	Workspace       string                `json:"workspace"`
	InlineWorkspace map[string]string     `json:"inlineWorkspace"`
	Panel           *panelArg             `json:"panel"`
	Authority       []review.AuthorityDoc `json:"authority"`
	WaitSeconds     *int                  `json:"waitSeconds"`
	// MaxParallel bounds how many reviewer seats call their model CLI at once; omitted, the whole panel runs
	// in parallel. It is per call because the right value depends on the caller's machine and provider rate
	// limits. Every seat still reviews.
	MaxParallel *int `json:"maxParallel"`
	// DryRun resolves the plan, panel, authority documents and preflight, then stops before the first model
	// call and returns the run's shape: the seats, the calls and the model-call range. It raises every
	// configuration error a real run would, without spending.
	DryRun bool `json:"dryRun"`
	// VerifyReadiness spends one bounded one-token call per distinct adapter and model before dispatching,
	// so a seat blocked on login or folder trust halts the run before the panel spends. A dry run prices
	// these calls without making them.
	VerifyReadiness bool   `json:"verifyReadiness"`
	IdempotencyKey  string `json:"idempotencyKey"`
	// Roots are extra absolute directories this call reads beside its workspace, such as a folder holding
	// authority documents. See callScope.
	Roots []string `json:"roots"`
}

// callScope builds the confinement for one call from the paths it declares: its workspace and extra
// roots. Each path must be absolute, must not be the filesystem root, a home directory, a system tree
// or a protected directory, and must lie inside the --root ceiling when set; a legacy client's roots
// narrow further. The returned env carries the resolver.
//
// A call that declares no path gets the zero resolver, which refuses every filesystem path.
func (s *Server) callScope(env *proto.RequestEnv, workspace string, extra []string) (*proto.RequestEnv, *proto.CallToolResult, error) {
	var paths []string
	if ws := strings.TrimSpace(workspace); ws != "" {
		paths = append(paths, ws)
	}
	for _, r := range extra {
		if strings.TrimSpace(r) == "" {
			return nil, nil, proto.InvalidParams("invalid params: `roots` entries must be non-empty absolute directory paths")
		}
		paths = append(paths, r)
	}
	scoped := *env
	if len(paths) == 0 {
		scoped.Roots, scoped.Trust, scoped.Narrowed = nil, &scope.Resolver{}, false
		return &scoped, nil, nil
	}
	r, err := acp.CallScope(paths, s.Ceiling, "")
	if err != nil {
		switch fault.ReasonOf(err) {
		case acp.ReasonCallPathRelative, acp.ReasonCallNoPath:
			return nil, nil, proto.InvalidParams("invalid params: %v", err)
		}
		return nil, domainRefusal("", err), nil
	}
	client := s.clientRoots()
	if len(client) > 0 {
		narrow, nerr := scope.New(client...)
		if nerr != nil || narrow == nil {
			return nil, domainRefusal("", fault.New(fault.Containment, "the roots this client declared cannot be resolved, so no path is admitted").
				WithHalt("M6").WithReason(string(scope.ReasonUnresolvable))), nil
		}
		for _, p := range r.Roots() {
			if _, derr := narrow.ResolveRead(p); derr != nil {
				return nil, domainRefusal("", fault.New(fault.Containment, fmt.Sprintf("%q is outside the roots this client declared; a client's roots narrow what a call may read", p)).
					WithHalt("M6").WithReason(string(scope.ReasonOutsideRoot))), nil
			}
		}
	}
	scoped.Roots, scoped.Trust, scoped.Narrowed = r.Roots(), r, len(client) > 0
	return &scoped, nil, nil
}

// remediateArgs adds the write parameters. The server enforces fromRun's exclusivity itself rather than
// trusting the schema.
type remediateArgs struct {
	runArgs
	FromRun    string `json:"fromRun"`
	Output     string `json:"output"`
	AllowWrite *bool  `json:"allowWrite"`
	// Select names, by host-computed fingerprint, which of the source run's accepted findings to write.
	// Absent applies the whole accepted set; an empty list is refused. It is a pointer so absent and []
	// differ.
	Select *[]string `json:"select"`
}

// decode strictly decodes a tool's arguments, so an unknown or misspelled field is refused rather than
// ignored.
func decode(args json.RawMessage, into any) error {
	if len(args) > maxArgumentBytes {
		return proto.InvalidParams("invalid params: arguments are %d bytes, exceeding the %d-byte limit for one call", len(args), maxArgumentBytes)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return proto.InvalidParams("invalid params: %v", err)
	}
	return nil
}

// --- review_report ---

func (s *Server) reportHandler(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
	var args runArgs
	if err := decode(c.Arguments, &args); err != nil {
		return nil, err
	}
	prep, res, err := s.prepare(c.Env(), args, review.ModeReport, toolReport)
	if err != nil {
		return nil, err
	}
	if res != nil {
		return res, nil
	}
	wait, werr := s.waitBudget(args.WaitSeconds)
	if werr != nil {
		prep.cleanup()
		return nil, werr
	}
	return s.startReport(ctx, c, toolReport, prep, wait, strings.TrimSpace(args.IdempotencyKey), nil)
}

// prepared is everything a validated call needs in order to run.
type prepared struct {
	request run.Request
	pick    panelPick
	inline  bool
	cleanup func()
}

// prepare validates a run-forming request before any spend: the panel, the workspace (declared path or
// materialized inline content) and the authority declaration. It returns a prepared run, a domain-halt
// result, or a protocol error.
//
// A malformed request is a JSON-RPC invalid-params error. A refusal about the world, such as a path
// outside the call's scope or a mismatched hash pin, is an isError result carrying the halt taxonomy.
// Every path is judged against env.Trust, the resolver captured when the call arrived.
func (s *Server) prepare(env *proto.RequestEnv, args runArgs, mode review.Mode, tool string) (*prepared, *proto.CallToolResult, error) {
	// Build the call's scope first, so the workspace, authority documents and recorded roots are all judged
	// against one resolver.
	declared := args.Workspace
	if len(args.InlineWorkspace) > 0 {
		declared = ""
	}
	env, nres, nerr := s.callScope(env, declared, args.Roots)
	if nerr != nil || nres != nil {
		return nil, nres, nerr
	}
	pick, err := s.resolveSelection(args.Panel)
	if err != nil {
		return nil, nil, err
	}
	// An invalid maxParallel is refused rather than clamped.
	maxParallel := 0
	if args.MaxParallel != nil {
		maxParallel = *args.MaxParallel
		if maxParallel < 1 {
			return nil, nil, proto.InvalidParams("invalid params: maxParallel must be at least 1 (got %d) — omit it to run the whole panel in parallel", maxParallel)
		}
	}
	ws, inline, cleanup, res, err := s.resolveWorkspace(env, args, tool)
	if err != nil || res != nil {
		return nil, res, err
	}
	// Authority: declaration checks first (malformed requests), then full resolution (refusals about the
	// world).
	if verr := authority.Validate(args.Authority, mode); verr != nil {
		cleanup()
		return nil, nil, proto.InvalidParams("invalid params: authority: %v (reasonCode %s)", verr, fault.ReasonOf(verr))
	}
	if _, rerr := authority.Resolve(authority.Input{
		Docs: args.Authority, Mode: mode, Workspace: ws, Trust: env.Trust,
	}); rerr != nil {
		cleanup()
		structured, text := refusalResult("", rerr)
		return nil, proto.ErrorResult(text, structured), nil
	}
	req := run.Request{
		Workspace: ws, Mode: mode, Surface: "mcp",
		ReviewerPanel: pick.reviewers, ComposedRoles: pick.roles,
		Authority: args.Authority, TrustedRoots: env.Roots, Trace: traceOf(env),
		// Recorded on the durable decision set, so a later fromRun read from disk refuses an inline run by name.
		WorkspaceEphemeral: inline,
		// Bounded execution and containment waivers come from the operator's launch flags, never the call.
		VerifyCommands:      s.VerifyCommands,
		VerifyTimeout:       s.VerifyTimeout,
		VerifyBaseline:      s.VerifyBaseline,
		AllowProtectedPaths: s.AllowProtectedPaths,
		// From the call: how many CLIs the caller's machine can run at once.
		MaxParallel: maxParallel,
		// From the call: pricing is a question about this request.
		DryRun:          args.DryRun,
		VerifyReadiness: args.VerifyReadiness,
	}
	return &prepared{request: req, pick: pick, inline: inline, cleanup: cleanup}, nil, nil
}

// waitBudget validates the inline wait.
func (s *Server) waitBudget(v *int) (int, error) {
	wait := s.waitDefault()
	if v == nil {
		return wait, nil
	}
	if *v < 1 || *v > MaxWaitSeconds {
		return 0, proto.InvalidParams("invalid params: waitSeconds must be between 1 and %d (got %d) — it is the INLINE budget, not the run's own timeout; a longer run is fetched with review_run_result", MaxWaitSeconds, *v)
	}
	return *v, nil
}

// startReport admits, launches and waits inline for one review.
func (s *Server) startReport(ctx context.Context, c *proto.Call, tool string, prep *prepared, wait int, key string, then func(*record, review.RunOutcome, runview.View, error)) (*proto.CallToolResult, error) {
	// Check idempotency first: a retry after a dropped connection returns the original run instead of
	// starting a second panel.
	if prior := s.runs.existing(key); prior != nil {
		prep.cleanup()
		// A running prior run is handed back as a task to clients that declared the tasks extension, and as the
		// job shape otherwise.
		if taskHandOff(c, prior) {
			return nil, nil
		}
		return s.payloadFor(prior), nil
	}
	runID := newRunID()
	// The run id also names the run's directory, so its decision set is findable from the caller's handle.
	// See newRunID.
	prep.request.RunID = runID
	runCtx, cancel := context.WithCancel(context.Background())
	rec, aerr := s.runs.admit(runID, tool, string(prep.request.Mode), key, "", cancel)
	if aerr != nil {
		cancel()
		prep.cleanup()
		structured, text := refusalResult("", aerr)
		return proto.ErrorResult(text, structured), nil
	}
	// Attach the panel echo to the record so status and result calls can report it while the run is in
	// flight.
	rec.attachPick(prep.pick)

	// Progress is live only during the inline wait; notifications are correlated to this request's token.
	var sink atomic.Pointer[proto.Call]
	sink.Store(c)
	prep.request.OnEvent = func(ev audit.EventLine) {
		if live := sink.Load(); live != nil {
			if f, ok := phaseFraction[ev.EventType]; ok {
				live.Progress(f, 1, ev.Message)
			}
		}
		c.Log(logLevelFor(ev.Level), ServerName, map[string]any{
			"runId": runID, "eventType": ev.EventType, "message": ev.Message,
		})
	}

	go func() {
		defer cancel()
		defer prep.cleanup()
		tctx, tcancel := context.WithTimeout(runCtx, s.turnBudget())
		defer tcancel()
		out, rerr := s.Manager.RunContext(tctx, prep.request)
		view := runview.Build(runview.Input{
			Outcome: out, RequestedMode: prep.request.Mode, Err: rerr,
			ExitCode: int(fault.CodeOf(rerr)), ReasonCode: fault.ReasonOf(rerr), Signal: fault.SignalOf(rerr),
		})
		cancelled := errors.Is(rerr, context.Canceled) || runCtx.Err() != nil
		if then != nil {
			then(rec, out, view, rerr)
			return
		}
		s.finishReport(rec, prep, out, view, rerr, cancelled)
		s.runs.release(runID)
	}()

	return s.awaitRun(ctx, c, rec, prep.pick, cancel, wait, func() { sink.Store(nil) }), nil
}

// finishReport records a completed review: its result payload and, on success, the decision set a later
// fromRun remediation applies.
func (s *Server) finishReport(rec *record, prep *prepared, out review.RunOutcome, view runview.View, rerr error, cancelled bool) {
	if rerr != nil {
		structured, text := haltResult(rec, prep.pick, view, rerr, cancelled)
		state := StateHalted
		if cancelled {
			state = StateCancelled
		}
		rec.finish(state, structured, text, true)
		return
	}
	// Capture the decision set once, from the run that produced it, including the base hashes of every
	// targeted file.
	set := &decisionSet{
		Workspace: prep.request.Workspace, Inline: prep.inline,
		Profile: prep.request.Profile, Panel: prep.request.ReviewerPanel, Overrides: prep.request.ComposedRoles,
		Findings: out.Findings, Decisions: out.Decisions,
		Shown: map[string]bool{},
	}
	for _, f := range out.ShownFiles {
		set.Shown[f] = true
	}
	if !prep.inline {
		set.BaseHashes = run.BaseHashes(prep.request.Workspace, acceptedFiles(out))
		// Capture the reviewed root's identity so a remediation can prove it writes the reviewed tree. A capture
		// failure leaves the set unbound, and the write path refuses an unbound set.
		if id, ierr := run.CaptureWorkspaceIdentity(prep.request.Workspace); ierr == nil {
			set.WorkspaceIdentity = id
		}
	}
	structured, text, accepted := completeResult(rec, prep.pick, view, prep.inline, s.remediable(prep))
	set.Accepted = accepted
	rec.attach(set)
	rec.finish(StateComplete, structured, text, false)
}

// remediable reports whether a run's accepted set can be applied with review_remediate. An inline
// workspace cannot: its directory is deleted when the run ends.
func (s *Server) remediable(prep *prepared) bool {
	return !prep.inline
}

// acceptedFiles returns the workspace-relative files targeted by accepted findings, whose base hashes
// must still match at remediation.
func acceptedFiles(out review.RunOutcome) []string {
	seen := map[string]bool{}
	var files []string
	for i, f := range out.Findings {
		if i >= len(out.Decisions) || !run.AcceptedForApply(out.Decisions[i]) {
			continue
		}
		if f.File == "" || seen[f.File] {
			continue
		}
		seen[f.File] = true
		files = append(files, f.File)
	}
	return files
}

// awaitRun waits out the inline budget and then returns the run id, rather than holding a request open
// past the client's timeout.
//
// disarm disconnects the progress sink on every return path, so a run that outlives its inline wait
// stops sending progress for a request that has already been answered or cancelled.
func (s *Server) awaitRun(ctx context.Context, c *proto.Call, rec *record, pick panelPick, cancel func(), wait int, disarm func()) *proto.CallToolResult {
	if disarm != nil {
		defer disarm()
	}
	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()
	select {
	case <-rec.done:
		if c.HasProgressToken() {
			c.Progress(1, 1, "run complete")
		}
		return s.payloadFor(rec)
	case <-timer.C:
		// The inline budget expired with the run still going. A client that declared the tasks extension gets a
		// CreateTaskResult for the same run id; others get {runId, state: "running"}.
		if taskHandOff(c, rec) {
			return nil
		}
		structured, text := runningResult(rec, pick, wait)
		return proto.Result(text, structured)
	case <-ctx.Done():
		// The client cancelled or disconnected. Cancel the run so it stops spending and commits nothing. The
		// run record and log notification carry the outcome. The payload is the cancelled shape, which carries
		// the halt taxonomy and satisfies the outputSchema's cancelled branch.
		cancel()
		structured, text := runningResult(rec, pick, wait)
		structured["state"] = StateCancelled
		structured["haltClass"], structured["reasonCode"] = "cancelled", "run_cancelled"
		return proto.Result(text, structured)
	}
}

// payloadFor renders a record's current state as a tool result.
func (s *Server) payloadFor(rec *record) *proto.CallToolResult {
	state, structured, text, isErr := rec.snapshot()
	if state == StateRunning {
		out := map[string]any{"runId": rec.ID, "state": StateRunning, "tool": rec.Tool, "mode": rec.Mode}
		// The running branch of the review output schema requires the panel echo, which is known from admission.
		if rec.Tool == toolReport {
			out["panel"] = rec.pickSnapshot().echo(nil)
		}
		return proto.Result(fmt.Sprintf("Run %s is still running. Poll review_run_status, then fetch review_run_result.", rec.ID), out)
	}
	res := proto.Result(text, structured)
	if isErr {
		res = proto.ErrorResult(text, structured)
	}
	// Resource links ride every rendering of the record, since the call that collects a write is often not
	// the call that started it.
	res.Content = append(res.Content, rec.linksSnapshot()...)
	return res
}

// newRunID returns a run id of the form <UTC timestamp>-<sub-second>-<64 random bits>, for example
// 20260728T140311-0421-9f8e7d6c5b4a3210.
//
// The id is also the run directory's name, so review_remediate {fromRun} can find the stored decision
// set from the only handle an MCP client holds. It contains no host path. The timestamp prefix sorts
// runs chronologically beside CLI and ACP runs. The random suffix prevents two runs admitted in the same
// instant from sharing a directory, which audit.NewRun would not detect.
func newRunID() string {
	now := time.Now().UTC()
	ts := now.Format("20060102T150405") + fmt.Sprintf("-%04d", now.Nanosecond()/1e5)
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// If crypto/rand fails, fall back to the nanosecond clock.
		return ts + "-" + strconv.FormatInt(now.UnixNano(), 16)
	}
	return ts + "-" + hex.EncodeToString(b[:])
}

// --- review_remediate ---

func (s *Server) remediateHandler(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
	var args remediateArgs
	if err := decode(c.Arguments, &args); err != nil {
		return nil, err
	}
	mode, merr := s.remediationMode(args.Output)
	if merr != nil {
		return nil, merr
	}
	// An apply writes the workspace, so it is confirmed per call in addition to the launch grant. It is
	// checked first, before anything else about the request.
	if mode == review.ModeApply && (args.AllowWrite == nil || !*args.AllowWrite) {
		return nil, proto.InvalidParams(
			"invalid params: `allowWrite` must be present and literally true for output=apply — an apply writes the workspace and is confirmed per call. Corrected call: {\"fromRun\": \"<a review_report runId>\", \"workspace\": \"<absolute workspace>\", \"output\": \"apply\", \"allowWrite\": true}")
	}
	if len(args.InlineWorkspace) > 0 {
		return nil, proto.InvalidParams(
			"invalid params: `inlineWorkspace` cannot be remediated — the content was supplied over the wire and materialized into a directory this server owns and then deletes, so there is nothing real to write to. Review inline content with review_report, and remediate a workspace path that resolves inside this server's trusted roots.")
	}
	fromRun := strings.TrimSpace(args.FromRun)
	// MCP writes go only through fromRun. On 2026-07-28 stdio a cancelled request may receive no further
	// message, so a one-call write could leave the caller with no handle; fromRun is a value the caller
	// already received. The CLI keeps its one-call cycle because its user sees the run directory as the run
	// starts.
	if fromRun == "" {
		confirm := ""
		if mode == review.ModeApply {
			confirm = ", \"allowWrite\": true"
		}
		return nil, proto.InvalidParams(
			"invalid params: review_remediate works only from an existing run's accepted findings — call review_report first, then review_remediate {\"fromRun\": \"<runId>\", \"workspace\": \"<absolute workspace>\", \"output\": %q%s}. "+
				"The run id from review_report is your DURABLE HANDLE: it survives cancellation of this call.",
			string(mode), confirm)
	}
	if err := refuseRunFormingArgs(args.runArgs, fromRun, mode); err != nil {
		return nil, err
	}
	workspace := strings.TrimSpace(args.Workspace)
	if workspace == "" {
		return nil, proto.InvalidParams(
			"invalid params: `workspace` is required beside `fromRun` — name the ABSOLUTE workspace the source run reviewed. It locates that run's record, and it must be the same tree the run judged. Corrected call: {\"fromRun\": %q, \"workspace\": \"<absolute workspace>\", \"output\": %q}", fromRun, string(mode))
	}
	wait, werr := s.waitBudget(args.WaitSeconds)
	if werr != nil {
		return nil, werr
	}
	// Validate select before any spend: an empty list is refused with a correction.
	var selection []string
	if args.Select != nil {
		selection = *args.Select
		if len(trimmedNonEmpty(selection)) == 0 {
			return nil, proto.InvalidParams(
				"invalid params: `select` is present and empty. An empty narrowing filter names ZERO findings — it does not mean \"apply everything\", and accepting it would let a call read as a normal apply while writing nothing. Omit `select` to apply run %q's whole accepted set, or name the `fingerprint` values from that run's accepted findings.", fromRun)
		}
	}
	// The call's scope also governs the write; its declared roots are what the receipt records.
	env, nres, nerr := s.callScope(c.Env(), workspace, args.Roots)
	if nerr != nil {
		return nil, nerr
	}
	if nres != nil {
		return nres, nil
	}
	return s.remediateFromRun(ctx, c, env, workspace, fromRun, mode, wait, strings.TrimSpace(args.IdempotencyKey), selection)
}

// trimmedNonEmpty returns ss without blank entries, the same normalization the write path applies.
func trimmedNonEmpty(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// refuseRunFormingArgs rejects run-forming parameters on review_remediate by name. The schema forbids
// them, but the decoder embeds runArgs, and the panel and authority were settled by the source run.
func refuseRunFormingArgs(args runArgs, fromRun string, mode review.Mode) error {
	var named []string
	if args.Panel != nil {
		named = append(named, "`panel`")
	}
	if len(args.Authority) > 0 {
		named = append(named, "`authority`")
	}
	if args.DryRun {
		// A dry run prices a review; a remediation writes, so accepting the flag would suggest nothing is
		// written.
		named = append(named, "`dryRun`")
	}
	if args.VerifyReadiness {
		named = append(named, "`verifyReadiness`")
	}
	if len(named) == 0 {
		return nil
	}
	return proto.InvalidParams(
		"invalid params: %s cannot be set on review_remediate — it acts on an accepted set that a prior review_report run already produced, so the panel and the authority manifest are the source run's and are not re-decided here. The parameter is refused rather than ignored. Drop it and pass {\"fromRun\": %q, \"workspace\": \"<absolute workspace>\", \"output\": %q}, or run a fresh review_report with the panel and authority you want.",
		strings.Join(named, " and "), fromRun, string(mode))
}

// remediationMode validates `output` against this server's write grant.
func (s *Server) remediationMode(output string) (review.Mode, error) {
	switch review.Mode(strings.TrimSpace(output)) {
	case review.ModePatch:
		return review.ModePatch, nil
	case review.ModeApply:
		if !s.AllowWrites {
			return "", proto.InvalidParams(
				"invalid params: this server was launched without --allow-writes, so aimesh cannot apply changes here and `output: \"apply\"` is refused. Use `output: \"patch\"` to receive the complete diff and apply it with your own file tools.")
		}
		return review.ModeApply, nil
	default:
		return "", proto.InvalidParams(
			"invalid params: `output` must be \"patch\" (produce a diff, change nothing) or \"apply\" (write the live workspace); got %q", output)
	}
}

// remediateFromRun applies a prior review's accepted set.
func (s *Server) remediateFromRun(ctx context.Context, c *proto.Call, env *proto.RequestEnv, workspace, fromRun string, mode review.Mode, wait int, key string, selection []string) (*proto.CallToolResult, error) {
	// Idempotency has two forms: the key covers explicit retries, and the source-run guard covers retries
	// without the key. Both return the original receipt. These lookups are a fast path; the binding decision
	// is made atomically at reservation (registry.reserve).
	if prior := s.runs.existing(key); prior != nil {
		return s.attachTo(ctx, c, prior, wait), nil
	}
	if prior := s.runs.existingForSource(fromRun); prior != nil {
		// A selective apply gets no second write window on the same source run: a second call with a different
		// select is still a second application of the same set. It attaches to the winner and returns the
		// original receipt, including its selection. To apply more findings, run a fresh review_report.
		return s.attachTo(ctx, c, prior, wait), nil
	}
	rec := s.runs.get(fromRun)
	if rec == nil {
		// A registry miss is not final: the registry is bounded and in-memory, so a handle for a run this process
		// produced (evicted, or from before a restart) is resolved from the run's directory, as on ACP.
		return s.remediateFromDisk(ctx, c, env, workspace, fromRun, mode, wait, key, selection)
	}
	state, _, _, _ := rec.snapshot()
	if state != StateComplete {
		return domainRefusal("", fault.New(fault.Policy, fmt.Sprintf(
			"run %q is %s — only a COMPLETED review_report run has an accepted set to apply. Poll review_run_status until it completes.", fromRun, state)).
			WithReason("source_run_not_complete")), nil
	}
	set := rec.decisions()
	if set == nil {
		return domainRefusal("", fault.New(fault.Policy, fmt.Sprintf(
			"run %q carries no decision set (only a review_report run does; a remediation cannot be remediated)", fromRun)).
			WithReason("source_run_not_reviewable")), nil
	}
	if set.Inline {
		return domainRefusal("", fault.New(fault.Policy, fmt.Sprintf(
			"run %q reviewed an INLINE workspace: the content was supplied over the wire and materialized into a directory this server has since deleted, so there is nothing real to write to.", fromRun)).
			WithReason("inline_workspace_not_remediable")), nil
	}
	if set.Accepted == 0 {
		return domainRefusal("", fault.New(fault.Policy, fmt.Sprintf(
			"run %q has no accepted findings to apply (%d finding(s), none in the accepted set). There is nothing to write.", fromRun, len(set.Findings))).
			WithReason("no_accepted_findings")), nil
	}
	if !sameWorkspace(set.Workspace, workspace) {
		return domainRefusal("", workspaceMismatch(fromRun)), nil
	}
	req := run.RemediateRequest{
		Workspace: set.Workspace, WorkspaceIdentity: set.WorkspaceIdentity,
		Mode: mode, Surface: "mcp",
		Profile: set.Profile, ReviewerPanel: set.Panel, ComposedRoles: set.Overrides,
		TrustedRoots: env.Roots, SourceRunID: fromRun,
		Findings: set.Findings, Decisions: set.Decisions,
		// The selection passes unexamined to the governed write path; this surface owns only the empty-list
		// refusal.
		Select: selection,
		Shown:  set.Shown, BaseHashes: set.BaseHashes,
		// This call's trace, not the report run's.
		Trace: traceOf(env),
		// The operator's launch waivers apply to fromRun writes as to any governed write.
		AllowProtectedPaths: s.AllowProtectedPaths,
	}
	return s.startRemediate(ctx, c, req, wait, key)
}

// remediateFromDisk resolves a run handle the registry no longer holds from the run's directory, using
// the same reader and refusals as ACP. It runs only after both in-process idempotency lookups missed.
//
// The handle is verified, not trusted: it is joined to each of the workspace's run-record locations and
// checked lexically, then canonically, then for schema version and runId matching the directory name.
// Every failure answers unknownRun.
//
// After a restart a second apply does not return the original receipt; the first apply changed the
// pinned files, so it halts with stale_decision_set having written nothing. No durable "already applied"
// marker is kept, since a wrong one would report false success.
func (s *Server) remediateFromDisk(ctx context.Context, c *proto.Call, env *proto.RequestEnv, workspace, fromRun string,
	mode review.Mode, wait int, key string, selection []string) (*proto.CallToolResult, error) {

	set, _, err := s.Manager.ReadDecisionSetByID(workspace, fromRun)
	if err != nil {
		// Nothing on disk either. A decision set is written only by a completed report run, so a running, halted
		// or remediation run has none, and source_run_not_complete and source_run_not_reviewable cannot arise
		// here.
		return unknownRun(fromRun), nil
	}
	// The same refusals, in the same order and with the same reason codes, as the registry path.
	if rerr := set.Remediable(); rerr != nil {
		return domainRefusal("", rerr), nil
	}
	if !set.NamesWorkspace(workspace) {
		return domainRefusal("", workspaceMismatch(fromRun)), nil
	}
	// Re-verify the identity binding before any spend: the canonical path and device and inode key.
	identity, ierr := set.BindWorkspace()
	if ierr != nil {
		return domainRefusal("", ierr), nil
	}
	req := run.RemediateRequest{
		// Write against the canonical workspace, which BindWorkspace just verified.
		Workspace: set.WorkspaceCanonical, WorkspaceIdentity: identity,
		Mode: mode, Surface: "mcp",
		Profile: set.Profile, ReviewerPanel: set.Panel, ComposedRoles: set.Roles,
		// The roots this call declares; the governed write path re-gates the write against them.
		TrustedRoots: env.Roots,
		// Use the handle, which ReadDecisionSet proved equals set.RunID, so the reservation key matches the
		// fast-path lookup.
		SourceRunID: fromRun,
		Findings:    set.Findings, Decisions: set.Decisions,
		// The selection passes unexamined to the governed write path, as on the registry path.
		Select: selection,
		Shown:  set.ShownSet(), BaseHashes: set.BaseHashes,
		Trace: traceOf(env),
		// The operator's launch waivers, as on the registry path.
		AllowProtectedPaths: s.AllowProtectedPaths,
	}
	return s.startRemediate(ctx, c, req, wait, key)
}

// ReasonSourceRunWorkspaceMismatch refuses a remediation whose named workspace is not the tree the
// source run reviewed.
const ReasonSourceRunWorkspaceMismatch = "source_run_workspace_mismatch"

// sameWorkspace reports whether named is the tree recorded, compared after resolving symlinks.
func sameWorkspace(recorded, named string) bool {
	canon := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			return filepath.Clean(p)
		}
		if r, rerr := filepath.EvalSymlinks(abs); rerr == nil {
			return r
		}
		return abs
	}
	return recorded != "" && named != "" && canon(recorded) == canon(named)
}

func workspaceMismatch(fromRun string) error {
	return fault.New(fault.Policy, fmt.Sprintf(
		"run %q did not review the named workspace; name the absolute workspace that run reviewed", fromRun)).
		WithReason(ReasonSourceRunWorkspaceMismatch)
}

// attachTo waits out the inline budget on a run this call did not start: the winner of an idempotency
// or source-run reservation. It holds no cancel handle, so a spectator's disconnect cannot cancel the
// winner's write.
func (s *Server) attachTo(ctx context.Context, c *proto.Call, rec *record, wait int) *proto.CallToolResult {
	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()
	select {
	case <-rec.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	// A spectator gets a task handle on the same terms; polling a task is a read.
	if taskHandOff(c, rec) {
		return nil
	}
	return s.payloadFor(rec)
}

// startRemediate reserves and runs one fromRun remediation. The reservation is atomic over the
// idempotency key and the source run, so every other call for the same decision set attaches to the
// first instead of opening a second write window.
//
// This surface has no one-call review-and-write: on MCP a cancelled response could leave the caller
// without a handle. Use review_report, then review_remediate {fromRun}.
func (s *Server) startRemediate(ctx context.Context, c *proto.Call, req run.RemediateRequest, wait int, key string) (*proto.CallToolResult, error) {
	runID := newRunID()
	runCtx, cancel := context.WithCancel(context.Background())
	rec, prior, aerr := s.runs.reserve(runID, toolRemediate, string(req.Mode), key, req.SourceRunID, cancel)
	if aerr != nil {
		cancel()
		structured, text := refusalResult("", aerr)
		return proto.ErrorResult(text, structured), nil
	}
	if prior != nil {
		cancel()
		return s.attachTo(ctx, c, prior, wait), nil
	}
	req.OnEvent = func(ev audit.EventLine) {
		c.Log(logLevelFor(ev.Level), ServerName, map[string]any{
			"runId": runID, "eventType": ev.EventType, "message": ev.Message,
		})
	}
	go func() {
		defer cancel()
		defer s.runs.release(runID)
		tctx, tcancel := context.WithTimeout(runCtx, s.turnBudget())
		defer tcancel()
		s.runRemediation(tctx, c, rec, req)
	}()
	// A remediation emits only log notifications, which are not request-correlated, so there is no progress
	// sink to disarm.
	return s.awaitRun(ctx, c, rec, panelPick{}, cancel, wait, nil), nil
}

// runRemediation drives the write window and publishes the receipt as a log notification and on disk.
// A cancelled call gets no response, and on 2026-07-28 stdio not even the notification, so the durable
// receipt is the guarantee.
func (s *Server) runRemediation(ctx context.Context, c *proto.Call, rec *record, req run.RemediateRequest) {
	req.OnJournal = func(j run.Journal) {
		c.Log(proto.LevelNotice, ServerName, map[string]any{
			"runId": rec.ID, "eventType": "remediation_journaled",
			"message": fmt.Sprintf("about to write %d hunk(s) in mode %s", len(j.Hunks), j.Mode),
			"hunks":   len(j.Hunks),
		})
	}
	out, err := s.Manager.Remediate(ctx, req)
	// Publish the patch first, so the receipt can name the URI it is fetched from.
	link := s.publishPatch(rec, out)
	structured, text, isErr := remediateResult(rec, out, err, link)
	if link != nil {
		rec.attachLinks([]proto.Content{proto.ResourceLink(*link)})
	}
	level := proto.LevelNotice
	if isErr {
		level = proto.LevelWarning
	}
	c.Log(level, ServerName, map[string]any{
		"runId": rec.ID, "eventType": "remediation_receipt", "message": text,
		"receipt": structured["receipt"],
	})
	// Read the record's state back from the payload: a partial refusal is isError yet complete, so deriving
	// state from isErr would disagree.
	state, _ := structured["state"].(string)
	if state == "" {
		state = StateComplete
	}
	rec.finish(state, structured, text, isErr)
}

// patchArtifactName is the logical name a patch is addressed by, not its run-relative path, so the
// resource URI does not disclose the layout.
const patchArtifactName = "patch"

// publishPatch registers a completed remediation's patch as an MCP resource and returns the Resource to
// link, or nil when there is nothing to publish. The run directory stays in the store; the wire sees
// only aimesh://run/<runId>/patch. The receipt's digest is stored too, so a patch that no longer matches
// is refused at fetch time.
func (s *Server) publishPatch(rec *record, out run.RemediateOutcome) *proto.Resource {
	if out.RunDir == "" || out.Receipt.PatchArtifact == "" {
		return nil
	}
	uri := s.artifacts.Publish(rec.ID, proto.Artifact{
		Name:  patchArtifactName,
		Title: "Remediation patch (run " + rec.ID + ")",
		Description: "The COMPLETE unified diff this remediation produced, as a fetchable resource. " +
			"Its digest is receipt.patchSha256; the content is never inlined, because a truncated patch that still parses reads as complete.",
		MimeType: "text/x-diff",
		Dir:      out.RunDir,
		Rel:      out.Receipt.PatchArtifact,
		SHA256:   out.Receipt.PatchSHA256,
	})
	if uri == "" {
		s.core.Diagnosticf("aimesh review mcp: run %s produced a patch that could not be published as a resource; the receipt still carries its name and digest", rec.ID)
		return nil
	}
	return &proto.Resource{
		URI: uri, Name: rec.ID + "/" + patchArtifactName,
		Title:       "Remediation patch (run " + rec.ID + ")",
		Description: "The complete unified diff. Fetch with resources/read; verify against receipt.patchSha256.",
		MimeType:    "text/x-diff",
	}
}

// --- review_run_status / review_run_result ---

type runIDArgs struct {
	RunID string `json:"runId"`
}

func decodeRunID(args json.RawMessage) (string, error) {
	var a runIDArgs
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.RunID) == "" {
		return "", proto.InvalidParams("invalid params: `runId` is required and must not be blank")
	}
	return strings.TrimSpace(a.RunID), nil
}

// unknownRun returns the refusal for a run id this server cannot resolve. It is an isError result rather
// than a protocol error, because an expired run is a legitimate outcome the calling model must handle.
func unknownRun(id string) *proto.CallToolResult {
	return domainRefusal(id, fault.New(fault.Usage, fmt.Sprintf(
		"unknown or expired runId %q — finished runs are retained for a bounded time and count. If you still need the answer, start a new review.", id)).
		WithHalt("usage").WithReason("unknown_run_id"))
}

// domainRefusal builds an isError result carrying the halt taxonomy.
func domainRefusal(runID string, err error) *proto.CallToolResult {
	structured, text := refusalResult(runID, err)
	return proto.ErrorResult(text, structured)
}

func (s *Server) runStatusHandler(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
	id, err := decodeRunID(c.Arguments)
	if err != nil {
		return nil, err
	}
	rec := s.runs.get(id)
	if rec == nil {
		return unknownRun(id), nil
	}
	state, structured, _, _ := rec.snapshot()
	out := map[string]any{
		"runId": rec.ID, "state": state, "tool": rec.Tool, "mode": rec.Mode,
		"elapsedSeconds": elapsed(rec),
	}
	if set := rec.decisions(); set != nil {
		out["remediable"] = !set.Inline && set.Accepted > 0
	}
	text := fmt.Sprintf("Run %s (%s): %s after %.1fs.", rec.ID, rec.Tool, state, elapsed(rec))
	if state == StateRunning {
		text += " Keep polling; do not start another run for the same request."
	} else {
		text += " Fetch the full result with review_run_result."
	}
	// A poller that never fetches review_run_result must not read a partially refused run as clean, so the
	// outcome and refusal count are copied from the finished payload.
	if oc, ok := structured["outcome"].(string); ok && oc != "" {
		out["outcome"] = oc
		refused := 0
		if counts, ok := structured["counts"].(map[string]any); ok {
			refused, _ = counts["refused"].(int)
		}
		out["refusedCount"] = refused
		if refused > 0 {
			text += fmt.Sprintf(" %d finding(s) were REFUSED for a protected path — this run did NOT apply everything it was asked to.", refused)
		}
	}
	return proto.Result(text, out), nil
}

func (s *Server) runResultHandler(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
	id, err := decodeRunID(c.Arguments)
	if err != nil {
		return nil, err
	}
	rec := s.runs.get(id)
	if rec == nil {
		return unknownRun(id), nil
	}
	return s.payloadFor(rec), nil
}

func elapsed(rec *record) float64 {
	end := rec.End
	if state, _, _, _ := rec.snapshot(); state == StateRunning {
		end = time.Now()
	}
	return end.Sub(rec.Start).Seconds()
}

// --- panel composition ---

// resolveSelection turns the call's panel into the requested seats and the role seats the manager
// resolves them from. Every refusal happens before any spend and names the field and accepted values.
// Whether a model exists is left to the adapter (or verifyReadiness).
func (s *Server) resolveSelection(p *panelArg) (panelPick, error) {
	if p == nil {
		return panelPick{}, proto.InvalidParams(
			"invalid params: `panel` is required — every review names its own seats. Corrected call: {\"panel\": {\"reviewers\": [{\"adapter\": \"<adapter>\", \"model\": \"<model>\"}], \"author_remediator\": {\"adapter\": \"<adapter>\", \"model\": \"<model>\"}}}")
	}
	if len(p.Reviewers) == 0 {
		return panelPick{}, proto.InvalidParams(
			"invalid params: `panel.reviewers` needs at least 1 seat — it is the blind primary panel. Corrected call: {\"panel\": {\"reviewers\": [{\"adapter\": \"<adapter>\", \"model\": \"<model>\"}], \"author_remediator\": {\"adapter\": \"<adapter>\", \"model\": \"<model>\"}}}")
	}
	if len(p.Reviewers) > review.MaxReviewerSeats {
		return panelPick{}, proto.InvalidParams(
			"invalid params: `panel.reviewers` has %d seats, exceeding the cap of %d. Each seat is a real model CLI, so the cap is a spend control and is never clamped — send at most %d.",
			len(p.Reviewers), review.MaxReviewerSeats, review.MaxReviewerSeats)
	}
	// A panel must name its adjudicator, since its judgment becomes the accepted set.
	if p.AuthorRemediator == nil {
		return panelPick{}, proto.InvalidParams(
			"invalid params: `panel.author_remediator` is required — it is the HOST-ADJUDICATION seat whose judgment becomes the accepted set, and it is never defaulted for you. Corrected call: {\"panel\": {\"reviewers\": [...], \"author_remediator\": {\"adapter\": \"<adapter>\", \"model\": \"<model>\"}}}")
	}
	pick := panelPick{source: "adhoc", roles: map[review.Role]review.SeatSpec{}}
	for i, seat := range p.Reviewers {
		if err := s.checkConfigured(fmt.Sprintf("panel.reviewers[%d]", i), seat); err != nil {
			return panelPick{}, err
		}
		pick.reviewers = append(pick.reviewers, seat.spec())
	}
	roleSeats := []struct {
		role  review.Role
		field string
		seat  *seatArg
	}{
		{review.RoleAuthorRemediator, "panel.author_remediator", p.AuthorRemediator},
		{review.RoleCrossCheck, "panel.cross_check", p.CrossCheck},
		{review.RoleVerifier, "panel.verifier", p.Verifier},
	}
	for _, rs := range roleSeats {
		if rs.seat == nil {
			continue
		}
		if err := s.checkConfigured(rs.field, *rs.seat); err != nil {
			return panelPick{}, err
		}
		// Effort applies only to reviewer seats. A role seat takes an effort-bearing model identifier, so a
		// separate effort would be dropped and is refused.
		if strings.TrimSpace(rs.seat.Effort) != "" {
			return panelPick{}, proto.InvalidParams(
				"invalid params: %s.effort is not accepted — reasoning effort is per-seat only on `panel.reviewers[]`. Put an effort-bearing identifier in %s.model instead.", rs.field, rs.field)
		}
		pick.roles[rs.role] = rs.seat.spec()
	}
	return pick, nil
}

// checkConfigured requires a seat to name an adapter this server was launched with whose CLI can be
// started now. The refusal lists the available adapters.
func (s *Server) checkConfigured(field string, seat seatArg) error {
	spec := seat.spec()
	if spec.Adapter == "" || spec.Model == "" {
		return proto.InvalidParams("invalid params: %s needs a non-empty adapter and model (got adapter=%q model=%q)", field, spec.Adapter, spec.Model)
	}
	if s.Adapters.Empty() {
		return proto.InvalidParams("invalid params: %s cannot be resolved — this server was launched with no adapter. The operator must add --adapter <name> (or %s) to the host configuration that starts it.", field, launchflags.EnvVar)
	}
	if !s.Adapters.Has(spec.Adapter) {
		return proto.InvalidParams(
			"invalid params: %s names adapter %q, which this server was not launched with — available adapters are: %s. A call uses the adapters named at launch; it never introduces one.",
			field, spec.Adapter, strings.Join(s.Adapters.Names(), ", "))
	}
	if ok, why := s.Adapters.Available(spec.Adapter); !ok {
		return proto.InvalidParams("invalid params: %s names adapter %q, whose CLI cannot be started right now: %s", field, spec.Adapter, sanitizeDetail(why))
	}
	return nil
}

// --- workspace resolution ---

// resolveWorkspace turns workspace or inlineWorkspace into a directory to review. A named path is judged
// against the call's scope; inline content is materialized into a directory this process owns and needs
// no root.
func (s *Server) resolveWorkspace(env *proto.RequestEnv, args runArgs, tool string) (ws string, inline bool, cleanup func(), res *proto.CallToolResult, err error) {
	cleanup = func() {}
	path := strings.TrimSpace(args.Workspace)
	switch {
	case path != "" && len(args.InlineWorkspace) > 0:
		return "", false, cleanup, nil, proto.InvalidParams(
			"invalid params: provide EITHER `workspace` (an absolute directory path) OR `inlineWorkspace` (content supplied over the wire), never both")
	case path == "" && len(args.InlineWorkspace) == 0:
		return "", false, cleanup, nil, proto.InvalidParams(
			"invalid params: `workspace` is required (an absolute directory path). To review content this server cannot read from disk, supply `inlineWorkspace` instead.")
	case path != "":
		// Judge the workspace against this call's scope and the read denylist before any spend.
		if _, derr := env.Trust.ResolveRead(path); derr != nil {
			return "", false, cleanup, domainRefusal("", rootFault(derr, env.Roots)), nil
		}
		return path, false, cleanup, nil, nil
	}
	dir, merr := materializeInline(args.InlineWorkspace)
	if merr != nil {
		return "", false, cleanup, nil, proto.InvalidParams("invalid params: inlineWorkspace: %v", merr)
	}
	return dir, true, func() { os.RemoveAll(dir) }, nil, nil
}

// rootFault wraps a confinement refusal as a containment halt carrying the resolver's reason code, with a
// remedy appended.
func rootFault(err error, roots []string) error {
	hint := ""
	switch scope.ReasonOf(err) {
	case scope.ReasonNoRoots:
		hint = " — this call declared no path, so it reads no file; name an absolute `workspace` (and any extra `roots`)"
	case scope.ReasonOutsideRoot:
		hint = fmt.Sprintf(" — the path is outside the %d director(y/ies) this call declared; add it to `roots` if the call should read it", len(roots))
	}
	return fault.Wrap(fault.Containment, "refused by workspace scope policy", fmt.Errorf("%w%s", err, hint)).
		WithHalt("M6").WithReason(string(scope.ReasonOf(err))).WithSignal("")
}

// materializeInline writes caller-supplied content to a new temp directory. Each path must be
// workspace-relative and not excluded; on any violation the directory is removed and an error returned.
func materializeInline(files map[string]string) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("inlineWorkspace is empty")
	}
	if len(files) > maxInlineEntries {
		return "", fmt.Errorf("inlineWorkspace has %d entries, exceeding the limit of %d", len(files), maxInlineEntries)
	}
	total := 0
	for rel, content := range files {
		if len(content) > maxInlineEntryBytes {
			return "", fmt.Errorf("entry %q is %d bytes, exceeding the per-file limit of %d", rel, len(content), maxInlineEntryBytes)
		}
		total += len(content)
	}
	if total > maxInlineTotalBytes {
		return "", fmt.Errorf("inlineWorkspace is %d bytes in total, exceeding the limit of %d", total, maxInlineTotalBytes)
	}
	dir, err := os.MkdirTemp("", "reviewmesh-mcp-inline-*")
	if err != nil {
		return "", err
	}
	for rel, content := range files {
		clean := filepath.Clean(rel)
		if !workspace.SafeRel(rel) {
			os.RemoveAll(dir)
			return "", fmt.Errorf("unsafe path %q (entries must be relative and stay inside the workspace)", rel)
		}
		if workspace.IsExcluded(clean) {
			os.RemoveAll(dir)
			return "", fmt.Errorf("excluded path %q", rel)
		}
		dst := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

// --- progress + logging mapping ---

// phaseFraction maps a review event to the fraction of phases complete. It is a fixed ladder rather than
// a per-item counter, so progress never goes backwards on a retry. Other events are logged, not counted.
var phaseFraction = map[string]float64{
	"run_started":                   0.02,
	"doctor_completed":              0.08,
	"authority_resolved":            0.12,
	"containment_copy_created":      0.16,
	"panel_started":                 0.20,
	"panel_completed":               0.60,
	"cross_check_completed":         0.70,
	"verifier_completed":            0.78,
	"host_adjudication_completed":   0.86,
	"final_self_critique_completed": 0.90,
	"remediation_journaled":         0.93,
	"patch_written":                 0.96,
	"apply_committed":               0.98,
}

// logLevelFor maps a review event level onto the MCP logging levels.
func logLevelFor(level string) proto.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return proto.LevelDebug
	case "warn", "warning":
		return proto.LevelWarning
	case "error":
		return proto.LevelError
	default:
		return proto.LevelInfo
	}
}
