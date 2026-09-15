package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file tests the modern era on the wire: `server/discover`, the per-request `_meta` contract, the
// era latch, and the methods 2026-07-28 removed. The latch tests assert that a malformed or `-32022`
// opener does not latch, that concurrent modern openers both succeed, and that openers of different eras
// yield one served request and one teaching refusal.

// modernMeta builds a well-formed per-request protocol context; `protocolVersion` and
// `clientCapabilities` are required and `clientInfo` is not.
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

	// The probe's DiscoverResult carries the modern result envelope, including caching hints.
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
	// No log notifications are sent on the modern era, so the logging capability is not declared.
	if _, declared := caps["logging"]; declared {
		t.Error("server/discover declared the `logging` capability while emitting no notifications/message on this era — `server/utilities/logging`: \"Servers that emit log message notifications MUST declare the `logging` capability\", and the honest reading of a server that emits none is not to declare it")
	}
	// Legacy `initialize` still declares logging.
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
		// -32602 is not a recognized modern error, so a dual-era client falls back to `initialize`,
		// which this process serves.
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
		// An unimplemented version gets UnsupportedProtocolVersionError with the supported list. A
		// legacy version is refused here too, since it is reached through `initialize`, not `_meta`.
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

// --- the era latch: what does not latch ---

func TestLatch_AMalformedModernOpenerDoesNotLatch(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	// Modern-shaped (it carries the protocol-version key) but malformed: no clientCapabilities.
	resp, _ := c.call(t, "tools/list", map[string]any{"_meta": map[string]any{mcp.MetaKeyProtocolVersion: modern}})
	if resp.Error == nil || resp.Error.Code != mcp.CodeInvalidParams {
		t.Fatalf("malformed modern opener: %+v, want -32602", resp.Error)
	}
	// A following `initialize` still succeeds.
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
	// The client's retry from the advertised list is served, which requires -32022 not to latch.
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
	// A well-formed modern envelope naming a tool this server does not have is refused at admission
	// step 5, before the latch.
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
	if resp, _ := c.call(t, "initialize", map[string]any{"protocolVersion": 1}); resp.Error == nil {
		t.Fatal("a malformed initialize was accepted")
	}
	// The symmetric assertion to the modern one: a handshake that failed must not strand a modern
	// client either.
	resp, _ := c.call(t, "tools/list", modernParams(modern, nil))
	if resp.Error != nil {
		t.Fatalf("a refused initialize latched the legacy era: %+v", resp.Error)
	}
}

// --- the era latch: what does latch, and what happens after ---

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
	// The refusal names the supported versions, since a legacy client may surface only this message.
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
	// The legacy session still works after the refusal.
	if ok, _ := c.call(t, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}}); ok.Error != nil {
		t.Fatalf("the legacy session broke after refusing a modern request: %+v", ok.Error)
	}
}

func TestLatch_TwoPipelinedModernOpenersBothSucceed(t *testing.T) {
	// A host that pipelines its first two calls is common. Both frames arrive before either is
	// answered, so both are openers.
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
	for i := range 50 {
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

// pipelined feeds a server every frame before it has answered any, and returns the responses by id. It
// uses a pre-filled reader because over an unbuffered pipe the second write would block until the first
// response was read.
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
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
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

// --- --protocol legacy: a fully legacy server ---

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
	// An error response carries no `result` member.
	if resp.Result != nil {
		t.Errorf("the legacy-mode refusal carried a result member: %v", resp.Result)
	}
	// A dual-era client then falls back to `initialize`, which must work.
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
	// A modern-shaped `tools/list` before any handshake gets the legacy pre-initialization refusal,
	// not -32602, -32022 or a modern result.
	resp, _ := c.call(t, "tools/list", modernParams(modern, nil))
	if resp.Error == nil {
		t.Fatal("a legacy-pinned process served a modern-shaped request")
	}
	if resp.Error.Code != mcp.CodeNotInitialized {
		t.Errorf("code = %d, want %d (the legacy pre-initialization refusal)", resp.Error.Code, mcp.CodeNotInitialized)
	}
	handshake(t, c)
	// Once the session is live it is served as a legacy request; its modern `_meta` changes nothing.
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
			// A malformed level does not refuse the request; the field is accepted and ignored.
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
	// `notifications/progress` still flows on the modern era.
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
	// The modern stdio transport forbids server-to-client requests; Server.Request itself refuses.
	if reqErr == nil {
		t.Fatal("Server.Request succeeded on the modern era")
	}
	for _, n := range notes {
		if n.Method == "roots/list" {
			t.Fatal("a roots/list request frame reached stdout on the modern era")
		}
	}
}
