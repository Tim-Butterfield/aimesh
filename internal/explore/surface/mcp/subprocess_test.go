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

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// This file drives the REAL `exploremesh mcp` binary over real stdio with the official SDK client, and
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
// It is the twin of reviewmesh/internal/surface/mcp/subprocess_test.go.

var (
	buildOnce sync.Once
	testBin   string
	buildErr  error
)

// binary builds the exploremesh binary once for this package.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "exploremesh-mcp-bin")
		if err != nil {
			buildErr = err
			return
		}
		testBin = filepath.Join(dir, "exploremesh")
		if runtime.GOOS == "windows" {
			testBin += ".exe"
		}
		build := exec.Command("go", "build", "-o", testBin, "github.com/Tim-Butterfield/aimesh/cmd/aimesh")
		if out, berr := build.CombinedOutput(); berr != nil {
			buildErr = &buildFailure{err: berr, out: string(out)}
		}
	})
	if buildErr != nil {
		t.Fatalf("build exploremesh: %v", buildErr)
	}
	return testBin
}

type buildFailure struct {
	err error
	out string
}

func (b *buildFailure) Error() string { return b.err.Error() + "\n" + b.out }

// hermeticEnv isolates the child from the developer's real configuration and gives it an all-`fake`
// profile: two in-process fake explorers + a fake collator. No real CLI is ever spawned.
func hermeticEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, localstate.HomeDirName, roster.ComponentName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const profiles = `schemaVersion: 1
defaultProfile: default
profiles:
  default:
    explorers:
      - { adapter: fake, model: fake-a, effort: high }
      - { adapter: fake, model: fake-b, effort: medium }
    collator: { adapter: fake, model: fake-c, effort: high }
`
	if err := os.WriteFile(filepath.Join(dir, "profiles.yaml"), []byte(profiles), 0o644); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(),
		"AIMESH_HOME="+home,
		"EXPLOREMESH_ARTIFACT_DIR="+t.TempDir(),
		"AIMESH_INTERNAL_FAKE=1", // unlock the hidden internal fake harness for the child
	)
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
	// The child runs in a THROWAWAY cwd: config resolution is root-anchored, so launching inside the
	// checkout would bind it to this repo's project-scope config instead of the hermetic home.
	cmd := exec.Command(bin, "explore", "mcp", "--wait-seconds", "60")
	cmd.Dir = t.TempDir()
	cmd.Env = hermeticEnv(t)
	// `stderr` must be the concurrency-safe buffer: os/exec copies the child's stderr on its own
	// goroutine, and this test reads the accumulated text (in the failure messages below) while the
	// child is still running. A plain strings.Builder here is a real data race — latent on the happy
	// path only because a healthy child writes nothing to stderr, and therefore live exactly when a
	// diagnostic is being formatted.
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to `exploremesh mcp`: %v\nstderr:\n%s", err, stderr.String())
	}
	defer func() { _ = session.Close() }()

	if err := session.SetLoggingLevel(ctx, &sdk.SetLoggingLevelParams{Level: "debug"}); err != nil {
		t.Fatalf("logging/setLevel: %v", err)
	}
	list, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(list.Tools) != 6 {
		t.Errorf("tools = %d, want 6", len(list.Tools))
	}

	params := &sdk.CallToolParams{Name: "explore", Arguments: map[string]any{
		"purpose": "choose a datastore for the ingest service", "criteria": []string{"cost", "latency"}, "mode": "map",
	}}
	params.Meta = sdk.Meta{"progressToken": "sub-1"}
	res, err := session.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("tools/call explore: %v\nstderr:\n%s", err, stderr.String())
	}
	if res.IsError {
		t.Fatalf("explore halted: %s", textOf(res))
	}
	out := structured(t, res)
	if out["state"] != "complete" {
		t.Fatalf("state = %v, want complete (payload %v)", out["state"], out)
	}
	// A real run through the real binary still carries the whole governed payload.
	if out["governance"] == nil || out["identityCaveats"] == nil || out["panel"] == nil {
		t.Errorf("the shipped binary dropped part of the governed payload: %v", out)
	}
	// And it captured the run to disk by default (the audit record the run id names).
	if captured, _ := out["captured"].(bool); !captured {
		t.Errorf("`exploremesh mcp` must capture a run record by default: %v", out)
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
			t.Fatalf("stdout line %d of `exploremesh mcp` is not JSON: %q", i, line)
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
}
