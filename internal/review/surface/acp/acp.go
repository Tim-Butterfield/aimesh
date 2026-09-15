// Package acp serves reviewmesh as an ACP (Agent Client Protocol) agent: JSON-RPC 2.0 over stdio.
// It parses requests and routes them to the review manager; it adds no review logic. Framing is
// pluggable: newline-delimited JSON (the default) or LSP-style Content-Length headers.
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

	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
	// Fingerprint is imported so this surface computes finding identity with the same function as the
	// write path; two implementations could name different findings for one selector.
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
	"github.com/Tim-Butterfield/aimesh/internal/review/version"
)

// agentProtocolVersion is the ACP protocol version this agent implements (integer, per ACP v1).
const agentProtocolVersion = 1

// DefaultTurnTimeout is the wall-clock budget for one prompt when none is configured. It matches
// the explore agent surfaces.
const DefaultTurnTimeout = 10 * time.Minute

// turnBudget returns the configured turn timeout, or DefaultTurnTimeout.
func (s *Server) turnBudget() time.Duration {
	if s.TurnTimeout > 0 {
		return s.TurnTimeout
	}
	return DefaultTurnTimeout
}

// JSON-RPC 2.0 error codes, plus one reviewmesh server-range code.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeReviewHalt     = -32000 // a review halt; data carries exitCode + haltClass
)

// Reviewer is the manager capability the ACP surface routes to. It is context-aware, so a cancel
// request cancels the in-flight run.
//
// RunContext runs a review cycle: a report turn, or a patch turn that decides for itself.
// ReadDecisionSetFor and Remediate apply a set an earlier report turn adjudicated. A write turn
// carrying fromRun never calls RunContext, so the applied set is the set the host inspected.
type Reviewer interface {
	RunContext(ctx context.Context, r run.Request) (review.RunOutcome, error)
	// ReadDecisionSetFor resolves a run handle for a run over workspace to the decision set that
	// run recorded, verifying the handle against the workspace's run-record locations rather than
	// trusting it as a path. It returns the set and the canonical run directory.
	ReadDecisionSetFor(workspace, handle string) (*run.StoredDecisionSet, string, error)
	// Remediate applies an already-adjudicated decision set through the governed write path shared
	// with the CLI and MCP. Nothing is reviewed again.
	Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error)
}

// Server is the ACP agent server.
type Server struct {
	Manager Reviewer
	Caps    review.SurfaceCaps
	Framing string // "" / "newline" (default) | "content-length"
	// DegradeWhenModeUnavailable selects what happens when a requested mode exceeds the effective
	// ceiling: degrade to the ceiling with a reason (true), or fail with a usage error (false).
	DegradeWhenModeUnavailable bool

	// Adapters are the adapters this agent was launched with (`--adapter`). A turn's panel may name
	// only these; empty means every review turn is refused until the operator names one.
	Adapters launchflags.Set
	// AllowWrites is the operator's `--allow-writes` grant. Without it aimesh never changes project
	// content: an `apply` turn is refused, and a `patch` turn supplies the complete diff for the agent
	// to apply itself.
	AllowWrites bool

	// TurnTimeout bounds one prompt's wall clock (0 means DefaultTurnTimeout). Without it a prompt
	// could keep a panel of model CLIs spending after the host stopped waiting. A timeout is a halt,
	// never a partial result.
	TurnTimeout time.Duration

	// ValidateHostAdjudication makes every review exercise the author_remediator seat, using a
	// synthetic adjudication probe when the reviewers yield no findings. It is set only by validation
	// harnesses; a normal client leaves it false, so no extra model call occurs.
	ValidateHostAdjudication bool

	// Ceiling is the operator's optional --root set: every path a turn declares must lie inside it.
	// Empty means no ceiling. Each turn's scope is built from the paths that turn declares.
	Ceiling []string

	// Sessions is the optional durable store behind ACP session/resume. When nil, resume is not
	// advertised and session/resume returns method-not-found. When set, session/new persists the
	// session's cwd and session/resume restores it; no history is kept.
	Sessions SessionStore
	// VerifyCommands, VerifyTimeout and VerifyBaseline configure bounded execution: the project's own
	// build and test commands, run on the containment copy and recorded. Empty means nothing runs.
	//
	// They are launch settings only. A caller that could name a command here would have arbitrary
	// code execution on the operator's machine.
	VerifyCommands []string
	VerifyTimeout  time.Duration
	VerifyBaseline bool
	// AllowProtectedPaths admits a workspace under a protected configuration path (.git, .claude,
	// .vscode, …) and permits writes there. Secrets stay refused. It is a launch setting because
	// those paths execute code on a later command.
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

// sessionState is the in-process record for a session created by session/new. CWD is the
// workspace the host supplied; a session/prompt that omits workspace uses it.
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
	// FromRun and Select are the two-phase write rule's parameters; this method can reach a governed
	// write, so it enforces the same rule. See requireRunHandle.
	FromRun string   `json:"fromRun,omitempty"`
	Select  []string `json:"select,omitempty"`
	// Meta carries the same _meta.reviewmesh extension session/prompt accepts, so this method can form
	// the same runs.
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// writeTurn is the two-phase write rule's inputs for one turn, bundled so every entry point that can
// reach a governed write passes the same thing to the same check.
type writeTurn struct {
	FromRun string
	Select  []string
}

// acceptedFingerprints projects a run's accepted findings to the identifiers a later write turn
// can select on: the host-computed fingerprint, the file, and the finding title.
//
// Acceptance is run.AcceptedForApply's answer, shared with the write path, so the set shown here
// matches the set a write would consider. Findings the write path quarantines are omitted.
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

// metaEnvelope is the ACP _meta extension point, narrowed to the reviewmesh namespace. Other
// _meta keys belong to the host and are ignored.
type metaEnvelope struct {
	Reviewmesh json.RawMessage `json:"reviewmesh"`
}

// reviewmeshMeta is the reviewmesh-namespaced run-forming input carried on a prompt.
type reviewmeshMeta struct {
	// Authority lists the documents this review is judged against, with the same schema and
	// fail-closed rules as the CLI's --authority.
	Authority []review.AuthorityDoc `json:"authority"`
	// Panel is the seats this turn composes. See panelMeta.
	Panel *panelMeta `json:"panel,omitempty"`
	// Roots are extra absolute directories this turn reads beside its workspace, such as a folder
	// holding authority documents. See turnScope.
	Roots []string `json:"roots,omitempty"`
	// MaxParallel bounds how many reviewer seats call their model CLI at once; zero runs the whole
	// panel in parallel. It is per turn because the right value depends on the caller's machine and
	// provider rate limits. Every seat still reviews.
	MaxParallel int `json:"maxParallel,omitempty"`
	// DryRun resolves the plan, panel, authority documents and preflight, then stops before the first
	// model call and returns the run's shape. The turn reports the mode it priced and writes nothing.
	DryRun bool `json:"dryRun,omitempty"`
	// VerifyReadiness spends one bounded one-token call per distinct adapter and model before the turn
	// dispatches, so a seat blocked on login or folder trust halts the turn before the panel spends.
	// A dry run prices these calls without making them.
	VerifyReadiness bool `json:"verifyReadiness,omitempty"`
}

// parseReviewmeshMeta extracts the reviewmesh namespace from an ACP _meta. An unknown field inside
// that namespace is an error, because silently dropping a mistyped governance field would weaken
// the run. Other _meta keys are ignored.
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

// validateSelection checks the panel's shape before any spend when a turn names one. Whether a
// panel is required, and whether its adapters were launched, is decided in resolvePanel.
func (rm reviewmeshMeta) validateSelection() (string, error) {
	if rm.Panel == nil {
		return "", nil
	}
	return rm.Panel.validate()
}

// Serve reads and writes through the configured framing until exit or EOF.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	return s.ServeFramed(NewFramer(s.Framing, in, out))
}

// ServeFramed runs the dispatch loop over f. Review requests run concurrently so a later cancel
// can reach them; other methods are handled inline. Frame writes are serialized.
func (s *Server) ServeFramed(f Framer) error {
	// baseCtx parents every in-flight review. cancelAll runs before wg.Wait (defers are LIFO), so
	// blocked reviews unblock and shutdown never hangs.
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
		// Only a missing id makes a request a notification; `id: null` still gets a response.
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
			// Register the cancel before reading the next message, so a following cancel for this id
			// finds the run.
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
				// Store the host's cwd so a later session/prompt that omits workspace can use it.
				var sn sessionNewParams
				if len(req.Params) > 0 {
					if err := json.Unmarshal(req.Params, &sn); err != nil {
						s.write(f, errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error()))
						continue
					}
				}
				if msg, bad := relativeCWD(sn.CWD); bad {
					s.write(f, errResp(req.ID, codeInvalidParams, msg, map[string]any{"reasonCode": ReasonCallPathRelative}))
					continue
				}
				sid := s.newSession(sn.CWD)
				s.persistSession(sid) // durable record for cross-restart resume (best-effort)
				s.write(f, okResp(req.ID, map[string]any{"sessionId": sid}))
			}
		case "session/resume":
			// Restore a session persisted by an earlier agent process, without replaying history.
			// Method-not-found when no store is configured.
			if !notif {
				s.write(f, s.handleSessionResume(req))
			}
		// session/load is not implemented and falls to method-not-found: its contract is to replay a
		// conversation, which a one-shot reviewer does not have. Reconnect uses session/resume.
		case "session/prompt":
			// The session id must come from session/new. The cancel is registered under it so a later
			// session/cancel reaches the run.
			var sp sessionPromptParams
			_ = json.Unmarshal(req.Params, &sp)
			if sp.SessionID == "" || !s.knownSession(sp.SessionID) {
				if !notif {
					s.write(f, errResp(req.ID, codeInvalidParams, "invalid params: unknown or missing sessionId (call session/new first)"))
				}
				continue
			}
			ctx, cancel := context.WithCancel(baseCtx)
			// Register the cancel under the session id even for a notification prompt, so it stays
			// cancellable. A second prompt for a session with a run in flight is rejected.
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
				// Release the session slot before writing the response. The response tells the host the turn
				// is over, and a host that sends its next turn immediately must not see "session busy".
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

// initializeParams carries the host's client capabilities. A host can only narrow reviewmesh's
// capabilities; omitted fields leave a capability unchanged.
type initializeParams struct {
	ProtocolVersion    json.RawMessage `json:"protocolVersion"`
	ClientCapabilities *struct {
		FS *struct {
			ReadTextFile  *bool `json:"readTextFile"`
			WriteTextFile *bool `json:"writeTextFile"`
		} `json:"fs"`
	} `json:"clientCapabilities"`
}

// initialize builds the ACP v1 initialize result: an integer protocolVersion, agentCapabilities,
// agentInfo and authMethods. session/load is not supported, prompts are text-only, and
// sessionCapabilities.resume is advertised only when a session store is configured.
func (s *Server) initialize(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var ip initializeParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &ip) // best-effort; protocolVersion validated below
	}
	// The client must send an integer protocol version at least the agent's; a higher version
	// negotiates down. Anything else is a protocol error.
	cv, ok := parseIntProtocolVersion(ip.ProtocolVersion)
	if !ok || cv < agentProtocolVersion {
		return errResp(id, codeInvalidParams, fmt.Sprintf(
			"unsupported or missing protocolVersion: this agent implements ACP protocol version %d (send an integer protocolVersion >= %d)",
			agentProtocolVersion, agentProtocolVersion))
	}
	// A host that cannot write files drops FileWrite, which gates apply (see modeCeiling). The
	// narrowing is internal and not echoed in the result.
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
		// session/load would require replaying conversation history, which a reviewer does not have.
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
	// Advertise resume only when a durable store is configured.
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
		// Disclose who performs writes before the first prompt: aimesh with --allow-writes, otherwise
		// the agent, which applies the diff a patch turn supplies.
		"_meta": map[string]any{"reviewmesh": s.withWrites(map[string]any{})},
	})
}

// parseIntProtocolVersion returns the integer protocol version in raw, or ok=false if it is
// missing, not a JSON number, or not an integer.
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

// modeCeiling returns the highest mode this connection permits. Apply needs the operator's
// --allow-writes grant and a host that can write files; patch changes no project content and is
// always permitted.
func (s *Server) modeCeiling() review.Mode {
	s.capMu.Lock()
	caps := s.Caps
	s.capMu.Unlock()
	if s.AllowWrites && caps.FileWrite {
		return review.ModeApply
	}
	return review.ModePatch
}

var acpModeRank = map[review.Mode]int{review.ModeReport: 0, review.ModePatch: 1, review.ModeApply: 2}

// minMode returns the lower-privilege of two known modes. An unknown mode is returned unchanged
// for the resolver to reject.
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

// sessionNewParams carries the workspace root the host supplies at session/new.
type sessionNewParams struct {
	CWD string `json:"cwd"`
}

// sessionPromptParams carries an ACP v1 session/prompt request.
type sessionPromptParams struct {
	SessionID string `json:"sessionId"`
	Workspace string `json:"workspace"`
	Mode      string `json:"mode"`
	// Prompt is the host's prompt content. It is accepted but not parsed for review scope.
	Prompt json.RawMessage `json:"prompt,omitempty"`
	// InlineWorkspace maps workspace-relative paths to file content the host provides inline. It is
	// materialized to an isolated temp workspace and reviewed by path.
	InlineWorkspace map[string]string `json:"inlineWorkspace,omitempty"`
	// Permissions is the host's per-request permission decision. A denial narrows the mode ceiling
	// for this prompt.
	Permissions *promptPermissions `json:"permissions,omitempty"`
	// FromRun is the run handle a write turn must carry: the runDir returned by an earlier report
	// turn. See requireRunHandle.
	FromRun string `json:"fromRun,omitempty"`
	// Select names, by host-computed fingerprint, which accepted findings a write turn applies.
	//
	// It must stay declared: json.Unmarshal drops unknown fields, so an undeclared select would
	// silently widen the write to every accepted finding.
	//
	// With fromRun it narrows the decision set that run recorded, so a fingerprint from the report
	// turn names the same finding. Without fromRun (a patch turn deciding for itself) it narrows that
	// turn's own adjudication. A selector matching nothing is reported in selection.unmatched.
	Select []string `json:"select,omitempty"`
	// Meta is the ACP _meta extension point; _meta.reviewmesh carries the turn's panel, roots and
	// authority documents.
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// requireRunHandle enforces the ACP half of the two-phase write rule.
//
// A write whose response is lost tells the caller nothing, and ACP has no method for looking up a
// prior run. So a prompt whose effective mode is apply must carry fromRun: the runDir returned in
// the response to the host's own earlier report turn. The host knows whether it received that
// response, so any write stays discoverable at a handle the host holds. The rule binds every entry
// point that can reach a governed write, including the compatibility review method.
//
// This function only checks that a handle is present, before any spend. fromRunWrite resolves it
// against the workspace's run records and applies the decision set that run recorded, so the
// applied set is the inspected set. A handle this agent did not produce is refused lexically with
// run_handle_unknown, so the check reveals nothing about arbitrary paths. The source run's
// workspace identity and base hashes are re-verified before writing; a tree that has changed halts
// with stale_decision_set.
//
// select narrows the stored set. A selector matching nothing lands in selection.unmatched, and a
// selection matching nothing at all refuses the turn.
func requireRunHandle(effective review.Mode, fromRun string, selection []string) (string, map[string]any, bool) {
	if effective != review.ModeApply {
		// A report turn writes nothing and a patch turn writes only into the run directory it creates,
		// so neither needs a handle. select on a report turn is refused because nothing would be
		// narrowed; on a patch turn it is honored.
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

// checkSelection refuses an empty select list rather than treating it as "apply everything".
// Matching rules belong to the governed write path, so select means the same on every surface.
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

// promptPermissions carries a host's per-request write and patch decisions. A nil pointer means
// unspecified; false means denied and narrows the ceiling.
type promptPermissions struct {
	AllowWrite *bool `json:"allowWrite"`
	AllowPatch *bool `json:"allowPatch"`
}

// materializeInline writes host-provided content to a new temp workspace and returns its path.
// Each path must be workspace-relative and not excluded; on any violation the temp directory is
// removed and an error returned. The caller removes the directory when done.
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
		// SafeRel is the shared path-safety check used by workspace access too.
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
	// The compatibility review method accepts the same selection meta as session/prompt.
	if reason, serr := rm.validateSelection(); serr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+serr.Error(), map[string]any{"reasonCode": reason})
	}
	// The compatibility review method has no session: no session updates and no per-request
	// permission cap.
	return s.runAndRespond(ctx, req, notif, p.Workspace, false, p.Mode, "", "", rm,
		writeTurn{FromRun: p.FromRun, Select: p.Select}, f)
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
	// Inline content is materialized to a temp workspace and reviewed by path. ownedWorkspace marks
	// that this process created the directory, so it consumes no declared scope.
	ownedWorkspace := false
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
	// An explicit workspace wins; otherwise the session cwd. Nothing is inferred from this process's
	// working directory.
	if p.Workspace == "" {
		p.Workspace = s.sessionCWD(sessionID)
	}
	// An omitted session/prompt mode defaults to the safest mode, `report` (read-only).
	if p.Mode == "" {
		p.Mode = "report"
	}
	// A host permission denial for this prompt narrows the ceiling: deny-write caps at patch,
	// deny-patch caps at report.
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
	// Validate the selection before the run, so a bad panel costs no spend.
	if reason, serr := rm.validateSelection(); serr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+serr.Error(), map[string]any{"reasonCode": reason})
	}
	return s.runAndRespond(ctx, req, notif, p.Workspace, ownedWorkspace, p.Mode, sessionID, permCeiling, rm,
		writeTurn{FromRun: p.FromRun, Select: p.Select}, f)
}

// runAndRespond runs a review for workspace and mode and formats the ACP response for both
// session/prompt and the compatibility review method. With a sessionID it streams session/update
// progress notifications.
//
// ownedWorkspace marks a workspace this process created (a materialized inlineWorkspace); it
// consumes no declared scope.
func (s *Server) runAndRespond(ctx context.Context, req rpcRequest, notif bool, workspace string, ownedWorkspace bool, mode, sessionID string, ceilingCap review.Mode, sel reviewmeshMeta, wt writeTurn, f Framer) *rpcResponse {
	authDocs := sel.Authority
	if workspace == "" {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: workspace is required")
	}
	// Build the turn's scope before any spend: its workspace and extra roots are the only
	// directories it may read. The read denylist still applies inside them.
	turn, scopeReason, serr := s.turnScope(workspace, ownedWorkspace, sel.Roots)
	if serr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+serr.Error()+scopeHint(scopeReason),
			map[string]any{"reasonCode": scopeReason})
	}
	// Cap the requested mode to the effective ceiling. A request above it degrades with a reason
	// when DegradeWhenModeUnavailable is set, and otherwise fails before any run.
	requested := review.Mode(mode)
	// The effective ceiling is the connection's, narrowed by any per-request permission cap. It is
	// never widened.
	ceiling := s.modeCeiling()
	if ceilingCap != "" {
		ceiling = minMode(ceilingCap, ceiling)
	}
	if requested == "" {
		// Naming a review is not consent to write.
		requested = review.ModeReport
	} else if _, ok := acpModeRank[requested]; !ok {
		// The resolver does not validate modes, so an unknown mode is rejected here.
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid mode: "+string(requested), map[string]any{"mode": string(requested)})
	}
	// An apply turn without --allow-writes is refused rather than degraded, so the host learns that
	// aimesh will not write and that a patch turn supplies the diff.
	if requested == review.ModeApply && !s.AllowWrites {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams,
			"invalid params: this agent was launched without --allow-writes, so aimesh does not change project content and `mode: \"apply\"` is refused. Send `mode: \"patch\"` to receive the complete diff and apply it yourself.",
			map[string]any{"reasonCode": ReasonWritesNotGranted, "requestedMode": string(requested)})
	}
	effective := minMode(requested, ceiling)
	degradeReason := ""
	if effective != requested {
		degradeReason = fmt.Sprintf("requested mode %q exceeds the effective ceiling %q (host capability, per-request permission)", requested, ceiling)
		if !s.DegradeWhenModeUnavailable {
			if notif {
				return nil
			}
			// hostCeiling keeps its wire name but carries the effective ceiling.
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
	// Check the two-phase write rule on the effective mode, before any work. A turn already degraded
	// below apply writes nothing and needs no handle.
	if msg, data, ok := requireRunHandle(effective, wt.FromRun, wt.Select); !ok {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+msg, data)
	}
	// Validate and resolve authority documents before any spend, so a malformed document, a
	// scope-denied path, a broken hash pin or an oversized document is an invalid-params error with a
	// reason code rather than a halt after models ran. The manager resolves them again for the run.
	if verr := authority.Validate(authDocs, effective); verr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid authority: "+verr.Error(),
			map[string]any{"reasonCode": fault.ReasonOf(verr)})
	}
	// A write turn carrying fromRun applies the decision set that run recorded and reviews nothing.
	// It is keyed on the effective mode: a turn degraded to report runs the read-only cycle, its
	// fromRun is inert, and the response reports the degraded mode.
	if strings.TrimSpace(wt.FromRun) != "" && (effective == review.ModePatch || effective == review.ModeApply) {
		return s.fromRunWrite(ctx, req, notif, workspace, ownedWorkspace, effective, requested,
			degradeReason, sessionID, sel, wt, turn, f)
	}
	// A review turn composes its own panel, resolved before any authority document is read.
	reviewers, roles, panelReason, perr := s.resolvePanel(sel.Panel)
	if perr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+perr.Error(), map[string]any{"reasonCode": panelReason})
	}
	// Authority paths are request paths, so they must lie inside this turn's scope.
	if _, rerr := authority.Resolve(authority.Input{Docs: authDocs, Mode: effective, Workspace: workspace, Trust: turn}); rerr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid authority: "+rerr.Error(),
			map[string]any{"reasonCode": fault.ReasonOf(rerr)})
	}
	// The request carries the scope this surface checked, so the manager resolves authority against
	// the same roots. Composed seats pass their model identifiers to adapters verbatim.
	rq := run.Request{Workspace: workspace, Mode: effective, Surface: "acp", Authority: authDocs,
		ReviewerPanel: reviewers, ComposedRoles: roles, MaxParallel: sel.MaxParallel,
		DryRun:          sel.DryRun,
		VerifyReadiness: sel.VerifyReadiness,
		// The selection passes unexamined to the governed write path, so select is one rule on every
		// surface.
		Select: wt.Select,
		// A materialized inline workspace is recorded as ephemeral, so a later fromRun turn naming this
		// run is refused by name.
		WorkspaceEphemeral: ownedWorkspace,
		// Bounded execution and containment waivers come from the operator's launch flags, never from
		// the prompt.
		VerifyCommands:      s.VerifyCommands,
		VerifyTimeout:       s.VerifyTimeout,
		VerifyBaseline:      s.VerifyBaseline,
		AllowProtectedPaths: s.AllowProtectedPaths,
		TrustedRoots:        turn.Roots(), ValidateHostAdjudication: s.ValidateHostAdjudication}
	if sessionID != "" {
		// Forward manager events as session/update notifications while the run context is live, so
		// nothing is emitted after cancellation or the final response.
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
	// The turn budget wraps the request context, so a timeout cancels the run the same way a host
	// cancel does.
	runCtx, runCancel := context.WithTimeout(ctx, s.turnBudget())
	defer runCancel()
	out, err := s.Manager.RunContext(runCtx, rq)
	if notif {
		return nil // notification: ran for side effects, no response
	}
	// session/prompt returns an ACP v1 PromptResponse; the compatibility review method keeps the flat
	// reviewmesh shape.
	isSession := sessionID != ""
	if errors.Is(err, context.Canceled) {
		rm := s.withWrites(map[string]any{"status": "cancelled", "runDir": out.RunDir})
		if isSession {
			return okResp(req.ID, promptResponse("cancelled", rm))
		}
		return okResp(req.ID, rm)
	}
	if err != nil {
		// A halt is a JSON-RPC error on both paths, not a completed turn.
		halt := ""
		if out.Halt != nil {
			halt = string(*out.Halt)
		}
		// The halt carries machine-readable reason and signal values, matching halt-record.json.
		data := map[string]any{
			"exitCode": int(fault.CodeOf(err)), "haltClass": halt, "runDir": out.RunDir,
			"reasonCode": fault.ReasonOf(err),
		}
		if sig := fault.SignalOf(err); sig != "" {
			data["signal"] = sig
		}
		// Include the failing seat's role, adapter, model, exit code and a raw stderr excerpt, so a host
		// can explain why it failed. Consumers must sanitize and cap the excerpt.
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
	rm := s.withWrites(map[string]any{
		"status":   out.Status,
		"mode":     string(out.Mode),
		"findings": len(out.Findings),
		"runDir":   out.RunDir,
	})
	// Each accepted finding's fingerprint rides the report turn's response: it is the only way a host
	// learns values to select on, since ACP has no lookup method.
	//
	// The key is the host-computed fingerprint, never the model-authored finding ID, which a model
	// could relabel to change what a selection names.
	if accepted := acceptedFingerprints(out); len(accepted) > 0 {
		rm["accepted"] = accepted
	}
	// Citation grounding, from the same derivation the CLI and MCP use. Its note states what
	// grounding does not establish.
	if out.Grounding != nil {
		rm["grounding"] = out.Grounding
		if rows := run.GroundingRows(out); len(rows) > 0 {
			rm["groundingFindings"] = rows
		}
	}
	// Bounded execution results, when the operator supplied commands. The note states that no outcome
	// is a verdict on any finding.
	if out.Verification != nil {
		rm["verification"] = out.Verification
	}
	// Panel composition qualifies the agreement counts: agreement from duplicated models is weaker
	// than it looks.
	if out.Composition != nil {
		rm["composition"] = out.Composition
	}
	// Dissent, with a note: a blind seat's silence is not a vote against a finding.
	if out.Dissent != nil {
		rm["dissent"] = out.Dissent
	}
	// A partial panel is disclosed whenever a capacity failure cost this run a seat.
	if out.PartialPanel != nil {
		rm["partialPanel"] = out.PartialPanel
	}
	if out.Scope != nil {
		rm["scope"] = out.Scope
	}
	// A dry run's shape; without it the response would read like a review that found nothing.
	if out.Shape != nil {
		rm["shape"] = out.Shape
	}
	if out.Mode == review.ModePatch || out.Mode == review.ModeApply {
		rm["patchArtifact"] = out.RunDir + "/patches/changes.patch"
		// Every write turn reports its outcome and counts. applied sits beside refused because a partial
		// refusal sets stopReason to refusal, which alone could read as nothing written.
		rm["outcome"] = review.ApplyOutcome(out.Applied, len(out.Refusals))
		rm["applied"] = out.Applied
		rm["refused"] = len(out.Refusals)
		// The selection on a full-cycle write turn (no fromRun), where it narrows this run's own
		// adjudication. A fromRun turn narrows the stored set in fromrun.go.
		if out.Selection.Selective() {
			rm["selection"] = selectionPayload(out.Selection)
		}
	}
	// Protected-path refusals, keyed by host-computed fingerprint. Nothing was written for these; the
	// other accepted findings were applied.
	if len(out.Refusals) > 0 {
		refusals := make([]map[string]any, 0, len(out.Refusals))
		for _, r := range out.Refusals {
			refusals = append(refusals, map[string]any{
				"fingerprint": r.Fingerprint, "file": r.File, "reason": r.Reason,
			})
		}
		rm["refusals"] = refusals
	}
	// Identity caveats for seats that ran and returned valid output. Only structured identifiers are
	// included.
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
	// Files a containment rule kept out of the review, so a host can tell "not shown" from "no
	// problems". Paths are workspace-relative.
	if len(out.Withheld) > 0 {
		withheld := make([]map[string]any, 0, len(out.Withheld))
		for _, w := range out.Withheld {
			withheld = append(withheld, map[string]any{
				"path": w.Path, "reason": w.Reason, "rule": w.Rule, "detail": w.Detail, "stage": w.Stage,
			})
		}
		rm["withheld"] = withheld
	}
	// The executed panel roster: one entry per requested seat, with what ran and how it ended.
	if len(out.Panel) > 0 {
		rm["panel"] = out.Panel
	}
	// The authority inclusion manifest: per document its source, hashes, byte counts and whether it
	// was included in full.
	if len(out.Authority) > 0 {
		rm["authority"] = out.Authority
	}
	if degradeReason != "" {
		rm["modeDegraded"] = true
		rm["requestedMode"] = string(requested)
		rm["modeReason"] = degradeReason
	}
	if isSession {
		// A completed turn ends with end_turn, except a partial refusal, which ends with refusal so a host
		// reading stopReason alone does not treat it as clean. It is not an error response, because the
		// write was committed; _meta.reviewmesh.applied reports what was written.
		if len(out.Refusals) > 0 {
			return okResp(req.ID, promptResponse("refusal", rm))
		}
		return okResp(req.ID, promptResponse("end_turn", rm))
	}
	return okResp(req.ID, rm)
}

// newSession creates a session recording the host-supplied cwd and returns its id. With a durable
// store, ids are random tokens so they cannot collide across restarts; without one they are the
// in-process sequence s-NNNN.
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

// randomToken returns 16 hex characters from the system RNG, or a nanosecond token if the RNG is
// unavailable.
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

// sessionCWD returns the stored cwd for a session id, or "" if none.
func (s *Server) sessionCWD(id string) string {
	s.smu.Lock()
	defer s.smu.Unlock()
	if st := s.known[id]; st != nil {
		return st.CWD
	}
	return ""
}

// persistSession writes the durable record for a session created by session/new. A store failure
// is ignored; the session still works in-process. It does nothing without a store.
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

// sessionResumeParams is the ACP v1 ResumeSessionRequest. sessionId is required and cwd optional;
// other fields are accepted and ignored.
type sessionResumeParams struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
}

// handleSessionResume implements ACP v1 session/resume: it restores a session persisted by an
// earlier agent process, without replaying history, and returns an empty response. It returns
// method-not-found when no store is configured and invalid params for a missing id or an unknown,
// malformed or expired record.
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
	// A cwd supplied on reconnect takes precedence over the persisted one.
	if msg, bad := relativeCWD(p.CWD); bad {
		return errResp(req.ID, codeInvalidParams, msg, map[string]any{"reasonCode": ReasonCallPathRelative})
	}
	cwd := rec.CWD
	if c := cleanSessionCWD(p.CWD); c != "" {
		cwd = c
	}
	s.restoreSession(p.SessionID, cwd)
	// Refresh updatedAt so an active session does not expire.
	rec.CWD = cwd
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	_ = s.Sessions.Save(rec)
	return okResp(req.ID, map[string]any{})
}

// restoreSession re-registers a resumed session with its cwd and advances the session counter past
// a resumed s-NNNN id, so session/new cannot mint a colliding id.
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

// parseSessionNum returns N from an "s-NNNN" id, or 0 if id does not match.
func parseSessionNum(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "s-%d", &n); err != nil {
		return 0
	}
	return n
}

// relativeCWD reports whether a host-supplied cwd is present but not absolute, with the refusal
// message. The agent shares no working directory with its host, so a relative cwd has no meaning.
func relativeCWD(raw string) (string, bool) {
	c := strings.TrimSpace(raw)
	if c == "" || filepath.IsAbs(c) {
		return "", false
	}
	return fmt.Sprintf("invalid params: cwd %q is not an absolute path; send the absolute workspace directory", raw), true
}

// cleanSessionCWD trims and cleans a host-supplied cwd. A relative cwd is kept as sent, never resolved
// against this process's working directory. It returns "" for empty input.
func cleanSessionCWD(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return filepath.Clean(raw)
}

// putSession registers the in-flight run for a session id. It returns false if the session already
// has an active run, so a concurrent prompt is rejected rather than orphaning the first run.
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

// rpcNotification is a JSON-RPC notification, such as a session/update progress event.
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// sessionUpdateParams is the ACP v1 session/update params: sessionId and a SessionUpdate object
// keyed by its sessionUpdate discriminator.
type sessionUpdateParams struct {
	SessionID string         `json:"sessionId"`
	Update    map[string]any `json:"update"`
}

// acpProgressUpdate builds an ACP v1 SessionUpdate for a progress event. ACP has no progress
// variant, so the text is sent as agent_message_chunk and the structured event fields go under
// _meta.reviewmesh.
func acpProgressUpdate(eventType, level, message, timestamp string) map[string]any {
	return map[string]any{
		"sessionUpdate": "agent_message_chunk",
		// The display text ends in a newline so hosts that concatenate chunks show one line per event;
		// _meta.reviewmesh.message keeps the message without it.
		"content": map[string]any{"type": "text", "text": message + "\n"},
		"_meta": map[string]any{"reviewmesh": map[string]any{
			"eventType": eventType,
			"level":     level,
			"message":   message,
			"timestamp": timestamp,
		}},
	}
}

// promptResponse builds an ACP v1 PromptResponse. stopReason must be an official ACP stop reason;
// reviewmesh details go under _meta.reviewmesh.
func promptResponse(stopReason string, reviewmeshMeta map[string]any) map[string]any {
	return map[string]any{
		"stopReason": stopReason,
		"_meta":      map[string]any{"reviewmesh": reviewmeshMeta},
	}
}

// notify writes a JSON-RPC notification, serialized with responses so frames never interleave.
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
