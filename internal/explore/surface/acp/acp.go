// Package acp is exploremesh's ACP (Agent Client Protocol) surface: exploremesh runs as a local agent
// subprocess speaking JSON-RPC 2.0 over stdio, so another tool can drive ONE exploration over ACP. It is
// host-neutral and thin — it parses requests and routes them to the exploration pipeline through the
// existing architecture; it adds no exploration logic. Transport framing is pluggable (newline-delimited
// JSON, the ACP/Zed default, and LSP-style Content-Length headers), reused from meshcore/acp.
//
// Unlike reviewmesh's ACP surface there is NO write-gating: exploremesh reviews nothing on disk — the
// pipeline runs every model call in a fresh isolated work dir — so there is no workspace, no mode, no
// permission ceiling, and no capability narrowing. The one exploremesh-specific convention is
// criteria-via-_meta: on `session/prompt` the ACP text prompt becomes the task PURPOSE and the load-
// bearing CRITERIA arrive under `_meta.exploremesh.criteria` (absent/blank → invalid-params, never
// silently invented). The same `_meta.exploremesh` carries the REQUIRED `panel` — the explorers and the
// collator this turn composes from the adapters the agent was launched with (`--adapter`). The applied task
// AND the panel that ran are echoed back in the PromptResponse `_meta.exploremesh`.
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
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/capture"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/runview"
	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
)

// agentProtocolVersion is the ACP protocol version this agent implements (integer, per ACP v1).
const agentProtocolVersion = 1

// agentVersion labels this agent in the ACP `initialize` agentInfo. exploremesh ships no release-version
// package yet, so a stable "dev" label is honest (verify-on-provision against a real host build).
const agentVersion = "dev"

// JSON-RPC 2.0 error codes (standard + one exploremesh range code).
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeExploreHalt    = -32000 // an exploration halt; data carries exitCode + haltClass + failure
)

// v1 budget caps. The frame cap (16 MiB) comes free from meshcore/acp; these bound the
// exploration itself before any model subprocess is spawned.
const (
	// maxRosterExplorers bounds how many explorers a single ACP turn may fan out to — a hostile/huge
	// roster must not spawn an unbounded number of heavy model-CLI subprocesses.
	maxRosterExplorers = 16
	// maxPromptParamsBytes bounds the session/prompt params payload (prompt text + _meta) before dispatch.
	maxPromptParamsBytes = 1 << 20 // 1 MiB
	// defaultTurnTimeout is the total-turn wall-clock budget applied when the server has no explicit
	// TurnTimeout wired (the CLI sets one via --turn-timeout).
	defaultTurnTimeout = 10 * time.Minute
)

// Explorer is the capability the ACP surface routes an exploration to (Client → pipeline). It is
// context-aware so an ACP `cancel`/`session/cancel` cancels the in-flight run, and takes an OnEvent hook
// so the surface can stream progress as `session/update` notifications. The default implementation wires
// registry.Build + pipeline.Run (see NewPipelineExplorer); tests substitute a fake.
type Explorer interface {
	Run(ctx context.Context, plan roster.Plan, raw schema.RawTask, opts pipeline.Options, onEvent func(audit.EventLine)) (pipeline.Result, error)
}

// pipelineExplorer is the default Explorer: it runs the exploremesh pipeline over a pre-built registry.
// The registry is resolved once at launch from the adapters the agent was launched with; each prompt
// supplies the task and the panel it composes from them.
type pipelineExplorer struct {
	reg pipeline.Registry
}

// NewPipelineExplorer builds the default Explorer from a resolved registry.
func NewPipelineExplorer(reg pipeline.Registry) Explorer {
	return pipelineExplorer{reg: reg}
}

func (p pipelineExplorer) Run(ctx context.Context, plan roster.Plan, raw schema.RawTask, opts pipeline.Options, onEvent func(audit.EventLine)) (pipeline.Result, error) {
	return pipeline.Run(ctx, p.reg, plan, raw, opts, onEvent)
}

// Server is the exploremesh ACP agent server.
type Server struct {
	// Explorer routes an exploration to the pipeline.
	Explorer Explorer
	// Adapters are the adapters this agent was launched with (`--adapter`). A prompt's panel may name only
	// these, and only while the adapter's CLI can be started; it can never introduce one. EMPTY means every
	// prompt is refused until the operator names an adapter.
	Adapters launchflags.Set
	Framing  string // "" / "newline" (default) | "content-length"
	// TurnTimeout is the total-turn wall-clock budget for one exploration (0 → defaultTurnTimeout).
	TurnTimeout time.Duration
	// Sessions is the OPTIONAL durable session store enabling ACP v1 `session/resume` across an agent
	// process restart. When nil, resume is unsupported: `sessionCapabilities.resume` is NOT advertised and
	// `session/resume` returns method-not-found. When set, session/new persists minimal session metadata
	// (the cwd) and session/resume restores it — no history.
	Sessions SessionStore
	wmu      sync.Mutex // serializes frame writes across goroutines

	smu      sync.Mutex                    // guards known + sessions + sessionN
	known    map[string]*sessionState      // session ids created by session/new → per-session state
	sessions map[string]context.CancelFunc // ACP session id → in-flight run cancel
	sessionN int                           // session id counter
}

// sessionState is the minimal in-process record for a session created by session/new. CWD is the
// workspace root the host supplied (Zed sends it as session/new.params.cwd). exploremesh reviews nothing
// on disk, so CWD is stored only for the session/resume record round-trip; it is never a run root.
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

// Serve reads/writes through the configured framing until `exit` or EOF.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	return s.ServeFramed(NewFramer(s.Framing, in, out))
}

// ServeFramed runs the dispatch loop over an explicit Framer. `session/prompt` requests run concurrently
// (so a later `cancel` can reach an in-flight run); all other methods are handled inline. Frame writes are
// serialized. It never panics on bad input.
func (s *Server) ServeFramed(f Framer) error {
	// baseCtx parents every in-flight exploration; on any return (EOF, transport error, `exit`) cancelAll
	// fires BEFORE wg.Wait (LIFO defers), so blocked runs unblock and the server never hangs on shutdown.
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
		// Only the ABSENCE of an `id` member makes a request a notification (→ no response). `id: null`
		// is a valid identifier and still receives a response.
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
		case "session/new":
			if !notif {
				// Zed supplies the workspace root in params.cwd; store it so a later session/resume can
				// re-register the handle (exploremesh never runs a model call in it).
				var sn sessionNewParams
				_ = json.Unmarshal(req.Params, &sn)
				sid := s.newSession(sn.CWD)
				s.persistSession(sid) // durable record for cross-restart resume (best-effort)
				s.write(f, okResp(req.ID, map[string]any{"sessionId": sid}))
			}
		case "session/resume":
			// ACP v1 reconnect-without-history: restore a session persisted by an EARLIER agent process
			// from the durable store (no conversation replay). Method-not-found when no store is configured.
			if !notif {
				s.write(f, s.handleSessionResume(req))
			}
		// session/load is intentionally NOT implemented → method-not-found (default case). Its ACP contract
		// is to replay the full prior conversation, which a stateless one-shot explorer has none of.
		case "session/prompt":
			// An exploration request. The sessionId MUST have been created by session/new. Register cancel
			// under it (so a later session/cancel reaches it), then run in a goroutine.
			var sp struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(req.Params, &sp)
			if sp.SessionID == "" || !s.knownSession(sp.SessionID) {
				if !notif {
					s.write(f, errResp(req.ID, codeInvalidParams, "invalid params: unknown or missing sessionId (call session/new first)"))
				}
				continue
			}
			ctx, cancel := context.WithCancel(baseCtx)
			// Register the cancel UNCONDITIONALLY under the session id — session/cancel keys on sessionId
			// (always present). Reject a concurrent prompt for a session that already has an in-flight run
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
				// Release the session single-flight BEFORE writing the terminal response. A client that
				// receives the response may immediately send its next prompt on the same session; if the
				// in-flight marker were still held (a deferred delSession runs only AFTER the write), that
				// next prompt races into a spurious "session busy" (-32600). The run is already complete
				// here, so the registered cancel is moot — releasing first is safe.
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

// initializeParams carries the host-advertised protocol version. exploremesh narrows no capability from
// the host (it has no write surface to gate), so clientCapabilities are accepted but unused.
type initializeParams struct {
	ProtocolVersion json.RawMessage `json:"protocolVersion"`
}

// initialize builds the ACP v1 `initialize` result (or a protocol error). The result uses the ACP v1
// schema — an INTEGER protocolVersion, agentCapabilities, agentInfo, authMethods. It is deliberately
// minimal + honest: conversation `session/load` is NOT supported (its history-replay contract has no
// exploremesh equivalent), prompts are text-only, and there is no MCP. Durable reconnect is served by
// `session/resume`, advertised via `sessionCapabilities.resume` ONLY when a session store is configured.
func (s *Server) initialize(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var ip initializeParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &ip) // best-effort; protocolVersion validated below
	}
	// Protocol-version negotiation. The client must send an INTEGER >= the agent's version; a higher
	// client version negotiates DOWN to the agent's supported version. A missing, non-numeric (e.g. the
	// string "0.1"), non-integer (e.g. 0.1), or too-low value is a deterministic protocol error.
	cv, ok := parseIntProtocolVersion(ip.ProtocolVersion)
	if !ok || cv < agentProtocolVersion {
		return errResp(id, codeInvalidParams, fmt.Sprintf(
			"unsupported or missing protocolVersion: this agent implements ACP protocol version %d (send an integer protocolVersion >= %d)",
			agentProtocolVersion, agentProtocolVersion))
	}
	agentCaps := map[string]any{
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
	// Advertise sessionCapabilities.resume ONLY when a durable store is wired — resume reconnects WITHOUT
	// replaying history (the honest fit for a stateless explorer). With no store, the capability is absent
	// and session/resume is method-not-found.
	if s.Sessions != nil {
		agentCaps["sessionCapabilities"] = map[string]any{"resume": map[string]any{}}
	}
	return okResp(id, map[string]any{
		"protocolVersion": agentProtocolVersion,
		"agentInfo": map[string]any{
			"name":    "exploremesh",
			"version": agentVersion,
		},
		"agentCapabilities": agentCaps,
		"authMethods":       []any{},
	})
}

// parseIntProtocolVersion returns the integer protocol version from a raw JSON value, or ok=false if it
// is missing, not a JSON number, or not an integer (e.g. the string "0.1" or the number 0.1).
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

// sessionNewParams carries the workspace root the host supplies at session/new. Other fields are ignored.
type sessionNewParams struct {
	CWD string `json:"cwd"`
}

// sessionPromptParams is the ACP v1 `session/prompt` params for exploremesh: the protocol session id, the
// host's prompt content, and the exploremesh `_meta` carrying the load-bearing criteria (criteria-via-_meta).
type sessionPromptParams struct {
	SessionID string          `json:"sessionId"`
	Prompt    json.RawMessage `json:"prompt,omitempty"`
	Meta      *promptMeta     `json:"_meta,omitempty"`
}

// promptMeta is the sanctioned `_meta` extension point; exploremesh reads only its own `exploremesh` key,
// and decodes it strictly (see decodeExploreMeta).
type promptMeta struct {
	Exploremesh json.RawMessage `json:"exploremesh,omitempty"`
}

// decodeExploreMeta decodes `_meta.exploremesh` STRICTLY: an unknown key inside exploremesh's own namespace
// is invalid-params rather than a silently ignored field, because a mistyped run-forming field that is
// quietly dropped would run something the driver did not ask for. Keys elsewhere in `_meta` belong to the
// host and are ignored.
func decodeExploreMeta(p sessionPromptParams) (*exploreMetaIn, error) {
	if p.Meta == nil {
		return nil, nil
	}
	raw := bytes.TrimSpace(p.Meta.Exploremesh)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var em exploreMetaIn
	if err := dec.Decode(&em); err != nil {
		return nil, fmt.Errorf("_meta.exploremesh is malformed: %w", err)
	}
	return &em, nil
}

// exploreMetaIn is the incoming `_meta.exploremesh` shape. criteria are REQUIRED (absent/blank →
// invalid-params); priorContext is optional; purpose, when present, OVERRIDES the prompt text (documented
// so a driver can supply an exact purpose independent of the human prompt). mode selects the app-owned
// exploration mode (absent/blank → the default map; an unknown mode → invalid-params). panel is REQUIRED.
type exploreMetaIn struct {
	Criteria     []string `json:"criteria,omitempty"`
	PriorContext string   `json:"priorContext,omitempty"`
	Purpose      string   `json:"purpose,omitempty"`
	Mode         string   `json:"mode,omitempty"`
	// Artifact is the ARTIFACT UNDER REVIEW — the thing the panel attacks, supplied
	// inline by the driver. It is optional at the protocol level and REQUIRED by whichever modes say so
	// (mode.ValidateTask); a mode that needs it and does not get it is invalid-params, never a run that
	// reviews nothing.
	Artifact string `json:"artifact,omitempty"`
	// The FIXED-SPACE declarations: the option set / criteria /
	// estimation target the requester fixes BEFORE any explorer speaks. Like `artifact` they are optional at
	// the protocol level and REQUIRED by whichever mode says so (mode.ValidateTask) — a mode that needs one
	// and does not get it is invalid-params, never a run over a space nobody declared.
	Options         []string                  `json:"options,omitempty"`
	CompareCriteria []schema.CompareCriterion `json:"compareCriteria,omitempty"`
	Target          string                    `json:"target,omitempty"`
	Unit            string                    `json:"unit,omitempty"`
	Horizon         string                    `json:"horizon,omitempty"`
	// ConditioningEvent is the optional "assume this holds" clause of a forecast.
	ConditioningEvent string `json:"conditioningEvent,omitempty"`
	// Panel is the REQUIRED panel composition: 2..16 explorers plus a collator, each named by (adapter,
	// model, effort) IDENTIFIER. It is the ACP analogue of the CLI's repeatable --explorer/--collator and
	// MCP's `panel` parameter. Every adapter must be one the agent was launched with, so a prompt can never
	// introduce an adapter, a binary path or a launch argument. Model identifiers pass to the adapter's CLI
	// verbatim.
	Panel *panelSpec `json:"panel,omitempty"`
	// Canonicalizers names the two identities that propose the canonicalization — the ACP
	// analogue of the CLI's repeatable `--canonicalizer` and MCP's `canonicalizers` argument. EITHER absent
	// (the host derives them: slot a from the collator, slot b from the first explorer in panel order that
	// differs from it) or EXACTLY TWO; one entry is invalid-params, because it does not say which slot it
	// fills. Every adapter must be one the agent was launched with.
	Canonicalizers []slotSpec `json:"canonicalizers,omitempty"`
	// DumpRun opts this turn into the run-directory CAPTURE the CLI's --dump-run performs (envelopes,
	// raw outputs, prompts, the versioned manifest) under $EXPLOREMESH_ARTIFACT_DIR — the audit record every
	// governance claim is checkable against.
	DumpRun bool `json:"dumpRun,omitempty"`
	// MaxParallel bounds how many of this turn's explorers invoke their model CLI AT ONCE. Absent,
	// the whole panel runs in parallel. It is per-turn rather than a server flag for the same reason
	// it is per-call on MCP: the right number is a property of the machine the CLIs run on and of the
	// caller's provider rate limits, neither of which this agent can see. It bounds parallelism only
	// — every explorer still answers.
	MaxParallel int `json:"maxParallel,omitempty"`
	// DryRun resolves everything and spends nothing (surface parity with the CLI's --dry-run and MCP's
	// `dryRun`): the turn ends with status `planned` and a `shape` echo — the panel, every stage that would
	// be called, the exact model-call total, and the exact round-1 prompt — having stopped before the
	// identity pre-flight, which is an exploration's first model call. It is mutually exclusive with
	// dumpRun: a dry run performs no exploration, so there is nothing to record.
	DryRun bool `json:"dryRun,omitempty"`
	// VerifyReadiness asks every distinct panel identity whether its CLI can do real work — one bounded
	// one-token call each — before the identity pre-flight, and halts the turn if any cannot. It SPENDS;
	// a dry run prices the probes and performs none.
	VerifyReadiness bool `json:"verifyReadiness,omitempty"`
}

// panelSpec is the ad-hoc `_meta.exploremesh.panel` shape: ordered explorers + the single collator.
type panelSpec struct {
	Explorers []slotSpec `json:"explorers,omitempty"`
	Collator  *slotSpec  `json:"collator,omitempty"`
}

// slotSpec names ONE seat by identifier. There is deliberately no path/args/binary field: those are
// configuration, and configuration is human-only.
type slotSpec struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// panel is the exploration panel one prompt composed: the executable plan the pipeline runs.
type panel struct {
	plan roster.Plan
}

// panelExample is the corrected shape a refusal hands back, so the fix follows from the error.
const panelExample = `{"panel": {"explorers": [{"adapter": "<adapter>", "model": "<model>"}, {"adapter": "<adapter>", "model": "<model>"}], "collator": {"adapter": "<adapter>", "model": "<model>"}}}`

// resolvePanel resolves the REQUIRED `_meta.exploremesh.panel` into the plan a prompt runs, then applies
// the optional canonicalizer override. Every refusal is invalid-params (-32602), before any spend.
func (s *Server) resolvePanel(em *exploreMetaIn) (panel, *rpcError) {
	if em == nil || em.Panel == nil {
		return panel{}, &rpcError{Code: codeInvalidParams, Message: "invalid params: _meta.exploremesh.panel is required — every prompt composes its own explorers and collator from the adapters this agent was launched with. Corrected _meta.exploremesh: " + panelExample}
	}
	pan, perr := s.adHocPanel(em.Panel)
	if perr != nil {
		return panel{}, perr
	}
	// The CANONICALIZER override. It is resolved here, with the panel, because it is the same
	// kind of decision — which governed identities run — and both must be refused before the task is even
	// validated, so a bad governance spec costs nothing.
	if len(em.Canonicalizers) > 0 {
		cs, cerr := s.canonicalizerSlots(em.Canonicalizers)
		if cerr != nil {
			return panel{}, cerr
		}
		pan.plan.Canonicalizers = cs
	}
	return pan, nil
}

// canonicalizerSlots validates + converts `_meta.exploremesh.canonicalizers`. Each seat gets the same
// launch-set check a panel seat gets, and the 0-or-2 / distinct-identities rule is
// roster.ValidateCanonicalizers' — the one authority every surface and the profile schema share.
func (s *Server) canonicalizerSlots(specs []slotSpec) ([]roster.Explorer, *rpcError) {
	out := make([]roster.Explorer, 0, len(specs))
	for i, c := range specs {
		if cerr := s.checkConfigured(fmt.Sprintf("_meta.exploremesh.canonicalizers[%d]", i), c); cerr != nil {
			return nil, cerr
		}
		out = append(out, roster.Explorer{Adapter: strings.TrimSpace(c.Adapter), Model: strings.TrimSpace(c.Model), Effort: strings.TrimSpace(c.Effort)})
	}
	if err := roster.ValidateCanonicalizers(out); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: _meta.exploremesh." + err.Error()}
	}
	return out, nil
}

// adHocPanel builds the plan from a `_meta.exploremesh.panel` composition. Every failure is invalid-params
// with a field-addressed message: an under-sized panel, an over-sized one (the fan-out cap is a spend
// control, never clamped), a missing collator, a duplicate (adapter, model, effort) triple, or an adapter
// this agent was not launched with.
func (s *Server) adHocPanel(spec *panelSpec) (panel, *rpcError) {
	if len(spec.Explorers) < 2 {
		return panel{}, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
			"invalid params: _meta.exploremesh.panel.explorers needs at least 2 entries (got %d) — a panel with fewer than 2 explorers has nothing to be blind about", len(spec.Explorers))}
	}
	if len(spec.Explorers) > maxRosterExplorers {
		return panel{}, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
			"invalid params: _meta.exploremesh.panel.explorers has %d entries, exceeding the fan-out cap of %d", len(spec.Explorers), maxRosterExplorers)}
	}
	if spec.Collator == nil {
		return panel{}, &rpcError{Code: codeInvalidParams, Message: "invalid params: _meta.exploremesh.panel.collator is required — a panel must name the seat that collates it. Corrected _meta.exploremesh: " + panelExample}
	}
	var r roster.Roster
	for i, e := range spec.Explorers {
		if cerr := s.checkConfigured(fmt.Sprintf("_meta.exploremesh.panel.explorers[%d]", i), e); cerr != nil {
			return panel{}, cerr
		}
		r.Explorers = append(r.Explorers, roster.Explorer{Adapter: strings.TrimSpace(e.Adapter), Model: strings.TrimSpace(e.Model), Effort: strings.TrimSpace(e.Effort)})
	}
	if cerr := s.checkConfigured("_meta.exploremesh.panel.collator", *spec.Collator); cerr != nil {
		return panel{}, cerr
	}
	c := *spec.Collator
	r.Collator = roster.Collator{Adapter: strings.TrimSpace(c.Adapter), Model: strings.TrimSpace(c.Model), Effort: strings.TrimSpace(c.Effort)}
	plan, err := r.Plan() // enforces >=2 explorers, unique triples, non-empty adapter/model
	if err != nil {
		return panel{}, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	return panel{plan: plan}, nil
}

// checkConfigured enforces that one seat names a non-empty adapter + model, an adapter this agent was
// launched with, and one whose CLI can be started now. The refusal NAMES the launched set, so the corrected
// call is derivable from the error rather than guessable.
func (s *Server) checkConfigured(field string, slot slotSpec) *rpcError {
	adapter, model := strings.TrimSpace(slot.Adapter), strings.TrimSpace(slot.Model)
	if adapter == "" || model == "" {
		return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
			"invalid params: %s needs a non-empty adapter and model (got adapter=%q model=%q)", field, adapter, model)}
	}
	if s.Adapters.Empty() {
		return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
			"invalid params: %s cannot be resolved — this agent was launched with no adapter; the operator must add --adapter <name> (or %s) to the command that starts it", field, launchflags.EnvVar)}
	}
	if !s.Adapters.Has(adapter) {
		return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
			"invalid params: %s names adapter %q, which this agent was not launched with — available adapters are: %s (a prompt uses the adapters named at launch; it never introduces one)",
			field, adapter, strings.Join(s.Adapters.Names(), ", "))}
	}
	if ok, why := s.Adapters.Available(adapter); !ok {
		return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
			"invalid params: %s names adapter %q, whose CLI cannot be started right now: %s", field, adapter, why)}
	}
	return nil
}

func (s *Server) handleSessionPrompt(ctx context.Context, req rpcRequest, notif bool, f Framer, sessionID string) *rpcResponse {
	// v1 payload cap: reject an oversized params blob before parsing/dispatch.
	if len(req.Params) > maxPromptParamsBytes {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, fmt.Sprintf("invalid params: prompt payload exceeds %d bytes", maxPromptParamsBytes))
	}
	var p sessionPromptParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			if notif {
				return nil
			}
			return errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
		}
	}
	metaIn, merr := decodeExploreMeta(p)
	if merr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+merr.Error())
	}
	// -- the REQUIRED panel, resolved BEFORE the task is validated so a bad panel fails as cheaply as a bad
	// task.
	pan, perr := s.resolvePanel(metaIn)
	if perr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, perr.Code, perr.Message)
	}
	// v1 roster-size cap: a selected panel larger than the cap must not fan out over ACP.
	if len(pan.plan.Explorers) > maxRosterExplorers {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, fmt.Sprintf(
			"invalid params: roster has %d explorers, exceeding the ACP fan-out cap of %d", len(pan.plan.Explorers), maxRosterExplorers))
	}

	// -- criteria-via-_meta (the settled design decision) --
	// prompt text → purpose (the natural "what to explore"); _meta.exploremesh.{criteria,priorContext}
	// → the rest; a _meta.exploremesh.purpose override wins over the prompt text.
	raw := schema.RawTask{Purpose: promptText(p.Prompt)}
	if metaIn != nil {
		em := metaIn
		raw.Criteria = em.Criteria
		raw.PriorContext = em.PriorContext
		raw.Mode = strings.TrimSpace(em.Mode)
		raw.Artifact = em.Artifact
		// The FIXED-SPACE declarations, carried through verbatim: the mode's own ValidateTask below decides
		// whether they are sufficient, so this surface never guesses at a missing option set or unit.
		raw.Options = em.Options
		raw.CompareCriteria = em.CompareCriteria
		raw.Target = strings.TrimSpace(em.Target)
		raw.Unit = strings.TrimSpace(em.Unit)
		raw.Horizon = strings.TrimSpace(em.Horizon)
		raw.ConditioningEvent = strings.TrimSpace(em.ConditioningEvent)
		if strings.TrimSpace(em.Purpose) != "" {
			raw.Purpose = em.Purpose
		}
	}
	// v1 payload discipline: an oversized artifact is bounded by maxPromptParamsBytes above, which already
	// covers the whole params blob including this field.
	// ABSENT or all-whitespace criteria → invalid-params (-32602). NEVER silently invented.
	if countNonBlank(raw.Criteria) == 0 {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "exploremesh ACP requires _meta.exploremesh.criteria: a non-empty list of criteria")
	}
	// An explicit _meta.exploremesh.mode must be a known mode (empty → the default map). Reject an
	// unknown mode with invalid-params listing the known modes (never silently coerced).
	if raw.Mode != "" {
		if _, ok := mode.Lookup(raw.Mode); !ok {
			if notif {
				return nil
			}
			return errResp(req.ID, codeInvalidParams, fmt.Sprintf("invalid params: unknown mode %q (known modes: %s)", raw.Mode, strings.Join(mode.Names(), ", ")))
		}
	}
	if err := raw.Validate(); err != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	// The MODE's own task requirement: e.g. `challenge` needs `_meta.exploremesh.artifact`. It is
	// invalid-params — the same fail-closed posture criteria and the panel already take — so a driver is
	// told exactly what to supply instead of getting a run that reviewed nothing.
	if err := mode.ValidateTask(raw.Mode, raw); err != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	// dumpRun captures an EXPLORATION — envelopes, raw outputs, prompts, a manifest. A dry run produces none
	// of them, so the pair is refused rather than quietly writing a run directory that records an
	// exploration nobody performed. The CLI refuses `--dry-run --dump-run` for the same reason.
	if metaIn != nil && metaIn.DryRun && metaIn.DumpRun {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: dryRun cannot be combined with dumpRun — a dry run makes no model calls, so there are no envelopes, raw outputs or prompts to capture; the shape comes back in _meta.exploremesh.shape")
	}

	// Total-turn budget: wrap the exploration in a wall-clock timeout.
	turnTimeout := s.TurnTimeout
	if turnTimeout <= 0 {
		turnTimeout = defaultTurnTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()

	// In-process progress sink → session/update notifications. Gated on the run context so nothing is
	// emitted after cancellation (and not after the terminal response). Writes are serialized via notify.
	onEvent := func(ev audit.EventLine) {
		if runCtx.Err() != nil {
			return
		}
		s.notify(f, "session/update", sessionUpdateParams{
			SessionID: sessionID,
			Update:    acpProgressUpdate(ev.EventType, ev.Level, ev.Message, ev.Timestamp),
		})
	}

	maxParallel, dry, verify := 0, false, false
	if metaIn != nil {
		maxParallel, dry, verify = metaIn.MaxParallel, metaIn.DryRun, metaIn.VerifyReadiness
	}
	out, err := s.Explorer.Run(runCtx, pan.plan, raw, pipeline.Options{MaxParallel: maxParallel, DryRun: dry, VerifyReadiness: verify}, onEvent)
	if notif {
		return nil // notification: ran for side effects, no response
	}
	// Opt-in run CAPTURE (`_meta.exploremesh.dumpRun`), on BOTH the success and the halt path — a halted
	// run is the fixture most worth keeping. The capture is best-effort: a failed dump degrades the record,
	// it does not fail the turn, and the echo says so rather than staying quiet about it.
	capturedID, captureErr := "", ""
	if metaIn != nil && metaIn.DumpRun {
		status, faultMsg := "complete", ""
		if err != nil {
			status, faultMsg = "halted", err.Error()
		}
		capturedID, captureErr = dumpRun(pan.plan, raw, out, status, faultMsg)
	}
	echo := func(status string) map[string]any {
		em := exploreMeta(raw, out, status, pan)
		if metaIn != nil && metaIn.DumpRun {
			// The run ID, not a host path: the record is at $EXPLOREMESH_ARTIFACT_DIR/<runId>, which the
			// operator can resolve and a driver has no business being told.
			em["runCaptured"] = capturedID != ""
			if capturedID != "" {
				em["runId"] = capturedID
			}
			if captureErr != "" {
				em["runCaptureError"] = captureErr
			}
		}
		return em
	}
	if errors.Is(err, context.Canceled) || (errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil) {
		return okResp(req.ID, promptResponse("cancelled", echo("cancelled")))
	}
	if err != nil {
		// A halt (or a turn-timeout not caused by an external cancel) is an error condition → JSON-RPC
		// error with exitCode + haltClass + a sanitizable failure breakdown from the pipeline result.
		data := map[string]any{
			"exitCode":  int(fault.CodeOf(err)),
			"haltClass": haltClassOf(err),
			"failure":   failureFromResult(raw, out, err),
		}
		if capturedID != "" {
			data["runId"] = capturedID
		}
		return errResp(req.ID, codeExploreHalt, "exploration halted: "+err.Error(), data)
	}
	// A DRY RUN also ends the turn cleanly, but it explored nothing — so it reports `planned` rather than
	// `complete`, the same word reviewmesh's dry run uses. A driver that read `complete` here would have an
	// echo with a zero-size panel and no governance block, and the only honest reading of that is "the run
	// happened and found nothing", which is the opposite of what occurred.
	if out.Shape != nil {
		return okResp(req.ID, promptResponse("end_turn", echo("planned")))
	}
	// A successful turn completes → StopReason "end_turn"; echo the applied task + result summary back in
	// `_meta.exploremesh` so the driver sees exactly what ran.
	return okResp(req.ID, promptResponse("end_turn", echo("complete")))
}

// dumpRun records a finished turn under $EXPLOREMESH_ARTIFACT_DIR through the SAME capture the CLI's
// --dump-run uses, and returns the run id (or the error text). Before this, an ACP-driven exploration
// left no disk record at all while a CLI one could — the record is what every governance claim is
// checkable against, so which surface asked for the run cannot decide whether it exists.
func dumpRun(plan roster.Plan, raw schema.RawTask, out pipeline.Result, status, faultMsg string) (string, string) {
	run, err := audit.NewRun(capture.ArtifactDir(), "", time.Now())
	if err != nil {
		return "", err.Error()
	}
	if derr := capture.Dump(capture.Input{Run: run, Plan: plan, Result: out, Status: status, Fault: faultMsg, Task: raw}); derr != nil {
		return "", derr.Error()
	}
	return run.ID, ""
}

// promptText extracts the human prompt text from the ACP prompt content. ACP hosts send an array of
// content blocks (`[{"type":"text","text":"…"}]`); a plain string is also accepted. Non-text blocks are
// ignored (prompts are text-only in this build).
func promptText(rawPrompt json.RawMessage) string {
	if len(rawPrompt) == 0 {
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(rawPrompt, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	var s string
	if json.Unmarshal(rawPrompt, &s) == nil {
		return s
	}
	return ""
}

// countNonBlank counts the entries that are non-empty after trimming.
func countNonBlank(ss []string) int {
	n := 0
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			n++
		}
	}
	return n
}

// exploreMeta builds the PromptResponse `_meta.exploremesh` echo: the applied purpose + the criteria list
// actually used, the PANEL that ran, plus a summary of the run outcome (formulation source, collator
// identity, panel size, findings). It NEVER echoes priorContext content (only whether it was supplied).
//
// The governance block and the per-mode detail come from internal/surface/runview — the SAME projection
// the MCP surface returns, so the two surfaces cannot drift on what a run reported (surface-parity
// invariant).
func exploreMeta(raw schema.RawTask, out pipeline.Result, status string, pan panel) map[string]any {
	em := map[string]any{
		"status":            status,
		"purpose":           raw.Purpose,
		"criteria":          raw.Criteria,
		"mode":              runview.EffectiveMode(raw.Mode),
		"priorContext":      raw.PriorContext != "",
		"formulationSource": string(out.Formulation.Source),
		"collatorStatus":    string(out.CollatorStatus),
		"panelSize":         len(out.Envelopes),
		"dropped":           len(out.Dropped),
	}
	if out.CollatorCaveat != "" {
		em["collatorCaveat"] = out.CollatorCaveat
	}
	em["artifactSupplied"] = strings.TrimSpace(raw.Artifact) != ""
	// The panel echo: the composition the prompt asked for, reflected back, so "requested" and "executed"
	// are comparable without the driver having to remember what it sent.
	em["panelSource"] = "adhoc"
	em["explorers"] = slotEcho(pan.plan.Explorers)
	em["collator"] = map[string]any{"adapter": pan.plan.Collator.Adapter, "model": pan.plan.Collator.Model, "effort": pan.plan.Collator.Effort}
	// The CANONICALIZER echo: which identities held the merge-agreement rule, and WHO CHOSE THEM.
	// `canonicalizerSource` is the run's own provenance when a canonicalizing mode ran; before that it is
	// what the prompt asked for.
	em["canonicalizers"] = slotEcho(pan.plan.Canonicalizers)
	if src := out.CanonicalizerProvenance; src != "" {
		em["canonicalizerSource"] = src
	} else if len(pan.plan.Canonicalizers) > 0 {
		em["canonicalizerSource"] = "explicit"
	} else {
		em["canonicalizerSource"] = "derived"
	}
	// And what the pair is WORTH: `shared_model` says both canonicalizers ran one model behind two
	// adapters, so a held merge — and every corroboration count computed over the partition — is weaker
	// evidence than the same count from two different models. Allowed, so reporting it is the safeguard.
	if out.CanonicalizerIndependence != "" {
		em["canonicalizerIndependence"] = out.CanonicalizerIndependence
	}
	// Whether the blind round-1 panel was COLD. It is echoed on every run the operator enabled memory for,
	// including one that found nothing to carry, because a driver comparing two runs needs to know which of
	// them started from a previous iteration's conclusions and which did not.
	if gov := runview.Governance(out); gov != nil {
		em["governance"] = gov
	}
	// The DRY RUN disclosure, when this turn was one. It is marked with an explicit `dryRun` flag as well as
	// the `shape` block, so a driver branching on either reaches the same conclusion.
	if sh := runview.Shape(out); sh != nil {
		em["dryRun"] = true
		em["shape"] = sh
	}
	// The terminal output is per-mode: echo the generic one-line summary always, plus each mode's
	// own structured detail so a driver sees exactly what the collate step produced.
	maps.Copy(em, runview.ModeDetail(out))
	return em
}

// slotEcho renders the executed explorer slots as (adapter, model, effort) triples.
func slotEcho(explorers []roster.Explorer) []map[string]any {
	out := make([]map[string]any, 0, len(explorers))
	for _, e := range explorers {
		out = append(out, map[string]any{"adapter": e.Adapter, "model": e.Model, "effort": e.Effort})
	}
	return out
}

// failureFromResult builds the structured `data.failure` for a halt (the exploremesh analogue of
// reviewmesh's per-lane failure), shared with the MCP surface through runview. The web ACP validator
// sanitizes + caps every field before display.
func failureFromResult(raw schema.RawTask, out pipeline.Result, err error) map[string]any {
	return runview.Failure(raw, out, err)
}

// haltClassOf derives a short halt-class label from a pipeline halt.
func haltClassOf(err error) string { return runview.HaltClass(err) }

// --- ACP v1 session registry ---

// newSession creates a session, recording the host-supplied workspace cwd (cleaned), and returns its id.
// With a DURABLE store wired, ids are collision-resistant random tokens (the store is global under the
// home, so a per-process s-NNNN counter would collide across restarts). Without a store, ids stay the
// deterministic in-process s-NNNN sequence.
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

// randomToken returns 16 hex chars (64 bits of entropy) for a collision-resistant durable session id,
// falling back to a nanosecond token only if the system RNG is unavailable.
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

func (s *Server) sessionCWD(id string) string {
	s.smu.Lock()
	defer s.smu.Unlock()
	if st := s.known[id]; st != nil {
		return st.CWD
	}
	return ""
}

// persistSession writes the durable record for an in-process session (best-effort; a store failure is
// non-fatal — the session still works in-process this run). No-op without a store.
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

// sessionResumeParams is the ACP v1 ResumeSessionRequest: sessionId required, cwd optional (the host
// re-supplies the workspace root on reconnect).
type sessionResumeParams struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
}

// handleSessionResume implements ACP v1 `session/resume`: restore a session persisted by an EARLIER agent
// process from the durable store, WITHOUT replaying any conversation history (exploremesh holds none). On
// success it re-registers the in-process session handle and returns an empty ResumeSessionResponse.
// Method-not-found when no store is configured; structured invalid-params for a missing id or an unknown /
// malformed / expired record.
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
	cwd := rec.CWD
	if c := cleanSessionCWD(p.CWD); c != "" {
		cwd = c
	}
	s.restoreSession(p.SessionID, cwd)
	rec.CWD = cwd
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	_ = s.Sessions.Save(rec)
	return okResp(req.ID, map[string]any{})
}

// restoreSession re-registers a resumed session id with its cwd, and advances the session counter past a
// resumed s-NNNN id so a later session/new cannot mint a colliding id within this process.
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

// cleanSessionCWD normalizes a host-supplied cwd: trim + filepath.Clean, resolving a relative path against
// the ACP process working directory. It writes nothing and does not stat the path. Empty in → empty out.
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

// putSession registers the in-flight run for a session id. It returns false if the session already has an
// active run (so a concurrent prompt is rejected, not silently overwritten — which would orphan the first
// run from session/cancel).
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

// handleCancel handles `cancel` / `$/cancelRequest`. exploremesh keys in-flight runs on the sessionId
// (there is no non-session run path), so a bare cancel accepts a `sessionId` param and cancels that run.
func (s *Server) handleCancel(f Framer, req rpcRequest, notif bool) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	cancelled := p.SessionID != "" && s.cancelSession(p.SessionID)
	if !notif {
		s.write(f, okResp(req.ID, map[string]any{"cancelled": cancelled}))
	}
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

// rpcNotification is a JSON-RPC notification (no id, no response expected) — e.g. a session/update event.
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// sessionUpdateParams is the ACP v1 session/update (SessionNotification) params: { sessionId, update },
// where update is a SessionUpdate discriminated-union object keyed by the `sessionUpdate` discriminator.
type sessionUpdateParams struct {
	SessionID string         `json:"sessionId"`
	Update    map[string]any `json:"update"`
}

// acpProgressUpdate builds an ACP v1 SessionUpdate for an exploremesh progress event. ACP v1 has no
// dedicated "agent progress" variant, so progress text is carried as the least-misleading official
// variant — `agent_message_chunk` (a text ContentBlock) — and the structured exploremesh audit-event
// fields (no secrets) are preserved under the sanctioned `_meta.exploremesh` extension point.
func acpProgressUpdate(eventType, level, message, timestamp string) map[string]any {
	return map[string]any{
		"sessionUpdate": "agent_message_chunk",
		// A trailing newline so a host that concatenates streamed chunk text renders each progress line on
		// its own line. The structured `_meta.exploremesh.message` keeps the CLEAN message.
		"content": map[string]any{"type": "text", "text": message + "\n"},
		"_meta": map[string]any{"exploremesh": map[string]any{
			"eventType": eventType,
			"level":     level,
			"message":   message,
			"timestamp": timestamp,
		}},
	}
}

// promptResponse builds an ACP v1 PromptResponse: { stopReason, _meta: { exploremesh } }. stopReason MUST
// be an official StopReason (end_turn | cancelled | …); exploremesh result details go under
// `_meta.exploremesh`.
func promptResponse(stopReason string, exploremeshMeta map[string]any) map[string]any {
	return map[string]any{
		"stopReason": stopReason,
		"_meta":      map[string]any{"exploremesh": exploremeshMeta},
	}
}

// notify writes a JSON-RPC notification, serialized with responses via s.wmu so a progress frame never
// interleaves with a concurrent response frame.
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
