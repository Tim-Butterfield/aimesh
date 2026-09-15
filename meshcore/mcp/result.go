package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// This file builds the wire `result` object for a request according to its era.
//
// On the modern era (2026-07-28) every result carries `resultType` and
// `_meta["io.modelcontextprotocol/serverInfo"]`, and results of the cacheable operations
// (`server/discover`, `tools/list`, `prompts/list`, `resources/list`, `resources/templates/list`,
// `resources/read`) also carry `ttlMs` and `cacheScope`. A legacy result carries none of these, matching
// its revision; clients treat an absent `resultType` as `"complete"`.
//
// SUNSET-PATH (MCP26-SUNSET): at legacy removal the era branch is deleted and the modern branch remains.

// The ResultType values this transport emits. `"input_required"` is never emitted, because no
// multi-round-trip request is implemented.
const (
	// ResultTypeComplete marks a result that carries the final content of the request. The three
	// `tasks/*` methods also use it, although their results are task-shaped.
	ResultTypeComplete = "complete"
	// ResultTypeTask marks a CreateTaskResult, the asynchronous handle returned instead of a
	// `tools/call` result. Only Server.createTaskResponse produces it.
	ResultTypeTask = "task"
)

// The two cacheScope values. This transport emits only CacheScopePrivate.
const (
	// CacheScopePrivate confines a cached response to one authorization context, which on stdio is the
	// only context there is.
	CacheScopePrivate = "private"
	// CacheScopePublic is defined for completeness and is not emitted by this transport.
	CacheScopePublic = "public"
)

// MetaKeyServerInfo is the reserved `_meta` key carrying the server's self-description on every modern
// result. It identifies the server and makes no other claim.
const MetaKeyServerInfo = "io.modelcontextprotocol/serverInfo"

// cacheHint is the pair of caching fields owed on a cacheable operation's result. A nil *cacheHint means
// the operation is not cacheable and no hint is emitted.
type cacheHint struct {
	// TTLMs is how long the client may treat the result as fresh; it must be >= 0.
	TTLMs int64
	// Scope is `private` or `public`.
	Scope string
}

// The TTLs this transport publishes, one per cacheable operation.
const (
	// TTLToolsList bounds how long a client may show a tool list the running process does not have.
	// The tool set is fixed at startup, but the cache key excludes process identity, so a restart with
	// different flags is invisible to the client.
	TTLToolsList int64 = 300_000
	// TTLResourcesTemplatesList is long because the set is always empty: artifacts are addressed by
	// minted URI, so there is no template.
	TTLResourcesTemplatesList int64 = 3_600_000
	// TTLResourcesList is 0: the set changes as runs finish and as evicted runs are forgotten.
	TTLResourcesList int64 = 0
	// TTLResourcesRead is 0. Correctness does not depend on it: the digest in the URI makes a cached
	// read correct for its identity (see resources.go).
	TTLResourcesRead int64 = 0
)

// cacheHintFor returns the caching hint owed by a method, or nil for a method outside the cacheable set,
// such as `tools/call` and the task methods.
func cacheHintFor(method string) *cacheHint {
	switch method {
	case "server/discover":
		// The caching specification lists `server/discover` as cacheable.
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
		// tools/call is not cacheable, so a `ttlMs` on a task handle is the tasks extension's field.
		return nil
	}
}

// result builds the wire `result` object for one method; every success path uses it. A legacy result is
// the payload unchanged. A modern result adds `resultType`, the caching hint for cacheable methods, and
// the `_meta` serverInfo.
func (s *Server) result(env *RequestEnv, payload any) any {
	return s.resultAs(env, ResultTypeComplete, payload)
}

// resultAs is result with the discriminator supplied, for the tasks extension's CreateTaskResult. It is
// unexported so resultType only ever comes from the constants above.
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

// hintFor is the caching hint for this request. A retried multi-round-trip request gets none: its result
// must not be cached, and even `ttlMs: 0` permits serving stale bytes, so omitting the hint is the only
// way to honor that rule. No such retry is produced today.
func (s *Server) hintFor(env *RequestEnv) *cacheHint {
	if env.Retry {
		return nil
	}
	return cacheHintFor(env.Method)
}

// eraResult is a modern-era result: a payload plus the protocol fields, marshalled as one JSON object.
// The payload is marshalled first and merged, because payload types are heterogeneous.
type eraResult struct {
	payload    any
	resultType string
	hint       *cacheHint
	serverInfo *Implementation
}

// reservedResultFields are the keys this envelope always owns; a payload carrying one is a programming
// error and fails marshalling. `ttlMs` is reserved only when a caching hint is emitted
// (hintedReservedFields), because the tasks extension uses `ttlMs` on a task object for its lifetime.
var reservedResultFields = []string{"resultType", "cacheScope"}

// hintedReservedFields are additionally owned when this envelope is actually emitting a caching hint.
var hintedReservedFields = []string{"ttlMs"}

// MarshalJSON merges the payload's fields with the envelope's fields, in sorted key order.
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
		// A negative ttlMs is a bug in a hint table and must not reach the wire.
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

	// Sorted keys keep transcripts deterministic.
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

// mustRaw marshals a value this package constructed: a string, an int64, an Implementation or a map of
// raw messages, none of which can fail to marshal.
func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return json.RawMessage(b)
}
