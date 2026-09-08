package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// This file is where the MODERN ERA BECOMES REACHABLE: the supported-version set, the per-request
// `_meta` contract, the ERA LATCH, and `server/discover`.
//
// The rest of the modern machinery is invisible on the wire by construction — the envelope, the result
// builder's modern branch and the per-request resolver are all unreachable until a modern protocol
// version is accepted somewhere. This file accepts one, and therefore it is the file where a mistake is
// wire-visible.
//
// THE ORDER IS THE WHOLE DESIGN. A request latches the process's era only after it has passed every
// step that could reject it — envelope, method, required `_meta`, supported version, params — and the
// latch itself is a single atomic compare-and-set. The two failures that order prevents are concrete:
// one malformed request permanently selecting the era for a well-formed caller that follows it, and
// two openers racing with no defined outcome. `-32022` in particular NEVER latches: a client that
// probed with a version we do not implement is told what we do implement, and its RETRY is the request
// that chooses the era.
//
// SUNSET-PATH (MCP26-SUNSET; migration design §16.2): at legacy removal the latch, the era
// mismatch refusals and `--protocol legacy` are deleted and the process becomes an unconditionally
// stateless modern server — which is the shape `basic/index` §Statelessness wants.

// ProtocolVersion20260728 is the first MODERN revision: no `initialize` handshake, every request
// carrying its own protocol context in `params._meta`.
const ProtocolVersion20260728 = "2026-07-28"

// SupportedVersions is EVERY revision this transport implements, modern first and newest first within
// each era. It is what `server/discover` advertises and what every `-32022` carries in
// `data.supported` — one list, one order, two readers, so the two channels cannot disagree.
//
// It advertises legacy revisions deliberately. `server/discover` §Data Types defines the field with no
// era qualifier ("Protocol versions the server supports. The client should choose one of these for
// subsequent requests."), and `basic/versioning`'s own `UnsupportedProtocolVersionError` example lists
// `["2026-07-28", "2025-11-25"]` — a legacy revision, in a supported list, in the specification's own
// example. Modern-first ordering is the one signal available to make the right choice obvious.
//
// SUNSET-PATH (MCP26-SUNSET): at legacy removal this shrinks to the modern entry alone.
var SupportedVersions = []string{
	ProtocolVersion20260728,
	ProtocolVersion20250618,
	ProtocolVersion20250326,
	ProtocolVersion20241105,
}

// modernVersions are the revisions in SupportedVersions that are MODERN. A request naming a version
// from the legacy tail is refused with `-32022` rather than served: `_meta`-carried protocol context is
// a modern concept, and answering it under a legacy contract would be a silent downgrade.
var modernVersions = map[string]bool{ProtocolVersion20260728: true}

// The reserved `_meta` keys of the per-request protocol contract, from `basic/index` §`_meta`. Two are
// REQUIRED on every request ("fields marked as required MUST be included on every request"); the other
// two are not.
const (
	// MetaKeyProtocolVersion is required.
	MetaKeyProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	// MetaKeyClientCapabilities is required.
	MetaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	// MetaKeyClientInfo is NOT required (clients SHOULD send it). Advisory only — nothing branches on
	// it, because `basic/index` says implementations "SHOULD NOT use them to change the behavior of
	// the client or server".
	MetaKeyClientInfo = "io.modelcontextprotocol/clientInfo"
	// MetaKeyLogLevel is NOT required, and on the modern era it is ACCEPTED AND IGNORED. The Logging
	// feature is Deprecated as of 2026-07-28 and its named stdio migration — `stderr` — is what this
	// server already does, so there is no per-request log channel here to select a level for. See
	// Call.Log.
	MetaKeyLogLevel = "io.modelcontextprotocol/logLevel"
)

// ProtocolMode is the era posture a server process was LAUNCHED with.
//
// SUNSET-PATH (MCP26-SUNSET): the whole type, both modes and the flag that sets it are removed
// with the legacy era.
type ProtocolMode string

const (
	// ProtocolDual (the default) serves whichever era the client opens with, once, per the latch.
	ProtocolDual ProtocolMode = "dual"
	// ProtocolLegacy makes the process a pre-2026-07-28 server in every observable respect: no modern
	// `_meta` is parsed, no modern method is reachable, and `server/discover` is an unknown method
	// answered with -32601 and no `result` member.
	//
	// That last point is the mode's whole reason for existing, and it is not an oversight. Per
	// `basic/transports/stdio` §Backward Compatibility a `DiscoverResult` OF ANY CONTENT identifies a
	// modern server, and a dual-era client receiving one "**MUST NOT**" fall back to `initialize` —
	// it would look for a mutually supported modern version, find none it could use, and fail, with
	// this process's perfectly good legacy surface sitting right there unreachable. Answering the
	// probe with an ordinary method-not-found is what puts us in the specification's own
	// "Dual-era client / Legacy server" row, whose outcome is **Works**.
	ProtocolLegacy ProtocolMode = "legacy"
)

// ValidProtocolMode reports whether s names a launch mode.
func ValidProtocolMode(s string) bool {
	switch ProtocolMode(strings.ToLower(strings.TrimSpace(s))) {
	case ProtocolDual, ProtocolLegacy, "":
		return true
	}
	return false
}

// Mode returns the effective launch mode ("" means dual).
func (s *Server) Mode() ProtocolMode {
	if s.Protocol == ProtocolLegacy {
		return ProtocolLegacy
	}
	return ProtocolDual
}

// --- the era latch ---

const (
	latchUnlatched int32 = 0
	latchLegacy    int32 = 1
	latchModern    int32 = 2
)

// era reports the era this process has latched into, or "" while it is still unlatched.
func (s *Server) era() Era {
	switch s.latched.Load() {
	case latchLegacy:
		return EraLegacy
	case latchModern:
		return EraModern
	}
	return ""
}

// latchEra is the ONE writer of the era cell, and it writes by compare-and-set. It returns the era in
// force AFTERWARDS, which is `want` for the winner and the already-latched era for a loser.
//
// A LOSER DOES NOT ERROR ON THE RACE. It re-reads: if the era that won is its own, it is served
// normally — two concurrent modern openers both succeed, which is the common case for a host that
// pipelines its first two calls. Only a loser of the OTHER era is refused, and it is refused with the
// same teaching error it would have received had it arrived a millisecond later. There is no
// read-then-write window in which two openers can both believe they latched.
func (s *Server) latchEra(want Era) Era {
	w := latchLegacy
	if want == EraModern {
		w = latchModern
	}
	if s.latched.CompareAndSwap(latchUnlatched, w) {
		return want
	}
	return s.era()
}

// --- per-request `_meta`: the modern protocol context ---

// modernMeta is the parsed, VALIDATED per-request protocol context of one modern request.
type modernMeta struct {
	version      string
	capabilities json.RawMessage
	// extensions is `capabilities.extensions`, parsed once here so that a capability-gated
	// extension consults one parse rather than re-reading the raw object per check.
	extensions map[string]json.RawMessage
	clientInfo *Implementation
	progress   json.RawMessage
	trace      Trace
	retry      bool
}

// parseModernMeta applies steps 3 and 4 of the admission order to one request's params: the required
// `_meta` fields, then the requested version.
//
// It returns the refusal rather than an error value because both failures have exactly one wire form
// each, and threading them through a second decision point is how the two come to disagree.
func (s *Server) parseModernMeta(id json.RawMessage, params json.RawMessage) (*modernMeta, *rpcResponse) {
	var p paramsEnvelope
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			// A `_meta` this transport cannot parse cannot carry the required fields, so this is the
			// same failure as omitting them. `basic/index`: "A request missing any required field is
			// malformed; the server MUST reject it with JSON-RPC error code -32602 (Invalid params)."
			return nil, s.missingMetaRefusal(id, "params could not be read as a JSON object: "+err.Error())
		}
	}
	m := p.Meta
	if m == nil {
		return nil, s.missingMetaRefusal(id, "no `_meta` object")
	}
	// STEP 3 — the two REQUIRED fields, present AND well-typed. A present-but-wrong-typed field is a
	// missing field: a `protocolVersion` that is a number names no revision.
	var version string
	if len(m.ProtocolVersion) == 0 || json.Unmarshal(m.ProtocolVersion, &version) != nil || strings.TrimSpace(version) == "" {
		return nil, s.missingMetaRefusal(id, "`"+MetaKeyProtocolVersion+"` is absent or is not a protocol-version string")
	}
	version = strings.TrimSpace(version)
	if len(m.ClientCapabilities) == 0 || !isJSONObject(m.ClientCapabilities) {
		return nil, s.missingMetaRefusal(id, "`"+MetaKeyClientCapabilities+"` is absent or is not an object")
	}
	// STEP 4 — the version. A modern-SHAPED request naming a revision we do not implement modernly is
	// `-32022`, and it does NOT latch: we are about to serve nothing, so it has no business choosing
	// the process's era. `basic/versioning`: "If the server does not implement the requested version …
	// it MUST respond with an UnsupportedProtocolVersionError listing the versions it does support."
	//
	// A version from the LEGACY tail of SupportedVersions lands here too, and that is deliberate: the
	// list is what a client may choose from, but a legacy revision is reached through `initialize`,
	// not through `_meta`. The modern-first ordering is what makes the right retry obvious.
	if !modernVersions[version] {
		return nil, errResp(id, CodeUnsupportedProtocolVersion, fmt.Sprintf(
			"unsupported protocol version %q: this server implements %s (modern revisions are reached through per-request `_meta`; the legacy revisions in this list are reached through `initialize`)",
			version, strings.Join(SupportedVersions, ", ")),
			map[string]any{"supported": SupportedVersions, "requested": version})
	}

	out := &modernMeta{
		version: version, capabilities: m.ClientCapabilities,
		extensions: clientExtensions(m.ClientCapabilities),
	}
	if len(m.ClientInfo) > 0 {
		var info Implementation
		// ADVISORY ONLY, and therefore best-effort: an unreadable `clientInfo` is not a reason to
		// refuse a request whose behavior can never depend on it.
		if json.Unmarshal(m.ClientInfo, &info) == nil {
			out.clientInfo = &info
		}
	}
	if len(m.ProgressToken) > 0 && !isJSONNull(m.ProgressToken) {
		out.progress = m.ProgressToken
	}
	out.trace = Trace{TraceParent: m.TraceParent, TraceState: m.TraceState, Baggage: m.Baggage}
	// `server/utilities/caching`: results from requests "carrying `inputResponses` or `requestState`
	// MUST NOT be cached". We produce no InputRequiredResult, so nothing can currently produce such a
	// retry — the flag is read here anyway so that the non-caching rule is a property of the request
	// rather than of a future code path someone remembers to add it to.
	out.retry = len(p.InputResponses) > 0 || len(p.RequestState) > 0
	// `_meta.io.modelcontextprotocol/logLevel` is deliberately NOT read. It is accepted and ignored:
	// this server emits no `notifications/message` on the modern era at all, so there is no verbosity
	// for it to select. The unrecognized-level `-32602` SHOULD on `server/utilities/logging` attaches
	// to servers implementing per-request logging; refusing a whole `tools/call` over the spelling of
	// a field we were never going to act on would be hostile, and it would make our non-adoption MORE
	// visible than adoption. Declared as a deliberate non-adoption, not dropped.
	return out, nil
}

// missingMetaRefusal is the ONE wire form of "this modern request is missing required protocol
// context". The supported list rides along as a non-normative courtesy: -32602 is not a recognized
// modern error, so a dual-era client treats it as a legacy signal and falls back to `initialize` —
// which under `dual` this process serves.
func (s *Server) missingMetaRefusal(id json.RawMessage, why string) *rpcResponse {
	return errResp(id, CodeInvalidParams, fmt.Sprintf(
		"invalid params: a %s request must carry `%s` and `%s` in `params._meta` (%s) — this server implements %s",
		ProtocolVersion20260728, MetaKeyProtocolVersion, MetaKeyClientCapabilities, why, strings.Join(SupportedVersions, ", ")),
		map[string]any{"supported": SupportedVersions})
}

func isJSONObject(raw json.RawMessage) bool {
	var v map[string]json.RawMessage
	return json.Unmarshal(raw, &v) == nil
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// --- admission: the validate-then-latch order, in one place ---

// admit classifies ONE inbound request and returns exactly one of three outcomes:
//
//	env != nil, resp == nil   → serve it as MODERN, with this env
//	env == nil, resp != nil   → write resp and dispatch nothing (a refusal, or the probe's answer)
//	env == nil, resp == nil   → not a modern request: fall through to the legacy dispatch, unchanged
//
// The third outcome is what keeps the legacy surface byte-identical. A legacy client sends no
// `io.modelcontextprotocol/protocolVersion`, so it never reaches a line of this file's logic beyond
// the shape test.
func (s *Server) admit(req *rpcRequest) (*RequestEnv, *rpcResponse) {
	// `--protocol legacy`: this process IS a pre-2026-07-28 server. No modern `_meta` is parsed and no
	// modern method exists — `server/discover` included, which falls through to the ordinary
	// method-not-found. SUNSET-PATH (MCP26-SUNSET).
	if s.Mode() == ProtocolLegacy {
		return nil, nil
	}

	// The PROBE is era-neutral and never latches, whatever its outcome. A probe that changed the thing
	// it probes is a trap: a client asking "what are you?" must not thereby decide it.
	if req.Method == "server/discover" {
		return nil, s.discover(req.ID, req.Params)
	}

	// STEP 1 (envelope) and the batch refusal are applied in the read loop, which is the only place
	// that can see a frame that is not a request at all.

	modernShaped := carriesModernMeta(req.Params)
	if !modernShaped {
		if s.era() == EraModern {
			// The process is serving the sessionless revision, where this request is malformed rather
			// than legacy. `initialize` gets its own answer (below, in the read loop) because a legacy
			// client has no fall-forward mechanism and that message may be the only diagnostic it can
			// surface.
			if req.Method == "initialize" {
				return nil, nil // handled by the read loop's initialize case
			}
			return nil, s.missingMetaRefusal(req.ID, "no per-request protocol context, and this process has latched the "+ProtocolVersion20260728+" revision")
		}
		return nil, nil // legacy, or still unlatched: today's path, unchanged
	}

	// A modern-shaped request arriving at a process that already latched LEGACY is refused HERE,
	// before the version check, and the ordering is deliberate. `-32022` is a RECOGNIZED MODERN ERROR:
	// per `basic/transports/stdio` a client receiving one concludes "the server is modern" and is told
	// not to fall back to `initialize`. A legacy-latched process must not send that signal. Refusing
	// early cannot weaken the latch invariant — nothing here latches anything — it only decides which
	// refusal a request that will not be served receives.
	if s.era() == EraLegacy {
		return nil, s.crossEraRefusal(req.ID)
	}

	// STEP 2 — METHOD KNOWN, in the modern era. `ping` and `logging/setLevel` were REMOVED in
	// 2026-07-28 (changelog, Major changes #5: "Remove `ping`, `logging/setLevel`, and
	// `notifications/roots/list_changed`"), so they are unknown methods here, and an unknown method
	// does not latch.
	if !s.modernMethodImplemented(req.Method) {
		return nil, errResp(req.ID, CodeMethodNotFound, modernMethodNotFound(req.Method, s.Resources != nil))
	}

	// STEPS 3 + 4 — required `_meta`, then the requested version.
	meta, refusal := s.parseModernMeta(req.ID, req.Params)
	if refusal != nil {
		return nil, refusal
	}

	// STEP 5 — the method's own params. A request whose params we are about to refuse is not a request
	// we are serving, so it has no business choosing the process's era either.
	if refusal := s.validateModernParams(req.ID, req.Method, req.Params); refusal != nil {
		return nil, refusal
	}

	// STEP 5b — the client capabilities the method requires. Same rule, same reason.
	if refusal := s.validateModernCapabilities(req.ID, req.Method, meta); refusal != nil {
		return nil, refusal
	}

	// ONLY NOW: the latch.
	if got := s.latchEra(EraModern); got != EraModern {
		return nil, s.crossEraRefusal(req.ID)
	}
	return s.modernEnv(req.Method, meta), nil
}

// crossEraRefusal is the answer to a modern request on a legacy-latched process. It is a teaching
// error, not a silent behavior change: the caller is told what happened and what to do instead.
//
// The recorded tension, stated where it lives: `basic/index` §Statelessness says servers "MUST NOT
// rely on prior requests over the same connection to establish context". A legacy-latched process
// refusing a well-formed modern request is, literally, using prior connection state. Our reading is
// that a legacy-latched process is BEING a legacy server, and the statelessness rule binds servers of
// this revision serving this revision. SUNSET-PATH (MCP26-SUNSET): the refusal goes with the era, after which the
// process is unconditionally stateless.
func (s *Server) crossEraRefusal(id json.RawMessage) *rpcResponse {
	return errResp(id, CodeInvalidRequest, fmt.Sprintf(
		"invalid request: this process is serving a legacy MCP session (it answered `initialize`), so it cannot also serve the sessionless %s revision — start a new server process for %s, or launch this one with --protocol legacy if a legacy session is what you want",
		ProtocolVersion20260728, ProtocolVersion20260728))
}

// initializeAfterModernRefusal is what `initialize` gets once the process has latched modern.
//
// It NAMES OUR SUPPORTED VERSIONS, because `basic/versioning` asks for exactly that and says why: a
// server "SHOULD name the protocol versions it supports in any error it returns to an `initialize`
// request, on any transport: legacy clients have no fall-forward mechanism, and this message may be
// the only diagnostic they can surface to users."
func (s *Server) initializeAfterModernRefusal(id json.RawMessage) *rpcResponse {
	return errResp(id, CodeMethodNotFound, fmt.Sprintf(
		"method not found: initialize (this process has latched the %s revision, which has no handshake — every request carries its own protocol context in `params._meta`). This server implements %s; start a new server process if a legacy session is what you want",
		ProtocolVersion20260728, strings.Join(SupportedVersions, ", ")),
		map[string]any{"supported": SupportedVersions, "latest": ProtocolVersion20260728})
}

// carriesModernMeta reports whether a request's params carry the modern protocol-version key. It is
// the ONE shape test that separates the eras, and it is deliberately narrow: presence of the key, not
// its validity. A malformed value must reach the -32602/-32022 answers rather than be silently served
// as legacy.
func carriesModernMeta(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return false
	}
	_, ok := p.Meta[MetaKeyProtocolVersion]
	return ok
}

// modernMethodImplemented is the modern era's method set. It is not the legacy set minus a few names:
// it is the set 2026-07-28 defines that this transport serves.
func (s *Server) modernMethodImplemented(method string) bool {
	switch method {
	case "server/discover", "tools/list", "tools/call":
		return true
	case "resources/list", "resources/read", "resources/templates/list":
		// A server with no provider declares no `resources` capability, so the method genuinely does
		// not exist here — the same answer the legacy era gives.
		return s.Resources != nil
	case "tasks/get", "tasks/update", "tasks/cancel":
		// A server with no task provider advertises no `extensions` and implements none of these,
		// so they genuinely do not exist. One condition, one consequence — see TaskProvider.
		return s.Tasks != nil
	}
	return false
}

// modernMethodNotFound is the teaching message for a method that does not exist on this revision. The
// two removed methods get their replacement named, because "method not found" alone would read as a
// server defect to a client that used them last week.
func modernMethodNotFound(method string, hasResources bool) string {
	switch method {
	case "ping":
		return "method not found: ping (removed in MCP " + ProtocolVersion20260728 + "; there is no session to keep alive — issue the request you actually want, and use the stdio process lifecycle for liveness)"
	case "logging/setLevel":
		return "method not found: logging/setLevel (removed in MCP " + ProtocolVersion20260728 + "; the log level is now the per-request `" + MetaKeyLogLevel + "` field. This server emits no `notifications/message` on this revision at all — the Logging feature is deprecated in " + ProtocolVersion20260728 + " and this server writes its diagnostics to stderr, which is the migration the specification names. Use `_meta.progressToken` for in-flight progress.)"
	case "initialize", "notifications/initialized":
		return "method not found: " + method + " (removed in MCP " + ProtocolVersion20260728 + ": there is no handshake — every request carries its own protocol context in `params._meta`)"
	case "resources/list", "resources/read", "resources/templates/list":
		if !hasResources {
			return "method not found: " + method + " (this server declares no `resources` capability)"
		}
	}
	return "method not found: " + method
}

// validateModernParams is admission STEP 5: the method's own params, checked BEFORE the latch.
//
// It deliberately re-uses the handlers' own checks rather than approximating them — an admission gate
// that disagreed with the handler about what is valid would refuse requests the server would have
// served, or latch on requests it will not.
func (s *Server) validateModernParams(id json.RawMessage, method string, params json.RawMessage) *rpcResponse {
	switch method {
	case "tools/list":
		if _, refusal := s.toolsPage(id, params); refusal != nil {
			return refusal
		}
	case "tools/call":
		if _, refusal := s.toolCallParams(id, params); refusal != nil {
			return refusal
		}
	case "resources/read":
		if _, refusal := s.readResourceURI(id, params); refusal != nil {
			return refusal
		}
	case "tasks/get", "tasks/update", "tasks/cancel":
		if _, refusal := taskIDOf(id, method, params); refusal != nil {
			return refusal
		}
	}
	return nil
}

// validateModernCapabilities is admission STEP 5b: the CLIENT CAPABILITIES a method requires,
// checked BEFORE the latch for the same reason every other check is — a request we are about to
// refuse has no business choosing the process's era.
//
// Exactly one method family requires one: the tasks extension's three methods. They are defined ONLY
// by the extension, so unlike a task-augmented `tools/call` there is no core behavior to revert to
// and the reject alternative is the only one available. See missingTasksCapability for why the code
// is `-32021` and not the `-32003` the extension specifies, and for the declared deviation.
func (s *Server) validateModernCapabilities(id json.RawMessage, method string, m *modernMeta) *rpcResponse {
	if !isTaskMethod(method) {
		return nil
	}
	if _, ok := m.extensions[ExtensionTasks]; ok {
		return nil
	}
	return missingTasksCapability(id, method)
}

// --- server/discover ---

// TTLDiscover bounds how long a client may consider a discovery answer fresh. Capabilities here are a
// function of LAUNCH FLAGS (which tools were granted, whether a resource provider exists), and those
// cannot change without a restart the client cannot see — which is the same reason `tools/list` is not
// longer.
const TTLDiscover int64 = 300_000

// discover answers the `server/discover` probe. `basic/versioning`: "Servers MUST implement
// `server/discover`."
//
// It is ERA-NEUTRAL AND NEVER LATCHES, and it is answered whether or not the process has already
// latched — the probe is how a client learns what this server is, and refusing it would make the one
// method the specification requires the one method a client cannot rely on.
//
// It is still a MODERN result: `resultType`, the caching hints and `_meta.serverInfo` all ride on it,
// because a `DiscoverResult` exists only in the modern revision. That is why the env it builds carries
// EraModern without going anywhere near the latch.
func (s *Server) discover(id json.RawMessage, params json.RawMessage) *rpcResponse {
	// The probe is a request like any other and the required-field rule has no probe exemption. The
	// `server/discover` page's own request example supplies `protocolVersion`, `clientInfo` and
	// `clientCapabilities`, and the stdio probe instruction is explicit that the client sets "its
	// preferred modern version in `_meta`". A compliant probe always carries `_meta`.
	//
	// No client is stranded: -32602 is not a recognized modern error, so a dual-era client treats us as
	// legacy and falls back to `initialize`, which under `dual` we serve. A version we do not implement
	// gets -32022, which IS recognized, so that client correctly stays modern and retries from our list.
	meta, refusal := s.parseModernMeta(id, params)
	if refusal != nil {
		return refusal
	}
	caps := map[string]any{
		"tools": map[string]any{"listChanged": false},
	}
	if s.Resources != nil {
		caps["resources"] = map[string]any{"subscribe": false, "listChanged": false}
	}
	// THE EXTENSION ADVERTISEMENT — and it is the ONE wire delta the tasks extension makes visible to a
	// client that never opts in. `extensions/tasks/overview` §For MCP servers: "Include the extension
	// in your `server/discover` capabilities". Advertising it is HOW a client learns it may opt in,
	// so it cannot be withheld and still have the extension be reachable; everything else about the
	// extension is invisible until a client declares it on a request.
	//
	// The settings object is empty because the extension defines none.
	if s.Tasks != nil {
		caps["extensions"] = map[string]any{ExtensionTasks: map[string]any{}}
	}
	// NO `logging` CAPABILITY, and its absence is the honest advertisement. `server/utilities/logging`
	// says servers that emit log notifications MUST declare it; this server emits none on the modern
	// era, so declaring the capability that gates them would invite exactly the confusion the decision
	// exists to prevent — a host reading the capability, setting a log level, and getting silence.
	// Legacy `initialize` declares it exactly as it always did.
	env := &RequestEnv{
		Method: "server/discover", Era: EraModern, Version: meta.version,
		ClientInfo: meta.clientInfo, Capabilities: meta.capabilities, Retry: meta.retry,
	}
	return okResp(id, s.result(env, map[string]any{
		"supportedVersions": SupportedVersions,
		"capabilities":      caps,
		"instructions":      s.Instructions,
	}))
}

// eraLatchCell is the process's era, held as an int32 so that a compare-and-set is the only writer.
//
// It is deliberately NOT exported, and no accessor for it is either. An application discloses the era
// through `RequestEnv.Era` — the era of the request being served — because that is the question a
// caller is actually asking, and a process-global read would answer a different one at a moment when
// the two can differ (the probe is era-neutral and never latches).
type eraLatchCell = atomic.Int32
