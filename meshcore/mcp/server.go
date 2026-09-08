package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	// taskID is set by Call.CreateTask when the handler chose to answer this call with a TASK
	// HANDLE rather than a result. It is read once, by callTool, and it is written only through
	// CreateTask — which is where the "never return a task to a non-declaring client" rule lives.
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

// HasProgressToken reports whether the caller volunteered a progress token. A server MUST NOT invent one:
// without it, progress notifications are not merely unwanted, they are unroutable.
func (c *Call) HasProgressToken() bool { return c != nil && len(c.Env().ProgressToken) > 0 }

// Progress emits `notifications/progress` — ONLY when the caller supplied a token, and only when `done`
// strictly exceeds the last value emitted for this call. Monotonicity is enforced here rather than trusted
// from callers: the natural progress source (a fraction of phases complete) is monotonic, but a per-item
// counter reset by a retried phase is not, and a client that sees progress go backwards has no way to tell
// the difference between a bug and a restart.
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

// Log emits `notifications/message` at the given level, dropped when the client has raised its level
// above it. Log notifications are BEST-EFFORT BY CONTRACT: no governance-relevant fact may exist only
// here — anything that must survive belongs in the tool result or the run record.
//
// THE ERA DECIDES WHETHER ANYTHING IS EMITTED AT ALL; the session decides the level.
//
// The era half is per-request and is read from the env: `server/utilities/logging` is DEPRECATED in
// 2026-07-28, its named stdio migration is stderr (which this server already writes diagnostics to),
// so the modern era emits no `notifications/message` and Log becomes a no-op there.
//
// The LEVEL half deliberately stays session-live rather than moving onto the env. `logging/setLevel`
// is a control the client may send AT ANY TIME, including while a call that streams for minutes is
// running; filtering against a level snapshotted when the request arrived would silently ignore a
// client that asked to be quieted. RequestEnv.LogLevel therefore records the level at arrival and
// documents that it is not this filter — one fact, one reader, and the difference stated instead of
// discovered.
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

	// Protocol is the ERA POSTURE this process was launched with ("" → ProtocolDual). Under
	// ProtocolLegacy the process is a pre-2026-07-28 server in every observable respect — see the
	// constant's own comment for why `server/discover` must be an unknown method there.
	//
	// SUNSET-PATH (MCP26-SUNSET; migration design §16.2): the whole field goes with the era.
	Protocol ProtocolMode
	// PageSize bounds one `tools/list` page. 0 (the default) returns every tool in one page with no
	// nextCursor — the honest answer for a handful of tools. A non-zero value exercises real paging; a
	// cursor is always validated, never ignored.
	PageSize int
	// Diagnostics is where SERVER-side logging goes. It exists so that stdout stays pure JSON-RPC: a
	// server that spawns child processes has no second chance at that invariant. nil discards.
	Diagnostics io.Writer

	// WithheldTools names tools this server DELIBERATELY did not register, each mapped to the reason a
	// caller should be told. It changes nothing about advertisement — a withheld name is absent from
	// `tools/list`, which is the point of withholding it — and only replaces the generic "unknown tool"
	// refusal when someone calls the name anyway.
	//
	// It exists because those two facts are not the same and a caller must act differently on them. A
	// name that does not exist means "you are confused"; a name withheld by a launch decision means
	// "ask the operator to relaunch". "unknown tool" says the first about the second, which sends a
	// model looking for a different tool instead of reporting a missing grant.
	//
	// Values are supplied by the surface that withheld the tool: this package holds no vocabulary for
	// anyone's launch flags.
	WithheldTools map[string]string

	// Resources, when non-nil, declares the `resources` capability and backs `resources/list` and
	// `resources/read`. nil leaves both methods a -32601 — the honest answer for a server that publishes
	// nothing, rather than an advertised capability returning an empty list.
	Resources ResourceProvider

	// Tasks, when non-nil, declares the `io.modelcontextprotocol/tasks` extension in
	// `server/discover`, backs `tasks/get` / `tasks/update` / `tasks/cancel`, and lets a handler
	// answer a `tools/call` with a task handle (Call.CreateTask).
	//
	// nil is the whole extension off: no advertisement, three -32601s, and CreateTask always false.
	// The extension is MODERN-ERA ONLY by construction — a legacy client cannot declare it, and the
	// three methods live only in the modern method set. See tasks.go.
	Tasks TaskProvider

	// OnRoots, when non-nil, receives the client's declared roots after the handshake and again on every
	// `notifications/roots/list_changed`. It is called ONLY when the client declared the `roots`
	// capability: a client that declared nothing must leave the server's own root set exactly as it was.
	OnRoots func([]Root)

	// RequestTimeout bounds ONE server→client request (0 → DefaultRequestTimeout). A client that never
	// answers must cost this server a deadline, not a wedged session.
	RequestTimeout time.Duration

	// TrustFor supplies the EFFECTIVE trusted roots and the resolver for one request, so that
	// confinement is a property of the REQUEST rather than of the process (see env.go). It is called
	// once per dispatched request and its answer is carried on the Call.
	//
	// IT TAKES THE ERA, because confinement is era-dependent and this is the only seam that can see
	// both. Under the legacy era a client narrows the server through `roots/list`; the modern era
	// deletes that method, so the same launch configuration would otherwise grant strictly MORE
	// filesystem authority to a client merely because it speaks a newer revision. An application
	// answers that here — fail-closed — rather than in a handler that has no way to know which
	// revision asked. SUNSET-PATH (MCP26-SUNSET): the parameter goes with the era.
	//
	// nil means "this server's tools take no filesystem paths": the env then carries the zero
	// resolver, which refuses every path. That is the correct answer for such a server — a tool that
	// never asks is never told yes, and a tool that unexpectedly starts asking is refused rather than
	// silently granted the process's authority.
	TrustFor func(Era) TrustContext

	// StrictSchema turns on SEND-TIME validation of every `structuredContent` against its tool's declared
	// outputSchema. It is OFF by default because it is not free: the schema is compiled once per tool, but
	// each result is then re-marshalled and walked, so the cost scales with the PAYLOAD — a large
	// structured result is the expensive case, and it is paid on every call rather than once in CI. On a
	// violation the call fails LOUDLY (see enforceOutputSchema) instead of sending a payload that does
	// not satisfy the schema the client was handed.
	StrictSchema bool

	tools    []Tool
	handlers map[string]Handler
	schemas  schemaCache

	wmu sync.Mutex // serializes frame writes across goroutines

	// latched is THE era cell (era.go). It is an atomic because a compare-and-set is the only writer:
	// there must be no read-then-write window in which two openers both believe they latched. It is
	// deliberately NOT under `mu` — a mutex would make the winner and the losers agree, but it would
	// also let a future caller read it, decide, and write, which is the race the CAS forbids.
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

	// The server→client request table (see outgoing.go). It has its own mutex because a Request may be
	// issued from a handler goroutine while the read loop is delivering another one's response.
	//
	// SUNSET-PATH (MCP26-SUNSET; migration design §16.2). These four fields, the `OnRoots` and
	// `RequestTimeout` fields above, and the `closeOutgoing` / `deliver` / `refreshRoots` call sites
	// in this file all belong to outgoing.go and are deleted WITH that file. They are named here
	// because the checklist row says "file deleted outright" and the file's dependents live in this
	// one — deleting outgoing.go alone does not compile.
	omu     sync.Mutex
	outSeq  uint64
	pending map[string]chan *rpcResponse
	closed  bool
}

// Register declares a tool and its handler. It panics on a duplicate name or a missing input schema —
// both are programming errors caught at startup, in the same spirit as the mode registry.
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
	// The framer is published for the SERVER→CLIENT direction (roots/list). Shutdown order is
	// cancelAll → closeOutgoing → wg.Wait, deliberately: a handler blocked on a client answer must be
	// released BEFORE the loop waits for it, or a departed client would cost the shutdown a full
	// request timeout. (Defers run LIFO, so the registration order below is the reverse.)
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
		// Wire tolerance: JSON-RPC batching existed in the 2025-03-26 revision and was REMOVED in
		// 2025-06-18. An array on the wire is refused as one invalid request; it must never crash the
		// reader, because the client that sent it is still owed an answer it can act on.
		if raw[0] == '[' {
			s.write(f, errResp(nil, CodeInvalidRequest, "batch requests are not supported (JSON-RPC batching was removed in MCP 2025-06-18); send one request per message"))
			continue
		}
		var req rpcRequest
		if jerr := json.Unmarshal(raw, &req); jerr != nil {
			s.write(f, errResp(nil, CodeParse, "parse error"))
			continue
		}
		// A frame with an `id` and NO `method` is a RESPONSE — the client answering something this
		// server asked it (roots/list). It is routed to the waiting request and never treated as a
		// request, because replying to a response is a protocol violation. This check has to precede
		// the notification test: a response has an id, so it is not a notification either.
		if req.Method == "" && len(req.ID) > 0 {
			if s.deliver(raw) {
				continue
			}
		}
		// Only the ABSENCE of `id` makes a message a notification. `id: null` is a valid identifier and
		// still receives a response.
		notif := len(req.ID) == 0
		if req.JSONRPC != "2.0" {
			if !notif {
				s.write(f, errResp(req.ID, CodeInvalidRequest, `invalid request: jsonrpc must be "2.0"`))
			}
			continue
		}
		// ADMISSION — step 1 of the era order finishes here (the envelope is now known valid), and
		// steps 2–5 plus the latch happen inside admit. Three outcomes, and the third is the one that
		// keeps the legacy surface byte-identical: a request carrying no modern `_meta` falls straight
		// through to the switch below, exactly as before this file learned about eras.
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
			// The session is live, so the server→client half is now usable: ask the client for its
			// roots. It runs in its own goroutine because the read loop must keep reading — the
			// answer to this very request arrives through it.
			if s.OnRoots != nil && s.ClientDeclaredRoots() {
				wg.Add(1)
				go func() { defer wg.Done(); s.refreshRoots("after initialize") }()
			}
		case "notifications/cancelled":
			s.handleCancelled(req.Params)
		case "notifications/roots/list_changed":
			// The client's root set changed. RE-FETCH it: the notification carries no roots, and a
			// server that acknowledged it without asking again would keep enforcing a set the client
			// has already withdrawn.
			if s.OnRoots != nil && s.ClientDeclaredRoots() {
				wg.Add(1)
				go func() { defer wg.Done(); s.refreshRoots("roots/list_changed") }()
			}
		case "notifications/progress":
			// Client-originated notifications this server has no use for. Ignored by design: a
			// notification never gets a response, and refusing one would be a protocol violation.
		case "initialize":
			if notif {
				continue
			}
			// A handshake against a process that already latched MODERN is refused with our supported
			// versions named, because a legacy client has no fall-forward mechanism and this message
			// may be the only diagnostic it can surface.
			if s.era() == EraModern {
				s.write(f, s.initializeAfterModernRefusal(req.ID))
				continue
			}
			resp := s.initialize(req.ID, req.Params)
			// THE LEGACY LATCH, and it is the LAST step: only a handshake that actually succeeded
			// selects the era. A refused `initialize` — an unsupported or unreadable version — leaves
			// the process unlatched, so a modern client that follows it is still served.
			if resp.Error == nil {
				if got := s.latchEra(EraLegacy); got != EraLegacy {
					// Lost the race to a modern opener. Answer the modern refusal instead of a
					// handshake this process can no longer honour.
					resp = s.initializeAfterModernRefusal(req.ID)
					s.rollbackInitialize()
				}
			}
			s.write(f, resp)
		case "ping":
			// `ping` is the ONE request allowed before initialization completes (it is how a host proves
			// liveness during a slow handshake).
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
			// A server with no provider declares no `resources` capability, so the method genuinely
			// does not exist here — -32601, not an advertised capability answering emptily.
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
				// A CANCELLED request gets NO response: on 2025-06-18 servers SHOULD NOT send one and
				// clients are told to ignore a late one; 2026-07-28 stdio hardens that to MUST NOT, for
				// ANY further message. Either way the receipt of what it did or did not do has to live in
				// the handler's own durable record, never only in the response.
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

// serveModern dispatches an ADMITTED modern request. It is deliberately a separate switch from the
// legacy one rather than a flag threaded through it: the two eras have different method sets, and the
// modern era has NO pre-initialization state to check, so requireInitialized (and with it `-32002`, a
// code 2026-07-28 forbids emitting) is unreachable from here by construction rather than by care.
//
// SUNSET-PATH inverted (MCP26-SUNSET): at legacy removal this switch is what SURVIVES and the one below is deleted.
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
		// `tasks/cancel` is answered INLINE rather than on a goroutine, and that is not an
		// oversight: it does one non-blocking thing (deliver the cancel to the run's context) and
		// its acknowledgement promises nothing beyond that delivery. See cancelTask.
		s.write(f, s.cancelTask(env, req.ID, req.Params))
	case "tools/call":
		ctx, cancel := context.WithCancel(baseCtx)
		key := canonicalID(req.ID)
		s.putInflight(key, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			resp := s.callTool(ctx, f, env, req)
			// A CANCELLED request gets NO further message at all on 2026-07-28 stdio: "Servers SHOULD
			// stop work on a cancelled request as soon as practical and MUST NOT send any further
			// messages for it."
			if s.finishInflight(key) {
				return
			}
			s.write(f, resp)
		}()
	default:
		// Unreachable: admit already refused every method outside the modern set. Answering rather
		// than falling silent keeps the invariant checkable instead of assumed.
		s.write(f, errResp(req.ID, CodeMethodNotFound, modernMethodNotFound(req.Method, s.Resources != nil)))
	}
}

// rollbackInitialize undoes the session state a handshake wrote when the handshake then LOST the era
// race. It exists to keep the mutation and the latch paired: a process that answered `initialize` with
// an error must not be left holding `negotiated`.
//
// It is unreachable on the stdio read loop as written (admission is serialized there, so nothing can
// latch between the handshake's validation and its CAS). It is here because the pairing is a property
// of the code, not of the current caller — and an unpaired mutation is exactly the kind of thing a
// later dispatcher change turns into a bug nobody can see.
func (s *Server) rollbackInitialize() {
	s.mu.Lock()
	s.handshook, s.negotiated, s.clientRoots, s.level = false, "", false, ""
	s.mu.Unlock()
}

// requireInitialized returns the refusal for a request that arrived before the client's
// `notifications/initialized`, or nil when the session is ready.
//
// LEGACY-ONLY and SUNSET-PATH (MCP26-SUNSET): the modern era has no session to be outside of.
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
// POINTER because its presence is the whole signal: `{"roots": {}}` declares the capability, and an
// absent key declares nothing — which must leave the server's own root set untouched.
type clientCapabilities struct {
	Roots *struct {
		ListChanged bool `json:"listChanged,omitempty"`
	} `json:"roots,omitempty"`
}

// initialize answers the handshake: NEGOTIATE the protocol version (echo a supported one verbatim, refuse
// anything else), declare capabilities, and return serverInfo + instructions.
//
// SUNSET-PATH (MCP26-SUNSET; migration design §16.2). THE WHOLE HANDSHAKE GOES WITH THE LEGACY ERA,
// and this comment covers the block it anchors: `initialize`, `initializeParams`,
// `clientCapabilities`, `markInitialized`, `rollbackInitialize`, `NegotiatedVersion`, the
// `notifications/initialized` dispatch arm, and the session fields `initialized` / `handshook` /
// `negotiated` / `clientRoots` / `level`. `2026-07-28` has no session to open: every request carries
// its own protocol context in `_meta`, so there is nothing here to generalize — it is deleted.
// The `logging` capability this function declares goes with it, and the modern probe never
// advertised one.
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
	supported := false
	for _, v := range SupportedProtocolVersions {
		if v == want {
			supported = true
			break
		}
	}
	if !supported {
		// A clean refusal, not a silent downgrade. Answering with our own latest version (which the spec
		// permits) would let an unsupported client believe the handshake succeeded.
		return errResp(id, CodeInvalidParams, fmt.Sprintf(
			"unsupported or missing protocolVersion %q: this server implements %s",
			want, strings.Join(SupportedProtocolVersions, ", ")),
			map[string]any{"supported": SupportedProtocolVersions, "latest": LatestProtocolVersion})
	}
	s.mu.Lock()
	s.handshook = true
	s.negotiated = want
	// Whether the client declared `roots` is recorded HERE, once, from the handshake. Everything the
	// root round trip does is gated on it, and the gate is one-directional: a client that declared
	// nothing is never asked and can therefore never change what this server may read.
	s.clientRoots = ip.Capabilities != nil && ip.Capabilities.Roots != nil
	if s.level == "" {
		s.level = LevelInfo
	}
	s.mu.Unlock()

	caps := map[string]any{
		// listChanged is false and says so: this server binds its tool set at startup (compose, never
		// configure), so the list cannot change mid-session.
		"tools":   map[string]any{"listChanged": false},
		"logging": map[string]any{},
	}
	if s.Resources != nil {
		// Declared only when something backs it. `subscribe` and `listChanged` are false and say so:
		// a run's artifacts are published when the run finishes and dropped when it is evicted, and
		// this server sends no notification for either — a client re-lists.
		caps["resources"] = map[string]any{"subscribe": false, "listChanged": false}
	}
	return okResp(id, map[string]any{
		"protocolVersion": want, // ECHOED, not "our latest"
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

// toolsPage decodes and validates a `tools/list` request and returns the page it names. It is split
// out from listTools because admission STEP 5 must apply the SAME check before the era latch — an
// admission gate that disagreed with its handler about what is valid would latch on requests the
// server then refuses.
//
// A cursor is HONORED, not ignored: an unrecognized cursor is -32602 (the spec's requirement), and a
// page boundary produces a nextCursor the client can follow. With the default PageSize of 0 the whole
// list comes back with no nextCursor.
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

// cursorPrefix keeps a cursor OPAQUE-looking and lets an obviously foreign cursor be rejected rather than
// decoded into a plausible offset.
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

// log emits `notifications/message` when the level is at or above the client's selected minimum.
//
// SUNSET-PATH (MCP26-SUNSET; migration design §16.2). THE WHOLE `notifications/message` PATH IS
// DELETED OUTRIGHT at legacy removal, not un-gated: `Server.log`, `Call.Log`'s body, `setLevel` and
// `setLevelParams`, the session `level` field, and the `Level` vocabulary in mcp.go (the eight
// constants, `levelRank`, `ValidLevel`, `LevelNames`). There is no modern path to collapse into —
// `server/utilities/logging` is deprecated in `2026-07-28`, its named stdio migration is stderr, and
// this transport already writes diagnostics there. Applications currently calling `Call.Log` lose a
// no-op, not a channel.
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

// callToolParams is the `tools/call` params this transport reads. `_meta` is NOT parsed here: it is
// the request's protocol context, it is the same object on every method under the modern era, and it
// is read once by envFor so there is one parse of one shape rather than a per-method copy.
type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolCallParams decodes and validates a `tools/call` request. Split out for the same reason
// toolsPage is: admission STEP 5 runs it before the era latch, so a call naming a tool this server
// does not have never selects the process's era.
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
		// A tool WITHHELD by a launch decision gets its own answer. It is still absent from
		// tools/list; this only stops the refusal from telling a caller it was confused when in
		// fact it was ungranted.
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
	// THE PROTOCOL CONTEXT IS BUILT ONCE PER REQUEST AND CARRIED. Everything downstream — the progress
	// token, the era that decides whether logging is emitted at all, and the resolver every path in
	// this call is judged against — reads it from the Call instead of reaching back into the Server.
	// A modern call arrives with its env already built (and already validated, and already latched);
	// a legacy one builds it from session state here.
	if env == nil {
		var r *rpcResponse
		if env, r = s.envFor(req.Method, req.Params); r != nil {
			r.ID = req.ID
			return r
		}
	}
	c := &Call{Name: p.Name, Arguments: p.Arguments, srv: s, f: f, env: env}
	res, err := h(ctx, c)
	// THE TASK BRANCH, checked before anything else about the handler's return. A handler that
	// answered with a task handle returns `nil, nil` — there is no CallToolResult to send, because
	// the point of a `CreateTaskResult` is that the result does not exist yet.
	//
	// A protocol error still wins: a handler that both marked a task and failed has produced no run
	// to hand back a handle to, and reporting the failure is the only honest answer.
	if id := c.taskHandle(); id != "" && err == nil {
		return s.createTaskResponse(req.ID, env, id)
	}
	if err != nil {
		var re *RequestError
		if errors.As(err, &re) {
			return errResp(req.ID, re.Code, re.Message, re.Data)
		}
		// Anything else is a DOMAIN failure: it rides a successful response carrying IsError, so the
		// model sees it and can react.
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
	// `isError` rides `resultType: "complete"`: a domain halt is a COMPLETED request whose tool failed,
	// which is what server/tools says ("They are reported in tool results with `isError: true`", under a
	// heading distinguishing them from protocol errors returned as JSON-RPC errors). It reads wrong to a
	// human and it is what the specification asks for.
	return okResp(req.ID, s.result(env, res))
}

// --- cancellation ---

type cancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

// handleCancelled cancels the in-flight call with the given request id. The cancellation is RECORDED even
// when the call has already finished dispatching, so the response is suppressed rather than racing out.
//
// IT CANNOT CANCEL A TASK, and that is a requirement rather than a limitation. The tasks extension,
// verbatim (verified 2026-08-04): "The `notifications/cancelled` notification **MUST NOT** be used
// for task cancellation." The rule holds here BY CONSTRUCTION rather than by a special case: once a
// `CreateTaskResult` has been returned, the originating `tools/call` is complete, its id has already
// left `s.inflight`, and the lookup below finds nothing to cancel — so a `notifications/cancelled`
// naming it cancels nothing and, critically, never reaches the run's own context (which the task
// projects and which only `tasks/cancel` may signal).
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
		// A RESULT THAT WILL NOT MARSHAL STILL OWES THE CALLER A RESPONSE.
		//
		// Returning silently here would mean the client receives NOTHING for a request it
		// has an id outstanding for, blocking until its own timeout — a request that hangs is a
		// strictly worse failure than one that errors, because there is nothing to diagnose from.
		//
		// The path is reachable through the era envelope: `eraResult.MarshalJSON` returns a real
		// error for a payload that collides with a field the envelope owns. Two
		// writers of one field is a programming error and must stay loud (the envelope keeps
		// erroring), but "loud" has to mean an error the caller can see.
		//
		// A notification has no id and therefore no caller to answer; those still drop.
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

// Diagnosticf writes a SERVER-side diagnostic line to Diagnostics (never to the protocol stream). It is
// the only logging path a server should use for anything that must not be a client notification.
func (s *Server) Diagnosticf(format string, args ...any) {
	if s.Diagnostics == nil {
		return
	}
	fmt.Fprintf(s.Diagnostics, format+"\n", args...)
}
