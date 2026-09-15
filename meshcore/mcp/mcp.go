// Package mcp is a domain-free Model Context Protocol (MCP) stdio server: JSON-RPC 2.0 over the
// newline-delimited framing shared with meshcore/acp.
//
// It serves two protocol eras. A process commits to one era with an atomic latch on the first request
// it serves (era.go); Server.Protocol (`--protocol dual|legacy`) sets the launch posture.
//
//   - Legacy (`2025-06-18`, `2025-03-26`, `2024-11-05`) is session-based: `initialize` and
//     `notifications/initialized`, `tools/list` with cursor paging, `tools/call`, `ping`,
//     `logging/setLevel`, progress and cancellation notifications, `resources/*` when the application
//     supplies a provider, and server-to-client `roots/list` requests (outgoing.go).
//   - Modern (`2026-07-28`) is sessionless: `server/discover`, per-request protocol context in
//     `params._meta` (env.go), result metadata on every result (result.go), content-addressed resource
//     URIs, and the opt-in tasks extension (tasks.go). It does not implement `ping` or
//     `logging/setLevel`, sends no `notifications/message`, and sends no server-to-client requests,
//     which that revision's stdio transport forbids.
//
// Applications register tools with their own JSON Schemas and handlers, so the package carries no
// application vocabulary; the boundary check exempts only `task`, which the tasks extension names.
//
// Protocol positions:
//
//   - The protocol version is negotiated and echoed: a supported version is echoed verbatim and any
//     other is refused with the supported list, so a mismatch never hides inside a successful handshake.
//   - Legacy only: requests other than `ping` are refused until `notifications/initialized` arrives.
//   - Progress is sent only for a caller-supplied `_meta.progressToken`, and never goes backwards.
//   - Cancellation cancels the handler's context by request id, and the cancelled request gets no
//     response, so handlers must record their outcome elsewhere.
//   - Legacy only: log notifications are best effort and never the only carrier of a fact. The modern
//     era sends none; diagnostics go to stderr.
//   - A JSON-RPC batch array is refused as one invalid request.
//   - The server writes only JSON-RPC frames to its output stream, and server-side logging goes to
//     Diagnostics. Keeping child process output off that stream is the caller's responsibility.
//   - Legacy only: a server-to-client request is advisory. `roots/list` is sent only to a client that
//     declared the capability, has a deadline, and on any failure leaves application state unchanged.
//   - A resource is a registration, not a path: `resources/read` resolves only URIs the server minted,
//     and no URI discloses a host path.
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

// LatestProtocolVersion is the newest legacy version, reported to an `initialize` with no usable
// protocolVersion. The modern revision is excluded because it has no handshake.
//
// SUNSET-PATH (MCP26-SUNSET): goes with `initialize`.
const LatestProtocolVersion = ProtocolVersion20250618

// SupportedProtocolVersions is the set `initialize` echoes, newest first. The modern revision is
// reached through per-request `_meta` instead; SupportedVersions (era.go) is the full advertised set.
//
// SUNSET-PATH (MCP26-SUNSET).
var SupportedProtocolVersions = []string{ProtocolVersion20250618, ProtocolVersion20250326, ProtocolVersion20241105}

// JSON-RPC 2.0 error codes and the MCP-range codes this transport uses. Protocol errors are for
// malformed requests, unknown methods and tools, and pre-initialization calls. A domain failure is a
// successful response with CallToolResult.IsError, because clients often drop `error.data`.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	// CodeNotInitialized is returned for a non-ping request that arrives before
	// `notifications/initialized`. It is legacy only, so the modern era, which must not emit
	// -32002, never reaches it.
	//
	// SUNSET-PATH (MCP26-SUNSET).
	CodeNotInitialized = -32002

	// CodeMissingClientCapability is MissingRequiredClientCapabilityError, returned when a request
	// needs a client capability it did not declare; its data lists the missing capabilities.
	CodeMissingClientCapability = -32021
	// CodeUnsupportedProtocolVersion is UnsupportedProtocolVersionError, carrying `data.supported`
	// and `data.requested`. Modern clients key on it, so it is used for nothing else.
	CodeUnsupportedProtocolVersion = -32022
)

// Implementation identifies a peer (the `serverInfo` / `clientInfo` object).
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// ToolAnnotations are advisory behavior hints a client may use to decide whether to confirm a call;
// they are not an authorization boundary. DestructiveHint and OpenWorldHint are pointers because their
// spec default is true.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

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

// Content is one block of a tool result's unstructured rendering. Some clients show the model only
// this channel, so every result carries one. A block is `text` or `resource_link`, a pointer to a
// resource fetched with `resources/read`, used for artifacts too large to inline without truncation.
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
// IsError reports a domain failure: a successful JSON-RPC response whose content says what went wrong.
// Protocol errors are for malformed requests only.
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

// RequestError is a handler-returned error sent as a JSON-RPC protocol error rather than a tool result,
// for malformed requests such as unparseable arguments. Any other handler error becomes an IsError
// tool result.
type RequestError struct {
	Code    int
	Message string
	Data    any
}

// Error returns the message.
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
