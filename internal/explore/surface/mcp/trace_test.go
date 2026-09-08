package mcp_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The OpenTelemetry `_meta` trace context, end to end over the REAL binary: a modern `tools/call`
// carrying `traceparent`/`tracestate`/`baggage` must leave those values in the run directory this
// server writes, so the run is correlatable with whatever traced the caller.
//
// WHY THIS IS THE TEST THAT MATTERS. The transport has parsed these three keys since the modern era
// became reachable, and nothing read them: `RequestEnv.Trace` was filled and dropped. A unit test on
// the parser would have passed at every point in that history. This one asserts the only property a
// caller can observe — that the identity survives the whole path from `_meta` to disk — and it fails
// against a tree where the surface parses the keys and forgets them, which is exactly the tree this
// test was written against.
//
// It also pins the honest half. `measures: "correlation_only"` is written INTO the record, because no
// run in this repo records token counts or cost; a reader who finds a trace id must not conclude the
// run was metered.
//
// The values are carried VERBATIM and never validated — `basic/index` §`_meta` says of every reserved
// key that "implementations MUST NOT make assumptions about values at these keys", so the malformed
// `traceparent` below must be recorded exactly as it arrived rather than refused. A server that
// validated the W3C format would fail this test, deliberately: that MUST binds the sender.
func TestSubprocess_ModernTraceContextReachesTheRunRecord(t *testing.T) {
	artifacts := t.TempDir()
	c, stderr, stop := startMCPWithArtifacts(t, artifacts, "--protocol", "dual", "--wait-seconds", "60")
	defer stop()

	const (
		wantParent = "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01"
		wantState  = "congo=t61rcWkgMzE"
		wantBag    = "userId=alice,serverNode=DF%2028"
	)

	run := c.call(t, "tools/call", map[string]any{
		"name": "explore",
		"arguments": map[string]any{
			"purpose": "choose a datastore for the ingest service", "criteria": []string{"cost", "latency"}, "mode": "map",
		},
		"_meta": mergeMeta(c.meta(nil), map[string]any{
			"traceparent": wantParent, "tracestate": wantState, "baggage": wantBag,
		}),
	})
	if run.Error != nil {
		t.Fatalf("explore: %+v\nstderr:\n%s", run.Error, stderr.String())
	}
	payload, _ := run.Result["structuredContent"].(map[string]any)
	if captured, _ := payload["captured"].(bool); !captured {
		t.Fatalf("the run was not captured, so there is no record to correlate: %v", payload)
	}
	runID, _ := payload["runId"].(string)
	if runID == "" {
		t.Fatalf("no runId in %v", payload)
	}

	m := readManifest(t, filepath.Join(artifacts, runID, "manifest.json"))
	trace, ok := m["trace"].(map[string]any)
	if !ok {
		t.Fatalf("the run record carries no `trace` block; the caller's trace context reached the transport and was dropped:\n%v", m)
	}
	for _, want := range []struct{ key, value string }{
		{"traceparent", wantParent}, {"tracestate", wantState}, {"baggage", wantBag},
	} {
		if got, _ := trace[want.key].(string); got != want.value {
			t.Errorf("trace.%s = %q, want the caller's own %q carried verbatim", want.key, got, want.value)
		}
	}
	// The honesty field: this block is a correlation identity and NOT spend accounting.
	if got, _ := trace["measures"].(string); got != "correlation_only" {
		t.Errorf("trace.measures = %q, want %q — the record must state that it correlates a run without metering it",
			got, "correlation_only")
	}

	// A MALFORMED traceparent is recorded, not refused. `basic/index` reserves the key and forbids
	// implementations from assuming anything about its value; the W3C-format MUST binds the sender.
	const malformed = "not-a-w3c-traceparent"
	bad := c.call(t, "tools/call", map[string]any{
		"name": "explore",
		"arguments": map[string]any{
			"purpose": "choose a queue", "criteria": []string{"cost"}, "mode": "map",
		},
		"_meta": mergeMeta(c.meta(nil), map[string]any{"traceparent": malformed}),
	})
	if bad.Error != nil {
		t.Fatalf("a malformed traceparent refused the whole call: %+v — the format MUST binds the sender, not this server", bad.Error)
	}
	badPayload, _ := bad.Result["structuredContent"].(map[string]any)
	badID, _ := badPayload["runId"].(string)
	bm := readManifest(t, filepath.Join(artifacts, badID, "manifest.json"))
	badTrace, _ := bm["trace"].(map[string]any)
	if got, _ := badTrace["traceparent"].(string); got != malformed {
		t.Errorf("trace.traceparent = %q, want the malformed value %q recorded exactly as it arrived", got, malformed)
	}
}

// A run whose caller sent NO trace context writes no `trace` key at all — the absence is what keeps
// every existing run record byte-identical to one written before the field existed, which is what
// let this ship without moving the manifest's schema version.
func TestSubprocess_ARunWithoutTraceContextRecordsNoTraceBlock(t *testing.T) {
	artifacts := t.TempDir()
	c, stderr, stop := startMCPWithArtifacts(t, artifacts, "--protocol", "dual", "--wait-seconds", "60")
	defer stop()

	run := c.call(t, "tools/call", map[string]any{
		"name": "explore",
		"arguments": map[string]any{
			"purpose": "choose a datastore", "criteria": []string{"cost"}, "mode": "map",
		},
		"_meta": c.meta(nil),
	})
	if run.Error != nil {
		t.Fatalf("explore: %+v\nstderr:\n%s", run.Error, stderr.String())
	}
	payload, _ := run.Result["structuredContent"].(map[string]any)
	runID, _ := payload["runId"].(string)
	m := readManifest(t, filepath.Join(artifacts, runID, "manifest.json"))
	if _, present := m["trace"]; present {
		t.Errorf("a run whose caller sent no trace context wrote a `trace` block anyway: %v", m["trace"])
	}
}

func readManifest(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read run record %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, b)
	}
	return m
}

// startMCPWithArtifacts is startMCP with the run-capture directory named by the caller, so a test can
// read back the record the server wrote. startMCP's own hermetic env points the directory at a temp
// dir it does not return.
func startMCPWithArtifacts(t *testing.T, artifacts string, args ...string) (*modernClient, *lockedBuffer, func()) {
	t.Helper()
	bin := binary(t)
	cmd := exec.Command(bin, append([]string{"explore", "mcp"}, args...)...)
	cmd.Dir = t.TempDir()
	// hermeticEnv points the artifact dir at a temp dir it does not return; the entry is REPLACED
	// rather than appended, because a duplicated key in an exec environment is resolved differently
	// on different platforms and a test that reads the wrong directory would look like a lost trace.
	var env []string
	for _, kv := range hermeticEnv(t) {
		if strings.HasPrefix(kv, "EXPLOREMESH_ARTIFACT_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "EXPLOREMESH_ARTIFACT_DIR="+artifacts)
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
