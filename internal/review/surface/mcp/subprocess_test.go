package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file drives the REAL `reviewmesh mcp` binary over real stdio with the official SDK client, and
// asserts the STDOUT PURITY invariant end to end: while a run is in flight — progress and log
// notifications interleaving with responses — every line on the process's stdout parses as JSON-RPC 2.0.
//
// A stdio protocol server that spawns model CLIs gets exactly one chance at this: a single stray
// `fmt.Println` anywhere in the process, or a child that inherits the descriptor, corrupts a frame and
// takes the session down in a way that is very hard to diagnose from the client side. The binary
// therefore repoints `os.Stdout` at stderr before serving (the protocol stream is the writer captured
// beforehand), and meshcore's adapters capture child stdio explicitly. This test is the assertion that
// the arrangement holds for the shipped command, not just for the server type.
//
// It is the twin of exploremesh/internal/surface/mcp/subprocess_test.go. reviewmesh needs it MORE, not
// less: this binary announces a write tool at launch, on the same stderr the purity guard redirects to.

var (
	buildOnce sync.Once
	testBin   string
	buildErr  error
)

// binary builds the reviewmesh binary once for this package.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "reviewmesh-mcp-bin")
		if err != nil {
			buildErr = err
			return
		}
		testBin = filepath.Join(dir, "reviewmesh")
		if runtime.GOOS == "windows" {
			testBin += ".exe"
		}
		build := exec.Command("go", "build", "-o", testBin, "github.com/Tim-Butterfield/aimesh/cmd/aimesh")
		if out, berr := build.CombinedOutput(); berr != nil {
			buildErr = &buildFailure{err: berr, out: string(out)}
		}
	})
	if buildErr != nil {
		t.Fatalf("build reviewmesh: %v", buildErr)
	}
	return testBin
}

type buildFailure struct {
	err error
	out string
}

func (b *buildFailure) Error() string { return b.err.Error() + "\n" + b.out }

// hermeticEnv isolates the child from the developer's real configuration and points it at the
// shipped-but-hidden `fake-smoke` profile: a deterministic, fully in-process panel. No real CLI is
// ever spawned, and no artifact lands anywhere but a temp directory.
func hermeticEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".aimesh", "review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("schemaVersion: 1\ndefaultProfile: fake-smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(),
		"AIMESH_HOME="+home,
		"REVIEWMESH_ARTIFACT_DIR="+t.TempDir(),
		"REVIEWMESH_FAKE_SCENARIO=valid", // deterministic 1-finding report
		"AIMESH_INTERNAL_FAKE=1",         // unlock the hidden internal fake harness for the child
	)
}

// subprocessWorkspace is the reviewed tree — a real directory, because this server takes paths.
func subprocessWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "sample.go"), []byte("package sample\n\nfunc Sample() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// teeReadCloser feeds the SDK client while copying every byte of the protocol stream into a buffer, so
// the purity assertion sees exactly what the server wrote.
type teeReadCloser struct {
	r io.Reader
	c io.Closer
}

func (t teeReadCloser) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t teeReadCloser) Close() error               { return t.c.Close() }

func TestSubprocess_StdoutStaysPureJSONRPCThroughARealRun(t *testing.T) {
	bin := binary(t)
	ws := subprocessWorkspace(t)
	// `--allow-remediate` is passed deliberately: it is the launch that PRINTS TO STDERR before
	// serving (the write-capability banner), which is exactly the kind of print that would corrupt the
	// protocol stream if the redirect were wrong. The child runs in a THROWAWAY cwd so that config
	// resolution cannot bind it to this repo's project-scope config instead of the hermetic home.
	cmd := exec.Command(bin, "review", "mcp", "--root", ws, "--allow-remediate", "--wait-seconds", "60")
	cmd.Dir = t.TempDir()
	cmd.Env = hermeticEnv(t)
	// `stderr` must be the concurrency-safe buffer: os/exec copies the child's stderr on its own
	// goroutine, and this test reads the accumulated text (in failure messages, and in the banner
	// assertion) while the child is still running. A plain strings.Builder here is a real data race.
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
	var captured lockedBuffer
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	}()

	var progressSeen, logSeen bool
	var mu sync.Mutex
	client := sdk.NewClient(&sdk.Implementation{Name: "subprocess-client", Version: "1"}, &sdk.ClientOptions{
		ProgressNotificationHandler: func(ctx context.Context, req *sdk.ProgressNotificationClientRequest) {
			mu.Lock()
			progressSeen = true
			mu.Unlock()
		},
		LoggingMessageHandler: func(ctx context.Context, req *sdk.LoggingMessageRequest) {
			mu.Lock()
			logSeen = true
			mu.Unlock()
		},
	})
	transport := &sdk.IOTransport{
		Reader: teeReadCloser{r: io.TeeReader(stdout, &captured), c: stdout},
		Writer: stdin,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to `reviewmesh mcp`: %v\nstderr:\n%s", err, stderr.String())
	}
	defer func() { _ = session.Close() }()

	if err := session.SetLoggingLevel(ctx, &sdk.SetLoggingLevelParams{Level: "debug"}); err != nil {
		t.Fatalf("logging/setLevel: %v", err)
	}
	list, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(list.Tools) != 7 {
		names := make([]string, 0, len(list.Tools))
		for _, tool := range list.Tools {
			names = append(names, tool.Name)
		}
		t.Errorf("tools = %v, want 7 (the write tool is granted at launch)", names)
	}

	params := &sdk.CallToolParams{Name: "review_report", Arguments: map[string]any{"workspace": ws}}
	params.Meta = sdk.Meta{"progressToken": "sub-1"}
	res, err := session.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("tools/call review_report: %v\nstderr:\n%s", err, stderr.String())
	}
	if res.IsError {
		t.Fatalf("review_report halted: %s\nstderr:\n%s", textOf(res), stderr.String())
	}
	out := structured(t, res)
	if out["state"] != "complete" {
		t.Fatalf("state = %v, want complete (payload %v)", out["state"], out)
	}
	// A real run through the real binary still carries the whole governed payload.
	for _, key := range []string{"counts", "identityCaveats", "authority", "withheld", "panel", "workspaceSource"} {
		if _, ok := out[key]; !ok {
			t.Errorf("the shipped binary dropped %q from the governed payload: %v", key, out)
		}
	}
	// And a REPORT wrote nothing: the reviewed file is byte-for-byte what the fixture created.
	after, rerr := os.ReadFile(filepath.Join(ws, "sample.go"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(after) != "package sample\n\nfunc Sample() int { return 1 }\n" {
		t.Errorf("review_report modified the workspace:\n%s", after)
	}

	// The purity assertion, over everything the process wrote to stdout.
	lines := strings.Split(strings.TrimSpace(captured.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("captured %d stdout line(s); expected the handshake plus notifications:\n%s", len(lines), captured.String())
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("stdout line %d of `reviewmesh mcp` is not JSON: %q", i, line)
		}
		if v["jsonrpc"] != "2.0" {
			t.Errorf("stdout line %d is not JSON-RPC 2.0: %q", i, line)
		}
	}
	mu.Lock()
	gotProgress, gotLog := progressSeen, logSeen
	mu.Unlock()
	if !gotProgress || !gotLog {
		t.Errorf("the purity assertion is only meaningful with notifications interleaved (progress=%v log=%v)", gotProgress, gotLog)
	}
	// The write-capability banner really did go to stderr — proving the stream the purity assertion
	// just walked is not simply an empty-stderr accident.
	if !strings.Contains(stderr.String(), "REMEDIATION ENABLED") {
		t.Errorf("expected the launch banner on stderr, got:\n%s", stderr.String())
	}
}
