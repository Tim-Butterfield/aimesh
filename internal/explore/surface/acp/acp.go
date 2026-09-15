// Package acp serves explorations as an ACP (Agent Client Protocol) agent over stdio JSON-RPC 2.0, using
// the framing from meshcore/acp. It parses requests and routes them to the pipeline.
//
// Explorations read nothing from disk, so there is no workspace or write gating. On `session/prompt` the
// prompt text is the task purpose, and `_meta.exploremesh` carries the required criteria and panel (composed
// from the adapters the agent was launched with). The response's `_meta.exploremesh` echoes the task and
// panel that ran.
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

// agentVersion is the version reported in `initialize` agentInfo.
const agentVersion = "dev"

// JSON-RPC 2.0 error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeExploreHalt    = -32000 // an exploration halt; data carries exitCode, haltClass and failure
)

// Limits applied before any model subprocess starts. meshcore/acp separately caps a frame at 16 MiB.
const (
	// maxRosterExplorers bounds how many explorers one turn may fan out to.
	maxRosterExplorers = 16
	// maxPromptParamsBytes bounds the session/prompt params payload.
	maxPromptParamsBytes = 1 << 20 // 1 MiB
	// defaultTurnTimeout is the turn budget when Server.TurnTimeout is unset.
	defaultTurnTimeout = 10 * time.Minute
)

// Explorer runs an exploration. ctx cancellation stops the run, and onEvent receives progress events.
// NewPipelineExplorer provides the default implementation; tests use a fake.
type Explorer interface {
	Run(ctx context.Context, plan roster.Plan, raw schema.RawTask, opts pipeline.Options, onEvent func(audit.EventLine)) (pipeline.Result, error)
}

// pipelineExplorer runs the pipeline over a registry built once at launch.
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

// Server is the explore ACP agent server.
type Server struct {
	// Explorer routes an exploration to the pipeline.
	Explorer Explorer
	// Adapters are the adapters the agent was launched with. A panel may name only these; when empty, every
	// prompt is refused.
	Adapters launchflags.Set
	Framing  string // "" or "newline" (default), or "content-length"
	// TurnTimeout bounds one exploration; 0 means defaultTurnTimeout.
	TurnTimeout time.Duration
	// Sessions is an optional durable store that enables `session/resume` across restarts. When nil, resume
	// is neither advertised nor supported.
	Sessions SessionStore
	wmu      sync.Mutex // serializes frame writes

	smu      sync.Mutex                    // guards known, sessions and sessionN
	known    map[string]*sessionState      // sessions created by session/new
	sessions map[string]context.CancelFunc // in-flight run cancel, by session id
	sessionN int                           // session id counter
}

// sessionState is the in-process record for a session. CWD is kept only so it round-trips through
// session/resume; explorations never run in it.
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

// ServeFramed runs the dispatch loop over f. `session/prompt` requests run concurrently so a later cancel
// can reach them; other methods are handled inline.
func (s *Server) ServeFramed(f Framer) error {
	// Deferred in LIFO order: cancelAll runs before wg.Wait, so in-flight runs stop on shutdown.
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
		// Only a missing `id` makes a notification; `id: null` still gets a response.
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
				var sn sessionNewParams
				_ = json.Unmarshal(req.Params, &sn)
				sid := s.newSession(sn.CWD)
				s.persistSession(sid)
				s.write(f, okResp(req.ID, map[string]any{"sessionId": sid}))
			}
		case "session/resume":
			if !notif {
				s.write(f, s.handleSessionResume(req))
			}
		// session/load is not implemented: it replays conversation history, and explorations keep none.
		case "session/prompt":
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
			// Register the cancel under the session id, refusing a second concurrent prompt on the session.
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
				// Release the session before writing the response, so a client's immediate next prompt is
				// not refused as busy.
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

// initializeParams carries the client's protocol version; client capabilities are not used.
type initializeParams struct {
	ProtocolVersion json.RawMessage `json:"protocolVersion"`
}

// initialize returns the ACP v1 `initialize` result: text-only prompts, no session/load, no MCP, and
// `sessionCapabilities.resume` only when a session store is configured.
func (s *Server) initialize(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var ip initializeParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &ip) // protocolVersion is validated below
	}
	// The client must send an integer version of at least agentProtocolVersion; a higher one negotiates
	// down.
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

// parseIntProtocolVersion returns raw as an integer, or false if it is missing, not a number, or not an
// integer.
func parseIntProtocolVersion(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return int(i), true
}

// sessionNewParams carries the cwd a host sends with session/new.
type sessionNewParams struct {
	CWD string `json:"cwd"`
}

// sessionPromptParams is the `session/prompt` params: the session id, the prompt content and `_meta`.
type sessionPromptParams struct {
	SessionID string          `json:"sessionId"`
	Prompt    json.RawMessage `json:"prompt,omitempty"`
	Meta      *promptMeta     `json:"_meta,omitempty"`
}

// promptMeta holds the `_meta.exploremesh` value; other `_meta` keys belong to the host.
type promptMeta struct {
	Exploremesh json.RawMessage `json:"exploremesh,omitempty"`
}

// decodeExploreMeta decodes `_meta.exploremesh`, rejecting unknown keys so a misspelled field is an error
// rather than ignored. It returns nil when the key is absent or null.
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

// exploreMetaIn is the `_meta.exploremesh` request shape. criteria and panel are required. purpose, when
// set, overrides the prompt text. An empty mode means the default.
type exploreMetaIn struct {
	Criteria     []string `json:"criteria,omitempty"`
	PriorContext string   `json:"priorContext,omitempty"`
	Purpose      string   `json:"purpose,omitempty"`
	Mode         string   `json:"mode,omitempty"`
	// Artifact is the artifact under review, required by modes whose ValidateTask says so.
	Artifact string `json:"artifact,omitempty"`
	// Options through ConditioningEvent are the fixed-space declarations, required by the modes that use
	// them.
	Options         []string                  `json:"options,omitempty"`
	CompareCriteria []schema.CompareCriterion `json:"compareCriteria,omitempty"`
	Target          string                    `json:"target,omitempty"`
	Unit            string                    `json:"unit,omitempty"`
	Horizon         string                    `json:"horizon,omitempty"`
	// ConditioningEvent is a forecast's optional assumed condition.
	ConditioningEvent string `json:"conditioningEvent,omitempty"`
	// Panel is the required panel: 2 to 16 explorers and a collator, each using an adapter the agent was
	// launched with. Model identifiers are passed to the adapter as given.
	Panel *panelSpec `json:"panel,omitempty"`
	// Canonicalizers is either absent, so the host derives them, or exactly two identities.
	Canonicalizers []slotSpec `json:"canonicalizers,omitempty"`
	// DumpRun writes the run directory under $EXPLOREMESH_ARTIFACT_DIR, like the CLI's --dump-run.
	DumpRun bool `json:"dumpRun,omitempty"`
	// MaxParallel bounds how many explorers run at once; absent, the whole panel runs in parallel.
	MaxParallel int `json:"maxParallel,omitempty"`
	// DryRun ends the turn with status `planned` and a `shape` echo, before any model call. It cannot be
	// combined with DumpRun.
	DryRun bool `json:"dryRun,omitempty"`
	// VerifyReadiness makes one bounded call per distinct panel identity before the run and halts if any
	// fails.
	VerifyReadiness bool `json:"verifyReadiness,omitempty"`
}

// panelSpec is the `_meta.exploremesh.panel` shape.
type panelSpec struct {
	Explorers []slotSpec `json:"explorers,omitempty"`
	Collator  *slotSpec  `json:"collator,omitempty"`
}

// slotSpec names one seat. It has no path or argument fields; those come only from launch configuration.
type slotSpec struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// panel is the plan a prompt composed.
type panel struct {
	plan roster.Plan
}

// panelExample is the example panel included in refusal messages.
const panelExample = `{"panel": {"explorers": [{"adapter": "<adapter>", "model": "<model>"}, {"adapter": "<adapter>", "model": "<model>"}], "collator": {"adapter": "<adapter>", "model": "<model>"}}}`

// resolvePanel builds the prompt's plan from `_meta.exploremesh.panel` and applies any canonicalizers.
// Every refusal is invalid-params.
func (s *Server) resolvePanel(em *exploreMetaIn) (panel, *rpcError) {
	if em == nil || em.Panel == nil {
		return panel{}, &rpcError{Code: codeInvalidParams, Message: "invalid params: _meta.exploremesh.panel is required — every prompt composes its own explorers and collator from the adapters this agent was launched with. Corrected _meta.exploremesh: " + panelExample}
	}
	pan, perr := s.adHocPanel(em.Panel)
	if perr != nil {
		return panel{}, perr
	}
	if len(em.Canonicalizers) > 0 {
		cs, cerr := s.canonicalizerSlots(em.Canonicalizers)
		if cerr != nil {
			return panel{}, cerr
		}
		pan.plan.Canonicalizers = cs
	}
	return pan, nil
}

// canonicalizerSlots validates `_meta.exploremesh.canonicalizers` with the same checks as panel seats and
// roster.ValidateCanonicalizers.
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

// adHocPanel builds a plan from a panel spec. It refuses, with invalid-params naming the field, too few or
// too many explorers, a missing collator, a duplicate identity, or an unlaunched adapter.
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
	plan, err := r.Plan()
	if err != nil {
		return panel{}, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	return panel{plan: plan}, nil
}

// checkConfigured checks that a seat names an adapter and model, that the adapter was launched, and that
// its CLI can start. A refusal lists the launched adapters.
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
	pan, perr := s.resolvePanel(metaIn)
	if perr != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, perr.Code, perr.Message)
	}
	if len(pan.plan.Explorers) > maxRosterExplorers {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, fmt.Sprintf(
			"invalid params: roster has %d explorers, exceeding the ACP fan-out cap of %d", len(pan.plan.Explorers), maxRosterExplorers))
	}

	// The prompt text is the purpose unless _meta.exploremesh.purpose overrides it; the rest of the task
	// comes from _meta.exploremesh.
	raw := schema.RawTask{Purpose: promptText(p.Prompt)}
	if metaIn != nil {
		em := metaIn
		raw.Criteria = em.Criteria
		raw.PriorContext = em.PriorContext
		raw.Mode = strings.TrimSpace(em.Mode)
		raw.Artifact = em.Artifact
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
	// Criteria are never invented; absent or blank criteria are refused.
	if countNonBlank(raw.Criteria) == 0 {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "exploremesh ACP requires _meta.exploremesh.criteria: a non-empty list of criteria")
	}
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
	if err := mode.ValidateTask(raw.Mode, raw); err != nil {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	// A dry run performs no exploration, so there is nothing to capture.
	if metaIn != nil && metaIn.DryRun && metaIn.DumpRun {
		if notif {
			return nil
		}
		return errResp(req.ID, codeInvalidParams, "invalid params: dryRun cannot be combined with dumpRun — a dry run makes no model calls, so there are no envelopes, raw outputs or prompts to capture; the shape comes back in _meta.exploremesh.shape")
	}

	turnTimeout := s.TurnTimeout
	if turnTimeout <= 0 {
		turnTimeout = defaultTurnTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()

	// Progress becomes session/update notifications, stopping once the run context ends.
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
		return nil
	}
	// Capture is best-effort: a failure is reported in the echo and does not fail the turn.
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
			// Report the run id, not the host path.
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
		// A halt, or a turn timeout not caused by cancellation, is a JSON-RPC error.
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
	// A dry run reports `planned`, since nothing was explored.
	if out.Shape != nil {
		return okResp(req.ID, promptResponse("end_turn", echo("planned")))
	}
	return okResp(req.ID, promptResponse("end_turn", echo("complete")))
}

// dumpRun writes a finished turn's run directory under $EXPLOREMESH_ARTIFACT_DIR, as the CLI's --dump-run
// does, and returns the run id or the error text.
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

// promptText returns the text of an ACP prompt: the non-blank text blocks of a content-block array joined
// by newlines, or a plain string.
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

// exploreMeta builds the response's `_meta.exploremesh` echo: the task used, the panel, and a summary of
// the outcome. It reports whether priorContext was supplied but not its content. The governance and
// per-mode detail come from package runview, shared with the MCP surface.
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
	em["panelSource"] = "adhoc"
	em["explorers"] = slotEcho(pan.plan.Explorers)
	em["collator"] = map[string]any{"adapter": pan.plan.Collator.Adapter, "model": pan.plan.Collator.Model, "effort": pan.plan.Collator.Effort}
	// canonicalizerSource is the run's provenance when canonicalization ran, else what the prompt asked for.
	em["canonicalizers"] = slotEcho(pan.plan.Canonicalizers)
	if src := out.CanonicalizerProvenance; src != "" {
		em["canonicalizerSource"] = src
	} else if len(pan.plan.Canonicalizers) > 0 {
		em["canonicalizerSource"] = "explicit"
	} else {
		em["canonicalizerSource"] = "derived"
	}
	if out.CanonicalizerIndependence != "" {
		em["canonicalizerIndependence"] = out.CanonicalizerIndependence
	}
	if gov := runview.Governance(out); gov != nil {
		em["governance"] = gov
	}
	if sh := runview.Shape(out); sh != nil {
		em["dryRun"] = true
		em["shape"] = sh
	}
	maps.Copy(em, runview.ModeDetail(out))
	return em
}

// slotEcho renders explorers as adapter, model and effort maps.
func slotEcho(explorers []roster.Explorer) []map[string]any {
	out := make([]map[string]any, 0, len(explorers))
	for _, e := range explorers {
		out = append(out, map[string]any{"adapter": e.Adapter, "model": e.Model, "effort": e.Effort})
	}
	return out
}

// failureFromResult builds a halt's structured `data.failure` (see runview.Failure).
func failureFromResult(raw schema.RawTask, out pipeline.Result, err error) map[string]any {
	return runview.Failure(raw, out, err)
}

// haltClassOf returns a short halt-class label for err.
func haltClassOf(err error) string { return runview.HaltClass(err) }

// newSession creates a session with the given cwd and returns its id. With a durable store, ids are random
// so they do not collide across restarts; otherwise they are sequential (s-0001, s-0002, ...).
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

// randomToken returns 16 random hex characters, or a timestamp-based token if the system RNG fails.
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

// persistSession saves the session to the durable store, if any. A save failure is ignored; the session
// still works in this process.
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

// sessionResumeParams is the `session/resume` params: a required sessionId and an optional cwd.
type sessionResumeParams struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
}

// handleSessionResume restores a session saved by an earlier process, without history. It returns
// method-not-found when no store is configured and invalid-params for a missing or unloadable session.
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

// restoreSession registers a resumed session and advances the id counter past it, so session/new cannot
// reuse the id.
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

// parseSessionNum returns N from an "s-N" id, or 0.
func parseSessionNum(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "s-%d", &n); err != nil {
		return 0
	}
	return n
}

// cleanSessionCWD trims and cleans a host-supplied cwd, making a relative path absolute against the
// process working directory. It returns "" for an empty input.
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

// putSession registers the in-flight run's cancel for a session. It returns false if the session already
// has a run.
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

// handleCancel handles `cancel` and `$/cancelRequest`. Runs are keyed by session, so it cancels the run of
// the `sessionId` param.
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

// rpcNotification is a JSON-RPC notification.
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// sessionUpdateParams is the `session/update` params.
type sessionUpdateParams struct {
	SessionID string         `json:"sessionId"`
	Update    map[string]any `json:"update"`
}

// acpProgressUpdate builds a session update for a progress event. ACP has no progress variant, so the
// message is sent as an `agent_message_chunk` with the structured event under `_meta.exploremesh`.
func acpProgressUpdate(eventType, level, message, timestamp string) map[string]any {
	return map[string]any{
		"sessionUpdate": "agent_message_chunk",
		// The trailing newline keeps concatenated chunks on separate lines.
		"content": map[string]any{"type": "text", "text": message + "\n"},
		"_meta": map[string]any{"exploremesh": map[string]any{
			"eventType": eventType,
			"level":     level,
			"message":   message,
			"timestamp": timestamp,
		}},
	}
}

// promptResponse builds a PromptResponse with an ACP stopReason and the `_meta.exploremesh` echo.
func promptResponse(stopReason string, exploremeshMeta map[string]any) map[string]any {
	return map[string]any{
		"stopReason": stopReason,
		"_meta":      map[string]any{"exploremesh": exploremeshMeta},
	}
}

// notify writes a JSON-RPC notification, serialized with responses.
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
