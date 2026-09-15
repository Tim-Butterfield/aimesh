package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/acp"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Framer is the wire framing a server reads and writes through. MCP stdio is newline-delimited JSON, which
// meshcore/acp already implements (along with the LSP-style Content-Length variant), so the framing layer
// is shared rather than written twice.
type Framer = acp.Framer

// Framing names, re-exported so a caller (or an in-repo test client) selects the framing without also
// importing the ACP package.
const (
	FramingNewline       = acp.FramingNewline
	FramingContentLength = acp.FramingContentLength
)

// NewFramer builds a Framer over a reader/writer pair.
func NewFramer(name string, in io.Reader, out io.Writer) Framer { return acp.NewFramer(name, in, out) }

// Handler executes one `tools/call`. It receives the call context — cancelled when the client sends
// `notifications/cancelled` for this request, or when the server shuts down — and the Call, through which
// it emits progress and log notifications.
//
// Returning a *RequestError produces a JSON-RPC protocol error (the malformed-request case). Any other
// error becomes a CallToolResult with IsError set, because a domain failure the model should react to must
// not ride a channel clients flatten. Returning a result with IsError set is the normal way to report a
// domain halt.
type Handler func(ctx context.Context, c *Call) (*CallToolResult, error)

// Call is one in-flight tool invocation.
type Call struct {
	// Name is the tool being called; Arguments is the raw `arguments` object (nil when absent).
	Name      string
	Arguments json.RawMessage

	srv *Server
	f   Framer
	env *RequestEnv // the protocol context of THIS request; never nil for a dispatched call

	// taskID is set only by CreateTask, when the handler answers with a task handle instead of a
	// result.
	taskID string

	mu   sync.Mutex
	last float64 // last emitted progress value (monotonicity guard)
}

// Env is the protocol context of this request — the revision in force, the caller's progress token,
// and the trusted roots and resolver this call's paths are judged against (see env.go).
//
// It never returns nil, so a handler may read it without a guard. A Call built outside a dispatch
// (only a test does that) gets a fail-closed env: no roots, and the zero resolver, which refuses every
// path. "No context" must never read as "no restrictions".
func (c *Call) Env() *RequestEnv {
	if c == nil || c.env == nil {
		return &RequestEnv{Era: EraLegacy, RootSource: RootsNone, Trust: &scope.Resolver{}}
	}
	return c.env
}

// HasProgressToken reports whether the caller supplied a progress token. Without one, progress
// notifications cannot be routed.
func (c *Call) HasProgressToken() bool { return c != nil && len(c.Env().ProgressToken) > 0 }

// Progress emits `notifications/progress` only when the caller supplied a token and done exceeds the
// last value emitted for this call, so a client never sees progress go backwards.
func (c *Call) Progress(done, total float64, message string) {
	if !c.HasProgressToken() {
		return
	}
	c.mu.Lock()
	if done <= c.last {
		c.mu.Unlock()
		return
	}
	c.last = done
	c.mu.Unlock()
	params := map[string]any{"progressToken": c.Env().ProgressToken, "progress": done}
	if total > 0 {
		params["total"] = total
	}
	if message != "" {
		params["message"] = message
	}
	c.srv.notify(c.f, "notifications/progress", params)
}

// Log emits `notifications/message` at level, dropped when the client selected a higher level. Log
// notifications are best effort: anything that must survive belongs in the result or the run record.
// On the modern era Log does nothing. The filter reads the session's current level rather than a
// snapshot, because `logging/setLevel` may arrive while a call is running.
func (c *Call) Log(level Level, logger string, data any) {
	if c == nil {
		return
	}
	if c.Env().Era == EraModern {
		return
	}
	c.srv.log(c.f, level, logger, data)
}

// Server is an MCP stdio server. Register the tools, then Serve.
type Server struct {
	// Info is the `serverInfo` returned by initialize; Instructions is the cross-tool contract the client
	// may fold into its system prompt (how the tools relate, which facts must be surfaced, which calls
	// spend money).
	Info         Implementation
	Instructions string
	// Framing selects the wire framing ("" / "newline" → newline-delimited JSON, the MCP stdio default).
	Framing string

	// Protocol is the era posture this process was launched with ("" → ProtocolDual); see
	// ProtocolLegacy.
	//
	// SUNSET-PATH (MCP26-SUNSET): the whole field goes with the era.
	Protocol ProtocolMode
	// PageSize bounds one `tools/list` page. 0 (the default) returns every tool in one page with no
	// nextCursor. A cursor is always validated.
	PageSize int
	// Diagnostics receives server-side logging, keeping stdout pure JSON-RPC. nil discards.
	Diagnostics io.Writer

	// WithheldTools maps tools this server deliberately did not register to the reason a caller should
	// be told. Withheld tools stay absent from `tools/list`; calling one returns the reason instead of
	// "unknown tool", so a model reports a missing grant rather than searching for another tool.
	WithheldTools map[string]string

	// Resources, when non-nil, declares the `resources` capability and backs `resources/list` and
	// `resources/read`. With nil, both methods return -32601.
	Resources ResourceProvider

	// Tasks, when non-nil, declares the `io.modelcontextprotocol/tasks` extension in
	// `server/discover`, backs `tasks/get`, `tasks/update` and `tasks/cancel`, and lets a handler
	// answer a `tools/call` with a task handle (Call.CreateTask). nil disables the extension, which
	// is modern-era only; see tasks.go.
	Tasks TaskProvider

	// OnRoots, when non-nil, receives the client's declared roots after the handshake and on every
	// `notifications/roots/list_changed`. It is called only when the client declared the `roots`
	// capability.
	OnRoots func([]Root)

	// RequestTimeout bounds one server-to-client request (0 → DefaultRequestTimeout), so a client
	// that never answers costs a deadline rather than a stuck session.
	RequestTimeout time.Duration

	// TrustFor supplies the effective trusted roots and resolver for one request, so confinement is
	// per request (see env.go). It receives the era because legacy clients narrow roots through
	// `roots/list`, which the modern era lacks, and an application must not grant a modern client
	// more authority for that reason. SUNSET-PATH (MCP26-SUNSET): the parameter goes with the era.
	//
	// nil means this server's tools take no filesystem paths, and every path is refused.
	TrustFor func(Era) TrustContext

	// StrictSchema validates every `structuredContent` against its tool's outputSchema before sending,
	// failing the call on a violation (see enforceOutputSchema). It is off by default because the
	// cost scales with payload size on every call.
	StrictSchema bool

	tools    []Tool
	handlers map[string]Handler
	schemas  schemaCache

	wmu sync.Mutex // serializes frame writes across goroutines

	// latched is the era cell (era.go), written only by compare-and-set and deliberately not guarded
	// by mu.
	//
	// SUNSET-PATH (MCP26-SUNSET).
	latched eraLatchCell

	mu          sync.Mutex
	active      Framer // the live session's framer (nil outside ServeFramed)
	initialized bool   // the client's notifications/initialized has arrived
	handshook   bool   // initialize has been answered
	negotiated  string // the echoed protocol version
	clientRoots bool   // the client declared the `roots` capability at initialize
	level       Level  // client-selected minimum log level
	inflight    map[string]context.CancelFunc
	cancelled   map[string]bool

	// The server-to-client request table (see outgoing.go), with its own mutex because a handler
	// goroutine may issue a request while the read loop delivers another's response.
	//
	// SUNSET-PATH (MCP26-SUNSET): these four fields, OnRoots, RequestTimeout, and the closeOutgoing,
	// deliver and refreshRoots call sites in this file are deleted with outgoing.go.
	omu     sync.Mutex
	outSeq  uint64
	pending map[string]chan *rpcResponse
	closed  bool
}

// Register declares a tool and its handler. It panics on an empty or duplicate name, a missing input
// schema or a nil handler, all programming errors caught at startup.
func (s *Server) Register(t Tool, h Handler) {
	if s.handlers == nil {
		s.handlers = map[string]Handler{}
	}
	if t.Name == "" {
		panic("mcp: tool with an empty name")
	}
	if _, dup := s.handlers[t.Name]; dup {
		panic("mcp: duplicate tool registration for " + t.Name)
	}
	if len(t.InputSchema) == 0 {
		panic("mcp: tool " + t.Name + " has no inputSchema")
	}
	if h == nil {
		panic("mcp: tool " + t.Name + " has no handler")
	}
	s.tools = append(s.tools, t)
	s.handlers[t.Name] = h
}

// Tools returns the declared tools in registration order (the order `tools/list` pages through).
func (s *Server) Tools() []Tool { return append([]Tool(nil), s.tools...) }

// Serve reads and writes through the configured framing until EOF.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	return s.ServeFramed(NewFramer(s.Framing, in, out))
}

// ServeFramed runs the dispatch loop over an explicit Framer. `tools/call` requests run concurrently so a
// later `notifications/cancelled` can reach an in-flight call; every other method is handled inline. Frame
// writes are serialized. It never panics on bad input.
func (s *Server) ServeFramed(f Framer) error {
	baseCtx, cancelAll := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	// The framer is published for server-to-client requests. Shutdown runs cancelAll, closeOutgoing,
	// then wg.Wait, so a handler blocked on a client answer is released before the loop waits for it.
	// Defers run LIFO, so they are registered in reverse.
	s.mu.Lock()
	s.active = f
	s.mu.Unlock()
	defer wg.Wait()
	defer func() {
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
		s.closeOutgoing()
	}()
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
		// JSON-RPC batching was removed in 2025-06-18; an array is refused as one invalid request.
		if raw[0] == '[' {
			s.write(f, errResp(nil, CodeInvalidRequest, "batch requests are not supported (JSON-RPC batching was removed in MCP 2025-06-18); send one request per message"))
			continue
		}
		var req rpcRequest
		if jerr := json.Unmarshal(raw, &req); jerr != nil {
			s.write(f, errResp(nil, CodeParse, "parse error"))
			continue
		}
		// A frame with an id and no method is a response to a server-to-client request. It is checked
		// before the notification test, since a response has an id.
		if req.Method == "" && len(req.ID) > 0 {
			if s.deliver(raw) {
				continue
			}
		}
		// Only an absent id makes a message a notification; `id: null` still receives a response.
		notif := len(req.ID) == 0
		if req.JSONRPC != "2.0" {
			if !notif {
				s.write(f, errResp(req.ID, CodeInvalidRequest, `invalid request: jsonrpc must be "2.0"`))
			}
			continue
		}
		// Admission: the envelope is valid, and steps 2-5 plus the latch happen in admit. A request with
		// no modern `_meta` falls through to the legacy switch below.
		if !notif {
			menv, answer := s.admit(&req)
			if answer != nil {
				s.write(f, answer)
				continue
			}
			if menv != nil {
				s.serveModern(baseCtx, f, &wg, req, menv)
				continue
			}
		}
		switch req.Method {
		case "notifications/initialized", "initialized":
			s.markInitialized()
			// The session is live, so ask the client for its roots, in a goroutine because the answer
			// arrives through this read loop.
			if s.OnRoots != nil && s.ClientDeclaredRoots() {
				wg.Go(func() { s.refreshRoots("after initialize") })
			}
		case "notifications/cancelled":
			s.handleCancelled(req.Params)
		case "notifications/roots/list_changed":
			// The notification carries no roots, so fetch them again.
			if s.OnRoots != nil && s.ClientDeclaredRoots() {
				wg.Go(func() { s.refreshRoots("roots/list_changed") })
			}
		case "notifications/progress":
			// A client notification this server does not use; notifications never get a response.
		case "initialize":
			if notif {
				continue
			}
			// A handshake on a modern-latched process is refused with the supported versions named.
			if s.era() == EraModern {
				s.write(f, s.initializeAfterModernRefusal(req.ID))
				continue
			}
			resp := s.initialize(req.ID, req.Params)
			// The legacy latch is the last step: only a successful handshake selects the era, so a
			// refused `initialize` leaves the process unlatched.
			if resp.Error == nil {
				if got := s.latchEra(EraLegacy); got != EraLegacy {
					// Lost the race to a modern opener.
					resp = s.initializeAfterModernRefusal(req.ID)
					s.rollbackInitialize()
				}
			}
			s.write(f, resp)
		case "ping":
			// `ping` is allowed before initialization completes, so a host can check liveness.
			if !notif {
				s.write(f, okResp(req.ID, map[string]any{}))
			}
		case "tools/list":
			if notif {
				continue
			}
			if resp := s.requireInitialized(req.ID, req.Method); resp != nil {
				s.write(f, resp)
				continue
			}
			s.write(f, s.listTools(nil, req.ID, req.Params))
		case "logging/setLevel":
			if notif {
				continue
			}
			if resp := s.requireInitialized(req.ID, req.Method); resp != nil {
				s.write(f, resp)
				continue
			}
			s.write(f, s.setLevel(req.ID, req.Params))
		case "resources/list", "resources/read", "resources/templates/list":
			if notif {
				continue
			}
			// Without a provider there is no `resources` capability, so the method does not exist.
			if s.Resources == nil {
				s.write(f, errResp(req.ID, CodeMethodNotFound, "method not found: "+req.Method+" (this server declares no `resources` capability)"))
				continue
			}
			if resp := s.requireInitialized(req.ID, req.Method); resp != nil {
				s.write(f, resp)
				continue
			}
			switch req.Method {
			case "resources/list":
				s.write(f, s.listResources(baseCtx, nil, req.ID, req.Params))
			case "resources/read":
				s.write(f, s.readResource(baseCtx, nil, req.ID, req.Params))
			default:
				// Templates are declared-and-empty rather than absent: this server addresses artifacts
				// by minted URI, so there is no template a client could expand into one.
				env, refusal := s.envFor(req.Method, req.Params)
				if refusal != nil {
					refusal.ID = req.ID
					s.write(f, refusal)
					continue
				}
				s.write(f, okResp(req.ID, s.result(env, map[string]any{"resourceTemplates": []any{}})))
			}
		case "tools/call":
			if notif {
				continue // a tool call with no id has nowhere to return its result; refuse to spend on it
			}
			if resp := s.requireInitialized(req.ID, req.Method); resp != nil {
				s.write(f, resp)
				continue
			}
			ctx, cancel := context.WithCancel(baseCtx)
			key := canonicalID(req.ID)
			s.putInflight(key, cancel)
			wg.Add(1)
			go func(req rpcRequest, key string, ctx context.Context, cancel context.CancelFunc) {
				defer wg.Done()
				defer cancel()
				resp := s.callTool(ctx, f, nil, req)
				// A cancelled request gets no response; the handler records its outcome.
				if s.finishInflight(key) {
					return
				}
				s.write(f, resp)
			}(req, key, ctx, cancel)
		default:
			if !notif {
				if strings.HasPrefix(req.Method, "notifications/") {
					continue
				}
				s.write(f, errResp(req.ID, CodeMethodNotFound, "method not found: "+req.Method))
			}
		}
	}
}

// serveModern dispatches an admitted modern request. It is separate from the legacy switch because the
// method sets differ and the modern era has no pre-initialization state, so -32002 is unreachable here.
//
// SUNSET-PATH (MCP26-SUNSET): this switch survives legacy removal; the legacy one is deleted.
func (s *Server) serveModern(baseCtx context.Context, f Framer, wg *sync.WaitGroup, req rpcRequest, env *RequestEnv) {
	switch req.Method {
	case "tools/list":
		s.write(f, s.listTools(env, req.ID, req.Params))
	case "resources/list":
		s.write(f, s.listResources(baseCtx, env, req.ID, req.Params))
	case "resources/read":
		s.write(f, s.readResource(baseCtx, env, req.ID, req.Params))
	case "resources/templates/list":
		s.write(f, okResp(req.ID, s.result(env, map[string]any{"resourceTemplates": []any{}})))
	case "tasks/get":
		s.write(f, s.getTask(env, req.ID, req.Params))
	case "tasks/update":
		s.write(f, s.updateTask(env, req.ID, req.Params))
	case "tasks/cancel":
		// Answered inline: it only delivers the cancel to the run's context. See cancelTask.
		s.write(f, s.cancelTask(env, req.ID, req.Params))
	case "tools/call":
		ctx, cancel := context.WithCancel(baseCtx)
		key := canonicalID(req.ID)
		s.putInflight(key, cancel)
		wg.Go(func() {
			defer cancel()
			resp := s.callTool(ctx, f, env, req)
			// A cancelled request gets no further message on 2026-07-28 stdio.
			if s.finishInflight(key) {
				return
			}
			s.write(f, resp)
		})
	default:
		// Unreachable: admit refuses methods outside the modern set.
		s.write(f, errResp(req.ID, CodeMethodNotFound, modernMethodNotFound(req.Method, s.Resources != nil)))
	}
}

// rollbackInitialize undoes the session state a handshake wrote when it then lost the era race, so a
// process that answered `initialize` with an error holds no negotiated version. The stdio read loop
// serializes admission, so this is currently unreachable.
func (s *Server) rollbackInitialize() {
	s.mu.Lock()
	s.handshook, s.negotiated, s.clientRoots, s.level = false, "", false, ""
	s.mu.Unlock()
}

// requireInitialized returns the refusal for a request that arrived before the client's
// `notifications/initialized`, or nil when the session is ready.
//
// Legacy only. SUNSET-PATH (MCP26-SUNSET): the modern era has no session.
func (s *Server) requireInitialized(id json.RawMessage, method string) *rpcResponse {
	s.mu.Lock()
	ready := s.initialized
	shook := s.handshook
	s.mu.Unlock()
	if ready {
		return nil
	}
	if !shook {
		return errResp(id, CodeNotInitialized, "server not initialized: send `initialize`, then the `notifications/initialized` notification, before "+method)
	}
	return errResp(id, CodeNotInitialized, "server not initialized: the `notifications/initialized` notification has not arrived yet — send it before "+method)
}

func (s *Server) markInitialized() {
	s.mu.Lock()
	s.initialized = true
	s.mu.Unlock()
}

// initializeParams is the subset of InitializeRequest this transport reads.
type initializeParams struct {
	ProtocolVersion json.RawMessage     `json:"protocolVersion"`
	ClientInfo      *Implementation     `json:"clientInfo,omitempty"`
	Capabilities    *clientCapabilities `json:"capabilities,omitempty"`
}

// clientCapabilities is the subset of the client's declaration this transport acts on. `roots` is a
// pointer because presence is the signal: `{"roots": {}}` declares the capability.
type clientCapabilities struct {
	Roots *struct {
		ListChanged bool `json:"listChanged,omitempty"`
	} `json:"roots,omitempty"`
}

// initialize answers the handshake: it negotiates the protocol version (echoing a supported one and
// answering any other with the latest supported version), declares capabilities, and returns
// serverInfo and instructions.
//
// SUNSET-PATH (MCP26-SUNSET): the whole handshake goes with the legacy era, including
// initializeParams, clientCapabilities, markInitialized, rollbackInitialize, NegotiatedVersion, the
// `notifications/initialized` dispatch arm, the session fields initialized, handshook, negotiated,
// clientRoots and level, and the `logging` capability declared here.
func (s *Server) initialize(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var ip initializeParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &ip) // best-effort; the version is validated below
	}
	var want string
	if len(ip.ProtocolVersion) > 0 {
		if json.Unmarshal(ip.ProtocolVersion, &want) != nil {
			return errResp(id, CodeInvalidParams, fmt.Sprintf(
				"invalid params: protocolVersion must be a string — this server implements %s",
				strings.Join(SupportedProtocolVersions, ", ")))
		}
	}
	want = strings.TrimSpace(want)
	// The MCP lifecycle requires an unsupported version to be answered with a supported one; the client
	// decides whether to proceed. Refusing breaks clients that open with a newer revision.
	if !slices.Contains(SupportedProtocolVersions, want) {
		want = LatestProtocolVersion
	}
	s.mu.Lock()
	s.handshook = true
	s.negotiated = want
	// Whether the client declared `roots` is recorded once, here. A client that declared nothing is
	// never asked and cannot change what this server may read.
	s.clientRoots = ip.Capabilities != nil && ip.Capabilities.Roots != nil
	if s.level == "" {
		s.level = LevelInfo
	}
	s.mu.Unlock()

	caps := map[string]any{
		// The tool set is fixed at startup, so the list never changes.
		"tools":   map[string]any{"listChanged": false},
		"logging": map[string]any{},
	}
	if s.Resources != nil {
		// Declared only with a provider. No change notifications are sent; clients re-list.
		caps["resources"] = map[string]any{"subscribe": false, "listChanged": false}
	}
	return okResp(id, map[string]any{
		"protocolVersion": want, // echoed
		"capabilities":    caps,
		"serverInfo":      s.Info,
		"instructions":    s.Instructions,
	})
}

// NegotiatedVersion returns the protocol version echoed at initialize ("" before the handshake).
func (s *Server) NegotiatedVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.negotiated
}

// --- tools/list, with real cursor paging ---

type listToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// toolsPage decodes a `tools/list` request and returns the page it names; admission step 5 runs the
// same check before the latch. An unrecognized cursor is -32602. With PageSize 0 the whole list is
// returned without nextCursor.
func (s *Server) toolsPage(id json.RawMessage, params json.RawMessage) (map[string]any, *rpcResponse) {
	var p listToolsParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, errResp(id, CodeInvalidParams, "invalid params: "+err.Error())
		}
	}
	start := 0
	if p.Cursor != "" {
		n, err := decodeCursor(p.Cursor)
		if err != nil || n < 0 || n > len(s.tools) {
			return nil, errResp(id, CodeInvalidParams, fmt.Sprintf("invalid params: unknown cursor %q", p.Cursor))
		}
		start = n
	}
	end := len(s.tools)
	if s.PageSize > 0 && start+s.PageSize < end {
		end = start + s.PageSize
	}
	out := map[string]any{"tools": s.tools[start:end]}
	if end < len(s.tools) {
		out["nextCursor"] = encodeCursor(end)
	}
	return out, nil
}

// listTools answers `tools/list` under the era its env carries. `env` is nil for a legacy caller that
// has not built one yet.
func (s *Server) listTools(env *RequestEnv, id json.RawMessage, params json.RawMessage) *rpcResponse {
	out, refusal := s.toolsPage(id, params)
	if refusal != nil {
		return refusal
	}
	if env == nil {
		var r *rpcResponse
		if env, r = s.envFor("tools/list", params); r != nil {
			r.ID = id
			return r
		}
	}
	return okResp(id, s.result(env, out))
}

// cursorPrefix lets a foreign cursor be rejected rather than decoded into a plausible offset.
const cursorPrefix = "tools:"

func encodeCursor(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.Itoa(n)))
}

func decodeCursor(s string) (int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, err
	}
	rest, ok := strings.CutPrefix(string(b), cursorPrefix)
	if !ok {
		return 0, fmt.Errorf("foreign cursor")
	}
	return strconv.Atoi(rest)
}

// --- logging/setLevel ---

type setLevelParams struct {
	Level string `json:"level"`
}

func (s *Server) setLevel(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p setLevelParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return errResp(id, CodeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if !ValidLevel(p.Level) {
		return errResp(id, CodeInvalidParams, fmt.Sprintf(
			"invalid params: unknown logging level %q (want one of: %s)", p.Level, strings.Join(LevelNames(), ", ")))
	}
	s.mu.Lock()
	s.level = Level(strings.ToLower(strings.TrimSpace(p.Level)))
	s.mu.Unlock()
	return okResp(id, map[string]any{})
}

// log emits `notifications/message` when level is at or above the client's selected minimum.
//
// SUNSET-PATH (MCP26-SUNSET): the whole `notifications/message` path is deleted at legacy removal:
// Server.log, the body of Call.Log, setLevel, setLevelParams, the session level field, and the Level
// vocabulary in mcp.go.
func (s *Server) log(f Framer, level Level, logger string, data any) {
	s.mu.Lock()
	min := s.level
	ready := s.initialized
	s.mu.Unlock()
	if !ready {
		return // nothing may be sent before the session is live
	}
	if min == "" {
		min = LevelInfo
	}
	if levelRank[level] < levelRank[min] {
		return
	}
	params := map[string]any{"level": string(level), "data": data}
	if logger != "" {
		params["logger"] = logger
	}
	s.notify(f, "notifications/message", params)
}

// --- tools/call ---

// callToolParams is the `tools/call` params this transport reads. `_meta` is read once, by envFor.
type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolCallParams decodes and validates a `tools/call` request. Admission step 5 runs it before the
// latch, so a call naming an unknown tool never selects the era.
func (s *Server) toolCallParams(id json.RawMessage, params json.RawMessage) (*callToolParams, *rpcResponse) {
	var p callToolParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, errResp(id, CodeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, errResp(id, CodeInvalidParams, "invalid params: tools/call requires a tool `name`")
	}
	if _, ok := s.handlers[p.Name]; !ok {
		// A tool withheld by a launch decision returns its reason instead of "unknown tool".
		if why := s.WithheldTools[p.Name]; why != "" {
			return nil, errResp(id, CodeInvalidParams, fmt.Sprintf("tool %q is not available on this server: %s", p.Name, why))
		}
		return nil, errResp(id, CodeInvalidParams, fmt.Sprintf("unknown tool %q (call tools/list for the available tools)", p.Name))
	}
	return &p, nil
}

func (s *Server) callTool(ctx context.Context, f Framer, env *RequestEnv, req rpcRequest) *rpcResponse {
	p, refusal := s.toolCallParams(req.ID, req.Params)
	if refusal != nil {
		return refusal
	}
	h := s.handlers[p.Name]
	// The protocol context is built once per request and carried on the Call. A modern call arrives
	// with it already built and validated; a legacy call builds it from session state here.
	if env == nil {
		var r *rpcResponse
		if env, r = s.envFor(req.Method, req.Params); r != nil {
			r.ID = req.ID
			return r
		}
	}
	c := &Call{Name: p.Name, Arguments: p.Arguments, srv: s, f: f, env: env}
	res, err := h(ctx, c)
	// A handler that answered with a task handle returns nil, nil. A returned error still wins,
	// since there is then no run to hand back.
	if id := c.taskHandle(); id != "" && err == nil {
		return s.createTaskResponse(req.ID, env, id)
	}
	if err != nil {
		if re, ok := errors.AsType[*RequestError](err); ok {
			return errResp(req.ID, re.Code, re.Message, re.Data)
		}
		// Any other error is a domain failure, returned as an IsError result.
		return okResp(req.ID, s.result(env, ErrorResult(err.Error(), nil)))
	}
	if res == nil {
		res = ErrorResult("the tool returned no result", nil)
	}
	// The opt-in send-time check, applied to the payload as it would actually go on the wire — after
	// the handler, before the frame. Off by default; see Server.StrictSchema.
	res = s.enforceOutputSchema(p.Name, res)
	if res.Content == nil {
		res.Content = []Content{}
	}
	// An IsError result still uses `resultType: "complete"`: the request completed and its tool failed.
	return okResp(req.ID, s.result(env, res))
}

// --- cancellation ---

type cancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

// handleCancelled cancels the in-flight call with the given request id. The cancellation is recorded
// even when the call has already finished, so its response is suppressed.
//
// It cannot cancel a task, as the tasks extension requires: once a CreateTaskResult is returned, the
// originating call is no longer in flight, so only `tasks/cancel` reaches the run.
func (s *Server) handleCancelled(params json.RawMessage) {
	var p cancelledParams
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	key := canonicalID(p.RequestID)
	s.mu.Lock()
	if s.cancelled == nil {
		s.cancelled = map[string]bool{}
	}
	cancel := s.inflight[key]
	if cancel != nil {
		s.cancelled[key] = true
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) putInflight(key string, cancel context.CancelFunc) {
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]context.CancelFunc{}
	}
	s.inflight[key] = cancel
	s.mu.Unlock()
}

// finishInflight deregisters a call and reports whether it was cancelled (⇒ send no response).
func (s *Server) finishInflight(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key)
	was := s.cancelled[key]
	delete(s.cancelled, key)
	return was
}

// --- frame writing ---

func (s *Server) write(f Framer, resp *rpcResponse) {
	if resp == nil {
		return
	}
	b, err := json.Marshal(resp)
	if err != nil {
		// A result that will not marshal still owes the caller a response, or the client waits for its
		// own timeout. eraResult.MarshalJSON fails when a payload collides with a field it owns.
		// Messages without an id have no caller and are dropped.
		if len(resp.ID) == 0 || resp.Error != nil {
			s.Diagnosticf("mcp: a response could not be marshalled and was dropped: %v", err)
			return
		}
		s.Diagnosticf("mcp: a result could not be marshalled; answering -32603: %v", err)
		if eb, eerr := json.Marshal(errResp(resp.ID, CodeInternal,
			"internal error: this server produced a result it could not encode")); eerr == nil {
			s.wmu.Lock()
			_ = f.WriteMessage(eb)
			s.wmu.Unlock()
		}
		return
	}
	s.wmu.Lock()
	_ = f.WriteMessage(b)
	s.wmu.Unlock()
}

// notify writes a JSON-RPC notification, serialized with responses so a progress frame never interleaves
// with a concurrent response frame.
func (s *Server) notify(f Framer, method string, params any) {
	b, err := json.Marshal(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return
	}
	s.wmu.Lock()
	_ = f.WriteMessage(b)
	s.wmu.Unlock()
}

// Diagnosticf writes a server-side diagnostic line to Diagnostics, never to the protocol stream.
func (s *Server) Diagnosticf(format string, args ...any) {
	if s.Diagnostics == nil {
		return
	}
	fmt.Fprintf(s.Diagnostics, format+"\n", args...)
}
