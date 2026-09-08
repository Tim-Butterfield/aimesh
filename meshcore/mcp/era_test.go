package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the MODERN ERA on the wire: `server/discover`, the per-request `_meta` contract, the
// era latch, and the two methods 2026-07-28 removed.
//
// AGAINST A TREE WITH NO MODERN ERA EVERY TEST HERE FAILS, and most of them fail to COMPILE rather than to
// assert — `mcp.ProtocolVersion20260728`, `mcp.SupportedVersions`, `mcp.ProtocolLegacy` and
// `mcp.CodeUnsupportedProtocolVersion` did not exist, because no modern version was accepted anywhere.
// That is stated rather than dressed up as a behavioral failure: a test that cannot compile against
// the old tree proves the API is new, and the tests that DO compile (the `server/discover` -32601, and
// the era of a served request) are the ones that prove the behavior is new. Both are named below.
//
// The order the latch validates in is the thing under test, more than any single answer. Four
// assertions carry it: a malformed opener does not latch, a `-32022` opener does not latch, two
// concurrent modern openers both succeed, and two openers of different eras produce exactly one served
// request and one teaching refusal.

// modernMeta builds a well-formed per-request protocol context. `basic/index` marks
// `protocolVersion` and `clientCapabilities` REQUIRED and `clientInfo` not.
func modernMeta(version string) map[string]any {
	return map[string]any{
		mcp.MetaKeyProtocolVersion:    version,
		mcp.MetaKeyClientCapabilities: map[string]any{},
		mcp.MetaKeyClientInfo:         map[string]any{"name": "era-test-client", "version": "1"},
	}
}

// modernParams wraps a method's params with the modern `_meta`.
func modernParams(version string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = modernMeta(version)
	return params
}

const modern = mcp.ProtocolVersion20260728

// --- server/discover ---

func TestDiscover_AdvertisesEveryImplementedRevisionModernFirst(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()

	resp, notes := c.call(t, "server/discover", modernParams(modern, nil))
	if resp.Error != nil {
		t.Fatalf("server/discover: %+v — `basic/versioning`: \"Servers MUST implement server/discover\"", resp.Error)
	}
	if len(notes) != 0 {
		t.Errorf("the probe emitted notifications: %+v", notes)
	}

	versions, _ := resp.Result["supportedVersions"].([]any)
	got := make([]string, 0, len(versions))
	for _, v := range versions {
		s, _ := v.(string)
		got = append(got, s)
	}
	want := mcp.SupportedVersions
	if len(got) != len(want) {
		t.Fatalf("supportedVersions = %v, want every implemented revision %v — modern-ONLY would strand every legacy client this process still serves", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supportedVersions = %v, want %v (modern first: the ordering is the one signal that makes the right choice obvious)", got, want)
		}
	}
	if got[0] != modern {
		t.Errorf("supportedVersions[0] = %q, want the modern revision first", got[0])
	}

	// The modern result envelope rides on the probe: it is a DiscoverResult, which exists only in the
	// modern revision. server/utilities/caching names `server/discover` first in the cacheable set.
	if resp.Result["resultType"] != "complete" {
		t.Errorf("resultType = %v, want \"complete\"", resp.Result["resultType"])
	}
	if ttl, _ := resp.Result["ttlMs"].(float64); ttl != float64(mcp.TTLDiscover) {
		t.Errorf("ttlMs = %v, want %d", resp.Result["ttlMs"], mcp.TTLDiscover)
	}
	if resp.Result["cacheScope"] != mcp.CacheScopePrivate {
		t.Errorf("cacheScope = %v, want %q", resp.Result["cacheScope"], mcp.CacheScopePrivate)
	}
	meta, _ := resp.Result["_meta"].(map[string]any)
	info, _ := meta[mcp.MetaKeyServerInfo].(map[string]any)
	if info == nil || info["name"] != "test" {
		t.Errorf("_meta[%s] = %v, want the server's own identity", mcp.MetaKeyServerInfo, meta)
	}
	if resp.Result["instructions"] != "instructions here" {
		t.Errorf("instructions = %v, want the same string initialize returns", resp.Result["instructions"])
	}
}

func TestDiscover_AdvertisesNoLoggingCapability(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	resp, _ := c.call(t, "server/discover", modernParams(modern, nil))
	caps, _ := resp.Result["capabilities"].(map[string]any)
	if caps == nil || caps["tools"] == nil {
		t.Fatalf("capabilities = %v, want tools declared", resp.Result["capabilities"])
	}
	// `server/utilities/logging` is a DEPRECATION NOTICE in 2026-07-28 and its named stdio migration —
	// stderr — is what this server already does, so it emits no notifications/message on this era.
	// Declaring the capability that gates them would be a false advertisement, and it would invite
	// exactly the confusion the decision exists to prevent: a host reads the capability, sets a log
	// level, and gets silence.
	if _, declared := caps["logging"]; declared {
		t.Error("server/discover declared the `logging` capability while emitting no notifications/message on this era — `server/utilities/logging`: \"Servers that emit log message notifications MUST declare the `logging` capability\", and the honest reading of a server that emits none is not to declare it")
	}
	// And legacy `initialize` is UNTOUCHED: it still declares logging exactly as it always did.
	init := handshake(t, c)
	icaps, _ := init.Result["capabilities"].(map[string]any)
	if icaps["logging"] == nil {
		t.Error("legacy initialize stopped declaring the logging capability — the legacy surface must be byte-identical")
	}
}

func TestDiscover_IsEraNeutralAndDoesNotLatch(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	// Probe first, exactly as a dual-era client is told to ("SHOULD probe with server/discover before
	// sending any other request").
	if resp, _ := c.call(t, "server/discover", modernParams(modern, nil)); resp.Error != nil {
		t.Fatalf("server/discover: %+v", resp.Error)
	}
	// A probe that changed the thing it probes is a trap: the handshake must still be available.
	resp, _ := c.call(t, "initialize", map[string]any{"protocolVersion": mcp.LatestProtocolVersion})
	if resp.Error != nil {
		t.Fatalf("initialize after a probe: %+v — the probe latched the era, which makes it unaskable", resp.Error)
	}
}

func TestDiscover_RequiresTheProtocolMetaAndNamesWhatWeSupport(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{"no params at all", nil},
		{"no _meta", map[string]any{"x": 1}},
		{"no protocolVersion", map[string]any{"_meta": map[string]any{mcp.MetaKeyClientCapabilities: map[string]any{}}}},
		{"no clientCapabilities", map[string]any{"_meta": map[string]any{mcp.MetaKeyProtocolVersion: modern}}},
		{"protocolVersion is not a string", map[string]any{"_meta": map[string]any{
			mcp.MetaKeyProtocolVersion: 20260728, mcp.MetaKeyClientCapabilities: map[string]any{}}}},
		{"clientCapabilities is not an object", map[string]any{"_meta": map[string]any{
			mcp.MetaKeyProtocolVersion: modern, mcp.MetaKeyClientCapabilities: "yes"}}},
	} {
		resp, _ := c.call(t, "server/discover", tc.params)
		if resp.Error == nil {
			t.Errorf("%s: the probe was served — `basic/index`: \"A request missing any required field is malformed; the server MUST reject it with JSON-RPC error code -32602\"", tc.name)
			continue
		}
		if resp.Error.Code != mcp.CodeInvalidParams {
			t.Errorf("%s: code = %d, want %d", tc.name, resp.Error.Code, mcp.CodeInvalidParams)
		}
		// -32602 is NOT a recognized modern error, so per `basic/transports/stdio` a dual-era client
		// treats us as legacy and falls back to `initialize` — which this process serves. Nobody is
		// stranded by the rejection.
		if resp.Error.Data == nil || resp.Error.Data["supported"] == nil {
			t.Errorf("%s: the refusal named no supported versions", tc.name)
		}
	}
}

func TestDiscover_UnsupportedVersionIsTheRecognizedModernError(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	for _, version := range []string{"1900-01-01", mcp.ProtocolVersion20250618} {
		resp, _ := c.call(t, "server/discover", modernParams(version, nil))
		if resp.Error == nil {
			t.Fatalf("%s: the probe was served", version)
		}
		// `basic/versioning`: "If the server does not implement the requested version … it MUST
		// respond with an UnsupportedProtocolVersionError listing the versions it does support."
		// A LEGACY version here is refused too: it is in our advertised list because a client may
		// choose it, but it is reached through `initialize`, not through `_meta`.
		if resp.Error.Code != mcp.CodeUnsupportedProtocolVersion {
			t.Errorf("%s: code = %d, want %d (UnsupportedProtocolVersion)", version, resp.Error.Code, mcp.CodeUnsupportedProtocolVersion)
		}
		sup, _ := resp.Error.Data["supported"].([]any)
		if len(sup) != len(mcp.SupportedVersions) {
			t.Errorf("%s: data.supported = %v, want the same list server/discover advertises", version, resp.Error.Data["supported"])
		}
		if resp.Error.Data["requested"] != version {
			t.Errorf("%s: data.requested = %v", version, resp.Error.Data["requested"])
		}
	}
}

// --- the era latch: what does NOT latch ---

func TestLatch_AMalformedModernOpenerDoesNotLatch(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	// Modern-SHAPED (it carries the protocol-version key, so the era gate reads it) but malformed: no
	// clientCapabilities. It reaches -32602 and must go no further.
	resp, _ := c.call(t, "tools/list", map[string]any{"_meta": map[string]any{mcp.MetaKeyProtocolVersion: modern}})
	if resp.Error == nil || resp.Error.Code != mcp.CodeInvalidParams {
		t.Fatalf("malformed modern opener: %+v, want -32602", resp.Error)
	}
	// THE ASSERTION: a following `initialize` still succeeds. If the malformed request had latched, a
	// well-formed legacy client would be stranded by a frame it never sent.
	if got, _ := c.call(t, "initialize", map[string]any{"protocolVersion": mcp.LatestProtocolVersion}); got.Error != nil {
		t.Fatalf("initialize after a malformed modern opener: %+v — one malformed request permanently selected the process's era", got.Error)
	}
}

func TestLatch_AnUnsupportedVersionOpenerDoesNotLatch(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	resp, _ := c.call(t, "tools/list", modernParams("2099-01-01", nil))
	if resp.Error == nil || resp.Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Fatalf("unsupported-version opener: %+v, want -32022", resp.Error)
	}
	// THE ASSERTION: the client retries from our list and IS SERVED. That is the flow
	// `basic/transports/stdio` describes — "Use one of the versions in its advertised `supported`
	// list" — and it only works if `-32022` left the era unlatched.
	retry, _ := c.call(t, "tools/list", modernParams(modern, nil))
	if retry.Error != nil {
		t.Fatalf("the retry from our own advertised list was refused: %+v", retry.Error)
	}
	if retry.Result["resultType"] != "complete" {
		t.Errorf("the retry was not served under the modern era: %v", retry.Result)
	}
}

func TestLatch_AnUnknownOrRemovedMethodDoesNotLatch(t *testing.T) {
	for _, method := range []string{"ping", "logging/setLevel", "nonsense/method"} {
		t.Run(method, func(t *testing.T) {
			c, stop := serve(t, newServer())
			defer stop()
			resp, _ := c.call(t, method, modernParams(modern, map[string]any{"level": "debug"}))
			if resp.Error == nil || resp.Error.Code != mcp.CodeMethodNotFound {
				t.Fatalf("%s: %+v, want -32601 (changelog, Major changes #5: \"Remove `ping`, `logging/setLevel`, and `notifications/roots/list_changed`\")", method, resp.Error)
			}
			if got, _ := c.call(t, "initialize", map[string]any{"protocolVersion": mcp.LatestProtocolVersion}); got.Error != nil {
				t.Fatalf("%s left the process latched: %+v", method, got.Error)
			}
		})
	}
}

func TestLatch_UnparseableParamsDoNotLatch(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	// A well-formed modern envelope naming a tool this server does not have. It is refused at
	// admission step 5, before the latch: a request whose params we are about to refuse is not a
	// request we are serving, so it has no business choosing the era.
	resp, _ := c.call(t, "tools/call", modernParams(modern, map[string]any{"name": "not_a_tool"}))
	if resp.Error == nil || resp.Error.Code != mcp.CodeInvalidParams {
		t.Fatalf("unknown tool under modern: %+v, want -32602", resp.Error)
	}
	if got, _ := c.call(t, "initialize", map[string]any{"protocolVersion": mcp.LatestProtocolVersion}); got.Error != nil {
		t.Fatalf("a request refused on its params latched the era: %+v", got.Error)
	}
}

func TestLatch_ARefusedInitializeDoesNotLatchEither(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	if resp, _ := c.call(t, "initialize", map[string]any{"protocolVersion": "1999-01-01"}); resp.Error == nil {
		t.Fatal("an unsupported initialize was accepted")
	}
	// The symmetric assertion to the modern one: a handshake that failed must not strand a modern
	// client either.
	resp, _ := c.call(t, "tools/list", modernParams(modern, nil))
	if resp.Error != nil {
		t.Fatalf("a refused initialize latched the legacy era: %+v", resp.Error)
	}
}

// --- the era latch: what DOES latch, and what happens after ---

func TestLatch_AValidModernRequestLatchesModernAndInitializeThenNamesOurVersions(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	resp, _ := c.call(t, "tools/list", modernParams(modern, nil))
	if resp.Error != nil {
		t.Fatalf("modern tools/list: %+v", resp.Error)
	}
	// Served under the modern envelope, with no handshake anywhere: `basic/index` §Statelessness,
	// "all the information needed to process a request is contained in the request itself".
	if resp.Result["resultType"] != "complete" || resp.Result["ttlMs"] == nil {
		t.Errorf("a modern tools/list result is missing its envelope: %v", resp.Result)
	}

	got, _ := c.call(t, "initialize", map[string]any{"protocolVersion": mcp.LatestProtocolVersion})
	if got.Error == nil {
		t.Fatal("initialize succeeded on a modern-latched process")
	}
	// `basic/versioning`: a modern server "SHOULD name the protocol versions it supports in any error
	// it returns to an `initialize` request, on any transport: legacy clients have no fall-forward
	// mechanism, and this message may be the only diagnostic they can surface to users."
	sup, _ := got.Error.Data["supported"].([]any)
	if len(sup) != len(mcp.SupportedVersions) {
		t.Errorf("the initialize refusal named no supported versions: %+v", got.Error)
	}
	for _, v := range mcp.SupportedVersions {
		if !strings.Contains(got.Error.Message, v) {
			t.Errorf("the initialize refusal message does not name %s: %q", v, got.Error.Message)
		}
	}
}

func TestLatch_AModernRequestAfterALegacyHandshakeIsTaughtRatherThanServed(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	handshake(t, c)

	resp, _ := c.call(t, "tools/list", modernParams(modern, nil))
	if resp.Error == nil {
		t.Fatal("a modern request was served beside a live legacy session — the process is holding session state a stateless caller cannot see and did not consent to")
	}
	if resp.Error.Code != mcp.CodeInvalidRequest {
		t.Errorf("code = %d, want %d (a teaching refusal, not a recognized modern error: `-32022` would tell a dual-era client this server is modern and to stop falling back to `initialize`)", resp.Error.Code, mcp.CodeInvalidRequest)
	}
	if !strings.Contains(resp.Error.Message, "legacy") {
		t.Errorf("the refusal does not say what happened: %q", resp.Error.Message)
	}
	// And the legacy session is UNHARMED by the refusal.
	if ok, _ := c.call(t, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}}); ok.Error != nil {
		t.Fatalf("the legacy session broke after refusing a modern request: %+v", ok.Error)
	}
}

func TestLatch_TwoPipelinedModernOpenersBothSucceed(t *testing.T) {
	// A host that pipelines its first two calls is the COMMON case, not an exotic one. Both frames are
	// in the server's input before it has answered either, so both are openers.
	got := pipelined(t, newServer(),
		frame(101, "tools/list", modernParams(modern, nil)),
		frame(102, "tools/call", modernParams(modern, map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}})),
	)
	for _, id := range []int{101, 102} {
		resp := got[id]
		if resp == nil {
			t.Fatalf("pipelined opener %d was never answered", id)
		}
		if resp.Error != nil {
			t.Fatalf("pipelined opener %d was refused: %+v — a loser of the latch race must RE-READ, not error, when the era that won is its own", id, resp.Error)
		}
		if resp.Result["resultType"] != "complete" {
			t.Errorf("pipelined opener %d was not served under the modern era: %v", id, resp.Result)
		}
	}
}

func TestLatch_TwoPipelinedOpenersOfDifferentErasYieldOneServedAndOneRefusal(t *testing.T) {
	// Run it repeatedly: the claim is "on every run", and a latch that was merely usually right would
	// pass a single-shot test.
	for i := 0; i < 50; i++ {
		got := pipelined(t, newServer(),
			frame(201, "initialize", map[string]any{"protocolVersion": mcp.LatestProtocolVersion}),
			frame(202, "tools/list", modernParams(modern, nil)),
		)
		served, refused := 0, 0
		for _, id := range []int{201, 202} {
			resp := got[id]
			if resp == nil {
				t.Fatalf("run %d: opener %d was never answered", i, id)
			}
			if resp.Error == nil {
				served++
			} else {
				refused++
			}
		}
		if served != 1 || refused != 1 {
			t.Fatalf("run %d: %d served / %d refused — exactly one of two cross-era openers may win, and the other must get a teaching refusal rather than a second success or a second refusal", i, served, refused)
		}
	}
}

type wireFrame struct {
	id     int
	method string
	params map[string]any
}

func frame(id int, method string, params map[string]any) wireFrame {
	return wireFrame{id: id, method: method, params: params}
}

// pipelined feeds a server EVERY frame before it has answered any of them, and returns the responses
// by id.
//
// It uses a pre-filled reader rather than the shared io.Pipe harness deliberately, and the reason is
// the test's whole point: over an unbuffered pipe the client's SECOND write blocks until the server's
// FIRST response has been read, so "two requests in flight at once" cannot be expressed there at all.
// A test that cannot express the condition it names is not a test of it.
func pipelined(t *testing.T, s *mcp.Server, frames ...wireFrame) map[int]*response {
	t.Helper()
	var in strings.Builder
	for _, fr := range frames {
		req := map[string]any{"jsonrpc": "2.0", "id": fr.id, "method": fr.method}
		if fr.params != nil {
			req["params"] = fr.params
		}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		in.WriteString(string(b) + "\n")
	}
	var out strings.Builder
	if err := s.Serve(strings.NewReader(in.String()), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	got := map[int]*response{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var resp response
		if json.Unmarshal([]byte(line), &resp) != nil || len(resp.ID) == 0 {
			continue
		}
		var id int
		if json.Unmarshal(resp.ID, &id) != nil {
			continue
		}
		r := resp
		got[id] = &r
	}
	return got
}

// --- --protocol legacy: a GENUINELY legacy server ---

func TestProtocolLegacy_DiscoverIsAnUnknownMethodAndTheProcessStillServesInitialize(t *testing.T) {
	c, stop := serve(t, newServer(func(s *mcp.Server) { s.Protocol = mcp.ProtocolLegacy }))
	defer stop()

	resp, _ := c.call(t, "server/discover", modernParams(modern, nil))
	if resp.Error == nil {
		t.Fatal("a legacy-pinned process answered the probe — per `basic/transports/stdio` a DiscoverResult OF ANY CONTENT identifies a modern server, so a dual-era client would stop looking for `initialize` and fail with this process's legacy surface sitting right there unreachable")
	}
	if resp.Error.Code != mcp.CodeMethodNotFound {
		t.Errorf("code = %d, want %d — the same answer any other unimplemented method gets", resp.Error.Code, mcp.CodeMethodNotFound)
	}
	// NO `result` MEMBER IN ANY FORM. An error response carrying a result is not a JSON-RPC error, and
	// a client that reads `result` first would see a DiscoverResult-shaped thing that is not one.
	if resp.Result != nil {
		t.Errorf("the legacy-mode refusal carried a result member: %v", resp.Result)
	}
	// The specification's own "Dual-era client / Legacy server" row: "Works. stdio: the probe returns
	// a non-modern error or times out, and the client falls back to `initialize`." So it must.
	handshake(t, c)
	ok, _ := c.call(t, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}})
	if ok.Error != nil {
		t.Fatalf("the same process could not then complete a legacy call: %+v", ok.Error)
	}
	if ok.Result["resultType"] != nil {
		t.Errorf("a legacy result carried resultType: %v — a 2025-06-18 response should be exactly what 2025-06-18 defines", ok.Result)
	}
}

func TestProtocolLegacy_NoModernMetaIsParsedAtAll(t *testing.T) {
	c, stop := serve(t, newServer(func(s *mcp.Server) { s.Protocol = mcp.ProtocolLegacy }))
	defer stop()
	// A modern-shaped `tools/list` before any handshake is answered as the LEGACY server would answer
	// it — pre-initialization — not with -32602, -32022 or a modern result. The `_meta` is simply not
	// read for modern fields here.
	resp, _ := c.call(t, "tools/list", modernParams(modern, nil))
	if resp.Error == nil {
		t.Fatal("a legacy-pinned process served a modern-shaped request")
	}
	if resp.Error.Code != mcp.CodeNotInitialized {
		t.Errorf("code = %d, want %d (the legacy pre-initialization refusal)", resp.Error.Code, mcp.CodeNotInitialized)
	}
	handshake(t, c)
	// And once the session is live it is served as a LEGACY request: the `_meta` it carried names a
	// revision this mode does not implement, and it changes nothing.
	ok, _ := c.call(t, "tools/list", modernParams(modern, nil))
	if ok.Error != nil {
		t.Fatalf("tools/list: %+v", ok.Error)
	}
	if ok.Result["resultType"] != nil || ok.Result["ttlMs"] != nil {
		t.Errorf("a legacy-mode result carried modern envelope fields: %v", ok.Result)
	}
}

// --- no notifications/message at all, on the modern era ---

func TestModern_EmitsNoLogNotificationsUnderEveryLogLevelShape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		level any
		omit  bool
	}{
		{name: "logLevel absent", omit: true},
		{name: "logLevel present and valid", level: "debug"},
		{name: "logLevel present and malformed", level: "not-a-level"},
		{name: "logLevel present and not a string", level: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(func(s *mcp.Server) {
				s.Register(mcp.Tool{Name: "chatty", InputSchema: json.RawMessage(`{"type":"object"}`)},
					func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
						// The handler asks for a log line at every severity. On the modern era Call.Log
						// is a no-op, so asking loudly is the strongest form of the assertion.
						for _, lv := range []mcp.Level{mcp.LevelDebug, mcp.LevelInfo, mcp.LevelError, mcp.LevelEmergency} {
							c.Log(lv, "chatty", map[string]any{"said": "something"})
						}
						return mcp.Result("ok", map[string]any{"ok": true}), nil
					})
			})
			c, stop := serve(t, s)
			defer stop()
			meta := modernMeta(modern)
			if !tc.omit {
				meta[mcp.MetaKeyLogLevel] = tc.level
			}
			resp, notes := c.call(t, "tools/call", map[string]any{
				"name": "chatty", "arguments": map[string]any{}, "_meta": meta,
			})
			// A malformed level does NOT refuse the request. The `-32602`-for-an-unrecognized-level
			// SHOULD on `server/utilities/logging` attaches to servers implementing per-request
			// logging; this one does not, so the field is ACCEPTED AND IGNORED. Rejecting a whole
			// tools/call over the spelling of a field we were never going to act on would be hostile,
			// and would make the non-adoption more visible than adoption.
			if resp.Error != nil {
				t.Fatalf("the request was refused over a log level this server never reads: %+v", resp.Error)
			}
			for _, n := range notes {
				if n.Method == "notifications/message" {
					t.Fatalf("a notifications/message frame was written on the modern era (%s) — the Logging feature is deprecated in %s and this server emits none at all", tc.name, modern)
				}
			}
		})
	}
}

func TestModern_ProgressStillFlows(t *testing.T) {
	// Declining protocol logging costs nothing a modern client needs, and this is the check on that
	// claim rather than the assertion of it: `notifications/progress` is NOT deprecated and is the
	// live channel for in-flight visibility.
	s := newServer(func(s *mcp.Server) {
		s.Register(mcp.Tool{Name: "worker", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				c.Progress(0.5, 1, "halfway")
				c.Progress(1, 1, "done")
				return mcp.Result("ok", map[string]any{"ok": true}), nil
			})
	})
	c, stop := serve(t, s)
	defer stop()
	meta := modernMeta(modern)
	meta["progressToken"] = "tok-modern"
	resp, notes := c.call(t, "tools/call", map[string]any{
		"name": "worker", "arguments": map[string]any{}, "_meta": meta,
	})
	if resp.Error != nil {
		t.Fatalf("worker: %+v", resp.Error)
	}
	seen := 0
	for _, n := range notes {
		if n.Method == "notifications/progress" {
			seen++
			if n.Params["progressToken"] != "tok-modern" {
				t.Errorf("progressToken = %v, want the caller's own token echoed", n.Params["progressToken"])
			}
		}
	}
	if seen != 2 {
		t.Errorf("progress notifications = %d, want 2 — the one live in-flight channel must still work on the modern era", seen)
	}
}

// --- the server→client direction is legacy-only ---

func TestModern_ServerToClientRequestsAreRefusedBeforeAnythingIsWritten(t *testing.T) {
	var reqErr error
	done := make(chan struct{})
	s := newServer(func(s *mcp.Server) {
		s.Register(mcp.Tool{Name: "asker", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				_, reqErr = s.Request(ctx, "roots/list", map[string]any{})
				close(done)
				return mcp.Result("ok", map[string]any{"ok": true}), nil
			})
	})
	c, stop := serve(t, s)
	defer stop()
	resp, notes := c.call(t, "tools/call", modernParams(modern, map[string]any{
		"name": "asker", "arguments": map[string]any{},
	}))
	if resp.Error != nil {
		t.Fatalf("asker: %+v — the handler must have RUN for this assertion to mean anything", resp.Error)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never reached its outgoing request")
	}
	// `basic/transports/stdio` §Receiving Messages: "The server MUST NOT write JSON-RPC *requests* to
	// `stdout`." The refusal is at the write function, not at the one caller, because an
	// outgoing-request mechanism that is safe only because today's single caller checks first is not
	// safe — it is lucky.
	if reqErr == nil {
		t.Fatal("Server.Request succeeded on the modern era")
	}
	for _, n := range notes {
		if n.Method == "roots/list" {
			t.Fatal("a roots/list request frame reached stdout on the modern era")
		}
	}
}
