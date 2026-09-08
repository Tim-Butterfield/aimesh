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
//  1. TRUSTED ROOTS ARE MANDATORY. Unlike exploremesh (which reads no files at all), this server
//     takes paths. The roots are established at LAUNCH by a human — `--root`, or the validated
//     launch cwd — exactly as on the ACP surface, and the same resolver judges every request path.
//     A request may narrow the roots; nothing in a request can widen them. No roots is not
//     "unrestricted", it is "every path refused".
//  2. WRITING IS DOUBLY GATED, and the gates are different in kind. `review_remediate` is not even
//     LISTED unless the config store grants this surface the `allowRemediate` capability (which
//     `--allow-remediate` sets) — an operator-level decision. And every individual call must pass
//     `allowWrite: true` — a per-call confirmation the calling model has to state. Neither gate is
//     an exception above the policy ceiling: the ceiling is computed FROM the config, so granting
//     the capability RAISES it (see config.SurfaceCeiling) rather than stepping around it.
//  3. THE HONEST LIMIT, STATED. The double opt-in and the tool annotations are HINTS that
//     coordinate with the host's permission layer; they are not an authorization boundary against a
//     confused or hostile client. What holds regardless is root confinement, the non-overridable
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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
	// The trusted-root resolver is the ACP surface's and is reused unchanged: a peer-process caller is
	// a peer-process caller whatever protocol it speaks, and two root models in one binary would be
	// two confinement guarantees to keep in agreement. Only its types are imported here.
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

// capabilityName is the config capability that permits `review_remediate` (re-exported so `review_list`
// can NAME the thing an operator would have to grant).
const capabilityName = config.CapabilityAllowRemediate

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
	// ReadDecisionSet resolves a run handle to the decision set that run recorded, VERIFYING the
	// handle against the agent's own artifact directory rather than trusting it as a path. It
	// returns the set and the canonical run directory. Declared exactly as the ACP surface declares
	// it (surface/acp/acp.go), because it is the same reader answering the same question — the two
	// surfaces differ in how a handle is SPELLED, not in what makes one trustworthy.
	// See run.ReadDecisionSet.
	ReadDecisionSet(handle string) (*run.StoredDecisionSet, string, error)
	// RunHandle spells a run id as the handle ReadDecisionSet resolves. This surface hands a client
	// an OPAQUE run id and never a host path, so it cannot form one itself and must not invent a
	// second opinion about where this agent's runs live. See run.RunHandle.
	RunHandle(runID string) string
	Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error)
}

// Server is the reviewmesh MCP server.
type Server struct {
	// Manager routes a run to the review pipeline; Config supplies the sanitized `review_list`/`review_doctor`
	// projections.
	Manager Reviewer
	Config  Config

	// Adapters is the STARTUP-BOUND adapter set an ad-hoc panel may compose from. Empty means
	// ad-hoc composition is refused — fail-closed, because "no configured set" must not read as
	// "anything goes".
	Adapters []string

	// Roots are the MANDATORY trusted workspace roots of this server — the directories a HUMAN
	// authorized before any request arrived (`aimesh review mcp --root <dir>`, or the validated launch
	// cwd; see surface/acp/roots.go, whose model this reuses unchanged). Empty = every request path
	// is refused.
	Roots []string
	// ClientRoots, when non-empty, are the roots an MCP client declared. The effective set is the
	// INTERSECTION with Roots, never the union: a client may narrow what this server may read and
	// can never widen it.
	//
	// It is the STARTING value only. Once the session is live the transport issues `roots/list` (and
	// re-issues it on `notifications/roots/list_changed`), and the answer replaces this through
	// applyClientRoots — which is why every read of the resolver goes through trusted() rather than
	// through a field captured at build time.
	ClientRoots []string

	// RootSource is the PROVENANCE of Roots as ResolveTrustedRoots reported it: a human typed them
	// (`--root`), or this process inferred them from its launch directory. Under the modern protocol
	// era the difference decides whether there is a trusted root at all — see trustFor.
	RootSource acp.RootSource
	// AllowInferredRoot is the operator's `--allow-inferred-root` opt-in (D6): accept an INFERRED
	// launch cwd as a trusted root even on the era that removed the client's ability to narrow this
	// server. Default false, which is the fail-closed answer.
	//
	// It loosens nothing else. The degenerate-root rule already refused `/`, a home directory,
	// home's parent and the system trees before this field is ever read, so the waiver can only admit
	// a plausible project directory; scope.Resolver, the read denylist and every write-layer rule are
	// untouched by it.
	AllowInferredRoot bool

	// PolicyCeiling is the config write-authority ceiling for the `mcp` surface
	// (`surfaces.defaultModeBySurface.mcp`; the shipped seed is `report`). AllowRemediate is the
	// `allowRemediate` capability read from the SAME config. Both are supplied by the launching
	// command from one config snapshot, so the tool list, the per-call refusal and the Manager's
	// own resolution all read the same policy.
	PolicyCeiling  review.Mode
	AllowRemediate bool
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
	// SUNSET-PATH (MCP26-SUNSET; migration design §16.2): removed with the legacy era.
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

	// trustMu guards trust, which is REPLACED whenever the client's declared roots arrive or change.
	// Every read goes through trusted(): a resolver captured once at build time would keep enforcing a
	// root set the client has since narrowed.
	trustMu sync.RWMutex
	trust   *scope.Resolver
}

// trusted is THE resolver in force right now. It never returns nil: a root set that cannot be
// canonicalized, or an intersection that came out empty, yields the zero resolver — which refuses every
// path, rather than degrading to "unrestricted".
//
// IT IS NOT REACHABLE FROM A HANDLER. Every handler resolves through `c.Env().Trust` instead — the
// resolver captured for THAT request (see trustFor). This function has exactly two callers left: the
// per-request capture itself, and the diagnostics line in applyClientRoots. That is the point: after
// the per-request envelope there is no code path from a tool handler to a process-global resolver, so
// per-call narrowing is expressible and confinement is mechanical rather than a convention.
func (s *Server) trusted() *scope.Resolver {
	s.trustMu.RLock()
	defer s.trustMu.RUnlock()
	if s.trust == nil {
		return &scope.Resolver{}
	}
	return s.trust
}

// trustFor captures the confinement for ONE request: the effective roots as the resolver canonicalized
// them, their provenance, whether anything narrowed the operator's startup set, and the resolver
// itself. The transport calls it once per dispatched request and carries the answer on the Call.
//
// Reading the resolver and the client-roots flag in ONE critical section matters: a `roots/list_changed`
// arriving between two separate reads could otherwise pair a resolver with a `narrowed` flag describing
// a different root set.
// THE MODERN ERA FAILS CLOSED ON AN INFERRED ROOT, and that is this function's second job.
//
// Under 2025-06-18 a host narrows this server through `roots/list`. 2026-07-28 deletes that method,
// so ClientRoots is permanently empty there and `len(ClientRoots) == 0 ⇒ the server's own roots
// stand` always wins — meaning the SAME launch configuration grants strictly MORE filesystem
// authority to a client that happens to speak the newer revision. A host that used to narrow us from
// an inferred repo root down to its one open folder simply stops narrowing us, silently, because of
// a version string.
//
// The fix is not a diagnostic (a diagnostic does not obtain anybody's consent). Under the modern era
// authority must have been TYPED BY A HUMAN: an inferred launch cwd is not a trusted root unless the
// operator waived it with `--allow-inferred-root`. Refused means the zero resolver — every
// filesystem path denied with scope.ReasonNoRoots — which is a refusal the operator fixes with one
// flag, not a widening nobody noticed. It is not a brick either: `inlineWorkspace` consumes no
// trusted root, so a modern client with zero roots can still perform a complete inline review.
//
// SUNSET-PATH (MCP26-SUNSET; migration design §16.2): at legacy removal the era parameter and
// this branch collapse into the modern rule.
func (s *Server) trustFor(era proto.Era) proto.TrustContext {
	s.trustMu.RLock()
	r, narrowed := s.trust, len(s.ClientRoots) > 0
	s.trustMu.RUnlock()
	if r == nil {
		r = &scope.Resolver{}
	}
	// The era-conditional refusal that used to live here is gone, replaced by an evidence test at
	// the point of inference (acp.resolveTrustedRoots: the cwd is adopted only when it carries a
	// PROJECT MARKER). It was keyed on the wrong variable. The stated risk was that the newest
	// revision removes the client's ability to narrow the server — but a legacy client that simply
	// declines the roots capability does not narrow it either, and that case was granted the
	// unnarrowed cwd silently, so the rule missed the very exposure it named while firing on a
	// default launch that a new user had no way to diagnose. Asking "is there any reason to believe
	// this directory is the work" answers both, identically on both eras.
	return proto.TrustContext{Roots: r.Roots(), Source: rootSourceOf(s.RootSource), Narrowed: narrowed, Resolver: r}
}

// rootSourceOf maps the resolver's provenance onto the transport's. Two enums exist because the
// transport is domain-free and the root resolver is reviewmesh's: neither should have to import the
// other to name the same three facts.
func rootSourceOf(src acp.RootSource) proto.RootSource {
	switch src {
	case acp.RootsExplicit:
		return proto.RootsExplicit
	case acp.RootsInferredCwd:
		return proto.RootsInferredCwd
	case acp.RootsNone:
		return proto.RootsNone
	default:
		// A server constructed without provenance (every in-repo test that sets Roots directly) says
		// so rather than claiming a human typed them. The env still resolves an empty set to
		// RootsNone, which is the answer that fails closed.
		return proto.RootsUnknown
	}
}

// rebuildTrust recomputes the resolver from the effective (startup ∩ client) roots.
func (s *Server) rebuildTrust() {
	s.trustMu.Lock()
	defer s.trustMu.Unlock()
	s.rebuildTrustLocked()
}

// rebuildTrustLocked is the same, for a caller that is already replacing ClientRoots under the lock.
// The read of ClientRoots and the swap of the resolver have to be ONE critical section: two overlapping
// root refreshes (the one after initialize and one from a `list_changed` that arrived immediately after)
// would otherwise be able to interleave a set with a resolver built from the other.
func (s *Server) rebuildTrustLocked() {
	r, err := scope.New(s.effectiveRoots()...)
	if err != nil || r == nil {
		r = &scope.Resolver{}
	}
	s.trust = r
}

// applyClientRoots is what the `roots/list` round trip feeds. It is the ONLY place the client can affect
// what this server may read, and it can only ever narrow:
//
//   - The effective set is startup ∩ client, computed by intersectRoots, which for each pair keeps the
//     DEEPER directory and keeps nothing at all when the two are disjoint. A client that offers a root
//     the operator did not launch this server with contributes nothing; a client that offers a WIDER
//     root than the server's leaves the server's own narrower one in force.
//   - An empty intersection is fail-closed BY CONSTRUCTION: scope.New with no roots refuses every path.
//   - A client that declares the capability but lists NO roots changes nothing. An empty list is the
//     absence of a declaration ("I have no roots to tell you about"), not a declaration of nothing, and
//     reading it as "restrict yourself to the empty set" would break a host that simply has no workspace
//     open. The same rule the field already had: len(ClientRoots) == 0 ⇒ the server's own roots stand.
//   - A root URI that is not a local `file://` path is IGNORED rather than guessed at.
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
	s.trustMu.Lock()
	s.ClientRoots = paths
	s.rebuildTrustLocked()
	eff := s.trust.Roots()
	s.trustMu.Unlock()
	// Stated on the DIAGNOSTICS sink (never the protocol stream), because an operator watching a
	// server narrow itself mid-session should not have to infer it from a later refusal.
	s.core.Diagnosticf("aimesh review mcp: client declared %d root(s) (%d not a local file:// path, ignored); effective trusted roots = startup ∩ client = %d",
		len(roots), skipped, len(eff))
	if len(paths) > 0 && len(eff) == 0 {
		s.core.Diagnosticf("aimesh review mcp: the client's roots are DISJOINT from this server's — the intersection is empty, so every filesystem path is now refused (fail-closed). A client can narrow this server's roots; it can never widen them.")
	}
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
// It also applies review's own core wiring (OnRoots, TrustFor) to that shared core. Those are
// harmless to a co-hosted explore: explore consults no roots, so a client narrowing this server's
// root set affects only the tools that read paths.
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
		s.core.TrustFor = s.trustFor
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
			// PER-REQUEST CONFINEMENT. The transport captures this once per dispatched request and hands
			// it to the handler on the Call, so every path a call names is judged against the roots in
			// force when that call arrived — not against whatever a process-global resolver happens to
			// hold when the handler gets round to asking.
			TrustFor: s.trustFor,
			// `resources/*`, so a governed write can actually be collected. The store is this server's
			// own registration table; it publishes no host path.
			Resources: &s.artifacts,
			// The `io.modelcontextprotocol/tasks` extension, as a PROJECTION over the run registry —
			// `taskId == runId`, no second execution model (see tasks.go). It is opt-in per client and
			// modern-era only, so a client that declares nothing sees exactly the job shape it saw
			// before this existed.
			Tasks: taskProvider{s},
		}
	}
	// THE resolver for this server, built from the EFFECTIVE roots. One resolver governs every
	// request-supplied path — the workspace and the authority documents — so confinement cannot
	// diverge between two code paths that both take paths. A root set that cannot be canonicalized
	// does NOT degrade to "unrestricted": it yields the zero resolver, which refuses everything.
	s.rebuildTrust()
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

// allowRemediate reports whether this server may write. It is the config capability and nothing
// else: there is deliberately no second way to reach true.
func (s *Server) allowRemediate() bool { return s.AllowRemediate }

// remediationCeiling is how far this server may write. Without the capability it is the surface's
// configured mode ceiling (the shipped `report`), which is why the tool is not listed at all.
func (s *Server) remediationCeiling() review.Mode {
	if !s.allowRemediate() {
		if s.PolicyCeiling != "" {
			return s.PolicyCeiling
		}
		return review.ModeReport
	}
	return review.ModeApply
}

// effectiveRoots is the trusted-root set in force: the server's roots, INTERSECTED with any the
// client declared.
func (s *Server) effectiveRoots() []string {
	if len(s.ClientRoots) == 0 {
		return s.Roots
	}
	return intersectRoots(s.Roots, s.ClientRoots)
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
			"SPENDS MONEY: it launches the configured model CLIs. Paths must resolve inside this server's trusted roots. " +
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

	// DOUBLE OPT-IN, first gate: the write primitive is not merely refused without the capability —
	// it is not ADVERTISED. A model cannot be talked into calling a tool it was never told exists,
	// and a client that never sees it cannot present it to a user as available.
	if s.allowRemediate() {
		s.core.Register(proto.Tool{
			Name:  toolRemediate,
			Title: "Apply accepted review findings",
			Description: "WRITES TO THE WORKSPACE. Applies the ALREADY-ADJUDICATED accepted findings of a prior review_report run (`fromRun`) — nothing is re-reviewed and nothing is re-judged. " +
				"Use output=patch to produce a diff artifact and change nothing, or output=apply to write the live files. Every call must pass allowWrite: true. " +
				"`fromRun` is the ONLY form on this surface: it lets a human read the accepted set BEFORE anything is written. " +
				"NEITHER a retry NOR a restart can make it write twice, but they report differently: while this server is running, a retry returns the ORIGINAL receipt; after a restart the run is re-read from disk, the first write has already changed the files the decision set pins, and the second call HALTS (reasonCode stale_decision_set) having written nothing. " +
				"If any targeted file has changed since the review, the call HALTS rather than applying a stale decision. To review without changing anything, use review_report.",
			InputSchema:  raw(remediateInputSchema),
			OutputSchema: raw(remediateResultSchema),
			Annotations:  spendAnnotations("Remediate (writes files)", true),
		}, s.remediateHandler)
	} else {
		// SAY WHICH GATE IS MISSING. The tool stays unadvertised — that is gate one and it is the
		// stronger property — but a caller that names it anyway is told the truth about WHY, because
		// the two refusals need opposite responses from a model: "the operator must relaunch this
		// server" is a message to pass to a human, whereas "add allowWrite and retry" is something
		// the model can fix itself. A bare "unknown tool" reads as the third thing, "you imagined
		// this tool", and sends it hunting for a different one.
		if s.core.WithheldTools == nil {
			s.core.WithheldTools = map[string]string{}
		}
		s.core.WithheldTools[toolRemediate] = "this server was launched WITHOUT the write grant, so it cannot write to the workspace at all. This is an OPERATOR decision made before your request and you cannot change it from here — ask the human to relaunch with --allow-remediate (or to grant the " + capabilityName + " capability in config). Do not retry, and do not look for another tool that writes: there is none. review_report still works and reports findings without writing."
	}

	s.core.Register(proto.Tool{
		Name:         toolList,
		Title:        "Report the configured adapters and profiles",
		Description:  "Reports what this server can run: adapter IDENTIFIERS and their readiness, the configured profiles (their ordered blind panel and single-slot lanes), the review modes, whether remediation is permitted, and the admission limits in force. Read-only. Reports no binary paths, launch arguments, trusted-root paths or environment detail, and cannot change any configuration. Call this before composing a panel.",
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
	Profile         string                `json:"profile"`
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
	// Roots is the NARROWING-ONLY per-call root argument: the effective set for this call is
	// intersectRoots(env.Roots, Roots). See narrowRoots.
	Roots []string `json:"roots"`
}

// narrowRoots applies a caller-supplied `roots` argument to one request's protocol context.
//
// THE ENTIRE SAFETY ARGUMENT IS intersectRoots. It keeps the deeper directory of each overlapping
// pair, keeps nothing when a pair is disjoint, and is NEVER a union. Therefore:
//
//   - the argument can only ever SHRINK the authority the call already had;
//   - a path outside the startup roots is not granted by naming it — the intersection drops it;
//   - an empty intersection is a refusal, not a fallback to "the roots you had";
//   - a hostile or confused model can, at worst, refuse its own call. It cannot reach anything new.
//
// This is the migration the `client/roots` page itself names — "existing implementations SHOULD
// migrate to passing directories or files via tool parameters, resource URIs, or server
// configuration" — applied literally, and it reproduces what `roots/list` did with the same code.
//
// IT IS ERA-NEUTRAL. Under the legacy era the intersection is three-way (startup ∩ client ∩ call);
// under modern the client leg is absent and it is two-way. One code path, testable before the modern
// era is reachable at all, and it survives legacy removal unchanged — so this mechanism is not
// scaffolding.
//
// The operator's `--root` set remains the CEILING in both eras. The model may lower it for one call
// and may not raise it, which is why accepting this from a model is composition rather than
// configuration: it touches no config store, it is transient, and it cannot grant authority.
func narrowRoots(env *proto.RequestEnv, call []string) (*proto.RequestEnv, *proto.CallToolResult, error) {
	if len(call) == 0 {
		return env, nil, nil
	}
	cleaned := make([]string, 0, len(call))
	for _, raw := range call {
		p := strings.TrimSpace(raw)
		if p == "" || !filepath.IsAbs(p) {
			return nil, nil, proto.InvalidParams(
				"invalid params: every entry of `roots` must be an ABSOLUTE directory path; got %q. `roots` NARROWS this call to a subset of the roots the operator established at launch — it cannot add one, and a relative path has no meaning to a server that does not share your working directory.", raw)
		}
		cleaned = append(cleaned, filepath.Clean(p))
	}
	// CANONICALIZE THE CALLER'S PATHS THE SAME WAY THE ENFORCER DOES, before comparing them. env.Roots
	// are the resolver's canonical forms; a caller's spelling is not. Intersecting the two as typed
	// would make `/tmp/x` and `/private/tmp/x` — the same directory through a symlinked prefix, which
	// is the ordinary case on macOS — look DISJOINT, and a narrowing that silently became a refusal is
	// a fail-closed bug rather than a safe one. scope.New is reused rather than a local normalizer for
	// the same reason: two canonicalizations of one path is how they come to disagree.
	asked, aerr := scope.New(cleaned...)
	if aerr != nil || asked == nil {
		return nil, nil, proto.InvalidParams(
			"invalid params: `roots` cannot be resolved: %v", aerr)
	}
	eff := intersectRoots(env.Roots, asked.Roots())
	if len(eff) == 0 {
		// FAIL-CLOSED, and stated as a refusal about the world rather than a malformed request: the
		// call was well-formed and asks for a scope this server was not granted. The roots themselves
		// are not named — their paths map the operator's machine and a caller does not need them in
		// order to correct the call.
		return nil, domainRefusal("", fault.New(fault.Containment, fmt.Sprintf(
			"the `roots` argument names %d director(y/ies) that do not intersect this server's %d trusted root(s), so this call has no effective root and every path in it is refused. `roots` can only NARROW the operator's launch-time roots; it can never add one.",
			len(cleaned), len(env.Roots))).
			WithHalt("M6").WithReason(string(scope.ReasonNoRoots))), nil
	}
	r, err := scope.New(eff...)
	if err != nil || r == nil {
		r = &scope.Resolver{}
	}
	narrowed := *env
	narrowed.Roots, narrowed.Trust, narrowed.Narrowed = r.Roots(), r, true
	return &narrowed, nil, nil
}

// remediateArgs adds the write parameters. `fromRun` and the full-cycle parameters are mutually
// exclusive; the server enforces that itself rather than trusting the schema's `oneOf`.
type remediateArgs struct {
	runArgs
	FromRun    string `json:"fromRun"`
	Output     string `json:"output"`
	AllowWrite *bool  `json:"allowWrite"`
	// Select is the SELECTIVE-APPLY filter: host-computed fingerprints from the source run's
	// accepted findings, naming which of them to write (D8-A). Absent applies the whole accepted
	// set; present-and-empty is refused, never widened.
	//
	// A pointer to a slice, so that "absent" and "[]" are DIFFERENT requests. This decoder is
	// strict (`DisallowUnknownFields`), so before this parameter existed a `select` was a -32602
	// rather than a silently dropped narrowing — which is the failure a narrowing filter must never
	// have, and the reason the ACP surface had to declare it explicitly to get the same property.
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
	// The per-call narrowing is applied FIRST, so that every judgement below — the workspace, the
	// authority documents, and the roots recorded on the run request — is made against the same,
	// already-narrowed resolver. Applying it later would let one of them be judged against a scope
	// the call had asked to give up.
	env, nres, nerr := narrowRoots(env, args.Roots)
	if nerr != nil || nres != nil {
		return nil, nres, nerr
	}
	pick, ov, ovm, err := s.resolveSelection(args.Profile, args.Panel)
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
		Workspace: ws, Mode: mode, Surface: "mcp", Profile: strings.TrimSpace(args.Profile),
		AdapterOverride: ov, ModelOverride: ovm, ReviewerPanel: pick.reviewers,
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
		Profile: prep.request.Profile, Panel: prep.request.ReviewerPanel,
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
	return s.allowRemediate() && !prep.inline
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
	// DOUBLE OPT-IN, second gate. It is checked FIRST, before anything else about the request is
	// considered, so a call that never confirmed the write cannot spend a cent learning that its
	// panel was also malformed.
	if args.AllowWrite == nil || !*args.AllowWrite {
		return nil, proto.InvalidParams(
			"invalid params: `allowWrite` must be present and literally true — review_remediate WRITES to the workspace, and this server requires per-call confirmation in addition to the operator's launch-time opt-in. Corrected call: {\"fromRun\": \"<a review_report runId>\", \"output\": \"patch\", \"allowWrite\": true}")
	}
	mode, merr := s.remediationMode(args.Output)
	if merr != nil {
		return nil, merr
	}
	if len(args.InlineWorkspace) > 0 {
		return nil, proto.InvalidParams(
			"invalid params: `inlineWorkspace` cannot be remediated — the content was supplied over the wire and materialized into a directory this server owns and then deletes, so there is nothing real to write to. Review inline content with review_report, and remediate a workspace path that resolves inside this server's trusted roots.")
	}
	fromRun := strings.TrimSpace(args.FromRun)
	// D5: MCP WRITES GO THROUGH `fromRun`, AND ONLY THROUGH `fromRun`.
	//
	// The problem it answers is a protocol one. On 2026-07-28 stdio a cancelled request may receive no
	// further message at all, so a write whose response is cancelled tells the caller nothing — and if
	// that call was also the call that CREATED the run, the caller is left holding no handle to the
	// run that may have written to its files. The full-cycle form is the only form with that shape.
	//
	// `fromRun` closes it with no new parameter and no new concept: it is not a key the caller invents,
	// it is a value the caller ALREADY RECEIVED, in a completed response, before the write request was
	// ever sent. Cancellation cannot retract a message already delivered, so at the instant the write
	// window opens the caller provably holds a handle. It is also the spec's own prescribed pattern —
	// basic/index: "State that needs to span multiple requests (e.g., long-running tasks,
	// application-level handles) MUST be referenced by an explicit identifier the client passes on each
	// request."
	//
	// The rule is ERA-NEUTRAL — applied on legacy MCP as well — because making it era-conditional would
	// mean two write shapes on one surface, which is worse than the breaking change. It is a deliberate
	// breaking change to this tool's input schema and docs/mcp.md says so.
	//
	// The CLI keeps its full cycle, and that asymmetry is the justified one: the CLI caller is a human
	// at a terminal who is shown the run directory on stderr as the run starts, and there is no
	// response to lose because the process is the answer.
	if fromRun == "" {
		return nil, proto.InvalidParams(
			"invalid params: review_remediate on MCP writes only from an existing run's accepted findings — call review_report first, then review_remediate {\"fromRun\": \"<runId>\", \"output\": %q, \"allowWrite\": true}. "+
				"The run id from review_report is your DURABLE HANDLE: it survives cancellation of the write call, which on 2026-07-28 stdio cannot be answered at all. "+
				"The one-call form (review and write together) is not offered on this surface, because a caller whose response is cancelled would be left holding nothing while a write may have happened.",
			string(mode))
	}
	if err := refuseRunFormingArgs(args.runArgs, fromRun, mode); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Workspace) != "" {
		return nil, proto.InvalidParams(
			"invalid params: `workspace` cannot be set beside `fromRun` — the workspace is the SOURCE RUN's, captured when it was reviewed, and re-naming it here would let a write land somewhere the accepted set was never judged against. Corrected call: {\"fromRun\": %q, \"output\": %q, \"allowWrite\": true}", fromRun, string(mode))
	}
	wait, werr := s.waitBudget(args.WaitSeconds)
	if werr != nil {
		return nil, werr
	}
	// THE SELECTIVE-APPLY FILTER, validated before any spend (D8-A). An EMPTY `select` is refused
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
	// The per-call narrowing applies to the WRITE too, and it must be resolved before the write window
	// opens: the roots this call carries are the roots the receipt records the write against.
	env, nres, nerr := narrowRoots(c.Env(), args.Roots)
	if nerr != nil {
		return nil, nerr
	}
	if nres != nil {
		return nres, nil
	}
	return s.remediateFromRun(ctx, c, env, fromRun, mode, wait, strings.TrimSpace(args.IdempotencyKey), selection)
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

// refuseRunFormingArgs rejects the run-forming parameters on the `fromRun` branch of
// `review_remediate`. The schema already forbids `profile`/`panel`/`authority` there — but a schema
// is advisory to a client, and the decoder embeds `runArgs` wholesale, so until now they parsed and
// were dropped on the floor.
//
// Silently ignoring them is not a small thing on this tool. Each names a GOVERNANCE input: which
// models judged, which panel composed the accepted set, which intent it was judged against. On a
// `fromRun` call all three already exist — they came from the source run, captured once, and the
// accepted set was adjudicated under them. A caller that passes different ones is asking for
// something this call cannot do, and accepting the call would tell it the opposite. So it is
// refused, by name, with the branch named too.
func refuseRunFormingArgs(args runArgs, fromRun string, mode review.Mode) error {
	var named []string
	if strings.TrimSpace(args.Profile) != "" {
		named = append(named, "`profile`")
	}
	if args.Panel != nil {
		named = append(named, "`panel`")
	}
	if len(args.Authority) > 0 {
		named = append(named, "`authority`")
	}
	if len(named) == 0 {
		return nil
	}
	return proto.InvalidParams(
		"invalid params: %s cannot be set on the `fromRun` branch of review_remediate — that branch APPLIES an accepted set that a prior review_report run already produced, so the panel, the profile and the authority manifest are the source run's and are not re-decided here. Nothing would have been re-reviewed or re-judged, so the parameter is refused rather than ignored. Drop it and pass {\"fromRun\": %q, \"output\": %q, \"allowWrite\": true}, or use the `workspace` branch to run a fresh governed cycle with the panel and authority you want.",
		strings.Join(named, " and "), fromRun, string(mode))
}

// remediationMode validates `output` against this server's ceiling.
func (s *Server) remediationMode(output string) (review.Mode, error) {
	switch review.Mode(strings.TrimSpace(output)) {
	case review.ModePatch:
		return review.ModePatch, nil
	case review.ModeApply:
		if s.remediationCeiling() != review.ModeApply {
			return "", proto.InvalidParams(
				"invalid params: this server's write-authority ceiling is %q, so `output: \"apply\"` is refused. Use `output: \"patch\"` to receive a diff artifact instead.", s.remediationCeiling())
		}
		return review.ModeApply, nil
	default:
		return "", proto.InvalidParams(
			"invalid params: `output` must be \"patch\" (produce a diff, change nothing) or \"apply\" (write the live workspace); got %q", output)
	}
}

// remediateFromRun is the PRIMARY form: apply a prior review's accepted set.
func (s *Server) remediateFromRun(ctx context.Context, c *proto.Call, env *proto.RequestEnv, fromRun string, mode review.Mode, wait int, key string, selection []string) (*proto.CallToolResult, error) {
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
		// deliberate rather than an oversight of D8-A. This guard exists so that a decision set is
		// never applied twice, and a second call naming a different `select` is still a second
		// application of the same set. It therefore attaches to the winner and returns the ORIGINAL
		// receipt — including the original `selection` — rather than opening a second write window
		// with a different filter.
		//
		// The consequence, stated so nobody discovers it: "apply findings 1–3, then 4–5" is NOT
		// available from one report run. Choose the selection in one call, or produce a fresh
		// review_report run for the second tranche. Loosening this would trade a caller convenience
		// for the one guarantee this surface exists to keep.
		return s.attachTo(ctx, c, prior, wait), nil
	}
	rec := s.runs.get(fromRun)
	if rec == nil {
		// THE REGISTRY MISS IS NOT THE END OF THE QUESTION. The registry is in-memory and bounded
		// by count and TTL, so a handle can name a run this process really did produce and really
		// did record — one evicted by retention, or one from before a restart — and answering
		// `unknownRun` for it was the surface-parity defect: an ACP agent honours the same handle,
		// from the same file, for the same run.
		return s.remediateFromDisk(ctx, c, env, fromRun, mode, wait, key, selection)
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
	req := run.RemediateRequest{
		Workspace: set.Workspace, WorkspaceIdentity: set.WorkspaceIdentity,
		Mode: mode, Surface: "mcp",
		Profile: set.Profile, ReviewerPanel: set.Panel,
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
// spelled as `<artifactDir>/<runId>` by the Manager (RunHandle) and then faces ReadDecisionSet's
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
func (s *Server) remediateFromDisk(ctx context.Context, c *proto.Call, env *proto.RequestEnv, fromRun string,
	mode review.Mode, wait int, key string, selection []string) (*proto.CallToolResult, error) {

	set, _, err := s.Manager.ReadDecisionSet(s.Manager.RunHandle(fromRun))
	if err != nil {
		// A handle that names nothing on disk EITHER. This also absorbs the two registry-path
		// refusals that have no on-disk counterpart, and they have none because the artifact says
		// so rather than because the check was dropped: a decision set is written once, by a
		// COMPLETED REPORT run, at the end of its cycle. A run still running, a run that halted,
		// and a remediation run all record no set at all — so `source_run_not_complete` and
		// `source_run_not_reviewable` cannot be reached from a file that does not exist. (A running
		// run is never evicted, so its record is always still here to answer with the first of
		// those; and an evicted handle already answered `unknownRun` before this change.)
		return unknownRun(fromRun), nil
	}
	// The same ladder, in the same order, answering with the SAME reason codes the registry path
	// reports — `run.ReasonInlineWorkspaceNotRemediable` and `run.ReasonNoAcceptedFindings`
	// are those strings, held in one place so the two paths cannot drift apart.
	if rerr := set.Remediable(); rerr != nil {
		return domainRefusal("", rerr), nil
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
		Profile: set.Profile, ReviewerPanel: set.Panel,
		// The trusted roots IN FORCE NOW, not the ones the source run was launched with. A decision
		// set outlives them, and the write is re-gated inside the governed write path.
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

// THE FULL-CYCLE WRITE FORM IS GONE FROM THIS SURFACE (D5).
//
// That form — run the governed cycle in report mode, then apply that run's accepted set through the
// same write window — is ABSENT, not merely disabled, because on MCP it is the one write
// shape whose caller can be left holding NOTHING — the run it would have to poll is a run only that
// same, cancelled response would have named. See remediateHandler for the rule and the teaching
// error, and docs/mcp.md for the breaking-change note.
//
// The capability is not lost, it is two calls: review_report, then review_remediate {fromRun}. The
// CLI keeps the one-call form, where the caller is a human who is shown the run directory on stderr.

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
		out["remediable"] = s.allowRemediate() && !set.Inline && set.Accepted > 0
	}
	text := fmt.Sprintf("Run %s (%s): %s after %.1fs.", rec.ID, rec.Tool, state, elapsed(rec))
	if state == StateRunning {
		text += " Keep polling; do not start another run for the same request."
	} else {
		text += " Fetch the full result with review_run_result."
	}
	// A POLLER THAT NEVER FETCHES `review_run_result` MUST STILL NOT READ A PARTIALLY-REFUSED RUN AS
	// CLEAN. `state` is "complete" for such a run and always will be (§13.4.2), so the two facts
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

// --- panel selection (compose, never configure) ---

// resolveSelection turns `profile` / `panel` into the requested panel plus the per-role overrides
// the Manager consumes. Every refusal here happens BEFORE any spend, and every message names the
// field and the accepted values so the corrected call is derivable rather than guessable.
//
// It deliberately does NOT decide whether an adapter/model pair RESOLVES — that is the resolver's
// single answer for every surface (config.ResolvePanel), so an MCP call and a CLI invocation can
// never disagree about what a name means. What it owns is what a surface owns: mutual exclusion,
// the bound, seat completeness, and the compose-not-configure boundary.
func (s *Server) resolveSelection(profile string, p *panelArg) (panelPick, map[review.Role]string, map[review.Role]string, error) {
	profile = strings.TrimSpace(profile)
	if p == nil {
		pick := panelPick{source: "default"}
		if profile != "" {
			pick.source, pick.profile = "profile", profile
			if err := s.checkProfile(profile); err != nil {
				return panelPick{}, nil, nil, err
			}
		}
		return pick, nil, nil, nil
	}
	if profile != "" {
		return panelPick{}, nil, nil, proto.InvalidParams(
			"invalid params: name EITHER `profile` (select a configured panel) OR `panel` (compose one), never both — a panel is composed or selected, never half of each. Call `review_list` to see the configured profiles.")
	}
	if len(p.Reviewers) == 0 {
		return panelPick{}, nil, nil, proto.InvalidParams(
			"invalid params: `panel.reviewers` needs at least 1 seat — it is the blind primary panel. Corrected call: {\"panel\": {\"reviewers\": [{\"adapter\": \"<configured adapter>\", \"model\": \"<model>\"}], \"author_remediator\": {\"adapter\": \"<configured adapter>\", \"model\": \"<model>\"}}}")
	}
	if len(p.Reviewers) > review.MaxReviewerSeats {
		return panelPick{}, nil, nil, proto.InvalidParams(
			"invalid params: `panel.reviewers` has %d seats, exceeding the cap of %d. Each seat is a real model CLI, so the cap is a spend control and is never clamped — send at most %d.",
			len(p.Reviewers), review.MaxReviewerSeats, review.MaxReviewerSeats)
	}
	// An ad-hoc panel MUST name its adjudicator. A hidden default would mean the caller composed a
	// panel whose judgment becomes the accepted set without ever seeing which model that was.
	if p.AuthorRemediator == nil {
		return panelPick{}, nil, nil, proto.InvalidParams(
			"invalid params: `panel.author_remediator` is required for an ad-hoc panel — it is the HOST-ADJUDICATION seat whose judgment becomes the accepted set, and it is never defaulted for you. Corrected call: {\"panel\": {\"reviewers\": [...], \"author_remediator\": {\"adapter\": \"<configured adapter>\", \"model\": \"<model>\"}}}")
	}
	pick := panelPick{source: "adhoc", roles: map[review.Role]review.SeatSpec{}}
	for i, seat := range p.Reviewers {
		if err := s.checkConfigured(fmt.Sprintf("panel.reviewers[%d]", i), seat); err != nil {
			return panelPick{}, nil, nil, err
		}
		pick.reviewers = append(pick.reviewers, seat.spec())
	}
	adapters := map[review.Role]string{}
	models := map[review.Role]string{}
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
			return panelPick{}, nil, nil, err
		}
		// A single-slot lane is addressed by ROLE, and a role override carries no effort — there is
		// no per-role effort anywhere in the configuration model. Accepting one here would silently
		// drop it, which is exactly the kind of quiet non-execution this surface refuses.
		if strings.TrimSpace(rs.seat.Effort) != "" {
			return panelPick{}, nil, nil, proto.InvalidParams(
				"invalid params: %s.effort is not settable per call — reasoning effort is per-seat only on `panel.reviewers[]` (where a different effort is a different vantage). Remove it, or configure it in a profile and select that profile.", rs.field)
		}
		spec := rs.seat.spec()
		adapters[rs.role], models[rs.role] = spec.Adapter, spec.Model
		pick.roles[rs.role] = spec
	}
	// A per-role override only reaches a lane the resolved profile actually HAS. Naming
	// cross_check/verifier on a profile without that lane would be silently ignored — the request
	// would run a panel the caller did not ask for — so it is refused with the reason named.
	if err := s.checkRoleLanes(pick.roles); err != nil {
		return panelPick{}, nil, nil, err
	}
	return pick, adapters, models, nil
}

// checkConfigured enforces compose-not-configure for one seat. The refusal NAMES the configured
// set, so the corrected call follows from the error.
func (s *Server) checkConfigured(field string, seat seatArg) error {
	spec := seat.spec()
	if spec.Adapter == "" || spec.Model == "" {
		return proto.InvalidParams("invalid params: %s needs a non-empty adapter and model (got adapter=%q model=%q)", field, spec.Adapter, spec.Model)
	}
	if len(s.Adapters) == 0 {
		return proto.InvalidParams("invalid params: %s cannot be resolved — this server bound no configured adapter set, so ad-hoc panel composition is refused", field)
	}
	if !slices.Contains(s.Adapters, spec.Adapter) {
		known := append([]string(nil), s.Adapters...)
		slices.Sort(known)
		return proto.InvalidParams(
			"invalid params: %s names adapter %q, which is not configured on this server — configured adapters are: %s. A call selects from the configured set; it never introduces an adapter, a path or a launch argument.",
			field, spec.Adapter, strings.Join(known, ", "))
	}
	return nil
}

// checkProfile refuses an unknown profile name pre-spend, naming the configured ones.
func (s *Server) checkProfile(name string) error {
	var names []string
	for _, p := range s.Config.Profiles() {
		if p.Name == name {
			return nil
		}
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return proto.InvalidParams(
		"invalid params: unknown profile %q — configured profiles are: %s (call `review_list` to see their panels), or compose a `panel` instead.",
		name, strings.Join(names, ", "))
}

// checkRoleLanes refuses a single-slot override for a lane the target profile does not define.
func (s *Server) checkRoleLanes(roles map[review.Role]review.SeatSpec) error {
	if len(roles) == 0 {
		return nil
	}
	target := s.Config.DefaultProfile()
	var lanes []SeatFact
	for _, p := range s.Config.Profiles() {
		if p.Name == target {
			lanes = p.Lanes
			break
		}
	}
	has := map[string]bool{}
	for _, l := range lanes {
		has[l.Role] = true
	}
	for role := range roles {
		if role == review.RoleAuthorRemediator {
			continue // every profile has an adjudicator; an absent one fails in the resolver
		}
		if !has[string(role)] {
			return proto.InvalidParams(
				"invalid params: `panel.%s` names a lane the default profile %q does not define, and a call cannot CREATE a lane (that is configuration). It would have been silently ignored, so it is refused instead: drop it, or select a profile that has that lane.",
				role, target)
		}
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
			"invalid params: provide EITHER `workspace` (a path inside this server's trusted roots) OR `inlineWorkspace` (content supplied over the wire), never both")
	case path == "" && len(args.InlineWorkspace) == 0:
		return "", false, cleanup, nil, proto.InvalidParams(
			"invalid params: `workspace` is required (a directory inside this server's trusted roots). To review content this server cannot read from disk, supply `inlineWorkspace` instead.")
	case path != "":
		// TRUSTED-ROOT CONFINEMENT. The path a peer named must resolve INSIDE the roots a human
		// authorized at LAUNCH. A path outside them — or any path at all when no root was
		// established — is refused HERE, before any spend, with the machine reason code attached.
		// The non-overridable read denylist still applies inside a trusted root, so a `.env` under
		// an allowed project is refused just the same.
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
	msg := err.Error()
	switch scope.ReasonOf(err) {
	case scope.ReasonNoRoots:
		msg += " — this MCP server has no trusted root, so it refuses every filesystem path; the operator must launch it as `aimesh review mcp --root <project-dir>`"
	case scope.ReasonOutsideRoot:
		// The roots themselves are NOT named: their paths map the operator's machine, and a caller
		// does not need them in order to correct the call.
		msg += fmt.Sprintf(" — this server has %d trusted root(s) established at launch; a request may narrow them but never widen them, and their paths are not reported over this surface", len(roots))
	}
	return fault.Wrap(fault.Containment, "refused by workspace scope policy", err).
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

// intersectRoots returns the INTERSECTION of two root sets: for each pair, the deeper directory
// when one contains the other, and nothing when they are disjoint. It is never a union — a client
// that declares a root this server was not launched with does not thereby gain it.
//
// An empty result is fail-closed by construction: with no effective root, every request path is
// refused.
func intersectRoots(server, client []string) []string {
	var out []string
	add := func(p string) {
		for _, have := range out {
			if sameRoot(have, p) {
				return
			}
		}
		out = append(out, p)
	}
	for _, c := range client {
		for _, sr := range server {
			switch {
			case underRoot(c, sr):
				add(c) // the client narrowed
			case underRoot(sr, c):
				add(sr) // the server is already narrower
			}
		}
	}
	return out
}

// foldRootPaths is true only on Windows, where the filesystem itself is case-insensitive and
// `C:\Proj` and `c:\proj` are the same directory. It mirrors meshcore/scope's `foldPaths`.
//
// It is a VAR rather than a `runtime.GOOS ==` written inline for one reason: the intersection rule is a
// containment guarantee, and its Windows branch is the one a unix machine can never execute. Holding the
// platform answer in a substitutable variable lets the Windows BRANCH be exercised on any platform — the
// same pattern `streamAliasing` and `reparseAttr` already use — so the branch is tested rather than
// merely compiled. It does not make the guarantee verified on real NTFS; see docs/security.md's platform
// matrix for what is and is not covered.
var foldRootPaths = runtime.GOOS == "windows"

func underRoot(p, root string) bool {
	p, root = filepath.Clean(strings.TrimSpace(p)), filepath.Clean(strings.TrimSpace(root))
	if p == "" || root == "" {
		return false
	}
	if sameRoot(p, root) {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if foldRootPaths {
		return strings.HasPrefix(strings.ToLower(p), strings.ToLower(prefix))
	}
	return strings.HasPrefix(p, prefix)
}

func sameRoot(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if foldRootPaths {
		return strings.EqualFold(a, b)
	}
	return a == b
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
