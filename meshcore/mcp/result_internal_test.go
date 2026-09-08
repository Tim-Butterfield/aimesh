package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// The era-parameterized result envelope, tested on BOTH branches.
//
// The modern branch is exercised here by unit-constructing a modern env rather than by driving the wire,
// so the envelope's contract is pinned independently of what any transport happens to admit: a change to
// admission cannot silently stop these assertions from covering the modern shape.
//
// The LEGACY assertions matter just as much: they are the unit-level statement of the same property the
// conformance transcript asserts end to end — a 2025-06-18 response is exactly what 2025-06-18 defines.

func modernEnv(method string) *RequestEnv {
	return &RequestEnv{Method: method, Era: EraModern, Version: "2026-07-28"}
}

func legacyEnv(method string) *RequestEnv {
	return &RequestEnv{Method: method, Era: EraLegacy, Version: ProtocolVersion20250618}
}

func marshalResult(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("result is not a JSON object: %v (%s)", err, b)
	}
	return out
}

func testServer() *Server {
	return &Server{Info: Implementation{Name: "testmesh", Version: "9.9.9"}}
}

// everyEnvelopedMethod is every method whose success path goes through the builder today.
// `server/discover` and the tasks methods do not go through it.
var everyEnvelopedMethod = []string{
	"tools/list", "tools/call", "resources/list", "resources/read", "resources/templates/list",
}

func TestResultEnvelope_LegacyAddsNothingAtAll(t *testing.T) {
	s := testServer()
	payload := map[string]any{"tools": []any{}}
	for _, m := range everyEnvelopedMethod {
		got, err := json.Marshal(s.result(legacyEnv(m), payload))
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		want, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		// BYTE-identical, not merely equivalent: the legacy golden is a transcript diff, and a
		// re-encoding that reorders one key would fail it for a reason nobody could name.
		if string(got) != string(want) {
			t.Fatalf("%s: legacy result changed on the wire\n got: %s\nwant: %s", m, got, want)
		}
	}
}

func TestResultEnvelope_ModernCarriesResultTypeOnEveryMethod(t *testing.T) {
	s := testServer()
	for _, m := range everyEnvelopedMethod {
		fields := marshalResult(t, s.result(modernEnv(m), map[string]any{"x": 1}))
		rt, ok := fields["resultType"]
		if !ok {
			t.Fatalf("%s: modern result carries no resultType (basic/index: the result MUST include a resultType field)", m)
		}
		if string(rt) != `"complete"` {
			t.Fatalf("%s: resultType = %s, want \"complete\"", m, rt)
		}
	}
}

func TestResultEnvelope_ModernCachingHintsMatchTheCacheableSet(t *testing.T) {
	s := testServer()
	// server/utilities/caching names exactly six cacheable operations. Of the methods this transport
	// serves today, four are in the set and `tools/call` is not — so `tools/call` gets NO hint, and
	// that omission is itself the correct hint ("If `ttlMs` is absent, clients SHOULD assume a default
	// of 0 (immediately stale)").
	for _, tc := range []struct {
		method  string
		wantTTL string // "" means: no ttlMs / cacheScope key at all
	}{
		{"tools/list", "300000"},
		{"resources/templates/list", "3600000"},
		{"resources/list", "0"},
		{"resources/read", "0"},
		{"tools/call", ""},
	} {
		fields := marshalResult(t, s.result(modernEnv(tc.method), map[string]any{"x": 1}))
		ttl, hasTTL := fields["ttlMs"]
		scope, hasScope := fields["cacheScope"]
		if tc.wantTTL == "" {
			if hasTTL || hasScope {
				t.Fatalf("%s is not a cacheable operation but carried ttlMs=%s cacheScope=%s", tc.method, ttl, scope)
			}
			continue
		}
		if !hasTTL {
			t.Fatalf("%s: no ttlMs (server/utilities/caching: servers MUST include caching hints on this operation)", tc.method)
		}
		if string(ttl) != tc.wantTTL {
			t.Fatalf("%s: ttlMs = %s, want %s", tc.method, ttl, tc.wantTTL)
		}
		// `private` everywhere. `public` asserts the response contains no caller-specific data and may
		// be served to any user by any shared intermediary — an assertion that is provably false for a
		// tool list that is a function of policy, and emphatically false for run artifacts.
		if !hasScope || string(scope) != `"private"` {
			t.Fatalf("%s: cacheScope = %s, want \"private\"", tc.method, scope)
		}
	}
}

func TestResultEnvelope_ModernCarriesServerInfoOnEveryResult(t *testing.T) {
	s := testServer()
	for _, m := range everyEnvelopedMethod {
		fields := marshalResult(t, s.result(modernEnv(m), map[string]any{"x": 1}))
		raw, ok := fields["_meta"]
		if !ok {
			t.Fatalf("%s: modern result carries no _meta", m)
		}
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("%s: _meta is not an object: %v", m, err)
		}
		info, ok := meta[MetaKeyServerInfo]
		if !ok {
			t.Fatalf("%s: _meta carries no %s (basic/index: servers SHOULD include it in every result's _meta)", m, MetaKeyServerInfo)
		}
		var got Implementation
		if err := json.Unmarshal(info, &got); err != nil {
			t.Fatalf("%s: serverInfo does not decode: %v", m, err)
		}
		if got.Name != "testmesh" || got.Version != "9.9.9" {
			t.Fatalf("%s: serverInfo = %+v, want the server's own name and version", m, got)
		}
	}
}

func TestResultEnvelope_ModernPreservesThePayloadUnderneath(t *testing.T) {
	s := testServer()
	res := &CallToolResult{Content: []Content{Text("hello")}, StructuredContent: map[string]any{"runId": "run-1"}, IsError: true}
	fields := marshalResult(t, s.result(modernEnv("tools/call"), res))
	if _, ok := fields["content"]; !ok {
		t.Fatal("the payload's content block did not survive the envelope")
	}
	if got := string(fields["structuredContent"]); got != `{"runId":"run-1"}` {
		t.Fatalf("structuredContent = %s", got)
	}
	// `isError: true` rides `resultType: "complete"`. A domain halt is a COMPLETED request whose tool
	// failed — server/tools puts tool execution errors in the result with isError, distinguishing them
	// from protocol errors. It reads wrong to a human and it is what the spec asks for.
	if got := string(fields["isError"]); got != "true" {
		t.Fatalf("isError = %s, want true", got)
	}
	if got := string(fields["resultType"]); got != `"complete"` {
		t.Fatalf("a domain halt must still be resultType complete, got %s", got)
	}
}

func TestResultEnvelope_RetrySuppressesTheCachingHints(t *testing.T) {
	s := testServer()
	env := modernEnv("tools/list")
	env.Retry = true
	fields := marshalResult(t, s.result(env, map[string]any{"tools": []any{}}))
	if _, ok := fields["ttlMs"]; ok {
		t.Fatal("a retried (MRTR) request's result carried a caching hint — server/utilities/caching: results from requests carrying inputResponses or requestState MUST NOT be cached")
	}
	if _, ok := fields["cacheScope"]; ok {
		t.Fatal("a retried request's result carried a cacheScope")
	}
	// resultType and serverInfo are NOT caching hints and are still owed.
	if got := string(fields["resultType"]); got != `"complete"` {
		t.Fatalf("resultType = %s on a retried request, want \"complete\"", got)
	}
	if _, ok := fields["_meta"]; !ok {
		t.Fatal("a retried request's result dropped serverInfo, which the non-caching rule does not touch")
	}
}

func TestResultEnvelope_RefusesAPayloadThatAlreadyOwnsAnEnvelopeField(t *testing.T) {
	s := testServer()
	for _, k := range []string{"resultType", "ttlMs", "cacheScope"} {
		_, err := json.Marshal(s.result(modernEnv("tools/list"), map[string]any{k: "mine"}))
		if err == nil {
			t.Fatalf("a payload carrying %q was silently merged; two writers of one field is how they come to disagree", k)
		}
	}
	// The same rule for the one _meta key the envelope owns.
	_, err := json.Marshal(s.result(modernEnv("tools/list"), map[string]any{
		"_meta": map[string]any{MetaKeyServerInfo: map[string]any{"name": "impostor"}},
	}))
	if err == nil {
		t.Fatal("a payload carrying its own serverInfo was silently overwritten or duplicated")
	}
	// A payload with an UNRELATED _meta key is merged, not refused: the envelope owns one key, not the
	// object.
	fields := marshalResult(t, s.result(modernEnv("tools/list"), map[string]any{
		"_meta": map[string]any{"com.example/thing": 1},
	}))
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(fields["_meta"], &meta); err != nil {
		t.Fatal(err)
	}
	if _, ok := meta["com.example/thing"]; !ok {
		t.Fatal("merging serverInfo into an existing _meta dropped the payload's own key")
	}
	if _, ok := meta[MetaKeyServerInfo]; !ok {
		t.Fatal("merging into an existing _meta dropped serverInfo")
	}
}

func TestResultEnvelope_TTLIsNeverNegative(t *testing.T) {
	// server/utilities/caching: "Servers MUST provide a `ttlMs` value that is `>= 0`." A negative value
	// is a bug in the hint table, and a bug in the hint table must fail loudly rather than reach a wire.
	r := &eraResult{payload: map[string]any{}, resultType: ResultTypeComplete, hint: &cacheHint{TTLMs: -1, Scope: CacheScopePrivate}}
	if _, err := json.Marshal(r); err == nil {
		t.Fatal("a negative ttlMs was marshalled")
	}
	// And every hint the table actually publishes satisfies it.
	for _, m := range everyEnvelopedMethod {
		if h := cacheHintFor(m); h != nil && h.TTLMs < 0 {
			t.Fatalf("%s publishes a negative ttlMs (%d)", m, h.TTLMs)
		}
	}
}

func TestResultEnvelope_ModernOrderingIsDeterministic(t *testing.T) {
	s := testServer()
	first, err := json.Marshal(s.result(modernEnv("tools/list"), map[string]any{"tools": []any{}, "nextCursor": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := json.Marshal(s.result(modernEnv("tools/list"), map[string]any{"tools": []any{}, "nextCursor": "x"}))
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("modern result field order varies run to run:\n%s\n%s", first, again)
		}
	}
}

// --- §7.4: content-addressed resource identity ---

func TestAdvertisedResources_ModernURIsCarryTheDigestAndLegacyDoesNot(t *testing.T) {
	list := []Resource{{URI: "aimesh://run/r1/patch", Name: "r1/patch", Digest: "sha256:abc123"}}

	legacy := advertisedResources(legacyEnv("resources/list"), list)
	if legacy[0].URI != "aimesh://run/r1/patch" {
		t.Fatalf("legacy URI changed: %s — legacy emits no caching hints, so nothing is invited to cache and there is nothing to content-address against", legacy[0].URI)
	}

	modern := advertisedResources(modernEnv("resources/list"), list)
	if !strings.Contains(modern[0].URI, "sha256=abc123") {
		t.Fatalf("modern URI does not carry the digest: %s — the digest must be part of the CACHE KEY, which ttlMs:0 cannot make it", modern[0].URI)
	}
	// The rewrite must not mutate the provider's own slice: the store hands out its published set.
	if list[0].URI != "aimesh://run/r1/patch" {
		t.Fatalf("advertising mutated the provider's list: %s", list[0].URI)
	}
	// An artifact published WITHOUT a digest has nothing to address it by and keeps its bare URI.
	nodigest := advertisedResources(modernEnv("resources/list"), []Resource{{URI: "aimesh://run/r1/log"}})
	if nodigest[0].URI != "aimesh://run/r1/log" {
		t.Fatalf("an artifact with no recorded digest got a fabricated one: %s", nodigest[0].URI)
	}
	// ParseResourceURI must still split a digest-bearing URI: the digest is an addressing suffix, not a
	// third path segment.
	runID, name, ok := ParseResourceURI(modern[0].URI)
	if !ok || runID != "r1" || name != "patch" {
		t.Fatalf("ParseResourceURI(%q) = %q,%q,%v", modern[0].URI, runID, name, ok)
	}
}

func TestSplitResourceDigest(t *testing.T) {
	for _, tc := range []struct{ in, base, digest string }{
		{"aimesh://run/r1/patch", "aimesh://run/r1/patch", ""},
		{"aimesh://run/r1/patch?sha256=deadbeef", "aimesh://run/r1/patch", "deadbeef"},
		{"aimesh://run/r1/patch?other=1", "aimesh://run/r1/patch", ""},
	} {
		base, digest := splitResourceDigest(tc.in)
		if base != tc.base || digest != tc.digest {
			t.Fatalf("splitResourceDigest(%q) = %q,%q want %q,%q", tc.in, base, digest, tc.base, tc.digest)
		}
	}
}
