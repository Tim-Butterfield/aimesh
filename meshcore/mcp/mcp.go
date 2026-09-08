// Package mcp is meshcore's Model Context Protocol (MCP) stdio transport: a hand-rolled, domain-free
// JSON-RPC 2.0 server over the newline-delimited framing MCP hosts speak (reused from meshcore/acp, which
// already implements both that framing and the LSP-style Content-Length variant).
//
// IT IS DUAL-ERA, and every era-specific statement below says which era it belongs to. A process serves
// one protocol family for its lifetime and commits to it with an atomic latch on the first request
// (era.go); `Server.Protocol` (`--protocol dual|legacy`) sets the launch posture.
//
//   - LEGACY — `2025-06-18`, `2025-03-26`, `2024-11-05`. Session-based: lifecycle
//     (initialize → notifications/initialized), `tools/list` with real cursor paging, `tools/call`,
//     `ping`, `logging/setLevel`, `notifications/progress`, `notifications/cancelled`, `resources/*`
//     when an application supplies a provider, and the OTHER direction — server→client requests, which
//     is what makes `roots/list` possible at all (see outgoing.go).
//   - MODERN — `2026-07-28`. Sessionless: `server/discover`, the per-request protocol context in
//     `params._meta` (env.go), `resultType` + `ttlMs`/`cacheScope` + `_meta.serverInfo` on every result
//     (result.go), content-addressed resource URIs, and the opt-in `io.modelcontextprotocol/tasks`
//     extension (tasks.go). It deliberately does NOT implement `ping` or `logging/setLevel` (both
//     `-32601`), emits NO `notifications/message` at all, and issues NO server→client requests —
//     `Server.Request` refuses there, because that revision's stdio page forbids writing a JSON-RPC
//     request to stdout.
//
// It knows nothing about what any tool DOES, and nothing about what a root or an artifact MEANS — every
// tool is registered by an application with its own JSON Schemas and handler, roots are handed straight
// back through a callback, and a resource is a registration the application made. So this package
// carries no app vocabulary and the meshcore boundary check stays green — with one narrow, documented
// exemption: `task` is permitted HERE, because the `2026-07-28` tasks extension is what names it (see
// `protocolExemptions` in scripts/boundarycheck).
//
// The deliberate protocol positions (all of them are requirements settled in the MCP surface design, not
// preferences):
//
//   - protocolVersion is NEGOTIATED AND ECHOED. A version this server implements is echoed back verbatim;
//     anything else is a clean refusal naming the supported versions. The spec's alternative — answer with
//     your own latest and let the client decide — hides a mismatch inside a successful handshake, which is
//     exactly the silent-degradation shape the rest of this codebase refuses.
//   - LEGACY ONLY: requests other than `ping` are REFUSED before the client's
//     `notifications/initialized` arrives. The modern era has no pre-initialization state to be in.
//   - Progress notifications are emitted ONLY when the caller supplied `_meta.progressToken`; the token is
//     echoed, never invented, and progress is monotonic (a fraction of phases complete, not a per-item
//     counter that can go backwards when a phase is retried).
//   - Cancellation correlates by JSON-RPC request id, cancels the handler's context, and sends NO response
//     for the cancelled request — a SHOULD NOT on 2025-06-18 (with clients told to ignore a late response
//     anyway), tightened to a MUST NOT covering ANY further message on 2026-07-28 stdio. Either way, a
//     handler must make its own receipt durable elsewhere.
//   - LEGACY ONLY: log notifications are BEST-EFFORT BY CONTRACT — dropped below the client's level, and
//     never the only carrier of a governance-relevant fact. On the MODERN era there are none at all:
//     the Logging feature is deprecated in `2026-07-28`, its named stdio migration is stderr (which
//     Diagnostics already is), so `Call.Log` is a no-op there and no `logging` capability is advertised.
//     The "never the only carrier" rule is what makes that loss bounded, and declining the channel turns
//     the rule into a structural fact.
//   - A JSON-RPC ARRAY on the wire does not crash the reader (batching existed in 2025-03-26 and was
//     removed in 2025-06-18), it is refused as one invalid request.
//   - STDOUT PURITY: this server writes JSON-RPC frames and nothing else to its output stream. A server
//     that spawns model CLIs must additionally keep child stdio off that stream; this package guarantees
//     only its own half, and exposes Diagnostics so server-side logging has an explicit non-stdout home.
//   - LEGACY ONLY: A SERVER→CLIENT REQUEST IS ADVISORY, NEVER LOAD-BEARING. `roots/list` is asked only
//     of a client that declared the capability, it carries a deadline, and every failure path —
//     refusal, timeout, an unreadable answer — leaves the application's existing state untouched and is
//     written to Diagnostics. A transport that could quietly change a server's confinement by failing
//     would be a worse thing than one that cannot ask at all. On the MODERN era there is no such
//     request: a client narrows the server through a tool argument instead.
//   - A RESOURCE IS A REGISTRATION, NOT A PATH. `resources/read` resolves a URI the server itself
//     minted against a table the application populated. There is no request shape that reaches a file
//     nobody published, and no URI this package mints discloses a host filesystem path.
package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Protocol versions this transport implements. They are the wire strings, in descending preference order.
const (
	ProtocolVersion20250618 = "2025-06-18"
	ProtocolVersion20250326 = "2025-03-26"
	ProtocolVersion20241105 = "2024-11-05"
)

// LatestProtocolVersion is the newest LEGACY version, and the one an `initialize` with no usable
// protocolVersion is told about. It is deliberately NOT the newest revision this transport implements:
// `initialize` is a legacy method, and answering it with a version that has no handshake would name a
// contract the caller cannot reach through the call it just made.
//
// SUNSET-PATH (MCP26-SUNSET): goes with `initialize`.
const LatestProtocolVersion = ProtocolVersion20250618

// SupportedProtocolVersions is the exact set `initialize` will echo, newest first. The modern
// revision is deliberately absent: it is reached through per-request `_meta`, never through the
// handshake. The full cross-era set this server ADVERTISES is SupportedVersions (era.go).
//
// SUNSET-PATH (MCP26-SUNSET).
var SupportedProtocolVersions = []string{ProtocolVersion20250618, ProtocolVersion20250326, ProtocolVersion20241105}

// JSON-RPC 2.0 error codes plus the one MCP-range code this transport uses. Protocol errors are reserved
// for MALFORMED requests, unknown methods/tools and pre-initialization calls — a domain halt rides a
// successful response carrying CallToolResult{IsError: true} instead (clients routinely flatten or drop
// `error.data`, which would lose a halt taxonomy exactly when the model needs it).
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	// CodeNotInitialized is returned for a non-ping request that arrives before the client's
	// `notifications/initialized`.
	//
	// LEGACY-ONLY, and SUNSET-PATH (MCP26-SUNSET). The modern era has no pre-initialization
	// state to be in, so this code is unreachable there — which matters, because `basic/index`
	// §Error Codes says implementations of 2026-07-28 "MUST NOT emit" `-32002` (it meant "resource
	// not found" in 2025-11-25 and earlier and is replaced by `-32602`). It is NOT renumbered: doing
	// so would change wire-visible behavior for the legacy clients that are, during the sunset
	// window, the only clients there are — in order to tidy a path already scheduled for deletion.
	CodeNotInitialized = -32002

	// CodeMissingClientCapability is `MissingRequiredClientCapabilityError`. `basic/index`: "If
	// processing a request requires a capability the client did not include in
	// `io.modelcontextprotocol/clientCapabilities`, the server MUST return a
	// MissingRequiredClientCapabilityError (`-32021`) whose `data.requiredCapabilities` lists the
	// missing capabilities." No CORE method of this transport requires a client capability, so it is
	// defined and not emitted; an extension is where it can first fire.
	CodeMissingClientCapability = -32021
	// CodeUnsupportedProtocolVersion is `UnsupportedProtocolVersionError`, carrying
	// `data.supported` + `data.requested`. It is the one RECOGNIZED MODERN ERROR a probing client
	// keys on, which is why it is never used for anything else.
	CodeUnsupportedProtocolVersion = -32022
)

// Implementation identifies a peer (the `serverInfo` / `clientInfo` object).
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// ToolAnnotations are the ADVISORY behavior hints a client may use to decide whether to confirm a call.
// Nothing server-side keys off them: they coordinate with the host's permission layer, they are not an
// authorization boundary. DestructiveHint/OpenWorldHint are pointers because their spec defaults are true
// — omitting them means "true", so a tool that is genuinely closed-world must say so explicitly.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Bool returns a pointer to b — for the ToolAnnotations fields whose spec default is true.
func Bool(b bool) *bool { return &b }

// Tool is one declared tool: its name, human framing, and the JSON Schemas of its arguments and of the
// structuredContent it returns. Schemas are raw JSON so an application owns them literally (strict
// `oneOf`, `additionalProperties: false`, `required` — none of which survive a round trip through a
// reflection-derived schema).
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  json.RawMessage  `json:"inputSchema"`
	OutputSchema json.RawMessage  `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
}

// Content is one block of a tool result's unstructured rendering. Some clients show the model ONLY this
// channel, which is why every result carries one even when the machine-readable payload is in
// StructuredContent.
//
// Two block types are produced: `text`, and `resource_link` — a POINTER to something the caller can
// fetch with `resources/read`. The link exists for exactly one shape of result: an artifact too large to
// inline honestly. A truncated-but-syntactically-valid patch reads as complete, which is worse than a
// reference, so the reference is what travels. Every non-text field is omitempty, so a text block's
// encoding is byte-identical to what it was before resource links existed.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// resource_link fields.
	URI         string `json:"uri,omitempty"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// Text builds a text content block.
func Text(s string) Content { return Content{Type: "text", Text: s} }

// ResourceLink builds a `resource_link` content block pointing at a resource this server publishes. The
// URI is opaque and run-scoped (see ResourceURI): it names what to fetch, never where it lives.
func ResourceLink(r Resource) Content {
	return Content{
		Type: "resource_link", URI: r.URI, Name: r.Name, Title: r.Title,
		Description: r.Description, MimeType: r.MimeType,
	}
}

// CallToolResult is the `tools/call` result: the human-readable rendering, the optional machine-readable
// payload (which must validate against the tool's declared OutputSchema), and the tool-level error flag.
//
// IsError is how a DOMAIN failure is reported — a successful JSON-RPC response whose content says what
// went wrong. Protocol errors are for malformed requests only.
type CallToolResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// Result builds a successful CallToolResult from a human rendering plus the structured payload.
func Result(text string, structured any) *CallToolResult {
	return &CallToolResult{Content: []Content{Text(text)}, StructuredContent: structured}
}

// ErrorResult builds a CallToolResult with IsError set: the human rendering AND the structured payload
// both travel, because a client that shows the model only text must still learn what failed.
func ErrorResult(text string, structured any) *CallToolResult {
	return &CallToolResult{Content: []Content{Text(text)}, StructuredContent: structured, IsError: true}
}

// RequestError is a handler-returned error that must ride as a JSON-RPC PROTOCOL error rather than as a
// tool result — the malformed-request case (unparseable arguments, a field the schema forbids). Anything
// else a handler returns becomes an IsError tool result instead.
type RequestError struct {
	Code    int
	Message string
	Data    any
}

func (e *RequestError) Error() string { return e.Message }

// InvalidParams builds the -32602 protocol error for a malformed request. The message is the place for a
// field-addressed, teaching correction — it is what the calling model reads.
func InvalidParams(format string, args ...any) *RequestError {
	return &RequestError{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// Level is an MCP logging level (RFC 5424 severities, as the spec names them).
type Level string

// The eight logging levels, least to most severe.
const (
	LevelDebug     Level = "debug"
	LevelInfo      Level = "info"
	LevelNotice    Level = "notice"
	LevelWarning   Level = "warning"
	LevelError     Level = "error"
	LevelCritical  Level = "critical"
	LevelAlert     Level = "alert"
	LevelEmergency Level = "emergency"
)

// levelRank orders the levels for `logging/setLevel` filtering (higher = more severe).
var levelRank = map[Level]int{
	LevelDebug: 0, LevelInfo: 1, LevelNotice: 2, LevelWarning: 3,
	LevelError: 4, LevelCritical: 5, LevelAlert: 6, LevelEmergency: 7,
}

// ValidLevel reports whether s names one of the eight spec levels.
func ValidLevel(s string) bool {
	_, ok := levelRank[Level(strings.ToLower(strings.TrimSpace(s)))]
	return ok
}

// LevelNames lists the accepted level names, least to most severe — for a teaching error.
func LevelNames() []string {
	return []string{
		string(LevelDebug), string(LevelInfo), string(LevelNotice), string(LevelWarning),
		string(LevelError), string(LevelCritical), string(LevelAlert), string(LevelEmergency),
	}
}

// --- JSON-RPC envelopes ---

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

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
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

// canonicalID normalizes a JSON-RPC id to a comparable key, so `notifications/cancelled` correlates a
// request id regardless of the whitespace or number formatting the client used. An id that is not valid
// JSON falls back to its literal bytes.
func canonicalID(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}
