package mcp_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// These tests drive the real reviewmesh mcp binary over stdio on the 2026-07-28 era.
//
// The client is hand-written, which is weaker evidence than the SDK-driven legacy transcript. An SDK
// release that speaks 2026-07-28 supports only {2026-07-28, 2025-11-25}, and Go allows one module
// version per build, so adopting it would drop the legacy conformance client; docs/mcp.md notes this.
//
// Two rules are asserted over every line the process wrote:
//
//  1. No frame carries both an id and a method: a stdio server must not write requests to stdout.
//  2. No notifications/message frames, whether _meta.logLevel is absent, valid or malformed, before
//     or after any response. This server logs to stderr instead.

// modernClient is a hand-written 2026-07-28 client: it sends per-request _meta, never handshakes, and
// keeps every raw line the server wrote.
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

// meta returns the per-request protocol context. logLevel is passed verbatim, including malformed
// values.
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
	Params map[string]any  `json:"params"`
	raw    map[string]json.RawMessage
}

type wireErr struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

// readUntil reads frames until the response with id arrives, recording every line.
func (m *modernClient) readUntil(t *testing.T, id int) *wireResp {
	t.Helper()
	for {
		fr := m.next(t)
		if fr == nil {
			t.Fatalf("the server closed its stream before answering id %d", id)
		}
		var got int
		if len(fr.ID) > 0 && json.Unmarshal(fr.ID, &got) == nil && got == id && fr.Method == "" {
			return fr
		}
	}
}

func (m *modernClient) next(t *testing.T) *wireResp {
	t.Helper()
	fr, line, ok := m.nextQuiet()
	if !ok {
		return nil
	}
	if fr == nil {
		t.Fatalf("a stdout line of `reviewmesh mcp` is not a JSON object: %q", line)
	}
	return fr
}

// nextQuiet is next without a testing dependency, so a goroutine that may outlive the test can call it.
// Unparseable frames are caught by the transcript walk.
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

// call sends a request and waits for its response.
func (m *modernClient) call(t *testing.T, method string, params map[string]any) *wireResp {
	t.Helper()
	return m.readUntil(t, m.send(t, method, params))
}

func (m *modernClient) transcript() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.lines...)
}

// assertTheTwoMustNots checks every frame the process wrote to stdout.
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
		// A frame with both an id and a method is a request, which the server must not write.
		_, hasID := fields["id"]
		_, hasMethod := fields["method"]
		if hasID && hasMethod {
			t.Errorf("stdout line %d carries both `id` and `method` — that is a server-initiated REQUEST, and `basic/transports/stdio`: \"The server MUST NOT write JSON-RPC *requests* to `stdout`.\" Frame: %s", i, line)
		}
		var method string
		if hasMethod {
			_ = json.Unmarshal(fields["method"], &method)
		}
		if method == "notifications/message" {
			logs++
		}
	}
	// No log notifications anywhere in the transcript.
	if logs != 0 {
		t.Errorf("%d notifications/message frame(s) were written on the modern era. The Logging feature is Deprecated as of %s and its named migration for stdio servers is stderr, which this server already writes its diagnostics to; this server emits none at all on this revision, whatever a request's `%s` says", logs, proto.ProtocolVersion20260728, proto.MetaKeyLogLevel)
	}
}

// startMCP launches the real binary and returns a modern client and its stderr buffer.
func startMCP(t *testing.T, args ...string) (*modernClient, *lockedBuffer, func()) {
	t.Helper()
	bin := binary(t)
	cmd := exec.Command(bin, append([]string{"review", "mcp"}, args...)...)
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
	ws := subprocessWorkspace(t)
	c, stderr, stop := startMCP(t, "--adapter", "fake", "--root", ws, "--allow-writes", "--wait-seconds", "60")
	defer stop()

	// Send a stray notifications/initialized. The legacy log sink will not emit before it, so without it a
	// zero log count would not prove the era check. The server must also ignore it.
	if _, err := fmt.Fprintf(c.in, "%s\n", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		t.Fatal(err)
	}

	// --- the discovery probe, with no handshake before or after.
	disc := c.call(t, "server/discover", map[string]any{"_meta": c.meta(nil)})
	if disc.Error != nil {
		t.Fatalf("server/discover: %+v\nstderr:\n%s", disc.Error, stderr.String())
	}
	versions, _ := disc.Result["supportedVersions"].([]any)
	if len(versions) == 0 || versions[0] != proto.ProtocolVersion20260728 {
		t.Fatalf("supportedVersions = %v, want the modern revision first", disc.Result["supportedVersions"])
	}
	if len(versions) < 2 {
		t.Errorf("supportedVersions = %v — modern-ONLY would strand every legacy client this dual-mode process still serves", disc.Result["supportedVersions"])
	}
	caps, _ := disc.Result["capabilities"].(map[string]any)
	if _, declared := caps["logging"]; declared {
		t.Error("server/discover declared the `logging` capability while this server emits no notifications/message on this era")
	}
	if disc.Result["resultType"] != "complete" || disc.Result["ttlMs"] == nil || disc.Result["cacheScope"] != "private" {
		t.Errorf("the DiscoverResult is missing its modern envelope: %v", disc.Result)
	}
	// The probe answers anyone who can spawn the process, so it must contain no host paths.
	instr, _ := disc.Result["instructions"].(string)
	info, _ := disc.Result["_meta"].(map[string]any)
	blob := instr + fmt.Sprint(info) + fmt.Sprint(caps)
	for _, leak := range []string{ws, "/Users/", "/home/", "/var/folders/", "C:\\"} {
		if strings.Contains(blob, leak) {
			t.Errorf("the probe's projection leaked a host path (%q) — `server/discover` is answerable by anyone who can spawn this process, before anything else, so it falls under the same no-paths rule as `list` and `doctor`", leak)
		}
	}

	// --- tools/list under the modern envelope.
	list := c.call(t, "tools/list", map[string]any{"_meta": c.meta(nil)})
	if list.Error != nil {
		t.Fatalf("tools/list: %+v\nstderr:\n%s", list.Error, stderr.String())
	}
	tools, _ := list.Result["tools"].([]any)
	if len(tools) != 7 {
		t.Errorf("tools = %d, want 7 (the write tool is granted at launch)", len(tools))
	}
	if list.Result["resultType"] != "complete" || list.Result["ttlMs"] == nil {
		t.Errorf("tools/list is missing its caching hints: %v", list.Result)
	}

	// --- doctor discloses the protocol mode.
	doc := c.call(t, "tools/call", map[string]any{
		"name": "review_doctor", "arguments": map[string]any{}, "_meta": c.meta(nil),
	})
	if doc.Error != nil {
		t.Fatalf("doctor: %+v", doc.Error)
	}
	payload, _ := doc.Result["structuredContent"].(map[string]any)
	if payload["protocolMode"] != "dual" {
		t.Errorf("doctor.protocolMode = %v, want dual", payload["protocolMode"])
	}
	if payload["protocolEra"] != "modern" {
		t.Errorf("doctor.protocolEra = %v, want modern — the call is being served under %s", payload["protocolEra"], proto.ProtocolVersion20260728)
	}

	// --- a real run with a progress token and a valid log level: progress flows, logs do not.
	run := c.call(t, "tools/call", map[string]any{
		"name": "review_report", "arguments": map[string]any{"workspace": ws, "panel": defaultPanel()},
		"_meta": mergeMeta(c.meta("debug"), map[string]any{"progressToken": "modern-1"}),
	})
	if run.Error != nil {
		t.Fatalf("review_report: %+v\nstderr:\n%s", run.Error, stderr.String())
	}
	if isErr, _ := run.Result["isError"].(bool); isErr {
		t.Fatalf("review_report halted: %v\nstderr:\n%s", run.Result, stderr.String())
	}
	out, _ := run.Result["structuredContent"].(map[string]any)
	if out["state"] != "complete" {
		t.Fatalf("state = %v, want complete", out["state"])
	}
	if run.Result["resultType"] != "complete" {
		t.Errorf("a tools/call result carried no resultType: %v", run.Result)
	}
	// tools/call is not cacheable, so no ttlMs.
	if _, has := run.Result["ttlMs"]; has {
		t.Errorf("tools/call carried a caching hint: %v", run.Result)
	}

	// --- a malformed log level is served, not refused: this server does not implement per-request logging.
	bad := c.call(t, "tools/call", map[string]any{
		"name": "review_report", "arguments": map[string]any{"workspace": ws, "panel": defaultPanel()},
		"_meta": c.meta("not-a-level"),
	})
	if bad.Error != nil {
		t.Fatalf("a malformed `%s` refused the whole call: %+v — refusing over the spelling of a field this server never reads would be hostile, and would make the non-adoption more visible than adoption", proto.MetaKeyLogLevel, bad.Error)
	}

	// --- the two methods removed in this era.
	for _, method := range []string{"ping", "logging/setLevel"} {
		resp := c.call(t, method, map[string]any{"_meta": c.meta(nil), "level": "debug"})
		if resp.Error == nil || resp.Error.Code != proto.CodeMethodNotFound {
			t.Errorf("%s: %+v, want -32601 (changelog, Major changes #5: \"Remove `ping`, `logging/setLevel`, and `notifications/roots/list_changed`\")", method, resp.Error)
		}
	}

	// --- initialize on a modern process names the supported versions.
	init := c.call(t, "initialize", map[string]any{"protocolVersion": proto.LatestProtocolVersion})
	if init.Error == nil {
		t.Fatal("initialize succeeded on a modern-latched process")
	}
	if init.Error.Data == nil || init.Error.Data["supported"] == nil {
		t.Errorf("the initialize refusal named no supported versions: %+v — legacy clients have no fall-forward mechanism and this message may be the only diagnostic they can surface", init.Error)
	}

	// --- resources/list, a cacheable operation.
	res := c.call(t, "resources/list", map[string]any{"_meta": c.meta(nil)})
	if res.Error != nil {
		t.Fatalf("resources/list: %+v", res.Error)
	}
	if res.Result["cacheScope"] != "private" {
		t.Errorf("resources/list cacheScope = %v, want private", res.Result["cacheScope"])
	}

	// --- a quiet window after the last response, to catch late emissions.
	drainFor(t, c, 750*time.Millisecond)

	lines := c.transcript()
	// The transcript must cover a real conversation, or the assertion could pass vacuously.
	if len(lines) < 12 {
		t.Fatalf("captured only %d stdout line(s); the MUST NOTs are asserted over a transcript, and this one is too short to be one:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	progress := 0
	for _, line := range lines {
		if strings.Contains(line, `"notifications/progress"`) {
			progress++
		}
	}
	if progress == 0 {
		t.Error("no notifications/progress arrived on the modern era — the zero-log assertion is only meaningful if the channel that REPLACED logging is live, and `notifications/progress` is the one the specification kept")
	}
	assertTheTwoMustNots(t, lines)
}

func mergeMeta(base map[string]any, extra map[string]any) map[string]any {
	maps.Copy(base, extra)
	return base
}

// drainFor reads for d, recording anything the server sends with no request pending. The read runs on
// its own goroutine because exec pipes have no read deadline, so that goroutine must not touch t.
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

func TestSubprocess_LegacyProtocolModeIsGenuinelyLegacy(t *testing.T) {
	ws := subprocessWorkspace(t)
	c, stderr, stop := startMCP(t, "--protocol", "legacy", "--adapter", "fake", "--root", ws, "--wait-seconds", "60")
	defer stop()

	// A modern probe answered by a process pinned to the legacy era.
	disc := c.call(t, "server/discover", map[string]any{"_meta": c.meta(nil)})
	if disc.Error == nil {
		t.Fatalf("a legacy-pinned process answered the probe: %v\nA `DiscoverResult` of ANY content identifies a modern server, so a dual-era client would look for a mutually supported modern version, find none it could use, and fail — with this process's legacy surface sitting right there unreachable", disc.Result)
	}
	if disc.Error.Code != proto.CodeMethodNotFound {
		t.Errorf("code = %d, want -32601 — the same answer any other unimplemented method gets", disc.Error.Code)
	}
	if disc.Result != nil {
		t.Errorf("the refusal carried a result member: %v", disc.Result)
	}
	if _, hasResult := disc.raw["result"]; hasResult {
		t.Error("the refusal frame carries a `result` key at all — no DiscoverResult in any form")
	}

	// A dual-era client falls back to initialize when a legacy server does not answer the probe.
	init := c.call(t, "initialize", map[string]any{
		"protocolVersion": proto.LatestProtocolVersion,
		"clientInfo":      map[string]any{"name": "fallback-client", "version": "1"},
		"capabilities":    map[string]any{},
	})
	if init.Error != nil {
		t.Fatalf("the fallback handshake failed: %+v\nstderr:\n%s", init.Error, stderr.String())
	}
	if init.Result["protocolVersion"] != proto.LatestProtocolVersion {
		t.Errorf("protocolVersion = %v, want the client's own echoed", init.Result["protocolVersion"])
	}
	// Legacy initialize still declares logging.
	icaps, _ := init.Result["capabilities"].(map[string]any)
	if icaps["logging"] == nil {
		t.Error("legacy initialize stopped declaring the logging capability — the legacy surface must not move")
	}
	if _, err := fmt.Fprintf(c.in, "%s\n", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		t.Fatal(err)
	}
	call := c.call(t, "tools/call", map[string]any{
		"name": "review_report", "arguments": map[string]any{"workspace": ws, "panel": defaultPanel()},
	})
	if call.Error != nil {
		t.Fatalf("the legacy call failed: %+v\nstderr:\n%s", call.Error, stderr.String())
	}
	if call.Result["resultType"] != nil {
		t.Errorf("a legacy-mode result carried resultType: %v — a 2025-06-18 response should be exactly what 2025-06-18 defines", call.Result)
	}
	// The mode is disclosed through doctor, not only on stderr.
	doc := c.call(t, "tools/call", map[string]any{"name": "review_doctor", "arguments": map[string]any{}})
	payload, _ := doc.Result["structuredContent"].(map[string]any)
	if payload["protocolMode"] != "legacy" {
		t.Errorf("doctor.protocolMode = %v, want legacy", payload["protocolMode"])
	}
	if !strings.Contains(stderr.String(), "compatibility fallback") {
		t.Errorf("the legacy pin was not announced at launch:\n%s", stderr.String())
	}
}

func TestSubprocess_ModernWithNoRootCeilingReviewsTheWorkspaceACallDeclares(t *testing.T) {
	// No --root and no setup: only an adapter is named, and each call declares its own absolute workspace.
	c, stderr, stop := startMCP(t, "--adapter", "fake", "--wait-seconds", "60")
	defer stop()

	ws := subprocessWorkspace(t)
	accepted := c.call(t, "tools/call", map[string]any{
		"name": "review_report", "arguments": map[string]any{"workspace": ws, "panel": defaultPanel()}, "_meta": c.meta(nil),
	})
	if accepted.Error != nil {
		t.Fatalf("review_report: %+v\nstderr:\n%s", accepted.Error, stderr.String())
	}
	if isErr, _ := accepted.Result["isError"].(bool); isErr {
		t.Fatalf("a declared absolute workspace was refused with no ceiling set: %v\nstderr:\n%s", accepted.Result["structuredContent"], stderr.String())
	}

	// A relative workspace is a malformed request.
	relative := c.call(t, "tools/call", map[string]any{
		"name": "review_report", "arguments": map[string]any{"workspace": "project", "panel": defaultPanel()}, "_meta": c.meta(nil),
	})
	if relative.Error == nil {
		t.Errorf("a relative workspace must be refused as invalid params, got %v", relative.Result)
	}

	inline := c.call(t, "tools/call", map[string]any{
		"name": "review_report",
		"arguments": map[string]any{"inlineWorkspace": map[string]any{
			"sample.go": "package sample\n\nfunc Sample() int { return 1 }\n",
		}, "panel": defaultPanel()},
		"_meta": c.meta(nil),
	})
	if inline.Error != nil {
		t.Fatalf("inline review_report: %+v\nstderr:\n%s", inline.Error, stderr.String())
	}
	if isErr, _ := inline.Result["isError"].(bool); isErr {
		t.Fatalf("the inline review was refused: %v", inline.Result["structuredContent"])
	}
	assertTheTwoMustNots(t, c.transcript())
}
