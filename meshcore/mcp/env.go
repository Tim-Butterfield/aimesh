package mcp

import (
	"bytes"
	"encoding/json"

	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// This file is the per-request protocol context — the ONE structural change the 2026-07-28 migration
// turns on.
//
// The problem it answers: this transport is a SESSION server. `initialize` negotiates a version once,
// `notifications/initialized` opens the session, and everything downstream reads session-scoped state
// (the negotiated version, the client's log level, whether it declared roots). The 2026-07-28 revision
// DELETES the session — every request carries its own protocol context in `params._meta`. The fix is
// not a second server: it is to move the context from the session to the request, construct one
// RequestEnv per request, and then have two ways of FILLING it — from session state (legacy) or from
// `params._meta` plus per-call narrowing arguments (modern). Everything downstream stays single-path.
//
// So this envelope is the POST-SUNSET architecture, arrived at early: at legacy removal the Era field,
// the legacy fill and the session plumbing are deleted and RequestEnv survives as the only context
// object.

// Era is which protocol revision family a request belongs to.
//
// SUNSET-PATH (MCP26-SUNSET; migration design §16.2). The Era field exists only for the
// transition window: once the legacy era is removed there is one era, and the field goes with it.
type Era string

const (
	// EraLegacy is the session-scoped family (2024-11-05 / 2025-03-26 / 2025-06-18): initialize
	// negotiates once and later requests inherit that context. SUNSET-PATH (MCP26-SUNSET).
	EraLegacy Era = "legacy"
	// EraModern is the sessionless family (2026-07-28 and later): every request carries its own
	// protocol context in `params._meta`. Reachable: see era.go for how a request selects it.
	EraModern Era = "modern"
)

// RootSource is the PROVENANCE of the trusted roots in force for a request. It is what lets a refusal
// and a readiness projection say *why* a path was refused without naming any path.
type RootSource string

const (
	// RootsUnknown is the zero value: provenance has not been plumbed through to this request yet.
	// It is deliberately distinct from RootsExplicit — claiming "a human named these roots" when the
	// question has not been asked would be exactly the silent-degradation shape this repo refuses.
	// A request whose roots have been resolved by ResolveTrustedRoots carries a real answer instead.
	RootsUnknown RootSource = ""
	// RootsNone means there is no trusted root at all, so every filesystem path is refused.
	RootsNone RootSource = "none"
	// RootsExplicit means a human named the roots (`--root`).
	RootsExplicit RootSource = "explicit"
	// RootsInferredCwd means the roots were inferred from the launch directory rather than named.
	// Under the modern era that is not a trusted root unless the operator waived it explicitly.
	RootsInferredCwd RootSource = "inferred_cwd"
)

// TrustContext is the confinement an application computes for ONE request: the effective trusted
// roots, where they came from, whether anything narrowed them, and the resolver that judges paths
// against them.
//
// It is a callback rather than a field because this package is domain-free: it knows nothing about
// what a root MEANS to an application, only that an application may want every request judged against
// its own set. A server whose tools take no filesystem paths supplies nothing and gets the zero
// resolver, which refuses everything — the honest answer for a server that should never be asked.
type TrustContext struct {
	// Roots are the effective roots AS THE RESOLVER CANONICALIZED THEM, so a caller reporting them
	// and the resolver enforcing them cannot describe different directories.
	Roots []string
	// Source is the provenance of Roots.
	Source RootSource
	// Narrowed reports whether something reduced the operator's startup set for this request.
	Narrowed bool
	// Resolver judges every path this request names. nil is replaced with the zero resolver, which
	// refuses every path: an empty or uncomputable root set never degrades to "unrestricted".
	Resolver *scope.Resolver
}

// Trace carries W3C trace context through a request untouched. It is CARRIED, never interpreted:
// correlating a request across processes is the client's business, and a server that parsed these
// would be inventing a meaning for a field it does not own.
type Trace struct {
	TraceParent string
	TraceState  string
	Baggage     string
}

// RequestEnv is the protocol context of ONE request. It is constructed once per request by
// Server.envFor, carried on the Call, and read by everything downstream — including the handler,
// which resolves paths through Trust rather than through any process-global resolver.
//
// Each field documents what FILLS it. A field nothing fills yet is the zero value and is documented as
// such rather than quietly repurposed.
type RequestEnv struct {
	// Method is the JSON-RPC method this env was built for. Under the modern era the required
	// `_meta` fields are validated per method, so the method has to be part of the env's own record
	// of what it is.
	Method string

	// Era is which revision family governs this request. SUNSET-PATH (MCP26-SUNSET): at legacy removal there is one
	// era and the field goes with it.
	Era Era
	// Version is the revision in force for THIS request. Under legacy it is the version negotiated
	// at initialize; under modern it is `_meta.protocolVersion`.
	Version string
	// ClientInfo is the peer's self-description. ADVISORY ONLY — nothing branches on it. A server
	// that changed behavior per client would be a server whose behavior is not documented anywhere.
	ClientInfo *Implementation
	// Capabilities is the client's raw capability declaration. We require nothing from it on any
	// core method; it is carried so a capability-gated extension can consult it without a second
	// parse.
	Capabilities json.RawMessage
	// Extensions is the client's declared protocol extensions, by name, parsed out of
	// `Capabilities.extensions` (`basic/versioning` §Extension Negotiation: "Extensions are
	// advertised in the `extensions` field of capabilities").
	//
	// It is PER-REQUEST, which is the whole point: an extension is opted into on every request, so
	// a server that cached the answer would be relying on prior connection state that the modern
	// revision deleted. Always empty on the legacy era — a 2025-06-18 client cannot declare a
	// 2026-07-28 extension, and DeclaredExtension refuses to pretend otherwise.
	Extensions map[string]json.RawMessage

	// LogLevel is the minimum log level in force WHEN THIS REQUEST ARRIVED.
	//
	// It is deliberately NOT the filter Call.Log applies on the legacy era — see the comment on
	// Call.Log. `logging/setLevel` is a LIVE session control, and a run that streams notifications
	// for minutes has to honour a level the client raises while it runs; a per-request snapshot
	// would silently ignore it. This field records what was in force at arrival (for diagnostics,
	// and for the modern era, where the level is a per-request `_meta` field with no live channel to
	// update it — and where we emit no notifications/message at all, so nothing reads it).
	//
	// LEGACY-ONLY and SUNSET-PATH (MCP26-SUNSET): logging is deprecated in 2026-07-28 and its named stdio migration
	// is stderr, which this server already does.
	LogLevel Level

	// ProgressToken is the caller's `_meta.progressToken`, echoed verbatim and never invented: a
	// progress notification without the caller's own token is not merely unwanted, it is unroutable.
	// nil when the caller volunteered none.
	ProgressToken json.RawMessage

	// Trace is W3C trace context, carried through and never interpreted. Filled by the modern era.
	Trace Trace
	// Retry reports that this request is a continuation carrying prior state rather than a fresh
	// call — the one case where a cacheable result must not be served from a cache. Filled by the
	// modern era.
	Retry bool

	// Roots are the EFFECTIVE trusted roots for THIS request; RootSource is their provenance;
	// Narrowed reports whether anything reduced the operator's startup set.
	Roots      []string
	RootSource RootSource
	Narrowed   bool
	// Trust is the resolver built from Roots. It is NEVER nil: a request with no roots carries the
	// zero resolver, which refuses every path. Handlers resolve through this and through nothing
	// else — that is the mechanical change that makes per-request confinement enforceable rather
	// than aspirational, because after it there is no reachable path from a handler to a
	// process-global resolver.
	Trust *scope.Resolver
}

// requestMeta is the `_meta` object a request may carry — ONE shape for every method, which is what
// the modern era requires ("Every request supplies this metadata in its `_meta` field") and what the
// legacy era can live with, since every field in it is optional there.
//
// The four `io.modelcontextprotocol/*` keys are the per-request protocol contract of `basic/index`.
// Two are required on a modern request and two are not; which is which is enforced in era.go, not
// here, because a struct field cannot express "required only on one era".
type requestMeta struct {
	ProgressToken json.RawMessage `json:"progressToken,omitempty"`

	ProtocolVersion    json.RawMessage `json:"io.modelcontextprotocol/protocolVersion,omitempty"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities,omitempty"`
	ClientInfo         json.RawMessage `json:"io.modelcontextprotocol/clientInfo,omitempty"`
	// LogLevel is parsed so that a malformed `_meta` is not mistaken for a well-formed one, and is
	// then DELIBERATELY NOT READ on either era. See MetaKeyLogLevel.
	LogLevel json.RawMessage `json:"io.modelcontextprotocol/logLevel,omitempty"`

	// W3C trace context. CARRIED, never interpreted: correlating a request across processes is the
	// client's business, and a server that parsed these would be inventing a meaning for a field it
	// does not own. `basic/index` reserves these three keys without the vendor prefix.
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
	Baggage     string `json:"baggage,omitempty"`
}

// paramsEnvelope is the part of ANY method's params this transport reads before the method's own
// decoding: the protocol context, and the two MRTR fields whose presence forbids caching the result.
type paramsEnvelope struct {
	Meta *requestMeta `json:"_meta,omitempty"`

	// InputResponses / RequestState mark a retried multi-round-trip request. This transport produces
	// no InputRequiredResult, so neither can currently arrive; they are read here anyway so that
	// `server/utilities/caching`'s non-caching rule is a property of the REQUEST rather than of a
	// future code path someone remembers to add it to.
	InputResponses json.RawMessage `json:"inputResponses,omitempty"`
	RequestState   json.RawMessage `json:"requestState,omitempty"`
}

// modernEnv builds the per-request context of an ADMITTED modern request — one that has already
// passed every validation step and won (or matched) the era latch.
//
// The contrast with the legacy fill below is the whole architecture: there, every value comes from
// session state the handshake established; here, every value came in on this request.
func (s *Server) modernEnv(method string, m *modernMeta) *RequestEnv {
	env := &RequestEnv{
		Method:       method,
		Era:          EraModern,
		Version:      m.version,
		ClientInfo:   m.clientInfo,
		Capabilities: m.capabilities,
		Extensions:   m.extensions,
		// LogLevel is deliberately left at its zero value. On the modern era it is never populated and
		// Call.Log is a no-op: this server emits no `notifications/message` at all there.
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
		// The era travels with the question. An application's confinement rule differs between the
		// era that has `roots/list` and the era that deleted it, and this is the only point that
		// knows which one asked.
		tc = s.TrustFor(env.Era)
	}
	env.Roots, env.RootSource, env.Narrowed, env.Trust = tc.Roots, tc.Source, tc.Narrowed, tc.Resolver
	if env.Trust == nil {
		// FAIL-CLOSED, always. The zero resolver refuses every path with a named reason; there is no
		// value of this field that means "unrestricted".
		env.Trust = &scope.Resolver{}
	}
	if len(env.Roots) == 0 && env.RootSource == RootsUnknown {
		env.RootSource = RootsNone
	}
}

// envFor builds the LEGACY protocol context for one request, filled from session state — which is why
// that era is invisible on the wire: every value it carries is the value the handler would have read
// from the Server itself. The modern counterpart is modernEnv, above.
//
// It still returns a refusal slot so that the two builders have the same shape at every call site;
// under the legacy era it is always nil, because every `_meta` field is optional there.
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
		// Best-effort: a `_meta` this transport cannot parse is not a malformed request under the
		// legacy era, where every field in it is optional. The modern era's required-field check is
		// in era.go, and it is the only place a parse failure is fatal.
		if json.Unmarshal(params, &p) == nil && p.Meta != nil {
			if len(p.Meta.ProgressToken) > 0 && string(bytes.TrimSpace(p.Meta.ProgressToken)) != "null" {
				env.ProgressToken = p.Meta.ProgressToken
			}
		}
	}
	s.fillTrust(env)
	return env, nil
}
