// Package mcp is reviewmesh's MCP (Model Context Protocol) surface: reviewmesh runs as a local
// stdio server so an MCP-speaking agent can drive a governed, blind, multi-model review — and,
// when the operator has enabled it, apply the accepted findings. The protocol itself is
// meshcore/mcp; this package owns the tools, the schemas, the job registry, the admission governor
// and the write window — everything that is reviewmesh grammar.
//
// It deliberately mirrors exploremesh's MCP surface (job shape, halts on `isError`, unconditional
// governance, compose-never-configure), because two servers in one repo that answered the same
// questions differently would be two contracts a client has to learn. What is NOT shared is the
// reason this package is the most safety-critical in the repo: ONE of its tools writes to a human's
// filesystem, on the say-so of a model.
//
// Everything that follows from that is deliberate:
//
//  1. THE SERVER READS NO CONFIGURATION. Its adapters, write grant and optional root ceiling come
//     from its launch arguments, so it works on a fresh install, and every call composes its own
//     panel from the adapters named at launch.
//  2. SCOPE IS DECLARED PER CALL. Unlike exploremesh (which reads no files at all), this server takes
//     paths. Each call names its absolute workspace (and any extra `roots`); that call is judged
//     against exactly those paths, inside the operator's `--root` ceiling when one was set. Nothing
//     is inferred from where the process started, and no call's scope is another call's.
//  3. WRITING IS DOUBLY GATED, and the gates are different in kind. `output: "apply"` is refused
//     unless the operator launched the server with `--allow-writes`; `output: "patch"` supplies the
//     diff on every server. And every apply must pass `allowWrite: true` — a per-call confirmation the
//     calling model has to state.
//  4. THE HONEST LIMIT, STATED. The double opt-in and the tool annotations are HINTS that
//     coordinate with the host's permission layer; they are not an authorization boundary against a
//     confused or hostile client. What holds regardless is scope confinement, the non-overridable
//     write denylist, the write-path rule (an applied hunk must trace to workspace evidence), and
//     `fromRun`'s requirement that the decision set already exist as an inspectable artifact.
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
	// The per-call scope builder is the ACP surface's and is reused unchanged: two scope models in one
	// binary would be two confinement guarantees to keep in agreement.
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

// maxArgumentBytes bounds one tool call's arguments before anything is decoded — the request-shape
// half of the admission governor.
const maxArgumentBytes = 1 << 20 // 1 MiB

// Inline-workspace bounds. Inline content is MODEL-AUTHORED and is materialized to disk, so it is
// bounded per entry, in total, and in count — the argument cap alone would let one call write a
// megabyte of files into a temp directory.
const (
	maxInlineEntries    = 200
	maxInlineEntryBytes = 256 << 10
	maxInlineTotalBytes = 768 << 10
)

// defaultTurnTimeout is the wall-clock budget for one run when none is configured (exploremesh's
// value, so the two servers time out alike).
const defaultTurnTimeout = 10 * time.Minute

// Reviewer is the Manager capability this surface routes to — the same seam the ACP surface uses
// for reviews, plus the two-phase remediation entry point. Both surfaces run the identical
// pipeline; only the transport differs, and a test can substitute a fake.
type Reviewer interface {
	RunContext(ctx context.Context, r run.Request) (review.RunOutcome, error)
	// ReadDecisionSetByID resolves a run id for a run over workspace to the decision set that run
	// recorded, VERIFYING the handle against that workspace's run-record locations rather than trusting
	// it as a path. It returns the set and the canonical run directory. See run.ReadDecisionSetByID.
	ReadDecisionSetByID(workspace, runID string) (*run.StoredDecisionSet, string, error)
	Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error)
}

// Server is the reviewmesh MCP server.
type Server struct {
	// Manager routes a run to the review pipeline; Config supplies the sanitized `review_list`/`review_doctor`
	// projections.
	Manager Reviewer
	Config  Config

	// Adapters are the adapters this server was launched with (`--adapter`). A call's seats may name
	// only these; empty means every run is refused until the operator names one.
	Adapters launchflags.Set

	// Ceiling is the operator's optional `--root` set: every path a call declares must lie inside it.
	// Empty means no ceiling.
	Ceiling []string
	// ClientRoots, when non-empty, are the roots a legacy MCP client declared through `roots/list`. They
	// only narrow: every path a call declares must also lie inside them.
	ClientRoots []string

	// AllowWrites is the operator's `--allow-writes` grant. Without it aimesh never changes project
	// content: review_remediate still supplies the diff (`output: "patch"`) and the agent applies it.
	AllowWrites bool
	// VerifyCommands / VerifyTimeout / VerifyBaseline carry BOUNDED EXECUTION: the project's own
	// build/test commands, run on the containment copy and recorded. Empty means nothing is executed.
	//
	// THEY ARE LAUNCH SETTINGS WITH NO PER-CALL EQUIVALENT, and this is the strictest case of that
	// rule in the codebase rather than another instance of it. Every other launch grant decides what
	// the tool may READ or WRITE; this one decides what it EXECUTES. A caller that could name a
	// command here would have arbitrary code execution on the operator's machine, dressed as a
	// review parameter. So the value comes from the operator's own command line and from nowhere
	// else, and the result is reported back with a note saying it gated nothing.
	VerifyCommands []string
	VerifyTimeout  time.Duration
	VerifyBaseline bool
	// AllowProtectedPaths admits a workspace under a protected configuration path (.git, .claude,
	// .vscode, …) and permits writes there. Secrets are NOT included and never are. It is
	// a LAUNCH setting for a sharper reason than the others: those paths execute code on someone's
	// next command, so a model that could grant itself this could arrange to run its own code.
	AllowProtectedPaths bool

	// WaitSeconds overrides the inline wait default; TurnTimeout bounds ONE run's wall clock (0 → 10
	// minutes). This server has NO admission bounds — neither a lifetime run cap nor an in-flight
	// one; see the note in runs.go for why, and for where the concurrency decision lives now
	// (`maxParallel`, stated per call).
	WaitSeconds int
	TurnTimeout time.Duration

	// StrictSchema turns on the OPT-IN send-time check that every `structuredContent` satisfies the
	// outputSchema its own tool declared. Off by default; see proto.Server.StrictSchema for the cost.
	StrictSchema bool

	Framing string
	// Protocol is the era posture this process was launched with (`--protocol dual|legacy`; ""
	// means dual). It is reported by `review_doctor` and not only announced on stderr, because a host
	// launches its servers from a config file and MAY discard stderr entirely.
	//
	// SUNSET-PATH (MCP26-SUNSET): removed with the legacy era.
	Protocol proto.ProtocolMode
	// Diagnostics is the server's own log sink. It must never be the protocol stream.
	Diagnostics io.Writer

	runs *registry
	core *proto.Server

	// attached is set when this server registers into an EXTERNAL core (the combined `aimesh mcp`).
	// It suppresses the SHARED tools the composer registers once for the whole server — registering
	// agents_md twice would panic, and two copies of one document is the thing internal/agentguide
	// exists to prevent.
	attached bool

	// artifacts publishes finished runs' artifacts as MCP resources. It is what makes a `patch`-mode
	// remediation collectable: the receipt names the patch and hashes it, and this is where the bytes
	// are actually fetched from.
	artifacts proto.RunStore

	// clientMu guards ClientRoots, which a legacy client's `roots/list` answer replaces mid-session.
	clientMu sync.RWMutex
}

// clientRoots returns the roots a legacy client declared, or nil when it declared none.
func (s *Server) clientRoots() []string {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return append([]string(nil), s.ClientRoots...)
}

// applyClientRoots records what the legacy `roots/list` round trip answered. Those roots only ever
// NARROW: every path a later call declares must also lie inside them (see callScope). A client that
// declares the capability but lists no roots narrows nothing, and a root URI that is not a local
// `file://` path is ignored rather than guessed at.
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

// Serve builds the tool set and serves MCP over the given streams until EOF. On return every
// in-flight run is cancelled: a disconnected client must not leave a panel of model CLIs running
// with nobody to receive their output.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	s.build()
	defer s.runs.cancelAll()
	return s.core.Serve(in, out)
}

// Core exposes the underlying protocol server (for tests that drive it over an explicit framer).
func (s *Server) Core() *proto.Server {
	s.build()
	return s.core
}

func (s *Server) build() { s.buildInto(nil) }

// Attach registers this domain's tools into an EXTERNAL protocol core — the seam the combined
// `aimesh mcp` server uses — and returns the two providers the composer must fold in, because a
// single proto.Server has one Resources field and one Tasks field and both domains need theirs.
//
// It also applies review's own core wiring (OnRoots) to that shared core. That is harmless to a
// co-hosted explore: explore reads no paths, so a client's declared roots affect only review's calls.
func (s *Server) Attach(core *proto.Server) (proto.ResourceProvider, proto.TaskProvider) {
	s.buildInto(core)
	return &s.artifacts, taskProvider{s}
}

// buildInto wires this server. With core == nil it owns a fresh one (the standalone
// `aimesh review mcp` case); with a core supplied it registers into that instead.
func (s *Server) buildInto(core *proto.Server) {
	if s.core != nil {
		return
	}
	s.runs = newRegistry()
	s.runs.onEvict = s.artifacts.Forget
	if core != nil {
		s.attached = true
		s.core = core
		// The shared core is the composer's, so only the fields that are REVIEW's semantics go on it
		// here. Info/Instructions/Framing/Protocol belong to the composed server as a whole.
		s.core.OnRoots = s.applyClientRoots
	} else {
		s.core = &proto.Server{
			Info:         proto.Implementation{Name: ServerName, Title: "reviewmesh", Version: version.Get().Version},
			Instructions: instructions,
			Framing:      s.Framing,
			Protocol:     s.Protocol,
			Diagnostics:  s.Diagnostics,
			StrictSchema: s.StrictSchema,
			// The `roots/list` round trip. It fires only when the CLIENT declared the capability, and it
			// can only narrow — see applyClientRoots.
			OnRoots: s.applyClientRoots,
			// `resources/*`, so a governed write can actually be collected. The store is this server's
			// own registration table; it publishes no host path.
			Resources: &s.artifacts,
			// The `io.modelcontextprotocol/tasks` extension, as a PROJECTION over the run registry —
			// `taskId == runId`, no second execution model (see tasks.go). It is opt-in per client and
			// modern-era only, so a client that declares nothing sees only the job shape.
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

// writesValue names who performs writes on this server: aimesh when the operator launched it with
// --allow-writes, else the agent itself.
func (s *Server) writesValue() string {
	if s.AllowWrites {
		return "aimesh"
	}
	return "agent"
}

// traceOf lifts the caller's W3C trace context off a request's protocol context into the run
// record's own shape. It returns nil when the caller sent none, which is every legacy-era request:
// the three keys are a `2026-07-28` `_meta` convention and the legacy env never fills them.
//
// The values are carried VERBATIM and never validated. `basic/index` §`_meta` says of every reserved
// key that "implementations MUST NOT make assumptions about values at these keys"; the W3C-format
// MUST on that page binds the SENDER, so refusing a malformed `traceparent` here would be this
// server asserting a meaning for a field it does not own. It is also why nothing branches on the
// value — a trace id is an identity for someone else's tooling, not an input to ours.
func traceOf(env *proto.RequestEnv) *review.Trace {
	if env == nil {
		return nil
	}
	return review.NewTrace(env.Trace.TraceParent, env.Trace.TraceState, env.Trace.Baggage)
}

// --- annotations ---

// spendAnnotations mark a tool that STARTS A RUN: not read-only, reaching an open world of external
// providers, not idempotent. These tools MUST NOT be annotated read-only — that is the single most
// dangerous kind of annotation mistake, because a host uses it to decide whether to ask the human.
func spendAnnotations(title string, destructive bool) *proto.ToolAnnotations {
	return &proto.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: proto.Bool(destructive),
		IdempotentHint:  false,
		OpenWorldHint:   proto.Bool(true),
	}
}

// readOnlyAnnotations mark a tool that only reports. Only `review_list`, `review_doctor`, `review_run_status` and
// `review_run_result` qualify — everything else spawns processes and spends, and one of them writes.
func readOnlyAnnotations(title string) *proto.ToolAnnotations {
	return &proto.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: proto.Bool(false),
		IdempotentHint:  true,
		OpenWorldHint:   proto.Bool(false),
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
		// The TITLE is read by a human in a permission prompt, and it has to agree with the hints
		// beside it. "Review (read-only)" did not: this tool spawns model CLIs, spends money and
		// writes a run directory, which is exactly why readOnlyHint is false. A title that says
		// read-only next to a hint that says otherwise is the mis-signal the annotations exist to
		// prevent, so the title now says the true thing — it changes no workspace file.
		Annotations: spendAnnotations("Review (reports findings; writes no workspace changes)", false),
	}, s.reportHandler)

	// review_remediate is listed on every server: its patch output changes no project content, so an
	// agent can always ask for the diff and apply it itself. Its apply output is gated per call
	// (allowWrite) and by the operator's --allow-writes launch grant — see remediationMode.
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

	// The agent guide, byte-identical to `aimesh agents-md`. Defined in internal/agentguide so this
	// server and the explore server cannot drift into serving two different documents.
	// Skipped when attached: the composed server registers it once for both domains.
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
				// An unreadable AGENTS_MD override is an ERROR, never a quiet fall back to the
				// embedded guide: falling back would hand the agent a document the operator did not
				// choose while reporting success.
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
	// MaxParallel bounds how many of this run's reviewer seats invoke their model CLI AT ONCE.
	// Omitted, the whole panel runs in parallel. It is a PER-CALL parameter and not a launch flag
	// because the right number is a fact about the machine the CLIs run on — resident memory and
	// process count for a cloud CLI, loaded weights for a local model — and about the caller's
	// provider rate limits, none of which this server can see. It bounds parallelism only: every
	// seat still reviews, so the findings are unchanged and only the wall clock moves.
	MaxParallel *int `json:"maxParallel"`
	// DryRun resolves this run's plan, panel, authority documents and preflight, then STOPS
	// before the first model call and answers with the run's SHAPE — the seats it would convene,
	// the lanes it would call, and the model-call range it is bounded by.
	//
	// It matters more here than on any other surface, because here the caller is a MODEL. A model
	// deciding whether to convene a five-seat panel on a large tree has no other way to learn what
	// that costs before committing to it, and a model that cannot find out cheaply will either
	// over-spend or refuse work it should have done. Every configuration error a real run would
	// raise is raised by this one, for free.
	DryRun bool `json:"dryRun"`
	// VerifyReadiness asks every configured agent whether it can do real work before dispatching
	// anything. It SPENDS one bounded, one-token call per distinct adapter/model, and it exists so a
	// panel does not pay for its first seat's whole prompt and then halt on a second seat whose CLI
	// was blocked on login or folder trust the entire time — failures `Available()` cannot see.
	// dryRun prices these calls and performs none of them.
	VerifyReadiness bool   `json:"verifyReadiness"`
	IdempotencyKey  string `json:"idempotencyKey"`
	// Roots are EXTRA absolute directories this call reads beside its workspace — for example the
	// project folder that holds authority documents while the workspace is a temporary directory.
	// See callScope.
	Roots []string `json:"roots"`
}

// callScope builds the confinement for ONE call from the paths that call declares: its workspace (when
// it names one) and its extra `roots`. Every path must be absolute, must not be the filesystem root, a
// home directory, a system tree or a protected directory, and must lie inside the operator's --root
// ceiling when one was set; a legacy client's declared roots narrow it further. The returned env carries
// that resolver, so everything the call reads is judged against its own scope and never another call's.
//
// A call that declares no path (an inline workspace with no extra roots) gets the zero resolver, which
// refuses every filesystem path: inline content consumes no root.
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

// remediateArgs adds the write parameters. `fromRun` and the full-cycle parameters are mutually
// exclusive; the server enforces that itself rather than trusting the schema's `oneOf`.
type remediateArgs struct {
	runArgs
	FromRun    string `json:"fromRun"`
	Output     string `json:"output"`
	AllowWrite *bool  `json:"allowWrite"`
	// Select is the SELECTIVE-APPLY filter: host-computed fingerprints from the source run's
	// accepted findings, naming which of them to write. Absent applies the whole accepted
	// set; present-and-empty is refused, never widened.
	//
	// A pointer to a slice, so that "absent" and "[]" are DIFFERENT requests. The decoder is strict
	// (`DisallowUnknownFields`), so a misspelled narrowing filter is a -32602 rather than silently
	// dropped — the failure a narrowing filter must never have.
	Select *[]string `json:"select"`
}

// decode strictly decodes a tool's arguments. Strict decoding is what makes
// `additionalProperties: false` a real boundary: a schema is advisory to a client, so a server that
// trusted it would accept a misspelled field and run something the caller did not ask for.
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

// prepare validates a run-forming request COMPLETELY before any spend: the panel selection, the
// workspace (against the trusted roots, or materialized from inline content), and the authority
// declaration (shape, then full resolution). It returns either a prepared run, or a domain-halt
// result, or a protocol error.
//
// The split matters. A malformed REQUEST is a JSON-RPC error (-32602) with a corrected example: the
// call was never well-formed. A refusal about the WORLD — a path outside the trusted roots, an
// oversized authority document, a pinned hash that no longer matches — is a domain halt on
// `isError` carrying the taxonomy, because the model has to react to it rather than re-spell it.
// `env` is the request's protocol context: every path this function judges is judged against
// `env.Trust`, the resolver captured when the call arrived, never against a process-global one.
func (s *Server) prepare(env *proto.RequestEnv, args runArgs, mode review.Mode, tool string) (*prepared, *proto.CallToolResult, error) {
	// The call's scope is built FIRST, so that every judgement below — the workspace, the authority
	// documents, and the roots recorded on the run request — is made against the same resolver.
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
	// maxParallel is refused rather than clamped: a caller who asked to run two seats at a time
	// because that is what their machine can host must not silently get the whole panel.
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
	// Authority: the pure declaration checks first (a malformed doc, or inline content in a
	// write-capable mode, is a malformed request), then the full resolution, whose refusals are
	// about the world.
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
		// Recorded on the run's DURABLE decision set, so the on-disk artifact says the same thing
		// about this run that this server's in-memory registry does. This surface READS it back:
		// `review_remediate {fromRun}` resolves a handle the registry no longer holds from the run's
		// own directory, and an inline run must be refused by NAME there too rather than failing
		// obscurely on a directory this process already deleted.
		WorkspaceEphemeral: inline,
		// BOUNDED EXECUTION and every containment/budget waiver, from the OPERATOR's launch flags and
		// never from the call. See the Server fields for why a calling model gets no say in any of
		// them: each one either executes code, widens what containment admits, or changes what the
		// model's own seats are shown.
		VerifyCommands:      s.VerifyCommands,
		VerifyTimeout:       s.VerifyTimeout,
		VerifyBaseline:      s.VerifyBaseline,
		AllowProtectedPaths: s.AllowProtectedPaths,
		// From the CALL, unlike those above: how many CLIs this machine can host at once is the
		// caller's fact to state, and stating it changes nothing about what gets reviewed.
		MaxParallel: maxParallel,
		// Also from the CALL: pricing a run is a question about THIS request, and a launch flag
		// could only answer it for every request at once.
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

// startReport admits, launches and inline-waits for one review.
func (s *Server) startReport(ctx context.Context, c *proto.Call, tool string, prep *prepared, wait int, key string, then func(*record, review.RunOutcome, runview.View, error)) (*proto.CallToolResult, error) {
	// IDEMPOTENCY first, before any admission accounting: a retry after a dropped connection must
	// return the ORIGINAL run, not a second panel billed to the same person for the same question.
	if prior := s.runs.existing(key); prior != nil {
		prep.cleanup()
		// A still-running prior run is handed back as a TASK to a client that declared the tasks
		// extension, and as the job shape to everyone else. The retry gets whatever the original
		// call would now get — which is the point of idempotency.
		if taskHandOff(c, prior) {
			return nil, nil
		}
		return s.payloadFor(prior), nil
	}
	runID := newRunID()
	// The run id the CALLER is about to be handed also NAMES the run's directory, so the decision
	// set this run records is findable from the only handle the caller holds. See newRunID.
	prep.request.RunID = runID
	runCtx, cancel := context.WithCancel(context.Background())
	rec, aerr := s.runs.admit(runID, tool, string(prep.request.Mode), key, "", cancel)
	if aerr != nil {
		cancel()
		prep.cleanup()
		structured, text := refusalResult("", aerr)
		return proto.ErrorResult(text, structured), nil
	}
	// The panel echo is attached to the RECORD, not just to this response, so a later review_run_status /
	// review_run_result can answer "which panel is this?" while the run is still in flight.
	rec.attachPick(prep.pick)

	// The progress sink is live only for the INLINE wait: a progress notification is correlated to
	// the request that supplied the token, and this call is the only request that did.
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

// finishReport records a completed review: the result payload AND, on success, the decision set a
// later `fromRun` remediation writes from.
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
	// The decision set is captured HERE, once, from the run that produced it — including the base
	// hashes of every targeted file. Recomputing any of it later would make the remediation act on
	// a different set than the one a human read.
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
		// The reviewed root's IDENTITY, captured here for the same reason the base hashes are: a
		// remediation must be able to prove it is writing to the tree that was judged, and a path
		// string cannot prove that. A capture failure is not a review failure — the report stands —
		// but it leaves the set unbound, and an unbound set is refused by the write path rather
		// than written on the strength of a pathname.
		if id, ierr := run.CaptureWorkspaceIdentity(prep.request.Workspace); ierr == nil {
			set.WorkspaceIdentity = id
		}
	}
	structured, text, accepted := completeResult(rec, prep.pick, view, prep.inline, s.remediable(prep))
	set.Accepted = accepted
	rec.attach(set)
	rec.finish(StateComplete, structured, text, false)
}

// remediable reports whether a run's accepted set could be handed to `review_remediate --fromRun`.
// An inline workspace never can: the directory the content was materialized into belongs to this
// process and is deleted when the run ends, so there is nothing real to write to.
func (s *Server) remediable(prep *prepared) bool {
	return !prep.inline
}

// acceptedFiles is the set of workspace-relative files the accepted findings target — exactly the
// files whose base hashes must still match when a remediation runs.
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

// awaitRun is the JOB SHAPE: wait out the inline budget, then hand back the run id rather than
// holding a request open past the client's timeout, where it would be killed mid-spend.
//
// `disarm` DISCONNECTS the progress sink, and it runs on every return path. A progress notification
// is correlated to the request that supplied the token, so once this call has answered — with a
// result, with `state: "running"`, or not at all because the client cancelled — the run must stop
// emitting progress against it. Without the disarm a run that outlived its inline budget kept
// firing `notifications/progress` for a request that had already been replied to: at best noise a
// client cannot correlate, at worst progress arriving for a request the client considers cancelled.
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
		// THE ONE PLACE THE TASKS EXTENSION CHANGES A `tools/call` ANSWER. The inline budget
		// expired and the run is still going — exactly the moment the job shape hands back
		// `{runId, state:"running"}`. A client that declared `io.modelcontextprotocol/tasks`
		// receives a `CreateTaskResult` for the SAME run id instead; a client that did not
		// receives the identical bytes it always did.
		if taskHandOff(c, rec) {
			return nil
		}
		structured, text := runningResult(rec, pick, wait)
		return proto.Result(text, structured)
	case <-ctx.Done():
		// The CLIENT cancelled (or the session went away). Kill the run: a cancelled call must not
		// keep spending, and — the rule that matters for the write primitive — it must not commit
		// anything afterwards. The receipt is the run record on disk plus the log notification the
		// remediation goroutine emits; the response a cancelled call never gets is not the only
		// place the outcome exists.
		//
		// The payload is the CANCELLED shape, not the running shape with a different `state`: it
		// carries the taxonomy (`haltClass`/`reasonCode`) a client branches on, and it satisfies the
		// `cancelled` branch of the declared outputSchema. Overwriting `state` on the running shape
		// produced a payload that matched no branch at all.
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
		// The panel echo is REQUIRED on the running branch of the review output schema, and it is
		// knowable from admission — the requested half always, the executed half empty until seats
		// report. Omitting it here made `review_run_result` on a still-running review emit a payload its own
		// declared schema rejected.
		if rec.Tool == toolReport {
			out["panel"] = rec.pickSnapshot().echo(nil)
		}
		return proto.Result(fmt.Sprintf("Run %s is still running. Poll review_run_status, then fetch review_run_result.", rec.ID), out)
	}
	res := proto.Result(text, structured)
	if isErr {
		res = proto.ErrorResult(text, structured)
	}
	// The resource links ride EVERY rendering of this record, including a `review_run_result` fetched long
	// after the starting call answered — the call that pays for a write is often not the call that
	// collects it.
	res.Content = append(res.Content, rec.linksSnapshot()...)
	return res
}

// newRunID mints a run id of the form `<UTC timestamp>-<sub-second>-<64 random bits>`, e.g.
// `20260728T140311-0421-9f8e7d6c5b4a3210`.
//
// It is also the NAME of the run's own directory (run.Request.RunID carries it into the
// Manager). That is what makes the handle durable: the id is the only thing an MCP client ever
// holds, so `review_remediate {fromRun}` can read the run's on-disk decision set only if the id
// names the directory it is stored in. One identifier for one run, instead of two.
//
// WHAT THE TIMESTAMP DOES AND DOES NOT DISCLOSE. A run id travels to a third party's inference log,
// so it carries NO host path and nothing about this machine — that part is unchanged and is the
// property worth protecting. The start time is not in that category: the caller is the one who
// made the call, so its own clock already tells it when. In exchange the directory sorts
// chronologically beside the CLI's and ACP's, which share this timestamp prefix
// (meshcore/audit.NewRun) — and an artifact directory that cannot be read in the order things
// happened is the one thing browsing it is for.
//
// The random suffix is not decoration. Two runs admitted inside the same 0.1 ms would otherwise
// mint the same name, and audit.NewRun's MkdirAll succeeds on an existing directory — so a
// collision would not fail, it would silently interleave two runs' artifacts in one directory.
// MCP is the surface where that is reachable, being the concurrent one.
func newRunID() string {
	now := time.Now().UTC()
	ts := now.Format("20060102T150405") + fmt.Sprintf("-%04d", now.Nanosecond()/1e5)
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice. If it ever does, the nanosecond clock is a worse
		// uniqueness source than 64 random bits but a far better one than nothing.
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
	// An apply WRITES THE WORKSPACE, so it is confirmed per call on top of the operator's launch grant.
	// It is checked before anything else about the request, so a call that never confirmed the write
	// cannot spend anything learning that the rest of it was also malformed.
	if mode == review.ModeApply && (args.AllowWrite == nil || !*args.AllowWrite) {
		return nil, proto.InvalidParams(
			"invalid params: `allowWrite` must be present and literally true for output=apply — an apply writes the workspace and is confirmed per call. Corrected call: {\"fromRun\": \"<a review_report runId>\", \"workspace\": \"<absolute workspace>\", \"output\": \"apply\", \"allowWrite\": true}")
	}
	if len(args.InlineWorkspace) > 0 {
		return nil, proto.InvalidParams(
			"invalid params: `inlineWorkspace` cannot be remediated — the content was supplied over the wire and materialized into a directory this server owns and then deletes, so there is nothing real to write to. Review inline content with review_report, and remediate a workspace path that resolves inside this server's trusted roots.")
	}
	fromRun := strings.TrimSpace(args.FromRun)
	// MCP WRITES GO THROUGH `fromRun`, AND ONLY THROUGH `fromRun`, on both protocol eras. On 2026-07-28
	// stdio a cancelled request may receive no further message at all, so a write whose response is
	// cancelled tells the caller nothing; `fromRun` is a value the caller ALREADY RECEIVED in a completed
	// response, so at the instant the write window opens the caller provably holds a handle. basic/index:
	// "State that needs to span multiple requests ... MUST be referenced by an explicit identifier the
	// client passes on each request." The CLI keeps its one-call cycle: its caller is a human at a
	// terminal who is shown the run directory as the run starts, with no response to lose.
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
	// THE SELECTIVE-APPLY FILTER, validated before any spend. An EMPTY `select` is refused
	// here rather than in the write path so the caller learns it from a -32602 that names the
	// correction, and so no run is admitted for a call that can only write nothing.
	var selection []string
	if args.Select != nil {
		selection = *args.Select
		if len(trimmedNonEmpty(selection)) == 0 {
			return nil, proto.InvalidParams(
				"invalid params: `select` is present and empty. An empty narrowing filter names ZERO findings — it does not mean \"apply everything\", and accepting it would let a call read as a normal apply while writing nothing. Omit `select` to apply run %q's whole accepted set, or name the `fingerprint` values from that run's accepted findings.", fromRun)
		}
	}
	// The call's scope applies to the WRITE too, and it is resolved before the write window opens: the
	// roots this call declares are the roots the receipt records the write against.
	env, nres, nerr := s.callScope(c.Env(), workspace, args.Roots)
	if nerr != nil {
		return nil, nerr
	}
	if nres != nil {
		return nres, nil
	}
	return s.remediateFromRun(ctx, c, env, workspace, fromRun, mode, wait, strings.TrimSpace(args.IdempotencyKey), selection)
}

// trimmedNonEmpty is the caller's list with blanks removed — the same normalization the write path
// applies, used here only to decide whether a supplied `select` names anything at all.
func trimmedNonEmpty(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// refuseRunFormingArgs rejects run-forming parameters on `review_remediate`. The schema forbids
// `panel` and `authority` there, but a schema is advisory to a client and the decoder embeds `runArgs`
// wholesale, so the server refuses them by name: each names a GOVERNANCE input — which panel produced
// the accepted set, which intent it was judged against — that already came from the source run.
func refuseRunFormingArgs(args runArgs, fromRun string, mode review.Mode) error {
	var named []string
	if args.Panel != nil {
		named = append(named, "`panel`")
	}
	if len(args.Authority) > 0 {
		named = append(named, "`authority`")
	}
	if args.DryRun {
		// A dry run prices a review and spends nothing; a remediation writes, so there is nothing for a
		// dry run to price. Accepting the flag would read as "nothing will be written".
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

// remediateFromRun is the PRIMARY form: apply a prior review's accepted set.
func (s *Server) remediateFromRun(ctx context.Context, c *proto.Call, env *proto.RequestEnv, workspace, fromRun string, mode review.Mode, wait int, key string, selection []string) (*proto.CallToolResult, error) {
	// IDEMPOTENCY, in two independent forms. The key covers an explicit retry; the source-run guard
	// covers the retry that forgot the key, or invented a new one. Both return the ORIGINAL
	// receipt, because the alternative is a second, unrequested write.
	//
	// These two lookups are the FAST PATH — a replay that arrives after the winner finished, which
	// is the common case and which must answer even if the source run's own record has since been
	// evicted. They are not the guarantee: two calls that arrive together both pass here, so the
	// binding decision is made once, atomically, at reservation (registry.reserve).
	if prior := s.runs.existing(key); prior != nil {
		return s.attachTo(ctx, c, prior, wait), nil
	}
	if prior := s.runs.existingForSource(fromRun); prior != nil {
		// A SELECTIVE APPLY DOES NOT GET A SECOND WINDOW ON THE SAME SOURCE RUN, and that is
		// deliberate. This guard exists so that a decision set is
		// never applied twice, and a second call naming a different `select` is still a second
		// application of the same set. It therefore attaches to the winner and returns the ORIGINAL
		// receipt — including the original `selection` — rather than opening a second write window
		// with a different filter.
		//
		// The consequence: "apply findings 1–3, then 4–5" is NOT
		// available from one report run. Choose the selection in one call, or produce a fresh
		// review_report run for the second tranche. Loosening this would trade a caller convenience
		// for the one guarantee this surface exists to keep.
		return s.attachTo(ctx, c, prior, wait), nil
	}
	rec := s.runs.get(fromRun)
	if rec == nil {
		// THE REGISTRY MISS IS NOT THE END OF THE QUESTION. The registry is in-memory and bounded
		// by count and TTL, so a handle can name a run this process really did produce and really
		// did record — one evicted by retention, or one from before a restart — and it is honoured from
		// the run's own directory, exactly as an ACP agent honours the same handle from the same file.
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
		// The narrowing filter travels UNEXAMINED into the one governed write path. This surface
		// does not resolve it, does not look a fingerprint up, and does not know which findings it
		// names — deliberately, so `select` cannot come to mean one thing on MCP and another on the
		// CLI. All this surface owns is the empty-list refusal, above, which is pre-spend.
		Select: selection,
		Shown:  set.Shown, BaseHashes: set.BaseHashes,
		// THIS turn's trace, not the report run's. The two are separate requests and a host that
		// traced them separately must be able to see that in the two run directories.
		Trace: traceOf(env),
		// The OPERATOR's launch waivers reach the two-phase apply too. writepath.go checks the
		// dirty-tree precondition in the one governed write path precisely BECAUSE this path
		// arrives there; leaving the waiver off here made the launch flag dead for `review_remediate
		// {fromRun}` while it worked for a single-call apply — the same operator act, two answers.
		AllowProtectedPaths: s.AllowProtectedPaths,
	}
	return s.startRemediate(ctx, c, req, wait, key)
}

// remediateFromDisk resolves a run handle the registry no longer holds, from the run's own
// directory — the same reader, the same file and the same refusals the ACP surface uses
// (run.ReadDecisionSet, surface/acp/fromrun.go). It is reached ONLY at the registry miss: both
// in-process idempotency lookups have already run and found nothing, so nothing here can answer for
// a remediation this process has already performed.
//
// WHAT THE HANDLE IS WORTH. It arrives from a peer, so it is VERIFIED rather than trusted: the id is
// joined to each of the named workspace's run-record locations and then faces ReadDecisionSet's
// four checks — lexical containment BEFORE any filesystem call, canonical containment after symlink
// resolution, the schema version, and `runId == the directory's own name`. Every failure answers
// with the same `unknownRun`, so a peer that guesses gains no existence oracle and no path outside
// this agent's own artifact directory is ever read.
//
// WHAT A SECOND APPLY GETS AFTER A RESTART, stated because it differs from the in-process answer and
// a client must be able to tell them apart. In-process, the source-run guard hands back the ORIGINAL
// receipt. Across a restart that guard is gone with the registry — and the write does NOT happen
// twice: the first apply changed the very files the stored set pins, so the base hashes no longer
// match and the second call HALTS with `stale_decision_set`, having written nothing. Both are
// refusals to double-write; one reports a receipt and one reports a halt. That is exactly what ACP
// does today, and a durable "already applied" marker was deliberately NOT added — a wrong durable
// record on a write path is a false success, which is worse than an honest halt.
func (s *Server) remediateFromDisk(ctx context.Context, c *proto.Call, env *proto.RequestEnv, workspace, fromRun string,
	mode review.Mode, wait int, key string, selection []string) (*proto.CallToolResult, error) {

	set, _, err := s.Manager.ReadDecisionSetByID(workspace, fromRun)
	if err != nil {
		// A handle that names nothing on disk EITHER. This also absorbs the two registry-path
		// refusals that have no on-disk counterpart, and they have none because the artifact says
		// so rather than because the check was dropped: a decision set is written once, by a
		// COMPLETED REPORT run, at the end of its cycle. A run still running, a run that halted,
		// and a remediation run all record no set at all — so `source_run_not_complete` and
		// `source_run_not_reviewable` cannot be reached from a file that does not exist. (A running
		// run is never evicted, so its record is always still here to answer with the first of
		// those.)
		return unknownRun(fromRun), nil
	}
	// The same ladder, in the same order, answering with the SAME reason codes the registry path
	// reports — `run.ReasonInlineWorkspaceNotRemediable` and `run.ReasonNoAcceptedFindings`
	// are those strings, held in one place so the two paths cannot drift apart.
	if rerr := set.Remediable(); rerr != nil {
		return domainRefusal("", rerr), nil
	}
	if !set.NamesWorkspace(workspace) {
		return domainRefusal("", workspaceMismatch(fromRun)), nil
	}
	// THE IDENTITY BINDING, re-established across the report→apply gap. The registry path carries a
	// live `fs.FileInfo` from the report run; a file cannot, so the canonical path and the durable
	// device+inode key are re-verified here instead. It is pre-spend: a swapped tree costs a round
	// trip rather than a write.
	identity, ierr := set.BindWorkspace()
	if ierr != nil {
		return domainRefusal("", ierr), nil
	}
	req := run.RemediateRequest{
		// The CANONICAL workspace, because BindWorkspace has just proven that is the tree the source
		// run judged. Resolving the write against the canonical form removes a whole class of
		// "same tree, different spelling" ambiguity from the one call that writes.
		Workspace: set.WorkspaceCanonical, WorkspaceIdentity: identity,
		Mode: mode, Surface: "mcp",
		Profile: set.Profile, ReviewerPanel: set.Panel, ComposedRoles: set.Roles,
		// The roots THIS call declares, not the ones the source run was reviewed under. A decision set
		// outlives them, and the write is re-gated inside the governed write path.
		TrustedRoots: env.Roots,
		// The handle, not `set.RunID`: ReadDecisionSet has just PROVEN they are the same string
		// (the recorded runId must be the directory's own name), and using the handle is what makes
		// the source-run reservation key the same value the fast-path lookup above used.
		SourceRunID: fromRun,
		Findings:    set.Findings, Decisions: set.Decisions,
		// The narrowing filter travels UNEXAMINED into the one governed write path, exactly as it
		// does on the registry path.
		Select: selection,
		Shown:  set.ShownSet(), BaseHashes: set.BaseHashes,
		Trace: traceOf(env),
		// The operator's launch waivers, exactly as on the registry path above — the two entry
		// points differ only in where the decision set came from, never in what is permitted.
		AllowProtectedPaths: s.AllowProtectedPaths,
	}
	return s.startRemediate(ctx, c, req, wait, key)
}

// ReasonSourceRunWorkspaceMismatch refuses a remediation whose named workspace is not the tree the
// source run reviewed.
const ReasonSourceRunWorkspaceMismatch = "source_run_workspace_mismatch"

// sameWorkspace reports whether the workspace a call names is the tree a run recorded, compared after
// symlink resolution so two spellings of one directory agree.
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

// attachTo waits out the inline budget on a run this call did NOT start — the winner of an
// idempotency or same-source reservation.
//
// It deliberately holds no cancel handle. A later caller is a SPECTATOR: its disconnect must not
// kill the write window the winner opened, and it must not report the winner's run as cancelled on
// its own behalf, because it decided nothing. What it returns is whatever the winner's record says
// — the receipt if it has finished, the running shape if it has not.
func (s *Server) attachTo(ctx context.Context, c *proto.Call, rec *record, wait int) *proto.CallToolResult {
	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()
	select {
	case <-rec.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	// A spectator gets a task handle on the same terms the winner would: the run is the winner's,
	// but the HANDLE is not exclusive — `taskId == runId`, and polling it is a read.
	if taskHandOff(c, rec) {
		return nil
	}
	return s.payloadFor(rec)
}

// THIS SURFACE HAS NO ONE-CALL REVIEW-AND-WRITE FORM.
//
// That form — run the governed cycle in report mode, then apply that run's accepted set through the
// same write window — is ABSENT, not merely disabled, because on MCP it is the one write
// shape whose caller can be left holding NOTHING — the run it would have to poll is a run only that
// same, cancelled response would have named. See remediateHandler for the rule and the teaching
// error.
//
// The capability is two calls: review_report, then review_remediate {fromRun}. The CLI has the
// one-call form, where the caller is a human who is shown the run directory on stderr.

// startRemediate reserves and runs one from-run remediation.
//
// The reservation is ATOMIC over both the idempotency key and the source run: whichever call gets
// there first opens the write window, and every other call for the same decision set attaches to it
// instead of opening a second one. "Never apply the same accepted set twice" is a property of that
// single lock, not of a check that happened to run earlier.
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
	// A from-run remediation has no progress sink to disarm: it emits log notifications only (the
	// receipt has to survive a cancelled call, which gets no response), and those are not
	// request-correlated.
	return s.awaitRun(ctx, c, rec, panelPick{}, cancel, wait, nil), nil
}

// runRemediation drives the write window and publishes the receipt on BOTH channels.
//
// The log notification is not decoration. A cancelled call gets no response — on 2025-06-18 the server
// SHOULD NOT send one and the client is told to ignore a late one anyway — so the notification plus the
// on-disk receipt are where the outcome exists, and "what did it actually write" stays answerable. Under
// 2026-07-28 over stdio even the notification is barred (MUST NOT send ANY further message for that
// request), which leaves the durable receipt as the only guarantee.
func (s *Server) runRemediation(ctx context.Context, c *proto.Call, rec *record, req run.RemediateRequest) {
	req.OnJournal = func(j run.Journal) {
		c.Log(proto.LevelNotice, ServerName, map[string]any{
			"runId": rec.ID, "eventType": "remediation_journaled",
			"message": fmt.Sprintf("about to write %d hunk(s) in mode %s", len(j.Hunks), j.Mode),
			"hunks":   len(j.Hunks),
		})
	}
	out, err := s.Manager.Remediate(ctx, req)
	// PUBLISH the patch before the receipt is rendered, so the receipt can name the URI it is fetched
	// by. Without this the call completes a governed write and hands back a run-record-relative name
	// whose root this surface deliberately withholds — a workflow that finishes and delivers nothing.
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
	// The record's state is READ BACK from the payload rather than re-derived beside it. It used
	// to be derived from `isErr`, and that stopped being the same fact when a PARTIAL REFUSAL
	// began riding `isErr: true` on a run whose state is legitimately "complete" — the write
	// committed. Two derivations of one fact is how they disagree; there is now one.
	state, _ := structured["state"].(string)
	if state == "" {
		state = StateComplete
	}
	rec.finish(state, structured, text, isErr)
}

// patchArtifactName is the LOGICAL artifact name a patch is addressed by. It is deliberately not the
// file's run-relative path (`patches/changes.patch`): a resource URI is an identifier, and one that spelt
// out a layout would be a half-disclosed path.
const patchArtifactName = "patch"

// publishPatch registers a completed remediation's patch as an MCP resource and returns the Resource to
// link, or nil when there is nothing to publish.
//
// This is the ONE place the run directory is used on this surface, and it never leaves this function: the
// store holds it privately and the wire sees only `aimesh://run/<runId>/patch`. The receipt's recorded
// digest is handed to the store too, so a patch that no longer matches the receipt is refused at fetch
// time rather than served as though the receipt still described it.
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

// unknownRun is the refusal for a run id this server cannot resolve. It rides `isError` rather than
// a protocol error because it is something the CALLING MODEL has to react to, and because an
// expired run is a legitimate outcome of the retention bound rather than a malformed request.
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
	// A POLLER THAT NEVER FETCHES `review_run_result` MUST STILL NOT READ A PARTIALLY-REFUSED RUN AS
	// CLEAN. `state` is "complete" for such a run and always will be, so the two facts
	// that distinguish it are lifted out of the terminal payload and repeated here. They are read
	// from the finished payload rather than recomputed, so status and result cannot disagree.
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

// resolveSelection turns the call's `panel` into the requested seats and the role seats the Manager
// resolves them from. Every refusal here happens BEFORE any spend, and every message names the field and
// the accepted values so the corrected call is derivable rather than guessable.
//
// It does not decide whether a seat's model exists — the adapter answers that when the run starts, and
// verifyReadiness can ask it first. What it owns is what a surface owns: completeness, the bound, and
// that every seat names an adapter this server was launched with.
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
	// A panel MUST name its adjudicator: its judgment becomes the accepted set, so a hidden default
	// would mean the caller composed a panel whose judge it never saw.
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
		// Reasoning effort is a per-seat vantage on the blind panel only. A single-slot role seat takes
		// an effort-bearing model identifier instead, so a separate effort here would be silently
		// dropped — which is why it is refused.
		if strings.TrimSpace(rs.seat.Effort) != "" {
			return panelPick{}, proto.InvalidParams(
				"invalid params: %s.effort is not accepted — reasoning effort is per-seat only on `panel.reviewers[]`. Put an effort-bearing identifier in %s.model instead.", rs.field, rs.field)
		}
		pick.roles[rs.role] = rs.seat.spec()
	}
	return pick, nil
}

// checkConfigured enforces that a seat names an adapter this server was launched with, and that the
// adapter's CLI can be started now. The refusal names the available set, so the corrected call follows
// from the error.
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

// resolveWorkspace turns `workspace` / `inlineWorkspace` into a directory to review. A named path
// is judged against the trusted roots; inline content is materialized into a directory THIS PROCESS
// owns, which is why it needs no root — it never touched this machine's filesystem.
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
		// The workspace is judged against this call's own scope (callScope built it from this path)
		// and the non-overridable read denylist, before any spend, with the machine reason code attached.
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

// rootFault types a confinement refusal as a containment halt carrying the resolver's own MACHINE
// reason code, and appends the operator-facing remedy — so a host user reads "how do I allow this"
// rather than only "refused".
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

// materializeInline writes caller-supplied content to a fresh isolated temp directory. Every path
// must be workspace-relative (no absolute/volume/`..` escape) and not excluded; any violation
// removes the directory and errors, so a malicious map cannot write outside it.
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

// phaseFraction maps a review event to the FRACTION OF PHASES complete. It is deliberately a fixed
// ladder over named phases rather than a per-item counter: a counter that resets when a phase is
// retried goes backwards, and a client that sees progress go backwards cannot tell a retry from a
// bug. Events that are not phase boundaries move nothing — they are logged, not counted.
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
