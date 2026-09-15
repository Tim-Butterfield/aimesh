package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// This file implements the modern era's entry points: the supported-version set, the per-request
// `_meta` contract, the era latch and `server/discover`.
//
// A request latches the process's era only after passing every step that could reject it (envelope,
// method, required `_meta`, supported version, params), and the latch is a single compare-and-set. A
// malformed request therefore never selects the era for a later well-formed one, and concurrent
// openers have a defined outcome. A `-32022` refusal never latches; the client's retry chooses the era.
//
// SUNSET-PATH (MCP26-SUNSET): at legacy removal the latch, the cross-era refusals and
// `--protocol legacy` are deleted, leaving a stateless modern server.

// ProtocolVersion20260728 is the first modern revision: no `initialize` handshake, and every request
// carries its own protocol context in `params._meta`.
const ProtocolVersion20260728 = "2026-07-28"

// SupportedVersions is every revision this transport implements, modern first and newest first within
// each era. `server/discover` advertises it and every `-32022` carries it, so the two cannot disagree.
// Legacy revisions are included, as in the specification's own examples.
//
// SUNSET-PATH (MCP26-SUNSET): at legacy removal this shrinks to the modern entry alone.
var SupportedVersions = []string{
	ProtocolVersion20260728,
	ProtocolVersion20250618,
	ProtocolVersion20250326,
	ProtocolVersion20241105,
}

// modernVersions are the modern revisions in SupportedVersions. A request naming a legacy version in
// `_meta` is refused with `-32022` rather than served under a legacy contract.
var modernVersions = map[string]bool{ProtocolVersion20260728: true}

// The reserved `_meta` keys of the per-request protocol contract. The first two are required on every
// request.
const (
	// MetaKeyProtocolVersion is required.
	MetaKeyProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	// MetaKeyClientCapabilities is required.
	MetaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	// MetaKeyClientInfo is optional and advisory; nothing branches on it.
	MetaKeyClientInfo = "io.modelcontextprotocol/clientInfo"
	// MetaKeyLogLevel is optional and ignored on the modern era, which sends no log notifications.
	MetaKeyLogLevel = "io.modelcontextprotocol/logLevel"
)

// ProtocolMode is the era posture a server process was launched with.
//
// SUNSET-PATH (MCP26-SUNSET): the whole type, both modes and the flag that sets it are removed
// with the legacy era.
type ProtocolMode string

const (
	// ProtocolDual (the default) serves whichever era the client opens with, once, per the latch.
	ProtocolDual ProtocolMode = "dual"
	// ProtocolLegacy makes the process a pre-2026-07-28 server in every observable respect: no modern
	// `_meta` is parsed, no modern method is reachable, and `server/discover` gets -32601. Any
	// DiscoverResult identifies a modern server, and a dual-era client that receives one must not fall
	// back to `initialize`, so method-not-found keeps the legacy surface reachable.
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

// latchEra is the only writer of the era cell and writes by compare-and-set. It returns the era in
// force afterwards. A caller that loses the race to its own era is served normally, so concurrent
// modern openers both succeed; a loser of the other era is refused.
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

// modernMeta is the parsed and validated protocol context of one modern request.
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

// parseModernMeta applies admission steps 3 and 4 to one request's params: the required `_meta`
// fields, then the requested version. It returns the wire refusal directly, so each failure has one
// wire form.
func (s *Server) parseModernMeta(id json.RawMessage, params json.RawMessage) (*modernMeta, *rpcResponse) {
	var p paramsEnvelope
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			// Unreadable params cannot carry the required fields, so they are refused as missing.
			return nil, s.missingMetaRefusal(id, "params could not be read as a JSON object: "+err.Error())
		}
	}
	m := p.Meta
	if m == nil {
		return nil, s.missingMetaRefusal(id, "no `_meta` object")
	}
	// Step 3: the two required fields, present and well-typed. A wrongly typed field counts as
	// missing.
	var version string
	if len(m.ProtocolVersion) == 0 || json.Unmarshal(m.ProtocolVersion, &version) != nil || strings.TrimSpace(version) == "" {
		return nil, s.missingMetaRefusal(id, "`"+MetaKeyProtocolVersion+"` is absent or is not a protocol-version string")
	}
	version = strings.TrimSpace(version)
	if len(m.ClientCapabilities) == 0 || !isJSONObject(m.ClientCapabilities) {
		return nil, s.missingMetaRefusal(id, "`"+MetaKeyClientCapabilities+"` is absent or is not an object")
	}
	// Step 4: the version. A revision not implemented in the modern era gets -32022 and does not latch.
	// Legacy revisions land here too, since they are reached through `initialize`, not `_meta`.
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
		// clientInfo is advisory, so an unreadable value is ignored.
		if json.Unmarshal(m.ClientInfo, &info) == nil {
			out.clientInfo = &info
		}
	}
	if len(m.ProgressToken) > 0 && !isJSONNull(m.ProgressToken) {
		out.progress = m.ProgressToken
	}
	out.trace = Trace{TraceParent: m.TraceParent, TraceState: m.TraceState, Baggage: m.Baggage}
	// Results of a request carrying `inputResponses` or `requestState` must not be cached. This server
	// produces no such retry, but the flag comes from the request so the rule holds regardless.
	out.retry = len(p.InputResponses) > 0 || len(p.RequestState) > 0
	// The `_meta` log level is accepted and ignored: no log notifications are sent on the modern era.
	return out, nil
}

// missingMetaRefusal is the wire form for a modern request missing required protocol context. -32602 is
// not a recognized modern error, so a dual-era client falls back to `initialize`, which `dual` serves;
// the supported list is included as a courtesy.
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

// admit classifies one inbound request and returns one of three outcomes:
//
//	env != nil, resp == nil   → serve it as modern, with this env
//	env == nil, resp != nil   → write resp and dispatch nothing (a refusal, or the probe's answer)
//	env == nil, resp == nil   → not a modern request: fall through to the legacy dispatch
//
// A legacy client sends no protocol-version key in `_meta`, so the legacy surface is unaffected.
func (s *Server) admit(req *rpcRequest) (*RequestEnv, *rpcResponse) {
	// `--protocol legacy`: no modern `_meta` is parsed and no modern method exists, `server/discover`
	// included. SUNSET-PATH (MCP26-SUNSET).
	if s.Mode() == ProtocolLegacy {
		return nil, nil
	}

	// The probe is era-neutral and never latches: asking what the server is must not decide it.
	if req.Method == "server/discover" {
		return nil, s.discover(req.ID, req.Params)
	}

	// Step 1 (envelope) and the batch refusal are applied in the read loop.

	modernShaped := carriesModernMeta(req.Params)
	if !modernShaped {
		if s.era() == EraModern {
			// On a modern-latched process this request is malformed. `initialize` gets its own answer
			// in the read loop, since a legacy client may be able to surface only that message.
			if req.Method == "initialize" {
				return nil, nil // handled by the read loop's initialize case
			}
			return nil, s.missingMetaRefusal(req.ID, "no per-request protocol context, and this process has latched the "+ProtocolVersion20260728+" revision")
		}
		return nil, nil // legacy, or still unlatched
	}

	// A modern request on a legacy-latched process is refused before the version check, because a
	// -32022 would tell the client the server is modern.
	if s.era() == EraLegacy {
		return nil, s.crossEraRefusal(req.ID)
	}

	// Step 2: the method must exist in the modern era, which removed `ping` and `logging/setLevel`.
	// An unknown method does not latch.
	if !s.modernMethodImplemented(req.Method) {
		return nil, errResp(req.ID, CodeMethodNotFound, modernMethodNotFound(req.Method, s.Resources != nil))
	}

	// Steps 3 and 4: required `_meta`, then the requested version.
	meta, refusal := s.parseModernMeta(req.ID, req.Params)
	if refusal != nil {
		return nil, refusal
	}

	// Step 5: the method's own params. A request about to be refused must not choose the era.
	if refusal := s.validateModernParams(req.ID, req.Method, req.Params); refusal != nil {
		return nil, refusal
	}

	// Step 5b: the client capabilities the method requires.
	if refusal := s.validateModernCapabilities(req.ID, req.Method, meta); refusal != nil {
		return nil, refusal
	}

	// Only now: the latch.
	if got := s.latchEra(EraModern); got != EraModern {
		return nil, s.crossEraRefusal(req.ID)
	}
	return s.modernEnv(req.Method, meta), nil
}

// crossEraRefusal answers a modern request on a legacy-latched process with a teaching error. A
// legacy-latched process is acting as a legacy server, so the modern statelessness rule does not bind
// it. SUNSET-PATH (MCP26-SUNSET): the refusal goes with the legacy era.
func (s *Server) crossEraRefusal(id json.RawMessage) *rpcResponse {
	return errResp(id, CodeInvalidRequest, fmt.Sprintf(
		"invalid request: this process is serving a legacy MCP session (it answered `initialize`), so it cannot also serve the sessionless %s revision — start a new server process for %s, or launch this one with --protocol legacy if a legacy session is what you want",
		ProtocolVersion20260728, ProtocolVersion20260728))
}

// initializeAfterModernRefusal answers `initialize` on a modern-latched process. It names the supported
// versions, because a legacy client may be able to surface only this message.
func (s *Server) initializeAfterModernRefusal(id json.RawMessage) *rpcResponse {
	return errResp(id, CodeMethodNotFound, fmt.Sprintf(
		"method not found: initialize (this process has latched the %s revision, which has no handshake — every request carries its own protocol context in `params._meta`). This server implements %s; start a new server process if a legacy session is what you want",
		ProtocolVersion20260728, strings.Join(SupportedVersions, ", ")),
		map[string]any{"supported": SupportedVersions, "latest": ProtocolVersion20260728})
}

// carriesModernMeta reports whether a request's params carry the modern protocol-version key. It
// checks presence, not validity, so a malformed value reaches the -32602 and -32022 answers instead of
// being served as legacy.
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
		// Without a provider there is no `resources` capability, as in the legacy era.
		return s.Resources != nil
	case "tasks/get", "tasks/update", "tasks/cancel":
		// Without a task provider these methods do not exist; see TaskProvider.
		return s.Tasks != nil
	}
	return false
}

// modernMethodNotFound is the teaching message for a method absent from this revision. Removed methods
// name their replacement.
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

// validateModernParams is admission step 5: the method's own params, checked before the latch with the
// handlers' own checks so admission and handling cannot disagree.
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

// validateModernCapabilities is admission step 5b: the client capabilities a method requires, checked
// before the latch. Only the tasks extension's methods require one; see missingTasksCapability for the
// error code.
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

// TTLDiscover bounds how long a client may treat a discovery answer as fresh. Capabilities depend on
// launch configuration, which changes only with a restart.
const TTLDiscover int64 = 300_000

// discover answers the `server/discover` probe, which every server must implement. It is era-neutral,
// never latches, and is answered whether or not the process has latched. Its result still carries the
// modern result metadata, since DiscoverResult exists only in the modern revision.
func (s *Server) discover(id json.RawMessage, params json.RawMessage) *rpcResponse {
	// The probe must carry the required `_meta` like any request. A dual-era client treats a -32602
	// refusal as a legacy signal and falls back to `initialize`; an unsupported version gets -32022.
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
	// Advertising the tasks extension is how a client learns it may opt in. The settings object is
	// empty because the extension defines none.
	if s.Tasks != nil {
		caps["extensions"] = map[string]any{ExtensionTasks: map[string]any{}}
	}
	// No `logging` capability: no log notifications are sent on the modern era.
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

// eraLatchCell holds the process's era as an int32 written only by compare-and-set. It is not
// exported; applications read the era of the request being served from RequestEnv.Era.
type eraLatchCell = atomic.Int32
