// Package acp is the ACP (Agent Client Protocol) surface Client: reviewmesh runs
// as a local agent subprocess speaking JSON-RPC 2.0 over stdio. It is host-neutral
// (not Devin-specific) and thin — it parses requests and routes them to the
// ReviewManager through the existing architecture; it adds no review logic.
//
// Transport framing is pluggable (see framing.go): newline-delimited JSON (the
// ACP/Zed style, default) and LSP-style Content-Length headers. The dispatch layer
// is framing-agnostic. Real-host interop (exact framing/handshake of a specific
// Devin Desktop / Zed / JetBrains build) is verify-on-provision.
package acp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
	// schema.Fingerprint is the HOST-COMPUTED finding identity. It is imported here so this surface
	// computes it with the same function the write path does — two spellings of one identity is how a
	// selector comes to name a different finding on two surfaces.
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
	"github.com/Tim-Butterfield/aimesh/internal/review/version"
)

// agentProtocolVersion is the ACP protocol version this agent implements (integer, per ACP v1).
const agentProtocolVersion = 1

// DefaultTurnTimeout is the wall-clock budget for one prompt when none is configured. It matches
// exploremesh's ACP/MCP default so a turn on any aimesh agent surface is bounded alike.
const DefaultTurnTimeout = 10 * time.Minute

// turnBudget is the configured turn timeout, or the default.
func (s *Server) turnBudget() time.Duration {
	if s.TurnTimeout > 0 {
		return s.TurnTimeout
	}
	return DefaultTurnTimeout
}

// JSON-RPC 2.0 error codes (standard + one reviewmesh range code).
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeReviewHalt     = -32000 // a review halt; data carries exitCode + haltClass
)

// Reviewer is the Manager capability the ACP surface routes to (Client → Manager).
// It is context-aware so an ACP `cancel` cancels the in-flight run.
//
// It has TWO halves, and the split is the two-turn write contract expressed in types. `RunContext`
// runs a governed cycle: a `report` turn, or a `patch` turn that decides for itself. The other two
// are FROM-RUN EXECUTION — resolve the handle a report turn handed back, then apply the set that run
// adjudicated. A write turn carrying `fromRun` never calls `RunContext`, because re-running the
// cycle is exactly what would make the applied set something other than the inspected set.
type Reviewer interface {
	RunContext(ctx context.Context, r run.Request) (review.RunOutcome, error)
	// ReadDecisionSet resolves a peer-supplied run handle to the decision set that run recorded,
	// VERIFYING the handle against the agent's own artifact directory rather than trusting it as a
	// path. It returns the set and the canonical run directory. See run.ReadDecisionSet.
	ReadDecisionSet(handle string) (*run.StoredDecisionSet, string, error)
	// Remediate applies an ALREADY-ADJUDICATED decision set through the one governed write path —
	// the same function the CLI and MCP reach. Nothing is re-reviewed and nothing is re-judged.
	Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error)
}

// Server is the ACP agent server.
type Server struct {
	Manager Reviewer
	Caps    review.SurfaceCaps
	Framing string // "" / "newline" (default) | "content-length"
	// DegradeWhenModeUnavailable: when a requested mode exceeds the host's capability
	// ceiling, degrade to the ceiling (true) with a logged reason, or fail with a usage
	// error (false). Wired from config.Surfaces.DegradeWhenModeUnavailable.
	DegradeWhenModeUnavailable bool

	// PolicyCeiling is the CONFIG write-authority ceiling for the ACP surface
	// (`surfaces.defaultModeBySurface.acp`; the shipped seed is `report`) — deliberate,
	// user-visible policy, NOT a host capability. It narrows the connection ceiling, so a
	// fully write-capable host cannot reach a live workspace write unless the user's config
	// opts the ACP surface in. The resolver caps the mode by the same policy; applying it
	// HERE too is what makes the cap visible to the host (a `mode_degraded` warn, or a
	// pre-run usage error when degradation is disabled) instead of a silent downgrade the
	// host only discovers in the result. An empty or unrecognized value narrows nothing (the
	// caller — see cli.runACP — is responsible for failing closed on a malformed policy).
	PolicyCeiling review.Mode

	// TurnTimeout bounds ONE prompt's wall clock (0 → DefaultTurnTimeout). It holds this surface to
	// the same parity as `exploremesh acp`/`exploremesh mcp`, which bound a turn: without it an ACP
	// prompt could run until the host gave up — which on a stdio agent means a panel of model CLIs
	// still spending with nobody waiting for them.
	// A timeout is a HALT with the ordinary taxonomy, never a partial result.
	TurnTimeout time.Duration

	// ValidateHostAdjudication, when set, makes every review this server runs exercise the configured
	// author_remediator host lane (a synthetic adjudication readiness probe when the reviewer yields no
	// findings). It is set ONLY by the web ACP-validation harness (via an env var the child reads at
	// startup); a normal ACP client leaves it false, so no synthetic host call / extra spend occurs.
	ValidateHostAdjudication bool

	// Roots are the TRUSTED workspace roots of this connection — the directories a HUMAN
	// authorized before any request arrived (`aimesh review acp --root <dir>`, or the launch
	// cwd; see roots.go). They are the ONLY thing that makes a filesystem path readable over
	// this surface: every request-supplied path (`session/prompt.workspace`, `session/new.cwd`,
	// authority document paths) must resolve INSIDE them, so a request may only NARROW the set
	// and can never authorize itself. Empty = fail CLOSED: every request path is refused (the
	// `inlineWorkspace` path still works, because that content is materialized into a directory
	// this process owns).
	Roots []string

	trustOnce sync.Once
	trust     *scope.Resolver

	// Sessions is the OPTIONAL durable session store enabling ACP v1 `session/resume` across
	// an agent process restart. When nil, resume is unsupported: `sessionCapabilities.resume`
	// is NOT advertised and `session/resume` returns method-not-found. When set, session/new
	// persists minimal session metadata and session/resume restores it (cwd) — no history.
	Sessions SessionStore
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
	// next command, so a caller that could grant itself this could arrange to run its own code.
	AllowProtectedPaths bool

	capMu sync.Mutex // guards Caps (narrowed by host-advertised capabilities at initialize)

	wmu    sync.Mutex                    // serializes frame writes across goroutines
	amu    sync.Mutex                    // guards active
	active map[string]context.CancelFunc // in-flight review id → cancel

	smu      sync.Mutex                    // guards known + sessions + sessionN
	known    map[string]*sessionState      // session ids created by session/new → per-session state
	sessions map[string]context.CancelFunc // ACP session id → in-flight run cancel
	sessionN int                           // session id counter
}

// sessionState is the minimal in-process record for a session created by session/new.
// CWD is the workspace root the host supplied (Zed sends it as `session/new.params.cwd`);
// it is used as the prompt workspace when a `session/prompt` omits an explicit `workspace`.
type sessionState struct {
	CWD string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type reviewParams struct {
	Workspace string `json:"workspace"`
	Mode      string `json:"mode"`
	// FromRun / Select are the two-phase write rule's parameters, carried here too because this
	// method can reach a governed write and a rule with one unguarded door is not a rule. See
	// requireRunHandle.
	FromRun string   `json:"fromRun,omitempty"`
	Select  []string `json:"select,omitempty"`
	// Meta carries the same `_meta.reviewmesh` extension the ACP v1 `session/prompt` path
	// accepts, so the compatibility method is not a parity hole (surface-parity invariant:
	// a run-forming capability must be expressible on every surface).
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// writeTurn is the two-phase write rule's inputs for one turn, bundled so every entry point that can
// reach a governed write passes the same thing to the same check.
type writeTurn struct {
	FromRun string
	Select  []string
	// WorkspaceNamed records whether the host NAMED a workspace on this turn, as opposed to
	// falling back to the session cwd or the process working directory. It matters only on a
	// from-run write: a named workspace is an assertion about which tree is being changed and is
	// checked against the source run's, while an implicit one asserts nothing and the source run's
	// workspace simply stands. Refusing the implicit case would break the ordinary session shape
	// (`session/new {cwd}`, then two prompts that name no workspace at all).
	WorkspaceNamed bool
}

// acceptedFingerprints projects a run's ACCEPTED findings to the identifiers a later write turn can
// select on: the host-computed fingerprint, the file, and the model-authored title for a human.
//
// "Accepted" is run.AcceptedForApply's answer and nothing else — one definition of the accepted
// set, shared with the write path, so the set a host is shown here cannot differ from the set a write
// would consider. A finding the write-path rule already quarantined (authority-only, no workspace
// evidence, weak identity) is NOT accepted and is deliberately absent: offering a caller a selector
// for something that can never be written would be an invitation to a refusal.
func acceptedFingerprints(out review.RunOutcome) []map[string]any {
	if len(out.Decisions) == 0 {
		return nil
	}
	byID := make(map[string]review.Decision, len(out.Decisions))
	for _, d := range out.Decisions {
		byID[d.FindingID] = d
	}
	rows := make([]map[string]any, 0, len(out.Findings))
	for _, fnd := range out.Findings {
		d, ok := byID[fnd.ID]
		if !ok || !run.AcceptedForApply(d) {
			continue
		}
		rows = append(rows, map[string]any{
			"fingerprint": schema.Fingerprint(fnd),
			"file":        fnd.File,
			"title":       fnd.Title,
		})
	}
	return rows
}

// metaEnvelope is the sanctioned ACP `_meta` extension point, narrowed to reviewmesh's
// namespace. Everything else in `_meta` belongs to the host and is ignored.
type metaEnvelope struct {
	Reviewmesh json.RawMessage `json:"reviewmesh"`
}

// reviewmeshMeta is the reviewmesh-namespaced run-forming input carried on a prompt.
type reviewmeshMeta struct {
	// Authority are the authority/context documents this review is judged AGAINST. Same
	// schema and same fail-closed rules as the CLI's `--authority`; only the encoding
	// differs (a transport mechanic, the one kind of per-surface difference the
	// surface-parity invariant allows).
	Authority []review.AuthorityDoc `json:"authority"`
	// Profile SELECTS a configured profile by name (absent/blank → the configured default),
	// the ACP equivalent of the CLI's `--profile`.
	Profile string `json:"profile,omitempty"`
	// Panel COMPOSES an ad-hoc blind reviewer panel — the ACP equivalent of repeatable
	// `--reviewer`. COMPOSE-NOT-CONFIGURE: a seat may only name an adapter and model the
	// SERVER'S configuration already defines. There is deliberately no field here for a binary
	// path, launch arguments, or an adapter definition: the absence IS the enforcement, so a
	// prompt can order and select configured identities but can never introduce one. Naming
	// both `profile` and `panel` is invalid params — a panel is composed OR selected.
	Panel []review.SeatSpec `json:"panel,omitempty"`
	// MaxParallel bounds how many of this turn's reviewer seats invoke their model CLI AT ONCE.
	// Absent, the whole panel runs in parallel. It is per-turn rather than a server flag for the same
	// reason it is per-call on MCP: the right number is a property of the machine the CLIs run on and
	// of the caller's provider rate limits, neither of which this agent can see. It bounds
	// parallelism only — every seat still reviews.
	MaxParallel int `json:"maxParallel,omitempty"`
	// DryRun resolves this turn's plan, panel, authority documents and preflight, then stops
	// before the first model call and answers with the run's SHAPE instead of a review. It is
	// per-turn for the same reason maxParallel is: "what would this cost" is a question about
	// THIS request, and a server flag could only answer it for every request at once. The turn
	// still reports the mode it priced — a dry run of an apply turn prices an apply turn — and
	// writes nothing whatever that mode says.
	DryRun bool `json:"dryRun,omitempty"`
	// VerifyReadiness asks every configured agent whether it can do real work before this turn
	// dispatches anything. It SPENDS one bounded, one-token call per distinct adapter/model, so a
	// panel does not pay for its first seat's whole prompt and then halt on a second seat blocked on
	// login or folder trust — failures a binary check cannot see. Per-turn for the same reason
	// dryRun is; dryRun prices these calls and performs none of them.
	VerifyReadiness bool `json:"verifyReadiness,omitempty"`
}

// parseReviewmeshMeta extracts the reviewmesh namespace from an ACP `_meta`. It is STRICT: an
// unknown field inside the reviewmesh namespace is an error rather than a silently ignored key,
// because a mistyped governance field that is quietly dropped is exactly the failure a
// fail-closed surface exists to prevent. Unknown keys ELSEWHERE in `_meta` are the host's
// business and are ignored.
func parseReviewmeshMeta(raw json.RawMessage) (reviewmeshMeta, error) {
	var rm reviewmeshMeta
	if len(bytes.TrimSpace(raw)) == 0 {
		return rm, nil
	}
	var meta metaEnvelope
	if err := json.Unmarshal(raw, &meta); err != nil {
		return rm, fmt.Errorf("_meta is not an object: %w", err)
	}
	if len(bytes.TrimSpace(meta.Reviewmesh)) == 0 {
		return rm, nil
	}
	dec := json.NewDecoder(bytes.NewReader(meta.Reviewmesh))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rm); err != nil {
		return rm, fmt.Errorf("_meta.reviewmesh is malformed: %w", err)
	}
	return rm, nil
}

// validateSelection enforces the run-forming rules a SURFACE owns for profile/panel selection,
// pre-spend and fail-closed: the two selectors are mutually exclusive, the panel is bounded, and
// each seat is structurally complete. It deliberately does NOT decide whether an adapter or model
// exists — that is the resolver's single answer for every surface (config.ResolvePanel), so an
// ACP prompt and a CLI invocation can never disagree about what resolves.
func (rm reviewmeshMeta) validateSelection() (string, error) {
	if rm.Profile != "" && len(rm.Panel) > 0 {
		return "panel_and_profile", fmt.Errorf("_meta.reviewmesh names both `profile` and `panel` — a blind reviewer panel is composed OR selected, never half of each")
	}
	if len(rm.Panel) > review.MaxReviewerSeats {
		return "panel_too_large", fmt.Errorf("_meta.reviewmesh.panel has %d seats, exceeding the cap of %d — each seat is a real model CLI, and the count is never trimmed for you", len(rm.Panel), review.MaxReviewerSeats)
	}
	for i, s := range rm.Panel {
		if strings.TrimSpace(s.Adapter) == "" || strings.TrimSpace(s.Model) == "" {
			return "panel_seat_incomplete", fmt.Errorf("_meta.reviewmesh.panel[%d] needs a non-empty `adapter` and `model` (example: {\"adapter\":\"codex-cli\",\"model\":\"codex-cli-default\"})", i)
		}
	}
	return "", nil
}

// Serve reads/writes through the configured framing until `exit` or EOF.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	return s.ServeFramed(NewFramer(s.Framing, in, out))
}

// ServeFramed runs the dispatch loop over an explicit Framer. `review` requests run
// concurrently (so a later `cancel` can reach an in-flight run); all other methods
// are handled inline. Frame writes are serialized. It never panics on bad input.
func (s *Server) ServeFramed(f Framer) error {
	// baseCtx parents every in-flight review; on any return (EOF, transport error,
	// `exit`) cancelAll fires BEFORE wg.Wait (LIFO defers), so blocked reviews unblock
	// and the server never hangs on shutdown.
	baseCtx, cancelAll := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancelAll()
	for {
		raw, err := f.ReadMessage()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		if raw[0] == '[' {
			s.write(f, errResp(nil, codeInvalidRequest, "batch requests are not supported"))
			continue
		}
		var req rpcRequest
		if jerr := json.Unmarshal(raw, &req); jerr != nil {
			s.write(f, errResp(nil, codeParse, "parse error"))
			continue
		}
		// Only the ABSENCE of an `id` member makes a request a notification (→ no
		// response). `id: null` is a valid identifier and still receives a response.
		notif := len(req.ID) == 0
		if req.JSONRPC != "2.0" {
			if !notif {
				s.write(f, errResp(req.ID, codeInvalidRequest, "invalid request: jsonrpc must be \"2.0\""))
			}
			continue
		}
		switch req.Method {
		case "initialize":
			if !notif {
				s.write(f, s.initialize(req.ID, req.Params))
			}
		case "review":
			// Register the cancel synchronously (before reading the next message) so a
			// following `cancel` for this id is guaranteed to find the in-flight run.
			ctx, cancel := context.WithCancel(baseCtx)
			key := string(req.ID)
			if !notif {
				s.putActive(key, cancel)
			}
			wg.Add(1)
			go func(req rpcRequest, notif bool, ctx context.Context, cancel context.CancelFunc, key string) {
				defer wg.Done()
				defer cancel()
				if !notif {
					defer s.delActive(key)
				}
				if resp := s.handleReview(ctx, req, notif, f); resp != nil {
					s.write(f, resp)
				}
			}(req, notif, ctx, cancel, key)
		case "session/new":
			if !notif {
				// Zed supplies the workspace root in `params.cwd` (+ `mcpServers`); store it so a
				// later session/prompt that omits `workspace` can fall back to it.
				var sn sessionNewParams
				_ = json.Unmarshal(req.Params, &sn)
				sid := s.newSession(sn.CWD)
				s.persistSession(sid) // durable record for cross-restart resume (best-effort)
				s.write(f, okResp(req.ID, map[string]any{"sessionId": sid}))
			}
		case "session/resume":
			// ACP v1 reconnect-without-history: restore a session persisted by an EARLIER agent
			// process from the durable store (no conversation replay). Method-not-found when no
			// store is configured (capability not advertised).
			if !notif {
				s.write(f, s.handleSessionResume(req))
			}
		// session/load is intentionally NOT implemented → method-not-found (default case). Its
		// ACP contract is to replay the full prior conversation, which a stateless one-shot
		// reviewer has none of; advertising/implementing it would be dishonest. `loadSession`
		// stays false and reconnect is served by the spec-correct `session/resume` above.
		case "session/prompt":
			// A session's review request. The sessionId MUST have been created by
			// session/new (authoritative lifecycle). Register cancel under it (so a later
			// session/cancel reaches it), then run like `review` in a goroutine.
			var sp sessionPromptParams
			_ = json.Unmarshal(req.Params, &sp)
			if sp.SessionID == "" || !s.knownSession(sp.SessionID) {
				if !notif {
					s.write(f, errResp(req.ID, codeInvalidParams, "invalid params: unknown or missing sessionId (call session/new first)"))
				}
				continue
			}
			ctx, cancel := context.WithCancel(baseCtx)
			// Register the cancel UNCONDITIONALLY under the session id — session/cancel keys
			// on sessionId (always present), so a notification prompt must stay cancellable.
			// Reject a concurrent prompt for a session that already has an in-flight run
			// (rather than overwriting + orphaning the first run).
			if !s.putSession(sp.SessionID, cancel) {
				cancel()
				if !notif {
					s.write(f, errResp(req.ID, codeInvalidRequest, "session busy: a prompt is already in flight for this session"))
				}
				continue
			}
			wg.Add(1)
			go func(req rpcRequest, notif bool, ctx context.Context, cancel context.CancelFunc, sid string) {
				defer wg.Done()
				defer cancel()
				resp := s.handleSessionPrompt(ctx, req, notif, f, sid)
				// THE SESSION SLOT IS RELEASED BEFORE THE RESPONSE GOES OUT, and the ordering is
				// load-bearing rather than tidy.
				//
				// The response IS the host's signal that the turn is over — the two-turn write
				// contract has a host send turn 2 the moment turn 1 answers, which is the whole
				// point of riding the run handle on a response. Releasing the slot in a `defer`
				// put the release AFTER this write, so a host that sequenced its turns exactly as
				// documented raced a cleanup it cannot observe and lost with `session busy`: a
				// -32600 for a correct sequence, intermittently.
				//
				// Nothing is left running at this point (handleSessionPrompt has returned), so a
				// `session/cancel` that arrives in the new gap correctly finds nothing to cancel.
				s.delSession(sid)
				if resp != nil {
					s.write(f, resp)
				}
			}(req, notif, ctx, cancel, sp.SessionID)
		case "session/cancel":
			s.handleSessionCancel(f, req, notif)
		case "cancel", "$/cancelRequest":
			s.handleCancel(f, req, notif)
		case "shutdown":
			if !notif {
				s.write(f, okResp(req.ID, map[string]any{"ok": true}))
			}
		case "exit":
			return nil
		default:
			if !notif {
				s.write(f, errResp(req.ID, codeMethodNotFound, "method not found: "+req.Method))
			}
		}
	}
}

// initializeParams carries the host-advertised client capabilities. A host can only
// NARROW reviewmesh's own caps (never grant beyond them); unknown/omitted fields leave a
// cap unchanged. Real-host capability shapes are verify-on-provision; this parses the
// common ACP `clientCapabilities.fs.{readTextFile,writeTextFile}` shape.
type initializeParams struct {
	ProtocolVersion    json.RawMessage `json:"protocolVersion"`
	ClientCapabilities *struct {
		FS *struct {
			ReadTextFile  *bool `json:"readTextFile"`
			WriteTextFile *bool `json:"writeTextFile"`
		} `json:"fs"`
	} `json:"clientCapabilities"`
}

// initialize builds the ACP v1 `initialize` result (or a protocol error). The result uses
// the ACP v1 schema — an INTEGER `protocolVersion`, `agentCapabilities`, `agentInfo`, and
// `authMethods` — NOT reviewmesh-internal `capabilities`/`serverInfo`. It is deliberately
// minimal + honest: conversation `session/load` is NOT supported (`loadSession:false` — its
// history-replay contract has no reviewmesh equivalent), prompts are text-only, and there is
// no MCP. Durable reconnect is served by `session/resume`, advertised via
// `sessionCapabilities.resume` ONLY when a session store is configured. reviewmesh's
// per-request mode gating + the compatibility `review` method still work when called but are
// not advertised here (a fuller ACP v1 surface is target/future, per-feature host-verified).
func (s *Server) initialize(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var ip initializeParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &ip) // best-effort; protocolVersion validated below
	}
	// Protocol-version negotiation. The client must send an INTEGER >= the agent's version;
	// a higher client version negotiates DOWN to the agent's supported version. A missing,
	// non-numeric (e.g. the string "0.1"), non-integer (e.g. 0.1), or too-low value is a
	// deterministic protocol error — never silently answered with a different version.
	cv, ok := parseIntProtocolVersion(ip.ProtocolVersion)
	if !ok || cv < agentProtocolVersion {
		return errResp(id, codeInvalidParams, fmt.Sprintf(
			"unsupported or missing protocolVersion: this agent implements ACP protocol version %d (send an integer protocolVersion >= %d)",
			agentProtocolVersion, agentProtocolVersion))
	}
	// Narrow SurfaceCaps from host-advertised clientCapabilities (narrowing only): a
	// read-only host (writeTextFile=false) drops FileWrite, which gates apply (see
	// modeCeiling). This is internal mode gating; it is NOT echoed in the ACP v1 result.
	if ip.ClientCapabilities != nil && ip.ClientCapabilities.FS != nil {
		s.capMu.Lock()
		if w := ip.ClientCapabilities.FS.WriteTextFile; w != nil {
			s.Caps.FileWrite = s.Caps.FileWrite && *w
		}
		if r := ip.ClientCapabilities.FS.ReadTextFile; r != nil {
			s.Caps.FileRead = s.Caps.FileRead && *r
		}
		s.capMu.Unlock()
	}
	agentCaps := map[string]any{
		// loadSession stays false: ACP `session/load` mandates replaying the full conversation
		// history, which a stateless reviewer has none of — advertising it would over-claim.
		"loadSession": false,
		"promptCapabilities": map[string]any{
			"image":           false,
			"audio":           false,
			"embeddedContext": false,
		},
		"mcpCapabilities": map[string]any{
			"http": false,
			"sse":  false,
		},
	}
	// Advertise `sessionCapabilities.resume` (an empty SessionResumeCapabilities object) ONLY
	// when a durable store is wired — resume reconnects WITHOUT replaying history (the honest
	// fit for a stateless reviewer). With no store, the capability is absent and session/resume
	// is method-not-found (capability advertised only when implemented).
	if s.Sessions != nil {
		agentCaps["sessionCapabilities"] = map[string]any{"resume": map[string]any{}}
	}
	return okResp(id, map[string]any{
		"protocolVersion": agentProtocolVersion,
		"agentInfo": map[string]any{
			"name":    "reviewmesh",
			"version": version.Get().Version,
		},
		"agentCapabilities": agentCaps,
		"authMethods":       []any{},
	})
}

// parseIntProtocolVersion returns the integer protocol version from a raw JSON value, or
// ok=false if it is missing, not a JSON number, or not an integer (e.g. the string "0.1"
// or the number 0.1).
func parseIntProtocolVersion(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false // not a JSON number (e.g. a string)
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false // not an integer (e.g. 0.1)
	}
	return int(i), true
}

// modeCeiling is the highest review mode the negotiated host capabilities permit: apply
// needs FileWrite (it commits to the live workspace), patch needs DiffContext (it emits a
// diff artifact), else report. Capping the effective mode to this ceiling guarantees a
// read-only host can never produce a live workspace write.
func (s *Server) modeCeiling() review.Mode {
	s.capMu.Lock()
	caps := s.Caps
	s.capMu.Unlock()
	switch {
	case caps.FileWrite:
		return review.ModeApply
	case caps.DiffContext:
		return review.ModePatch
	default:
		return review.ModeReport
	}
}

var acpModeRank = map[review.Mode]int{review.ModeReport: 0, review.ModePatch: 1, review.ModeApply: 2}

// minMode returns the lower-privilege of two known modes; an unknown mode is returned
// unchanged (left for the resolver to reject) rather than silently coerced.
func minMode(a, ceiling review.Mode) review.Mode {
	ra, ok := acpModeRank[a]
	if !ok {
		return a
	}
	if ra <= acpModeRank[ceiling] {
		return a
	}
	return ceiling
}

// sessionNewParams carries the workspace root the host supplies at session/new. Zed sends
// `{"cwd": "/abs/workspace", "mcpServers": []}`; other fields are ignored.
type sessionNewParams struct {
	CWD string `json:"cwd"`
}

// reviewParams (custom `review`) and sessionPromptParams (ACP v1 `session/prompt`)
// both carry a workspace + mode; session/prompt adds the protocol session id.
type sessionPromptParams struct {
	SessionID string `json:"sessionId"`
	Workspace string `json:"workspace"`
	Mode      string `json:"mode"`
	// Prompt is the host's prompt content (Zed sends `[{"type":"text","text":"..."}]`).
	// Accepted so the shape is not rejected, but NOT parsed for a target file in this build —
	// natural-language prompt-content → review scope is target/future.
	Prompt json.RawMessage `json:"prompt,omitempty"`
	// InlineWorkspace is a NARROW internal adapter for host-mediated reads: a map of
	// workspace-relative path → file content the host provides inline. The Client
	// materializes it to an isolated temp workspace (path-safe, exclusions applied) and
	// reviews by path — so WorkspaceAccess/ReviewManager stay path-based + ACP-unaware. The
	// real host's filesystem-callback *transport* (how content is fetched) is
	// verify-on-provision; this is the safe materialization half.
	InlineWorkspace map[string]string `json:"inlineWorkspace,omitempty"`
	// Permissions is the host's per-request permission decision (e.g. the result of a host
	// permission prompt). A denial narrows the effective mode ceiling for this prompt below
	// the connection capability. The host owns the prompt UI; reviewmesh adds no wizard.
	Permissions *promptPermissions `json:"permissions,omitempty"`
	// FromRun is the run handle a WRITE turn must carry: the `runDir` this agent returned in the
	// response to an earlier `report` turn. See requireRunHandle for why it is required and what it
	// does and does not guarantee.
	FromRun string `json:"fromRun,omitempty"`
	// Select is the selective-apply filter: host-computed fingerprints, from the report turn's
	// response, naming which accepted findings to write (D8-A).
	//
	// It is FUNCTIONAL, and it must stay DECLARED rather than merely unhandled: `json.Unmarshal`
	// drops unknown fields silently, so a host that sent `select` to a server that did not implement
	// it would have its write set SILENTLY WIDENED to every accepted finding — the exact failure a
	// narrowing filter exists to prevent.
	//
	// WHAT IT NARROWS: the decision set the `fromRun` run recorded. A selector taken from turn 1's
	// `accepted` list names the same OBJECT in turn 2 — the set is replayed, not re-derived — and a
	// selector naming nothing in that set lands in `selection.unmatched`. On a write turn with no
	// `fromRun` (a `patch` turn deciding for itself) it narrows that turn's own adjudication, which
	// is the same rule the CLI's full cycle applies. See requireRunHandle.
	Select []string `json:"select,omitempty"`
	// Meta is the ACP `_meta` extension point. `_meta.reviewmesh.authority[]` declares the
	// authority/context documents this review is judged against — the ACP half of the
	// surface-parity obligation that authority land on CLI and ACP together.
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// requireRunHandle is the ACP half of the two-phase write rule (D5, migration design §9.4).
//
// THE PROBLEM. A write whose response is lost — cancelled, dropped, or killed with the process —
// tells the caller nothing. If that same call was also the call that CREATED the run, the host is
// left holding no handle to a run that may already have written to its files. ACP makes this worse
// than MCP did: it has NO lookup surface at all. Its whole method set is initialize, review,
// session/new, session/resume, session/prompt, session/cancel, cancel, shutdown, exit — not one of
// them accepts a run identifier or returns anything about a prior run, and `session/resume`
// deliberately replays nothing.
//
// THE RULE. A prompt whose EFFECTIVE mode reaches `apply` must carry `fromRun`: the `runDir` this
// agent returned in a completed response to the host's own earlier `report` turn. On a JSON-RPC
// surface exactly one channel has the property we need — a response to the host's own request. If
// the host does not receive it, the host knows, because its request is still outstanding. So a write
// turn cannot begin unless the host already holds a handle it knows it received, and any write whose
// own response is then lost stays discoverable at that handle.
//
// It binds every entry point that can reach a governed write, including the compatibility `review`
// method, because a rule with one unguarded door is not a rule.
//
// THE HANDLE IS RESOLVED, NOT MERELY REQUIRED — and it is verified rather than trusted. This
// function checks only that one is PRESENT, pre-spend; `fromRunWrite` then resolves it against this
// agent's own artifact directory (run.ReadDecisionSet) and applies the decision set the named run
// recorded. Two consequences worth stating in the same breath:
//
//   - A handle naming a directory this agent did not produce is refused with one uniform
//     `run_handle_unknown`, decided LEXICALLY before the filesystem is consulted. A peer therefore
//     still gets no existence oracle over arbitrary absolute paths — the property the earlier
//     "never stat it" rule was protecting — while the handle it does hold now means something.
//   - A host that fabricates a handle no longer merely fools itself into a full review; it is told
//     no, before any spend.
//
// WHAT IS NOW GUARANTEED, and was not before: the applied set IS the inspected set. A write turn
// carrying `fromRun` applies the decision set the source run adjudicated — no reviewers, no second
// adjudication that could differ from the one the host read in turn 1 — through the same governed
// write path, with the source run's workspace-identity binding and its base-hash pins re-verified
// before anything is written and again per destination inside the commit. A tree that has moved on
// halts with `stale_decision_set`; it does not write.
//
// THAT IS WHAT `select` NOW MEANS HERE, and it is the sentence a host integrating selective apply
// needs: `select` narrows THE STORED SET. A fingerprint from turn 1's `accepted` list names the same
// object in turn 2 because it is the same object — not because a fresh panel happened to raise it
// again. A selector that names nothing in the stored accepted set lands in `selection.unmatched` and
// writes nothing, and a selection that matches nothing at all refuses the turn.
//
// `selection.requested/matched/unmatched` still rides every write turn that supplied a selection, so
// the narrowing remains inspectable rather than inferred.
func requireRunHandle(effective review.Mode, fromRun string, selection []string) (string, map[string]any, bool) {
	if effective != review.ModeApply {
		// A `report` turn writes nothing and a `patch` turn writes a diff artifact into the run
		// directory it is itself creating, so neither can leave a caller holding nothing.
		//
		// `select` on a REPORT turn is still refused: it names a filter over a write that will not
		// happen, so accepting it would report success for a narrowing that had no effect. A PATCH
		// turn does produce a write product, so a selection there is meaningful and is honoured.
		if len(selection) > 0 && effective == review.ModeReport {
			return "`select` narrows what is WRITTEN, and a `report` turn writes nothing — this turn's effective mode is " + string(effective) +
					". Send `select` on a `patch` or `apply` turn, or drop it.",
				map[string]any{"reasonCode": "select_without_write"}, false
		}
		if refusal, data, ok := checkSelection(selection); !ok {
			return refusal, data, false
		}
		return "", nil, true
	}
	if strings.TrimSpace(fromRun) == "" {
		return "an `apply` turn must carry `fromRun`: the `runDir` this agent returned in the response to an earlier `report` turn. " +
				"Run `session/prompt {mode: \"report\"}` first, read `_meta.reviewmesh.runDir` from its RESPONSE, then send `session/prompt {mode: \"apply\", fromRun: <runDir>}`. " +
				"The one-turn review-and-write form is not offered on this surface: a turn whose response is lost would leave you holding no handle to a run that may have written to your files.",
			map[string]any{"reasonCode": "write_without_run_handle"}, false
	}
	if refusal, data, ok := checkSelection(selection); !ok {
		return refusal, data, false
	}
	return "", nil, true
}

// checkSelection is the one rule this surface owns about `select`: an EMPTY list is a refusal, never
// a fallback to "apply everything".
//
// Everything else about the filter — what a fingerprint matches, what happens to one that matches
// nothing — belongs to the single governed write path and is deliberately not re-decided here, so
// `select` cannot come to mean one thing on ACP and another on the CLI or MCP.
func checkSelection(selection []string) (string, map[string]any, bool) {
	if selection == nil {
		return "", nil, true
	}
	for _, s := range selection {
		if strings.TrimSpace(s) != "" {
			return "", nil, true
		}
	}
	return "`select` is present and empty. An empty narrowing filter names ZERO findings — it does not mean \"apply everything\", and accepting it would let this turn read as a normal write while writing nothing. Omit `select` to write the whole accepted set, or name the `fingerprint` values from the `accepted` list in an earlier `report` turn's response.",
		map[string]any{"reasonCode": "select_empty"}, false
}

// promptPermissions carries a host's per-request write/patch decision. nil pointer = not
// specified (use connection capability). false = denied (narrows the ceiling).
type promptPermissions struct {
	AllowWrite *bool `json:"allowWrite"`
	AllowPatch *bool `json:"allowPatch"`
}

// materializeInline writes host-provided inline content to a fresh isolated temp workspace
// and returns its path. Each path must be workspace-relative (no absolute/volume/`..`
// escape) and not excluded; any violation removes the temp dir and errors — so a malicious
// host map cannot write outside the temp workspace. The caller removes the dir when done.
func materializeInline(files map[string]string) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("inlineWorkspace is empty")
	}
	dir, err := os.MkdirTemp("", "reviewmesh-acp-inline-*")
	if err != nil {
		return "", err
	}
	for rel, content := range files {
		clean := filepath.Clean(rel)
		// Shared cross-platform path-safety invariant (rejects absolute/rooted/volume/`..`
		// escape on both Windows and non-Windows) — one helper, no drift with WorkspaceAccess.
		if !workspace.SafeRel(rel) {
			os.RemoveAll(dir)
			return "", fmt.Errorf("unsafe path %q", rel)
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

func (s *Server) handleReview(ctx context.Context, req rpcRequest, notif bool, f Framer) *rpcResponse {
	var p reviewParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			if notif {
				return nil
			}
			return errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
		}
	}
	rm, aerr := parseReviewmeshMeta(p.Meta)
	if aerr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+aerr.Error(),
			map[string]any{"reasonCode": authority.ReasonDocInvalid})
	}
	// The compatibility `review` method accepts the SAME selection meta as session/prompt — a
	// compatibility method that could not select a profile or compose a panel would not be at
	// parity with the session path at all.
	if reason, serr := rm.validateSelection(); serr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+serr.Error(), map[string]any{"reasonCode": reason})
	}
	// The compatibility `review` method is not a session — no session id, no session/update,
	// no per-request permission cap. Its workspace is always a peer-named path, so it is
	// always judged against the trusted roots.
	return s.runAndRespond(ctx, req, notif, p.Workspace, false, p.Mode, "", "", rm,
		// The compatibility method has no session, so its workspace is ALWAYS the one the peer
		// named — there is no cwd to fall back to.
		writeTurn{FromRun: p.FromRun, Select: p.Select, WorkspaceNamed: strings.TrimSpace(p.Workspace) != ""}, f)
}

func (s *Server) handleSessionPrompt(ctx context.Context, req rpcRequest, notif bool, f Framer, sessionID string) *rpcResponse {
	var p sessionPromptParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			if notif {
				return nil
			}
			return errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
		}
	}
	// Host-mediated reads: materialize inline content to an isolated temp workspace, then
	// review it by path (Client-boundary adaptation; the temp dir is removed after the run).
	// ownedWorkspace records that WE created that directory, so it needs no trusted root —
	// the content came over the wire, not off this machine's filesystem.
	ownedWorkspace := false
	// Recorded BEFORE the cwd fallback below rewrites p.Workspace: "the host named this tree" and
	// "the host named nothing and we resolved one" are different assertions, and only the first is
	// checked against a source run's workspace on a from-run write.
	workspaceNamed := strings.TrimSpace(p.Workspace) != ""
	if len(p.InlineWorkspace) > 0 {
		if p.Workspace != "" {
			if notif {
				return nil
			}
			return errResp(req.ID, codeInvalidParams, "provide either workspace or inlineWorkspace, not both")
		}
		dir, err := materializeInline(p.InlineWorkspace)
		if err != nil {
			if notif {
				return nil
			}
			return errResp(req.ID, codeInvalidParams, "invalid inlineWorkspace: "+err.Error())
		}
		defer os.RemoveAll(dir)
		p.Workspace = dir
		ownedWorkspace = true
	}
	// Workspace resolution (ACP session only — the global CLI is unaffected): an explicit
	// `workspace` wins; otherwise fall back to the cwd the host supplied at session/new
	// (Zed's `params.cwd`); otherwise the ACP process working directory. If none resolves,
	// runAndRespond returns the existing "workspace is required" error. NONE of these
	// SELECTS anything: whatever wins is still judged against the trusted roots in
	// runAndRespond, so a host-supplied cwd cannot widen what this agent may read.
	if p.Workspace == "" {
		if cwd := s.sessionCWD(sessionID); cwd != "" {
			p.Workspace = cwd
		} else if wd, err := os.Getwd(); err == nil {
			p.Workspace = wd
		}
	}
	// An omitted session/prompt mode defaults to the safest mode, `report` (read-only).
	if p.Mode == "" {
		p.Mode = "report"
	}
	// Per-request host permission: a host that prompted the user and got a write/patch denial
	// for THIS prompt narrows the ceiling below the connection capability (no ACP-only wizard —
	// the host owns the prompt; reviewmesh just honors the result). deny-write → ≤patch;
	// deny-patch → report.
	permCeiling := review.Mode("")
	if p.Permissions != nil {
		if p.Permissions.AllowWrite != nil && !*p.Permissions.AllowWrite {
			permCeiling = review.ModePatch
		}
		if p.Permissions.AllowPatch != nil && !*p.Permissions.AllowPatch {
			permCeiling = review.ModeReport
		}
	}
	rm, aerr := parseReviewmeshMeta(p.Meta)
	if aerr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+aerr.Error(),
			map[string]any{"reasonCode": authority.ReasonDocInvalid})
	}
	// Selection is validated BEFORE the run so a bad panel costs a round trip, not a spend.
	if reason, serr := rm.validateSelection(); serr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+serr.Error(), map[string]any{"reasonCode": reason})
	}
	return s.runAndRespond(ctx, req, notif, p.Workspace, ownedWorkspace, p.Mode, sessionID, permCeiling, rm,
		writeTurn{FromRun: p.FromRun, Select: p.Select, WorkspaceNamed: workspaceNamed}, f)
}

// trusted returns THE resolver for this connection, built ONCE from the trusted roots. One
// resolver governs every request-supplied path on this surface — the workspace, the session
// cwd, and (through run.Request.TrustedRoots → authority.Input.Trust) the authority
// documents — so confinement cannot diverge between two code paths that both take paths.
//
// A root that cannot be canonicalized does NOT degrade to "unrestricted": it yields the
// zero resolver, which refuses everything. (The launching command validates roots first, so
// this is the belt to that suspenders.)
func (s *Server) trusted() *scope.Resolver {
	s.trustOnce.Do(func() {
		r, err := scope.New(s.Roots...)
		if err != nil || r == nil {
			s.trust = &scope.Resolver{} // fail CLOSED: no root is usable, so no path is
			return
		}
		s.trust = r
	})
	return s.trust
}

// checkRequestPath judges ONE host-supplied path against the trusted roots. It returns a
// *scope.Denial so the caller can attach the machine reason code to the protocol error.
//
// This is the whole G1 fix: the path in the request is EVIDENCE of what the peer wants, never
// the authority for it. Previously the resolver was built FROM the requested path, so the
// containment check compared the path with itself and always passed.
func (s *Server) checkRequestPath(path string) error {
	_, err := s.trusted().ResolveRead(path)
	return err
}

// rootHint appends the operator-facing remedy to a confinement refusal, so a host user reads
// "how do I allow this" rather than only "refused".
func (s *Server) rootHint(err error) string {
	switch scope.ReasonOf(err) {
	case scope.ReasonNoRoots:
		return " — this ACP agent has no trusted root, so it refuses every filesystem path; launch it as `aimesh review acp --root <project-dir>`"
	case scope.ReasonOutsideRoot:
		return fmt.Sprintf(" — the trusted roots of this agent are %s; a request may narrow them but never widen them (launch with `aimesh review acp --root <dir>` to add one)",
			strings.Join(s.trusted().Roots(), ", "))
	}
	return ""
}

// runAndRespond drives a review for a workspace/mode and formats the ACP response —
// shared by the ACP v1 `session/prompt` and the compatibility `review` method. When a
// sessionID is supplied (session/prompt), it streams `session/update` progress
// notifications tagged with that session as the Manager logs events.
//
// ownedWorkspace marks a workspace this PROCESS created and filled (the materialized
// `inlineWorkspace`): it is not a path the peer named, so the trusted-root check does not
// apply to it — that is the documented zero-config path, and it stays available on a server
// with no roots at all.
func (s *Server) runAndRespond(ctx context.Context, req rpcRequest, notif bool, workspace string, ownedWorkspace bool, mode, sessionID string, ceilingCap review.Mode, sel reviewmeshMeta, wt writeTurn, f Framer) *rpcResponse {
	authDocs := sel.Authority
	if workspace == "" {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: workspace is required")
	}
	// TRUSTED-ROOT CONFINEMENT. The workspace a peer named (an explicit `workspace` or the
	// session cwd it supplied at session/new) must resolve INSIDE the roots a human
	// authorized at launch. A path outside them — or any path at all when no root was
	// established — is refused HERE, before any spend, with the machine reason code
	// attached. The non-overridable read denylist still applies inside a trusted root, so a
	// `.env` under an allowed project is refused just the same.
	if !ownedWorkspace {
		if err := s.checkRequestPath(workspace); err != nil {
			if notif {
				return nil
			}
			return errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error()+s.rootHint(err),
				map[string]any{"reasonCode": string(scope.ReasonOf(err))})
		}
	}
	// Mode gating (Client-side; the Manager/resolver stay unaware). Cap the requested mode to
	// the effective ceiling: apply needs host write capability, patch needs diff capability,
	// and the config policy ceiling for this surface caps both. If the request exceeds the
	// ceiling, degrade to it (with a reason) when DegradeWhenModeUnavailable, else fail with a
	// usage error before any run.
	requested := review.Mode(mode)
	// Effective ceiling = connection capability, narrowed by the config policy ceiling for the
	// ACP surface, then by any per-request permission cap (e.g. the host denied write/patch for
	// this prompt). The narrowest wins; nothing here can ever WIDEN a ceiling.
	ceiling := s.modeCeiling()
	if _, known := acpModeRank[s.PolicyCeiling]; known {
		ceiling = minMode(s.PolicyCeiling, ceiling)
	}
	if ceilingCap != "" {
		ceiling = minMode(ceilingCap, ceiling)
	}
	if requested == "" {
		requested = ceiling
	} else if _, ok := acpModeRank[requested]; !ok {
		// Reject an unknown mode here, before any run — the resolver does not validate modes,
		// so an unknown mode would otherwise reach the Manager and run patch-like work.
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid mode: "+string(requested), map[string]any{"mode": string(requested)})
	}
	effective := minMode(requested, ceiling)
	degradeReason := ""
	if effective != requested {
		degradeReason = fmt.Sprintf("requested mode %q exceeds the effective ceiling %q (host capability, per-request permission, config surface policy)", requested, ceiling)
		if !s.DegradeWhenModeUnavailable {
			if notif {
				return nil
			}
			// `hostCeiling` keeps its wire name (hosts read it) but carries the EFFECTIVE
			// ceiling — capability, permission, and config policy already folded together.
			return errResp(req.ID, codeInvalidParams, "mode unavailable: "+degradeReason, map[string]any{
				"requestedMode": string(requested), "hostCeiling": string(ceiling),
			})
		}
		if sessionID != "" {
			s.notify(f, "session/update", sessionUpdateParams{
				SessionID: sessionID, Update: acpProgressUpdate("mode_degraded", "warn", degradeReason, ""),
			})
		}
	}
	// THE TWO-PHASE WRITE RULE, checked on the EFFECTIVE mode and BEFORE any work starts — before the
	// authority documents are resolved and long before a model is invoked. It is keyed on `effective`
	// rather than on `requested` deliberately: a turn the ceiling already degraded below `apply` writes
	// nothing to the workspace, and demanding a run handle from it would be ceremony.
	if msg, data, ok := requireRunHandle(effective, wt.FromRun, wt.Select); !ok {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+msg, data)
	}
	// Identity policy is now SHARED across surfaces (see review.classifyAndVerify): a lane whose model
	// identity is UNKNOWN (the adapter ran and returned schema-valid output but reported no verified
	// model) passes with a caveat instead of halting Class E; a proven MISMATCH still halts. Surface
	// is retained for audit/labeling only, not to gate the identity policy.
	// Authority/context documents, validated AND fully resolved BEFORE any spend. The
	// declaration check catches a malformed doc and the report-mode-only inline rule; the
	// resolve catches a scope-denied path, a broken hash pin, and an oversized document that
	// the no-silent-truncation rule refuses. All of them become a `-32602` carrying the
	// stable machine reason code, because an agent host must learn "this request is
	// unusable" before a model is ever invoked — not from a halt after the money is spent.
	// (The Manager resolves again, authoritatively, for the run: this surface never gets to
	// decide what the core embeds.)
	if verr := authority.Validate(authDocs, effective); verr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid authority: "+verr.Error(),
			map[string]any{"reasonCode": fault.ReasonOf(verr)})
	}
	// FROM-RUN EXECUTION. A write turn carrying `fromRun` APPLIES the decision set that run
	// recorded; it does not review anything. Everything below this branch — resolving authority
	// documents, composing a panel, invoking the Manager's governed cycle — belongs to a turn that
	// is deciding for itself, and a from-run turn decides nothing.
	//
	// It is keyed on the EFFECTIVE mode for the same reason the run-handle rule is: a turn the
	// ceiling already degraded to `report` writes nothing, so it runs the ordinary read-only cycle
	// and its `fromRun` is inert. That is deliberate — the host supplied the handle because it asked
	// for a mode this deployment does not grant, and refusing it would punish the host for obeying a
	// rule we then degraded away from. The response says `mode: "report"` (plus `modeDegraded`), so
	// nothing is hidden.
	if strings.TrimSpace(wt.FromRun) != "" && (effective == review.ModePatch || effective == review.ModeApply) {
		return s.fromRunWrite(ctx, req, notif, workspace, ownedWorkspace, effective, requested,
			degradeReason, sessionID, sel, wt, f)
	}
	// The SAME trusted resolver governs authority paths: an authority `path` is a request
	// path like any other, so it may narrow the trusted roots but never widen them (a peer
	// cannot declare `/etc/shadow` as "authority" and have the declaration authorize itself).
	if _, rerr := authority.Resolve(authority.Input{Docs: authDocs, Mode: effective, Workspace: workspace, Trust: s.trusted()}); rerr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid authority: "+rerr.Error(),
			map[string]any{"reasonCode": fault.ReasonOf(rerr)})
	}
	// TrustedRoots ride the request so the Manager's AUTHORITATIVE authority resolution uses
	// the same root set this surface just checked against — one model, no second code path.
	// Profile/panel SELECTION rides the request. Resolution (does this profile exist? does this
	// seat's adapter/model resolve? is the panel within 1..MaxReviewerSeats with no duplicate
	// identity?) is the Manager's single answer for every surface, so an unknown profile or an
	// unresolvable seat comes back as a config fault BEFORE any model call — the -32602 above
	// covers only what this surface itself owns (mutual exclusion, bound, seat completeness).
	rq := run.Request{Workspace: workspace, Mode: effective, Surface: "acp", Authority: authDocs,
		Profile: sel.Profile, ReviewerPanel: sel.Panel, MaxParallel: sel.MaxParallel,
		// Per-turn, like maxParallel: pricing is a question about THIS request.
		DryRun:          sel.DryRun,
		VerifyReadiness: sel.VerifyReadiness,
		// The narrowing selection travels UNEXAMINED into the one governed write path — this
		// surface resolves no fingerprint and knows which findings it names only from what comes
		// back. That is what keeps `select` one rule rather than three.
		Select: wt.Select,
		// A materialized `inlineWorkspace` is recorded on this run's decision set as ephemeral, so
		// a later `fromRun` turn naming this run is refused BY NAME rather than failing obscurely
		// on a directory this process has since deleted.
		WorkspaceEphemeral: ownedWorkspace,
		// BOUNDED EXECUTION and every containment/budget waiver, from the OPERATOR's launch flags —
		// never from the prompt. See the Server fields for why a caller cannot name a command here,
		// nor widen what containment admits, nor raise what its own prompt would carry.
		VerifyCommands:      s.VerifyCommands,
		VerifyTimeout:       s.VerifyTimeout,
		VerifyBaseline:      s.VerifyBaseline,
		AllowProtectedPaths: s.AllowProtectedPaths,
		TrustedRoots:        s.trusted().Roots(), ValidateHostAdjudication: s.ValidateHostAdjudication}
	if sessionID != "" {
		// In-process progress sink → session/update notifications. Gated on the run context
		// so nothing is emitted after cancellation (preserving no-write-after-cancel and
		// not emitting after the terminal response). Writes are serialized via s.notify.
		rq.OnEvent = func(ev audit.EventLine) {
			if ctx.Err() != nil {
				return
			}
			s.notify(f, "session/update", sessionUpdateParams{
				SessionID: sessionID,
				Update:    acpProgressUpdate(ev.EventType, ev.Level, ev.Message, ev.Timestamp),
			})
		}
	}
	// The turn budget. It wraps the request context (which the host's `cancel` already cancels), so
	// a run that outlives it is cancelled exactly as a host cancellation would cancel it — same
	// path, same no-write-after-cancel guarantee.
	runCtx, runCancel := context.WithTimeout(ctx, s.turnBudget())
	defer runCancel()
	out, err := s.Manager.RunContext(runCtx, rq)
	if notif {
		return nil // notification: ran for side effects, no response
	}
	// session/prompt (sessionID != "") returns the ACP v1 PromptResponse shape (a `stopReason`
	// + `_meta.reviewmesh`); the compatibility `review` method keeps the flat reviewmesh shape.
	isSession := sessionID != ""
	if errors.Is(err, context.Canceled) {
		rm := map[string]any{"status": "cancelled", "runDir": out.RunDir}
		if isSession {
			return okResp(req.ID, promptResponse("cancelled", rm))
		}
		return okResp(req.ID, rm)
	}
	if err != nil {
		// Review halts/errors stay JSON-RPC errors (exitCode + haltClass) on both paths — a
		// halt is an error condition, not a normal turn completion.
		halt := ""
		if out.Halt != nil {
			halt = string(*out.Halt)
		}
		// The halt taxonomy rides the error as MACHINE values (stable reason code +
		// classified actionability signal), matching what halt-record.json persists — a
		// host must not have to parse the message to learn why a run halted.
		data := map[string]any{
			"exitCode": int(fault.CodeOf(err)), "haltClass": halt, "runDir": out.RunDir,
			"reasonCode": fault.ReasonOf(err),
		}
		if sig := fault.SignalOf(err); sig != "" {
			data["signal"] = sig
		}
		// Surface the failing lane's actionable detail (role/adapter/model/exit + a captured
		// stderr excerpt) so a host/UI can explain WHY the adapter failed, not only that it did.
		// The excerpt is raw; consumers (e.g. the web-UI ACP validator) sanitize + cap it.
		if f := out.Failure; f != nil {
			data["failure"] = map[string]any{
				"role": f.Role, "adapter": f.Adapter, "model": f.Model, "modelArg": f.ModelArg,
				"exitCode": f.ExitCode, "haltClass": f.HaltClass, "reasonCode": f.ReasonCode,
				"signal":        f.Signal,
				"stderrExcerpt": f.StderrExcerpt, "stdoutExcerpt": f.StdoutExcerpt,
			}
		}
		return errResp(req.ID, codeReviewHalt, "review halted: "+err.Error(), data)
	}
	rm := map[string]any{
		"status":   out.Status,
		"mode":     string(out.Mode),
		"findings": len(out.Findings),
		"runDir":   out.RunDir,
	}
	// THE REPORT TURN'S RESPONSE CARRIES A FINGERPRINT PER ACCEPTED FINDING (migration design §9.4.2).
	//
	// This is the channel that makes selective apply expressible on this surface at all. A caller
	// cannot select on values it was never told, ACP has no lookup method to fetch them from, and with
	// a one-shot write there was no moment at which we could have told it. The response to the host's
	// own report request is that moment — and it is the one channel with a delivery property, which is
	// why the run handle rides it too rather than a `session/update`.
	//
	// The key is the HOST-COMPUTED fingerprint, never the model-authored Finding.ID. That is the whole
	// security content of it: a model-authored identifier is model-controlled, so keying a write set on
	// one would let a model relabel findings until "apply only this one" selected something else. A
	// fingerprint is computed here, from the finding's own content and target, so a model can change
	// what a fingerprint IS only by proposing a different finding, openly.
	if accepted := acceptedFingerprints(out); len(accepted) > 0 {
		rm["accepted"] = accepted
	}
	// CROSS-RUN DISPOSITION MEMORY's disclosure and per-finding annotations, at parity with the
	// CLI's `--json` projection and the MCP result: the same host-computed rows, from the same	// The CITATION-GROUNDING tally and its per-finding rows, from the same one derivation for the same
	// reason. The tally carries its own `note` stating what grounding does not establish; a host that
	// renders the ratio without it is making the stronger claim on this run's behalf.
	if out.Grounding != nil {
		rm["grounding"] = out.Grounding
		if rows := run.GroundingRows(out); len(rows) > 0 {
			rm["groundingFindings"] = rows
		}
	}
	// BOUNDED EXECUTION's record, when the operator supplied commands. `delta` is the field a host
	// should read; the `note` beside it states that neither outcome is a verdict on any finding.
	if out.Verification != nil {
		rm["verification"] = out.Verification
	}
	// WHAT THE PANEL WAS, beside the agreement counts it qualifies. A host that surfaces
	// `agreementCount` without `independence` is quoting a number that is weaker than it looks
	// whenever the panel doubled up a model.
	if out.Composition != nil {
		rm["composition"] = out.Composition
	}
	// WHAT THE PANEL AGREED ON, beside what its agreement was worth. A host that renders the
	// contested count without the `note` has turned a reading prompt into a verdict: a blind seat's
	// silence is not a vote against a finding, and nothing here was dropped or downgraded.
	if out.Dissent != nil {
		rm["dissent"] = out.Dissent
	}
	// THE REDUCED DENOMINATOR. It rides the echo whenever a capacity failure cost this run a seat,
	// because a quieter panel wearing the same name is the failure the disclosure exists to prevent.
	if out.PartialPanel != nil {
		rm["partialPanel"] = out.PartialPanel
	}
	if out.Scope != nil {
		rm["scope"] = out.Scope
	}
	// THE DRY RUN'S DISCLOSURE — the whole product of a turn that deliberately convened nobody.
	// Without it a host receives `status: planned` and an empty finding set, which reads exactly
	// like a review that found nothing.
	if out.Shape != nil {
		rm["shape"] = out.Shape
	}
	if out.Mode == review.ModePatch || out.Mode == review.ModeApply {
		rm["patchArtifact"] = out.RunDir + "/patches/changes.patch"
		// The write-outcome discriminator and its counts ride every WRITE turn. `applied` sits
		// beside `refused` deliberately: the stopReason below goes to `refusal` on a partial
		// refusal, and a host that read that alone could conclude nothing was written — the pair
		// of numbers is what stops the fail-safe reading from becoming a wrong one.
		rm["outcome"] = review.ApplyOutcome(out.Applied, len(out.Refusals))
		rm["applied"] = out.Applied
		rm["refused"] = len(out.Refusals)
		// THE SELECTION, on every write turn that supplied one. This is the FULL-CYCLE branch — a
		// write turn with no `fromRun`, so a `patch` turn deciding for itself — and here the filter
		// narrows THIS run's own adjudication. A selector carried over from an earlier run can
		// therefore legitimately match nothing, and `unmatched` is how a host sees that rather than
		// inferring it from a smaller-than-expected `applied`. (A `fromRun` turn narrows the STORED
		// set instead and never reaches this function; see fromrun.go.)
		if out.Selection.Selective() {
			rm["selection"] = selectionPayload(out.Selection)
		}
	}
	// PROTECTED-PATH REFUSALS. Nothing was written to any of these; every other accepted finding
	// was applied. They are keyed on the HOST-COMPUTED fingerprint, never the model-authored
	// finding id.
	if len(out.Refusals) > 0 {
		refusals := make([]map[string]any, 0, len(out.Refusals))
		for _, r := range out.Refusals {
			refusals = append(refusals, map[string]any{
				"fingerprint": r.Fingerprint, "file": r.File, "reason": r.Reason,
			})
		}
		rm["refusals"] = refusals
	}
	// Identity caveats (unknown / weak self-reported) for lanes that ran + returned schema-valid
	// output — the run still succeeded. Structured config identifiers only (role/adapter key/model
	// arg/evidence/status); the web validator maps them to display names + redacts before HTTP.
	if len(out.IdentityCaveats) > 0 {
		caveats := make([]map[string]any, 0, len(out.IdentityCaveats))
		for _, c := range out.IdentityCaveats {
			caveats = append(caveats, map[string]any{
				"role": c.Role, "adapter": c.Adapter, "requestedModel": c.RequestedModel,
				"evidence": string(c.Evidence), "status": c.Status, "reportedModel": c.ReportedModel,
			})
		}
		rm["identityCaveats"] = caveats
	}
	// Files a CONTAINMENT rule kept out of the reviewed set. A host must be able to tell "the
	// reviewers saw nothing wrong with this file" from "the reviewers were never shown this
	// file" — silence about a file nobody was shown is not approval. Paths are
	// workspace-relative (no absolute host paths) and the reason is meshcore's machine code.
	if len(out.Withheld) > 0 {
		withheld := make([]map[string]any, 0, len(out.Withheld))
		for _, w := range out.Withheld {
			withheld = append(withheld, map[string]any{
				"path": w.Path, "reason": w.Reason, "rule": w.Rule, "detail": w.Detail, "stage": w.Stage,
			})
		}
		rm["withheld"] = withheld
	}
	// The EXECUTED PANEL ROSTER rides every result: one entry per requested seat, with what ran
	// and how it ended. It is ALWAYS present (a panel of one included) so a host never has to
	// infer the panel's size — and a host that composed N seats can verify that N ran.
	if len(out.Panel) > 0 {
		rm["panel"] = out.Panel
	}
	// The authority INCLUSION MANIFEST rides the result: per document the source, the
	// full/embedded hashes, the byte counts and whether the inclusion was complete — so a
	// host can record exactly which intent this run was judged against.
	if len(out.Authority) > 0 {
		rm["authority"] = out.Authority
	}
	if degradeReason != "" {
		rm["modeDegraded"] = true
		rm["requestedMode"] = string(requested)
		rm["modeReason"] = degradeReason
	}
	if isSession {
		// A successful (single-pass report) turn completes → StopReason "end_turn".
		//
		// EXCEPT on a partial refusal, which answers "refusal" — the third official StopReason
		// this surface emits. `end_turn` is the word for a turn that ended normally, and a host
		// keying on stopReason alone would read it as clean; `refusal` is the fail-safe reading.
		// The risk it introduces — a host concluding that NOTHING was applied — is answered by
		// `_meta.reviewmesh.applied` sitting right beside it. It is deliberately not a `-32000`
		// halt: a halt is an error response, and an error response over a committed write would
		// tell the caller the turn failed while the receipt says otherwise.
		if len(out.Refusals) > 0 {
			return okResp(req.ID, promptResponse("refusal", rm))
		}
		return okResp(req.ID, promptResponse("end_turn", rm))
	}
	return okResp(req.ID, rm)
}

// --- ACP v1 session registry ---

// newSession creates a session, recording the host-supplied workspace cwd (cleaned), and
// returns its id. With a DURABLE store wired, ids are collision-resistant random tokens (the
// store is global under the home, so a deterministic per-process `s-NNNN` counter would
// collide across restarts — a fresh process's `s-0001` overwriting a prior session's record).
// Without a store, ids stay the deterministic in-process `s-NNNN` sequence.
func (s *Server) newSession(cwd string) string {
	s.smu.Lock()
	defer s.smu.Unlock()
	var id string
	if s.Sessions != nil {
		id = "s-" + randomToken()
	} else {
		s.sessionN++
		id = fmt.Sprintf("s-%04d", s.sessionN)
	}
	if s.known == nil {
		s.known = map[string]*sessionState{}
	}
	s.known[id] = &sessionState{CWD: cleanSessionCWD(cwd)}
	return id
}

// randomToken returns 16 hex chars (64 bits of entropy) for a collision-resistant durable
// session id, falling back to a nanosecond token only if the system RNG is unavailable.
func randomToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) knownSession(id string) bool {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.known[id] != nil
}

// sessionCWD returns the stored workspace cwd for a session id (empty if none/unknown).
func (s *Server) sessionCWD(id string) string {
	s.smu.Lock()
	defer s.smu.Unlock()
	if st := s.known[id]; st != nil {
		return st.CWD
	}
	return ""
}

// persistSession writes the durable record for an in-process session created by session/new
// (best-effort: a store failure is non-fatal — the session still works in-process this run).
// No-op without a store.
func (s *Server) persistSession(id string) {
	if s.Sessions == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_ = s.Sessions.Save(SessionRecord{
		SchemaVersion: SessionRecordSchema,
		SessionID:     id,
		CWD:           s.sessionCWD(id),
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

// sessionResumeParams is the ACP v1 ResumeSessionRequest: `sessionId` required, `cwd`
// optional (the host re-supplies the workspace root on reconnect). `additionalDirectories`
// and `mcpServers` are accepted but unused.
type sessionResumeParams struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
}

// handleSessionResume implements ACP v1 `session/resume`: restore a session persisted by an
// EARLIER agent process from the durable store, **without replaying any conversation history**
// (reviewmesh holds none). On success it re-registers the in-process session handle (so a
// subsequent `session/prompt` resolves the workspace via the cwd fallback) and returns an
// empty ResumeSessionResponse. Method-not-found when no store is configured; structured
// invalid-params for a missing id or an unknown / malformed / expired record.
func (s *Server) handleSessionResume(req rpcRequest) *rpcResponse {
	if s.Sessions == nil {
		return errResp(req.ID, codeMethodNotFound, "method not found: session/resume")
	}
	var p sessionResumeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if p.SessionID == "" {
		return errResp(req.ID, codeInvalidParams, "invalid params: sessionId is required")
	}
	rec, err := s.Sessions.Load(p.SessionID)
	if err != nil {
		return errResp(req.ID, codeInvalidParams, "cannot resume session: "+err.Error(),
			map[string]any{"sessionId": p.SessionID})
	}
	// Prefer a host-supplied cwd on reconnect (authoritative for this run); else the persisted
	// cwd. Restore the in-process handle keyed by the resumed id.
	cwd := rec.CWD
	if c := cleanSessionCWD(p.CWD); c != "" {
		cwd = c
	}
	s.restoreSession(p.SessionID, cwd)
	// Touch updatedAt so an active session stays fresh (createdAt preserved); best-effort.
	rec.CWD = cwd
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	_ = s.Sessions.Save(rec)
	// ACP v1 ResumeSessionResponse: all fields optional → an empty object is valid. No history.
	return okResp(req.ID, map[string]any{})
}

// restoreSession re-registers a resumed session id in the in-process registry with its cwd,
// and advances the session counter past a resumed `s-NNNN` id so a later session/new cannot
// mint a colliding id within this process.
func (s *Server) restoreSession(id, cwd string) {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.known == nil {
		s.known = map[string]*sessionState{}
	}
	s.known[id] = &sessionState{CWD: cwd}
	if n := parseSessionNum(id); n > s.sessionN {
		s.sessionN = n
	}
}

// parseSessionNum extracts the integer N from an "s-NNNN" id (0 if it does not match).
func parseSessionNum(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "s-%d", &n); err != nil {
		return 0
	}
	return n
}

// cleanSessionCWD normalizes a host-supplied workspace cwd: trim, `filepath.Clean`, and
// resolve a relative path against the ACP process working directory (`filepath.Abs`). It
// writes nothing and does not stat the path (the Manager's preflight validates existence).
// An empty input yields an empty result (no cwd stored).
func cleanSessionCWD(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) {
		if abs, err := filepath.Abs(clean); err == nil {
			clean = abs
		}
	}
	return clean
}

// putSession registers the in-flight run for a session id. It returns false if the
// session already has an active run (so a concurrent prompt is rejected, not silently
// overwritten — which would orphan the first run from session/cancel).
func (s *Server) putSession(id string, cancel context.CancelFunc) bool {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]context.CancelFunc{}
	}
	if _, busy := s.sessions[id]; busy {
		return false
	}
	s.sessions[id] = cancel
	return true
}

func (s *Server) delSession(id string) {
	s.smu.Lock()
	delete(s.sessions, id)
	s.smu.Unlock()
}

func (s *Server) cancelSession(id string) bool {
	s.smu.Lock()
	defer s.smu.Unlock()
	if cancel, ok := s.sessions[id]; ok {
		cancel()
		return true
	}
	return false
}

func (s *Server) handleSessionCancel(f Framer, req rpcRequest, notif bool) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	cancelled := s.cancelSession(p.SessionID)
	if !notif {
		s.write(f, okResp(req.ID, map[string]any{"cancelled": cancelled}))
	}
}

func (s *Server) handleCancel(f Framer, req rpcRequest, notif bool) {
	var p struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(req.Params, &p)
	cancelled := s.cancelActive(string(bytes.TrimSpace(p.ID)))
	if !notif {
		s.write(f, okResp(req.ID, map[string]any{"cancelled": cancelled}))
	}
}

// --- active-request registry ---

func (s *Server) putActive(key string, cancel context.CancelFunc) {
	s.amu.Lock()
	if s.active == nil {
		s.active = map[string]context.CancelFunc{}
	}
	s.active[key] = cancel
	s.amu.Unlock()
}

func (s *Server) delActive(key string) {
	s.amu.Lock()
	delete(s.active, key)
	s.amu.Unlock()
}

func (s *Server) cancelActive(key string) bool {
	s.amu.Lock()
	defer s.amu.Unlock()
	if cancel, ok := s.active[key]; ok {
		cancel()
		return true
	}
	return false
}

// --- response helpers ---

func (s *Server) write(f Framer, resp *rpcResponse) {
	if resp == nil {
		return
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	s.wmu.Lock()
	_ = f.WriteMessage(b)
	s.wmu.Unlock()
}

// rpcNotification is a JSON-RPC notification (no id, no response expected) — e.g. a
// `session/update` progress event.
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// sessionUpdateParams is the ACP v1 `session/update` (SessionNotification) params:
// `{ sessionId, update }`, where `update` is a SessionUpdate discriminated-union object
// (its variant is keyed by the `sessionUpdate` discriminator).
type sessionUpdateParams struct {
	SessionID string         `json:"sessionId"`
	Update    map[string]any `json:"update"`
}

// acpProgressUpdate builds an ACP v1 SessionUpdate for a reviewmesh progress event. ACP v1
// has no dedicated "agent progress" variant, so progress text is carried as the
// least-misleading official variant — **`agent_message_chunk`** (a text ContentBlock) — and
// the structured reviewmesh audit-event fields (no secrets / no raw provider metadata) are
// preserved under the sanctioned **`_meta.reviewmesh`** extension point. (Documented
// compromise: these are progress lines, not literal model output.)
func acpProgressUpdate(eventType, level, message, timestamp string) map[string]any {
	return map[string]any{
		"sessionUpdate": "agent_message_chunk",
		// Display text carries a trailing newline so a host that concatenates streamed
		// `agent_message_chunk` text (e.g. Zed) renders each progress line on its own line
		// instead of run-together. This is a generic ACP-valid choice (text content blocks may
		// contain newlines), not host-specific. The structured `_meta.reviewmesh.message` keeps
		// the CLEAN message (no display newline) for programmatic consumers.
		"content": map[string]any{"type": "text", "text": message + "\n"},
		"_meta": map[string]any{"reviewmesh": map[string]any{
			"eventType": eventType,
			"level":     level,
			"message":   message,
			"timestamp": timestamp,
		}},
	}
}

// promptResponse builds an ACP v1 PromptResponse: `{ stopReason, _meta: { reviewmesh } }`.
// stopReason MUST be an official StopReason (end_turn | cancelled | max_tokens |
// max_turn_requests | refusal); reviewmesh result details go under `_meta.reviewmesh`.
func promptResponse(stopReason string, reviewmeshMeta map[string]any) map[string]any {
	return map[string]any{
		"stopReason": stopReason,
		"_meta":      map[string]any{"reviewmesh": reviewmeshMeta},
	}
}

// notify writes a JSON-RPC notification, serialized with responses via s.wmu so a progress
// frame never interleaves with a concurrent response frame.
func (s *Server) notify(f Framer, method string, params any) {
	b, err := json.Marshal(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return
	}
	s.wmu.Lock()
	_ = f.WriteMessage(b)
	s.wmu.Unlock()
}

func okResp(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errResp(id json.RawMessage, code int, msg string, data ...any) *rpcResponse {
	e := &rpcError{Code: code, Message: msg}
	if len(data) > 0 {
		e.Data = data[0]
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: e}
}

func framingName(name string) string {
	if name == FramingContentLength {
		return FramingContentLength
	}
	return FramingNewline
}
