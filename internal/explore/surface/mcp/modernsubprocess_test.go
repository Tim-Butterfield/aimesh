package mcp_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// The exploremesh twin of reviewmesh's modern-era subprocess test: the REAL binary, real stdio, the
// 2026-07-28 revision, and the two MUST NOTs asserted over every frame the process wrote.
//
// The pair exists because the invariants are properties of the shared transport but the STREAM is a
// property of each binary. exploremesh spawns model CLIs too, and it is the server whose stdout-purity
// golden already exists — so if a frame with an `id` and a `method` were ever going to appear, it
// would appear here just as readily.
//
// The client is hand-written for the same recorded reason as reviewmesh's: adopting an SDK release
// that speaks 2026-07-28 would replace the legacy conformance client too (one module version per
// build), and that would have voided the byte-identical legacy transcript that must be preserved.

type modernClient struct {
	in  io.Writer
	sc  *bufio.Scanner
	seq int

	mu    sync.Mutex
	lines []string
}

func newModernClient(in io.Writer, out io.Reader) *modernClient {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	return &modernClient{in: in, sc: sc}
}

func (m *modernClient) meta(logLevel any) map[string]any {
	out := map[string]any{
		proto.MetaKeyProtocolVersion:    proto.ProtocolVersion20260728,
		proto.MetaKeyClientCapabilities: map[string]any{},
		proto.MetaKeyClientInfo:         map[string]any{"name": "modern-subprocess-client", "version": "1"},
	}
	if logLevel != nil {
		out[proto.MetaKeyLogLevel] = logLevel
	}
	return out
}

func (m *modernClient) send(t *testing.T, method string, params map[string]any) int {
	t.Helper()
	m.seq++
	req := map[string]any{"jsonrpc": "2.0", "id": m.seq, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal %s: %v", method, err)
	}
	if _, err := m.in.Write(append(b, '\n')); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	return m.seq
}

type wireResp struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *wireErr        `json:"error"`
	Method string          `json:"method"`
	raw    map[string]json.RawMessage
}

type wireErr struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func (m *modernClient) nextQuiet() (*wireResp, string, bool) {
	if !m.sc.Scan() {
		return nil, "", false
	}
	line := m.sc.Text()
	m.mu.Lock()
	m.lines = append(m.lines, line)
	m.mu.Unlock()
	var fr wireResp
	if json.Unmarshal([]byte(line), &fr) != nil || json.Unmarshal([]byte(line), &fr.raw) != nil {
		return nil, line, true
	}
	return &fr, line, true
}

func (m *modernClient) call(t *testing.T, method string, params map[string]any) *wireResp {
	t.Helper()
	id := m.send(t, method, params)
	for {
		fr, line, ok := m.nextQuiet()
		if !ok {
			t.Fatalf("the server closed its stream before answering %s (id %d)", method, id)
		}
		if fr == nil {
			t.Fatalf("a stdout line of `exploremesh mcp` is not a JSON object: %q", line)
		}
		var got int
		if len(fr.ID) > 0 && json.Unmarshal(fr.ID, &got) == nil && got == id && fr.Method == "" {
			return fr
		}
	}
}

func (m *modernClient) transcript() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.lines...)
}

func drainFor(t *testing.T, c *modernClient, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, ok := c.nextQuiet(); !ok {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// assertTheTwoMustNots walks every frame the process wrote to stdout.
//
//  1. No frame carries both an `id` and a `method` — `basic/transports/stdio`: "The server MUST NOT
//     write JSON-RPC *requests* to `stdout`."
//  2. Zero `notifications/message` frames, whatever `_meta.logLevel` said.
func assertTheTwoMustNots(t *testing.T, lines []string) {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("captured no stdout at all; an empty stream proves nothing")
	}
	logs := 0
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("stdout line %d is not a JSON object: %q", i, line)
		}
		if string(fields["jsonrpc"]) != `"2.0"` {
			t.Errorf("stdout line %d is not JSON-RPC 2.0: %q", i, line)
		}
		_, hasID := fields["id"]
		_, hasMethod := fields["method"]
		if hasID && hasMethod {
			t.Errorf("stdout line %d carries both `id` and `method` — that is a server-initiated REQUEST, which `basic/transports/stdio` forbids a server to write. Frame: %s", i, line)
		}
		var method string
		if hasMethod {
			_ = json.Unmarshal(fields["method"], &method)
		}
		if method == "notifications/message" {
			logs++
		}
	}
	if logs != 0 {
		t.Errorf("%d notifications/message frame(s) were written on the modern era. The Logging feature is Deprecated as of %s and its named migration for stdio servers is stderr; this server emits none at all on this revision, whatever a request's `%s` says", logs, proto.ProtocolVersion20260728, proto.MetaKeyLogLevel)
	}
}

func startMCP(t *testing.T, args ...string) (*modernClient, *lockedBuffer, func()) {
	t.Helper()
	bin := binary(t)
	cmd := exec.Command(bin, append([]string{"explore", "mcp"}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = hermeticEnv(t)
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	stop := func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
	return newModernClient(stdin, stdout), &stderr, stop
}

func TestSubprocess_ModernEraTranscriptOverTheRealBinary(t *testing.T) {
	// `--protocol dual` is passed EXPLICITLY here, and omitted in reviewmesh's twin, so both spellings
	// are exercised: the flag's stated value, and the default a host actually gets from a config file
	// that names no flags at all. They must be the same process.
	c, stderr, stop := startMCP(t, "--protocol", "dual", "--wait-seconds", "60")
	defer stop()

	// A STRAY `notifications/initialized` from a client that will then speak nothing but 2026-07-28.
	// It is here for a specific reason: the legacy log sink
	// independently refuses to emit before that notification arrives, so a modern transcript that
	// never sends it would report zero log frames even with the era check REMOVED — true for the
	// wrong reason, and catching nothing. Sending it leaves the era check as the only guard.
	if _, err := fmt.Fprintf(c.in, "%s\n", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		t.Fatal(err)
	}

	disc := c.call(t, "server/discover", map[string]any{"_meta": c.meta(nil)})
	if disc.Error != nil {
		t.Fatalf("server/discover: %+v\nstderr:\n%s", disc.Error, stderr.String())
	}
	versions, _ := disc.Result["supportedVersions"].([]any)
	if len(versions) < 2 || versions[0] != proto.ProtocolVersion20260728 {
		t.Fatalf("supportedVersions = %v, want every implemented revision with the modern one first", disc.Result["supportedVersions"])
	}
	caps, _ := disc.Result["capabilities"].(map[string]any)
	if _, declared := caps["logging"]; declared {
		t.Error("server/discover declared the `logging` capability while this server emits no notifications/message on this era")
	}
	if caps["resources"] == nil {
		t.Errorf("capabilities = %v, want `resources` declared — this server publishes run artifacts", caps)
	}
	blob, _ := disc.Result["instructions"].(string)
	for _, leak := range []string{"/Users/", "/home/", "/var/folders/", "C:\\"} {
		if strings.Contains(blob, leak) {
			t.Errorf("the probe's instructions leaked a host path (%q)", leak)
		}
	}

	doc := c.call(t, "tools/call", map[string]any{
		"name": "explore_doctor", "arguments": map[string]any{}, "_meta": c.meta(nil),
	})
	if doc.Error != nil {
		t.Fatalf("doctor: %+v", doc.Error)
	}
	payload, _ := doc.Result["structuredContent"].(map[string]any)
	if payload["protocolMode"] != "dual" || payload["protocolEra"] != "modern" {
		t.Errorf("doctor protocol disclosure = mode %v / era %v, want dual / modern", payload["protocolMode"], payload["protocolEra"])
	}

	// A real run, with a progress token AND a valid log level: progress must flow, logs must not.
	run := c.call(t, "tools/call", map[string]any{
		"name": "explore",
		"arguments": map[string]any{
			"purpose": "choose a datastore for the ingest service", "criteria": []string{"cost", "latency"}, "mode": "map",
		},
		"_meta": mergeMeta(c.meta("debug"), map[string]any{"progressToken": "modern-1"}),
	})
	if run.Error != nil {
		t.Fatalf("explore: %+v\nstderr:\n%s", run.Error, stderr.String())
	}
	if isErr, _ := run.Result["isError"].(bool); isErr {
		t.Fatalf("explore halted: %v\nstderr:\n%s", run.Result["structuredContent"], stderr.String())
	}
	if run.Result["resultType"] != "complete" {
		t.Errorf("a tools/call result carried no resultType: %v", run.Result)
	}

	// The same run shape with a MALFORMED log level must still be served: the unrecognized-level
	// -32602 SHOULD attaches to servers that implement per-request logging, and this one declines the
	// feature outright rather than half-implementing its error handling.
	bad := c.call(t, "tools/call", map[string]any{
		"name": "explore",
		"arguments": map[string]any{
			"purpose": "choose a queue", "criteria": []string{"cost"}, "mode": "map",
		},
		"_meta": c.meta("not-a-level"),
	})
	if bad.Error != nil {
		t.Fatalf("a malformed `%s` refused the whole call: %+v", proto.MetaKeyLogLevel, bad.Error)
	}

	for _, method := range []string{"ping", "logging/setLevel"} {
		resp := c.call(t, method, map[string]any{"_meta": c.meta(nil), "level": "debug"})
		if resp.Error == nil || resp.Error.Code != proto.CodeMethodNotFound {
			t.Errorf("%s: %+v, want -32601", method, resp.Error)
		}
	}

	init := c.call(t, "initialize", map[string]any{"protocolVersion": proto.LatestProtocolVersion})
	if init.Error == nil {
		t.Fatal("initialize succeeded on a modern-latched process")
	}
	if init.Error.Data == nil || init.Error.Data["supported"] == nil {
		t.Errorf("the initialize refusal named no supported versions: %+v", init.Error)
	}

	drainFor(t, c, 750*time.Millisecond)

	lines := c.transcript()
	if len(lines) < 10 {
		t.Fatalf("captured only %d stdout line(s); the MUST NOTs are asserted over a transcript:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	progress := 0
	for _, line := range lines {
		if strings.Contains(line, `"notifications/progress"`) {
			progress++
		}
	}
	if progress == 0 {
		t.Error("no notifications/progress arrived — the zero-log assertion is only meaningful if the channel that replaced logging is live")
	}
	assertTheTwoMustNots(t, lines)
}

func mergeMeta(base map[string]any, extra map[string]any) map[string]any {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

func TestSubprocess_LegacyProtocolModeIsGenuinelyLegacy(t *testing.T) {
	c, stderr, stop := startMCP(t, "--protocol", "legacy", "--wait-seconds", "60")
	defer stop()

	disc := c.call(t, "server/discover", map[string]any{"_meta": c.meta(nil)})
	if disc.Error == nil {
		t.Fatalf("a legacy-pinned process answered the probe: %v", disc.Result)
	}
	if disc.Error.Code != proto.CodeMethodNotFound {
		t.Errorf("code = %d, want -32601", disc.Error.Code)
	}
	if _, hasResult := disc.raw["result"]; hasResult {
		t.Error("the refusal frame carries a `result` key at all — no DiscoverResult in any form")
	}

	init := c.call(t, "initialize", map[string]any{
		"protocolVersion": proto.LatestProtocolVersion,
		"clientInfo":      map[string]any{"name": "fallback-client", "version": "1"},
		"capabilities":    map[string]any{},
	})
	if init.Error != nil {
		t.Fatalf("the fallback handshake failed: %+v\nstderr:\n%s", init.Error, stderr.String())
	}
	if _, err := fmt.Fprintf(c.in, "%s\n", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		t.Fatal(err)
	}
	call := c.call(t, "tools/call", map[string]any{
		"name": "explore",
		"arguments": map[string]any{
			"purpose": "choose a datastore", "criteria": []string{"cost"}, "mode": "map",
		},
	})
	if call.Error != nil {
		t.Fatalf("the legacy call failed: %+v\nstderr:\n%s", call.Error, stderr.String())
	}
	if call.Result["resultType"] != nil {
		t.Errorf("a legacy-mode result carried resultType: %v", call.Result)
	}
	doc := c.call(t, "tools/call", map[string]any{"name": "explore_doctor", "arguments": map[string]any{}})
	payload, _ := doc.Result["structuredContent"].(map[string]any)
	if payload["protocolMode"] != "legacy" || payload["protocolEra"] != "legacy" {
		t.Errorf("doctor protocol disclosure = mode %v / era %v, want legacy / legacy", payload["protocolMode"], payload["protocolEra"])
	}
}
