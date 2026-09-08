package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// This file is the ERA-PARAMETERIZED RESULT ENVELOPE: the one place that decides what a `result` object
// carries on the wire, given the era of the request that produced it.
//
// The 2026-07-28 revision adds three things to a result that 2025-06-18 has no concept of:
//
//   - `resultType`, a REQUIRED discriminator on every result (basic/index, verbatim: "The `result`
//     **MUST** include a `resultType` field to indicate the type of the result.");
//   - `ttlMs` + `cacheScope`, REQUIRED on the six cacheable operations (server/utilities/caching,
//     verbatim: "Servers MUST include caching hints on results with `resultType: \"complete\"` returned
//     by the following operations: `server/discover`, `tools/list`, `prompts/list`, `resources/list`,
//     `resources/templates/list`, `resources/read`");
//   - `_meta["io.modelcontextprotocol/serverInfo"]` on every result (basic/index, verbatim: "Servers
//     **SHOULD** include the following `io.modelcontextprotocol/*` field in every result's `_meta`,
//     unless specifically configured not to do so, to identify themselves without relying on any prior
//     connection state").
//
// A LEGACY result carries NONE of them. That is deliberate and it is not laziness: a 2025-06-18 response
// should be exactly what 2025-06-18 defines, and emitting a field from a later revision is a small lie
// about which contract is in force. Nothing is lost by the omission, because basic/index says so
// itself: "For backward compatibility with servers implementing earlier protocol versions, which do not
// include `resultType`, clients **MUST** treat an absent `resultType` as `\"complete\"`."
//
// SUNSET-PATH (MCP26-SUNSET; migration design §16.2): at legacy removal the era branch is deleted
// and the modern branch is what remains — this file gets shorter, not rewritten.
//
// The modern branch was UNREACHABLE when this file was written — no modern version was accepted, so
// envFor hard-coded EraLegacy and the branch was held to its contract by unit-constructing a modern
// env. **That is history: era.go now admits modern requests and the branch is live on the wire.** The
// unit tests remain, and the real-binary modern transcripts are what prove the branch is reached.

// The ResultType values this transport emits. basic/index defines `"complete"` and
// `"input_required"`; we never emit `"input_required"` (we implement no multi-round-trip request).
// `"task"` is the tasks extension's, and it has exactly one producer — see ResultTypeTask.
const (
	// ResultTypeComplete marks a result that carries the final content of the request.
	//
	// It is ALSO what all three `tasks/*` methods carry, which is easy to get backwards because the
	// objects they return are task-shaped. The extension is explicit (verified 2026-08-04): "The
	// `resultType` field **MUST** be set to `"complete"` on this object as it is the standard result
	// shape for the `tasks/get` request", and the same MUST is stated for `UpdateTaskResult` and
	// `CancelTaskResult`.
	ResultTypeComplete = "complete"
	// ResultTypeTask marks a `CreateTaskResult` — an asynchronous handle returned INSTEAD of the
	// final result of a `tools/call`. It is the only result that may carry it: "Servers **MUST NOT**
	// set `resultType` to `"task"` on result types other than `CreateTaskResult`" (verified
	// 2026-08-04). It is produced in exactly one place, Server.createTaskResponse.
	ResultTypeTask = "task"
)

// The two cacheScope values. server/utilities/caching: `"public"` asserts the response "does not contain
// user-specific data" and MAY be served "to any user" by "any client, shared gateway, or caching proxy".
// This transport never asserts that — see CacheScopePrivate's use at every hint site.
const (
	// CacheScopePrivate confines a cached response to one authorization context, which on stdio is the
	// only context there is.
	CacheScopePrivate = "private"
	// CacheScopePublic is defined for completeness and is not emitted by this transport.
	CacheScopePublic = "public"
)

// MetaKeyServerInfo is the reserved `_meta` key carrying the server's self-description on every modern
// result. It is self-reported and unverified, and basic/index says clients "**SHOULD NOT** use them to
// change the behavior of the client or server, and **SHOULD NOT** rely on them for security decisions" —
// we make no claim on it beyond identification.
const MetaKeyServerInfo = "io.modelcontextprotocol/serverInfo"

// cacheHint is the pair of caching fields owed on a cacheable operation's result. A nil *cacheHint means
// the operation is NOT in the cacheable set, so no hint is owed and none is emitted.
type cacheHint struct {
	// TTLMs is how long the client MAY consider the result fresh. server/utilities/caching: "Servers
	// **MUST** provide a `ttlMs` value that is `>= 0`."
	TTLMs int64
	// Scope is `private` or `public`.
	Scope string
}

// The TTLs this transport publishes, one named constant per cacheable operation so a reader can find the
// reasoning next to the number rather than in a commit message.
const (
	// TTLToolsList bounds the window in which a client could show a tool list the running process does
	// not have. The tool set is bound at startup, which makes a long TTL look free — it is not, because
	// the cache key is method + params and NOT process identity: an operator who restarts with different
	// flags produces a different tool list from a different process, and the client cannot see the
	// restart.
	TTLToolsList int64 = 300_000
	// TTLResourcesTemplatesList is long because the set is permanently empty by design: this transport
	// addresses artifacts by minted URI, so there is no template a client could expand into one, and
	// nothing can invalidate an empty answer.
	TTLResourcesTemplatesList int64 = 3_600_000
	// TTLResourcesList is 0: the set changes as runs finish and as evicted runs are forgotten, and a
	// stale list is a link to a run this server can no longer explain.
	TTLResourcesList int64 = 0
	// TTLResourcesRead is 0 — and the read guarantee does NOT rest on this number. `ttlMs: 0` is a hint
	// ("If `ttlMs` is `0`, the response **SHOULD** be considered immediately stale"), and the same page
	// permits serving stale bytes anyway: "Clients **MAY** serve stale responses if errors occur during
	// re-fetching". The mechanism is the digest in the URI (see resources.go): it makes the digest part
	// of the cache key, so a stale hit is by construction the correct bytes for that identity.
	TTLResourcesRead int64 = 0
)

// cacheHintFor returns the caching hint owed by a method, or nil for a method outside the cacheable set.
//
// The set is CLOSED and quoted at the top of this file. `tools/call` and the tasks methods are not in it,
// so they get no hint — and that is not a loss: "If `ttlMs` is absent, clients **SHOULD** assume a
// default of `0` (immediately stale) and rely on their own caching heuristics or notifications."
func cacheHintFor(method string) *cacheHint {
	switch method {
	case "server/discover":
		// In the set: the caching page's own list names `server/discover` first. (The 2026-07-28
		// changelog's summary of the same requirement omits it; the normative page is what this
		// follows.)
		return &cacheHint{TTLMs: TTLDiscover, Scope: CacheScopePrivate}
	case "tools/list":
		return &cacheHint{TTLMs: TTLToolsList, Scope: CacheScopePrivate}
	case "resources/list":
		return &cacheHint{TTLMs: TTLResourcesList, Scope: CacheScopePrivate}
	case "resources/read":
		return &cacheHint{TTLMs: TTLResourcesRead, Scope: CacheScopePrivate}
	case "resources/templates/list":
		return &cacheHint{TTLMs: TTLResourcesTemplatesList, Scope: CacheScopePrivate}
	default:
		// tools/call is deliberately here: it is not a cacheable operation, which is also why a
		// `ttlMs` on a task handle is unambiguously the tasks extension's field and not a caching hint.
		return nil
	}
}

// result builds the wire `result` object for one method, parameterized on the era of the request.
//
// THIS IS THE ONE BUILDER. Every method's success path goes through it, so "what does a result carry"
// has exactly one answer and the era branch exists in exactly one place.
//
//   - LEGACY: the payload, untouched. Byte-identical to what this transport emitted before the envelope
//     existed, which is what the legacy conformance golden asserts.
//   - MODERN: the payload plus `resultType`, plus the caching hint when the method is in the cacheable
//     set, plus `_meta[io.modelcontextprotocol/serverInfo]`.
func (s *Server) result(env *RequestEnv, payload any) any {
	return s.resultAs(env, ResultTypeComplete, payload)
}

// resultAs is result with the discriminator supplied. It exists for the ONE result that is not
// `complete` — the tasks extension's `CreateTaskResult` — and it is deliberately unexported: a
// second caller passing an invented `resultType` is the failure this parameterization could
// otherwise enable, so the value comes from the two constants above and from nowhere else.
func (s *Server) resultAs(env *RequestEnv, resultType string, payload any) any {
	if env == nil || env.Era != EraModern {
		return payload
	}
	info := s.Info
	return &eraResult{
		payload:    payload,
		resultType: resultType,
		hint:       s.hintFor(env),
		serverInfo: &info,
	}
}

// hintFor is the caching hint for this request, after the one rule that can suppress it.
//
// THE MRTR NON-CACHING RULE. server/utilities/caching, verbatim: "Results produced by retrying a request
// through the multi round-trip requests mechanism—that is, requests carrying `inputResponses` or
// `requestState`—**MUST NOT** be cached, as they depend on inputs that are not part of the cache key."
// A hint is how this server invites caching, so the honest way to say "do not cache this" is to send no
// invitation.
//
// Recorded rather than glossed: on a retried request the spec's own texts pull in two directions — the
// cacheable-set sentence says a `complete` result from these six operations MUST carry hints, and the
// sentence above says a retried request's result MUST NOT be cached. `ttlMs: 0` would satisfy the first
// and NOT the second, because a client "**MAY** serve stale responses if errors occur during
// re-fetching". Omission is the reading that keeps the stronger prohibition, so omission is what this
// does. It is currently unreachable: this transport produces no `InputRequiredResult` and therefore
// never receives a retried request (migration design §11.3).
func (s *Server) hintFor(env *RequestEnv) *cacheHint {
	if env.Retry {
		return nil
	}
	return cacheHintFor(env.Method)
}

// eraResult is a modern-era result: a payload plus the per-revision protocol fields, marshalled as ONE
// JSON object.
//
// It is a marshaller rather than a struct with an embedded payload because the payloads are heterogeneous
// (a map for tools/list, a typed CallToolResult for tools/call) and Go has no way to embed `any` into an
// object's field set. Marshalling the payload first and merging keeps every existing payload type
// untouched, which is what lets the legacy branch stay byte-identical.
type eraResult struct {
	payload    any
	resultType string
	hint       *cacheHint
	serverInfo *Implementation
}

// reservedResultFields are the keys this envelope ALWAYS owns. A payload that already carries one of
// them is a PROGRAMMING ERROR, not a merge to resolve: two writers of one field is how they come to
// disagree, and the failure has to be loud at the point the second writer appears rather than silent
// on the wire.
//
// `ttlMs` is deliberately NOT in this list, and `cacheScope` deliberately is. The reason is that
// `ttlMs` has TWO owners in the published specifications and they are different fields with the same
// name: `server/utilities/caching` owns it on the six cacheable operations (freshness), and the tasks
// extension owns it on a task object (LIFETIME FROM CREATION — "The server may discard the task after
// the TTL elapses"). This envelope only ever writes the caching one, and only when a hint is owed, so
// the collision check for it is applied there — see hintedReservedFields. `cacheScope` has one owner
// and stays unconditionally reserved.
var reservedResultFields = []string{"resultType", "cacheScope"}

// hintedReservedFields are additionally owned when this envelope is actually emitting a caching hint.
var hintedReservedFields = []string{"ttlMs"}

func (r *eraResult) MarshalJSON() ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if r.payload != nil {
		b, err := json.Marshal(r.payload)
		if err != nil {
			return nil, err
		}
		if trimmed := bytes.TrimSpace(b); !bytes.Equal(trimmed, []byte("null")) {
			if err := json.Unmarshal(b, &fields); err != nil {
				return nil, fmt.Errorf("mcp: a result payload must marshal to a JSON object (%s): %w", r.resultType, err)
			}
		}
	}
	reserved := reservedResultFields
	if r.hint != nil {
		reserved = append(append([]string{}, reservedResultFields...), hintedReservedFields...)
	}
	for _, k := range reserved {
		if _, dup := fields[k]; dup {
			return nil, fmt.Errorf("mcp: result payload already carries %q — the era envelope owns that field", k)
		}
	}

	fields["resultType"] = mustRaw(r.resultType)
	if r.hint != nil {
		// server/utilities/caching: "Servers **MUST** provide a `ttlMs` value that is `>= 0`." A
		// negative value is a bug in a hint table, and a bug in a hint table must not reach the wire.
		if r.hint.TTLMs < 0 {
			return nil, fmt.Errorf("mcp: ttlMs must be >= 0, got %d", r.hint.TTLMs)
		}
		fields["ttlMs"] = mustRaw(r.hint.TTLMs)
		fields["cacheScope"] = mustRaw(r.hint.Scope)
	}
	if r.serverInfo != nil {
		meta := map[string]json.RawMessage{}
		if existing, ok := fields["_meta"]; ok {
			if err := json.Unmarshal(existing, &meta); err != nil {
				return nil, fmt.Errorf("mcp: a result's `_meta` must be a JSON object: %w", err)
			}
			if _, dup := meta[MetaKeyServerInfo]; dup {
				return nil, fmt.Errorf("mcp: result `_meta` already carries %q — the era envelope owns that key", MetaKeyServerInfo)
			}
		}
		meta[MetaKeyServerInfo] = mustRaw(r.serverInfo)
		fields["_meta"] = mustRaw(meta)
	}

	// Deterministic key order. The wire does not care, but a transcript gate does: a result whose field
	// order varies run to run cannot be diffed, and a golden that cannot be diffed is not a gate.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(fields[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// mustRaw marshals a value this package constructed itself. Every caller passes a string, an int64, an
// Implementation or a map of already-valid RawMessages — none of which can fail to marshal — so an error
// here would be a corrupted build rather than a runtime condition, and the alternative (threading an
// error through six call sites that cannot produce one) buys nothing.
func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return json.RawMessage(b)
}
