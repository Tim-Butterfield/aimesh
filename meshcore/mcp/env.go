package mcp

import (
	"bytes"
	"encoding/json"

	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// This file defines the per-request protocol context. On the legacy era, `initialize` negotiates a
// version and later requests read session state; the 2026-07-28 revision has no session, and each
// request carries its own context in `params._meta`. RequestEnv holds that context per request, filled
// from session state on the legacy era and from `_meta` on the modern era, so everything downstream
// reads one shape. At legacy removal the legacy fill is deleted and RequestEnv remains.

// Era is which protocol revision family a request belongs to.
//
// SUNSET-PATH (MCP26-SUNSET): the type exists only while two eras coexist.
type Era string

// The protocol eras.
const (
	// EraLegacy is the session-scoped family (2024-11-05 / 2025-03-26 / 2025-06-18): initialize
	// negotiates once and later requests inherit that context. SUNSET-PATH (MCP26-SUNSET).
	EraLegacy Era = "legacy"
	// EraModern is the sessionless family (2026-07-28 and later): every request carries its own
	// protocol context in `params._meta`. See era.go for how a request selects it.
	EraModern Era = "modern"
)

// RootSource is the provenance of the trusted roots in force for a request, so a refusal can say why a
// path was refused without naming a path.
type RootSource string

// Root provenances.
const (
	// RootsUnknown is the zero value: the application supplied no provenance. It is distinct from
	// RootsExplicit, so unnamed roots are never reported as named.
	RootsUnknown RootSource = ""
	// RootsNone means there is no trusted root at all, so every filesystem path is refused.
	RootsNone RootSource = "none"
	// RootsExplicit means the application named the roots explicitly.
	RootsExplicit RootSource = "explicit"
)

// TrustContext is the confinement an application computes for one request: the effective trusted
// roots, their provenance, whether anything narrowed them, and the resolver that judges paths against
// them. Server.TrustFor supplies it; a server without TrustFor gets the zero resolver, which refuses
// every path.
type TrustContext struct {
	// Roots are the effective roots as the resolver canonicalized them, so what is reported and what
	// is enforced cannot differ.
	Roots []string
	// Source is the provenance of Roots.
	Source RootSource
	// Narrowed reports whether something reduced the operator's startup set for this request.
	Narrowed bool
	// Resolver judges every path this request names. nil is replaced with the zero resolver, which
	// refuses every path: an empty or uncomputable root set never degrades to "unrestricted".
	Resolver *scope.Resolver
}

// Trace carries W3C trace context through a request without interpreting it.
type Trace struct {
	TraceParent string
	TraceState  string
	Baggage     string
}

// RequestEnv is the protocol context of one request. It is built once per request, carried on the Call
// and read by everything downstream; handlers resolve paths through Trust.
type RequestEnv struct {
	// Method is the JSON-RPC method this env was built for.
	Method string

	// Era is which revision family governs this request. SUNSET-PATH (MCP26-SUNSET): the field goes
	// with the legacy era.
	Era Era
	// Version is the revision in force for this request: the version negotiated at initialize on the
	// legacy era, or `_meta.protocolVersion` on the modern era.
	Version string
	// ClientInfo is the peer's self-description. It is advisory; nothing branches on it.
	ClientInfo *Implementation
	// Capabilities is the client's raw capability declaration, carried for extensions that consult it.
	Capabilities json.RawMessage
	// Extensions are the client's declared protocol extensions by name, parsed from
	// `Capabilities.extensions`. They are per request, and always empty on the legacy era.
	Extensions map[string]json.RawMessage

	// LogLevel is the minimum log level in force when this request arrived, for diagnostics. It is
	// not the filter Call.Log applies, which reads the live session level.
	//
	// Legacy only. SUNSET-PATH (MCP26-SUNSET).
	LogLevel Level

	// ProgressToken is the caller's `_meta.progressToken`, echoed verbatim and never invented; nil
	// when the caller supplied none.
	ProgressToken json.RawMessage

	// Trace is W3C trace context, carried through and never interpreted. Filled by the modern era.
	Trace Trace
	// Retry reports that this request is a continuation carrying prior state rather than a fresh
	// call — the one case where a cacheable result must not be served from a cache. Filled by the
	// modern era.
	Retry bool

	// Roots are the effective trusted roots for this request; RootSource is their provenance;
	// Narrowed reports whether anything reduced the operator's startup set.
	Roots      []string
	RootSource RootSource
	Narrowed   bool
	// Trust is the resolver built from Roots and is never nil: a request with no roots carries the
	// zero resolver, which refuses every path. Handlers resolve paths only through it.
	Trust *scope.Resolver
}

// requestMeta is the `_meta` object a request may carry, one shape for every method. Which
// `io.modelcontextprotocol/*` keys are required on the modern era is enforced in era.go.
type requestMeta struct {
	ProgressToken json.RawMessage `json:"progressToken,omitempty"`

	ProtocolVersion    json.RawMessage `json:"io.modelcontextprotocol/protocolVersion,omitempty"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities,omitempty"`
	ClientInfo         json.RawMessage `json:"io.modelcontextprotocol/clientInfo,omitempty"`
	// LogLevel is parsed so a malformed `_meta` is detected, and is otherwise not read. See
	// MetaKeyLogLevel.
	LogLevel json.RawMessage `json:"io.modelcontextprotocol/logLevel,omitempty"`

	// W3C trace context, carried and never interpreted.
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
	Baggage     string `json:"baggage,omitempty"`
}

// paramsEnvelope is the part of any method's params read before the method's own decoding: the
// protocol context and the multi-round-trip retry fields.
type paramsEnvelope struct {
	Meta *requestMeta `json:"_meta,omitempty"`

	// InputResponses and RequestState mark a retried multi-round-trip request, whose result must not
	// be cached.
	InputResponses json.RawMessage `json:"inputResponses,omitempty"`
	RequestState   json.RawMessage `json:"requestState,omitempty"`
}

// modernEnv builds the context of an admitted modern request from values that arrived on the request.
func (s *Server) modernEnv(method string, m *modernMeta) *RequestEnv {
	env := &RequestEnv{
		Method:       method,
		Era:          EraModern,
		Version:      m.version,
		ClientInfo:   m.clientInfo,
		Capabilities: m.capabilities,
		Extensions:   m.extensions,
		// LogLevel stays zero: the modern era sends no log notifications.
		ProgressToken: m.progress,
		Trace:         m.trace,
		Retry:         m.retry,
	}
	s.fillTrust(env)
	return env
}

// fillTrust attaches the confinement for one request, fail-closed on every path.
func (s *Server) fillTrust(env *RequestEnv) {
	tc := TrustContext{}
	if s.TrustFor != nil {
		// The era is passed because an application's confinement rule depends on it.
		tc = s.TrustFor(env.Era)
	}
	env.Roots, env.RootSource, env.Narrowed, env.Trust = tc.Roots, tc.Source, tc.Narrowed, tc.Resolver
	if env.Trust == nil {
		// Fail closed: the zero resolver refuses every path.
		env.Trust = &scope.Resolver{}
	}
	if len(env.Roots) == 0 && env.RootSource == RootsUnknown {
		env.RootSource = RootsNone
	}
}

// envFor builds the legacy protocol context for one request from session state; modernEnv is the
// modern counterpart. The refusal result is always nil, because every `_meta` field is optional on the
// legacy era.
//
// SUNSET-PATH (MCP26-SUNSET): this whole function goes with the era, leaving modernEnv.
func (s *Server) envFor(method string, params json.RawMessage) (*RequestEnv, *rpcResponse) {
	env := &RequestEnv{Method: method, Era: EraLegacy}
	s.mu.Lock()
	env.Version = s.negotiated
	env.LogLevel = s.level
	s.mu.Unlock()
	if env.LogLevel == "" {
		env.LogLevel = LevelInfo
	}
	if len(params) > 0 {
		var p paramsEnvelope
		// An unreadable `_meta` is tolerated, since every field is optional on the legacy era.
		if json.Unmarshal(params, &p) == nil && p.Meta != nil {
			if len(p.Meta.ProgressToken) > 0 && string(bytes.TrimSpace(p.Meta.ProgressToken)) != "null" {
				env.ProgressToken = p.Meta.ProgressToken
			}
		}
	}
	s.fillTrust(env)
	return env, nil
}
