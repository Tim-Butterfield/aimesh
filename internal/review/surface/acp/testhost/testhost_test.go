package testhost

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
)

// testBin is the reviewmesh binary built once for the whole package.
var testBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "reviewmesh-acp-testhost")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	testBin = filepath.Join(dir, "reviewmesh")
	if runtime.GOOS == "windows" {
		testBin += ".exe" // `go build -o` emits/executes reviewmesh.exe on Windows
	}
	build := exec.Command("go", "build", "-o", testBin, "github.com/Tim-Butterfield/aimesh/cmd/aimesh")
	if out, berr := build.CombinedOutput(); berr != nil {
		fmt.Fprintf(os.Stderr, "build reviewmesh: %v\n%s", berr, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// launchAgent starts the child agent as a host configuration would, with only the internal fake
// adapter. allowWrites adds --allow-writes.
func launchAgent(t *testing.T, framing string, env []string, allowWrites bool) (*Client, func() error, error) {
	t.Helper()
	return LaunchWith(testBin, Options{Framing: framing, Adapters: []string{"fake"}, AllowWrites: allowWrites, Env: env})
}

func workspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// env isolates the child: a fresh AIMESH_HOME, a temp artifact directory, a deterministic fake
// scenario and the internal fake enabled.
func env(t *testing.T) []string {
	t.Helper()
	return envIn(t, t.TempDir())
}

// envIn is env with an explicit AIMESH_HOME, so two child processes can share a session store.
func envIn(t *testing.T, home string) []string {
	t.Helper()
	return append(os.Environ(),
		"REVIEWMESH_ARTIFACT_DIR="+t.TempDir(),
		"REVIEWMESH_FAKE_SCENARIO=valid", // deterministic 1-finding report
		"AIMESH_HOME="+home,              // never touch the real home (durable session store)
		"AIMESH_INTERNAL_FAKE=1",         // unlock the internal fake adapter for the child
	)
}

// panelMeta returns the _meta a review turn carries, with every seat on the fake adapter.
func panelMeta() map[string]any {
	seat := map[string]any{"adapter": "fake", "model": "fake-model"}
	return map[string]any{"reviewmesh": map[string]any{
		"panel": map[string]any{"reviewers": []any{seat}, "author_remediator": seat},
	}}
}

// promptRM returns a PromptResponse result's _meta.reviewmesh block.
func promptRM(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	meta, _ := res["_meta"].(map[string]any)
	rm, ok := meta["reviewmesh"].(map[string]any)
	if !ok {
		t.Fatalf("PromptResponse missing _meta.reviewmesh: %+v", res)
	}
	return rm
}

// updEventType returns update._meta.reviewmesh.eventType from a SessionUpdate.
func updEventType(upd map[string]any) string {
	meta, _ := upd["_meta"].(map[string]any)
	rm, _ := meta["reviewmesh"].(map[string]any)
	s, _ := rm["eventType"].(string)
	return s
}

// TestACP_ReportFlow runs the minimal ACP flow over a subprocess, for both framings, on a fresh home.
func TestACP_ReportFlow(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			ws := workspace(t)
			c, stop, err := launchAgent(t, framing, env(t), false)
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			defer func() {
				if err := stop(); err != nil {
					t.Errorf("server did not terminate cleanly: %v", err)
				}
			}()

			init, err := c.Call("initialize", map[string]any{"protocolVersion": 1})
			if err != nil {
				t.Fatalf("initialize: %v", err)
			}
			if init.Error != nil {
				t.Fatalf("initialize error: %+v", init.Error)
			}
			// ACP v1 initialize result: integer protocolVersion 1 and agentCapabilities.
			if pv, ok := init.Result["protocolVersion"].(float64); !ok || pv != 1 {
				t.Errorf("initialize protocolVersion must be numeric 1, got %#v", init.Result["protocolVersion"])
			}
			if _, ok := init.Result["agentCapabilities"].(map[string]any); !ok {
				t.Errorf("initialize must include agentCapabilities: %+v", init.Result)
			}
			if init.Result["capabilities"] != nil || init.Result["serverInfo"] != nil {
				t.Errorf("initialize must not carry top-level capabilities/serverInfo (ACP v1): %+v", init.Result)
			}
			if rm := promptRM(t, init.Result); rm["writes"] != "agent" || rm["diffAvailable"] != true {
				t.Errorf("initialize must disclose writes=agent diffAvailable=true without --allow-writes: %+v", rm)
			}

			rev, err := c.Call("review", map[string]any{"workspace": ws, "mode": "report", "_meta": panelMeta()})
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			if rev.Error != nil {
				t.Fatalf("review error: %+v", rev.Error)
			}
			if rev.Result["status"] != "stable" {
				t.Errorf("review status = %v, want stable", rev.Result["status"])
			}
			if rev.Result["mode"] != "report" {
				t.Errorf("review mode = %v, want report", rev.Result["mode"])
			}
			runDir, _ := rev.Result["runDir"].(string)
			if runDir == "" {
				t.Fatal("review result missing runDir")
			}
			if _, err := os.Stat(filepath.Join(runDir, "run-state.json")); err != nil {
				t.Errorf("expected audit artifact run-state.json: %v", err)
			}

			if sd, err := c.Call("shutdown", nil); err != nil || sd.Error != nil {
				t.Errorf("shutdown: err=%v resp=%+v", err, sd)
			}
		})
	}
}

// TestACP_SessionFlow runs initialize, session/new, a report session/prompt and shutdown over a
// subprocess, for both framings.
func TestACP_SessionFlow(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			ws := workspace(t)
			c, stop, err := launchAgent(t, framing, env(t), false)
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			defer func() {
				if err := stop(); err != nil {
					t.Errorf("server did not terminate cleanly: %v", err)
				}
			}()

			if init, err := c.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
				t.Fatalf("initialize: err=%v resp=%+v", err, init)
			}
			sn, err := c.Call("session/new", nil)
			if err != nil || sn.Error != nil {
				t.Fatalf("session/new: err=%v resp=%+v", err, sn)
			}
			sid, _ := sn.Result["sessionId"].(string)
			if sid == "" {
				t.Fatalf("session/new returned no sessionId: %+v", sn.Result)
			}
			pr, err := c.Call("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "report", "_meta": panelMeta()})
			if err != nil {
				t.Fatalf("session/prompt: %v", err)
			}
			if pr.Error != nil {
				t.Fatalf("session/prompt error: %+v", pr.Error)
			}
			if pr.Result["stopReason"] != "end_turn" {
				t.Errorf("session/prompt stopReason = %v, want end_turn", pr.Result["stopReason"])
			}
			if rm := promptRM(t, pr.Result); rm["status"] != "stable" || rm["mode"] != "report" {
				t.Errorf("session/prompt _meta.reviewmesh = %+v", rm)
			}
			if sd, err := c.Call("shutdown", nil); err != nil || sd.Error != nil {
				t.Errorf("shutdown: err=%v resp=%+v", err, sd)
			}
		})
	}
}

// session/prompt streams ordered session/update notifications tagged with the session id before the
// final response, for both framings.
func TestACP_SessionPromptProgress(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			ws := workspace(t)
			c, stop, err := launchAgent(t, framing, env(t), false)
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			defer func() {
				if err := stop(); err != nil {
					t.Errorf("server did not terminate cleanly: %v", err)
				}
			}()

			if init, err := c.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
				t.Fatalf("initialize: err=%v resp=%+v", err, init)
			}
			sn, err := c.Call("session/new", nil)
			if err != nil || sn.Error != nil {
				t.Fatalf("session/new: err=%v resp=%+v", err, sn)
			}
			sid, _ := sn.Result["sessionId"].(string)
			if sid == "" {
				t.Fatalf("session/new returned no sessionId: %+v", sn.Result)
			}

			pr, notes, err := c.CallCollecting("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "report", "_meta": panelMeta()})
			if err != nil {
				t.Fatalf("session/prompt: %v", err)
			}
			if pr.Error != nil {
				t.Fatalf("session/prompt error: %+v", pr.Error)
			}
			if pr.Result["stopReason"] != "end_turn" {
				t.Errorf("session/prompt stopReason = %v, want end_turn", pr.Result["stopReason"])
			}
			if rm := promptRM(t, pr.Result); rm["status"] != "stable" {
				t.Errorf("session/prompt _meta.reviewmesh status = %v, want stable", rm["status"])
			}
			if len(notes) == 0 {
				t.Fatal("expected session/update progress notifications, got none")
			}
			startIdx, doneIdx := -1, -1
			for i, n := range notes {
				if n.Method != "session/update" {
					t.Errorf("unexpected notification method %q", n.Method)
					continue
				}
				if n.Params["sessionId"] != sid {
					t.Errorf("notification sessionId = %v, want %s", n.Params["sessionId"], sid)
				}
				upd, ok := n.Params["update"].(map[string]any)
				if !ok || upd["sessionUpdate"] != "agent_message_chunk" {
					t.Errorf("session/update must carry an ACP v1 update (agent_message_chunk), got %v", n.Params["update"])
					continue
				}
				switch updEventType(upd) {
				case "run_started":
					if startIdx < 0 {
						startIdx = i
					}
				case "run_completed":
					doneIdx = i
				}
			}
			if startIdx < 0 || doneIdx < 0 {
				t.Fatalf("expected run_started + run_completed progress events; got %d notifications", len(notes))
			}
			if startIdx >= doneIdx {
				t.Errorf("run_started (idx %d) must precede run_completed (idx %d)", startIdx, doneIdx)
			}
			if sd, err := c.Call("shutdown", nil); err != nil || sd.Error != nil {
				t.Errorf("shutdown: err=%v resp=%+v", err, sd)
			}
		})
	}
}

// session/load returns method-not-found, for both framings.
func TestACP_SessionLoadMethodNotFound(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			c, stop, err := Launch(testBin, framing, env(t), nil)
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			defer func() {
				if err := stop(); err != nil {
					t.Errorf("server did not terminate cleanly: %v", err)
				}
			}()
			if init, err := c.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
				t.Fatalf("initialize: err=%v resp=%+v", err, init)
			}
			ld, err := c.Call("session/load", map[string]any{"sessionId": "s-0001"})
			if err != nil {
				t.Fatalf("session/load: %v", err)
			}
			if ld.Error == nil {
				t.Errorf("session/load should be method-not-found, got result %+v", ld.Result)
			}
			if sd, err := c.Call("shutdown", nil); err != nil || sd.Error != nil {
				t.Errorf("shutdown: err=%v resp=%+v", err, sd)
			}
		})
	}
}

// session/prompt with inline content and no workspace path is materialized and reviewed end to end,
// for both framings.
func TestACP_InlineWorkspace(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			c, stop, err := launchAgent(t, framing, env(t), false)
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			defer func() {
				if err := stop(); err != nil {
					t.Errorf("server did not terminate cleanly: %v", err)
				}
			}()
			if init, err := c.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
				t.Fatalf("initialize: err=%v resp=%+v", err, init)
			}
			sn, err := c.Call("session/new", nil)
			if err != nil || sn.Error != nil {
				t.Fatalf("session/new: err=%v resp=%+v", err, sn)
			}
			sid, _ := sn.Result["sessionId"].(string)
			pr, err := c.Call("session/prompt", map[string]any{
				"sessionId": sid, "mode": "report", "_meta": panelMeta(),
				"inlineWorkspace": map[string]any{"main.go": "package main\n\nfunc main() {}\n"},
			})
			if err != nil {
				t.Fatalf("session/prompt(inline): %v", err)
			}
			if pr.Error != nil {
				t.Fatalf("session/prompt(inline) error: %+v", pr.Error)
			}
			if pr.Result["stopReason"] != "end_turn" {
				t.Errorf("inline review stopReason = %v, want end_turn", pr.Result["stopReason"])
			}
			rm := promptRM(t, pr.Result)
			if rm["status"] != "stable" {
				t.Errorf("inline review status = %v, want stable", rm["status"])
			}
			if f, _ := rm["findings"].(float64); f < 1 {
				t.Errorf("expected >=1 finding over the materialized workspace, got %v", rm["findings"])
			}
			if sd, err := c.Call("shutdown", nil); err != nil || sd.Error != nil {
				t.Errorf("shutdown: err=%v resp=%+v", err, sd)
			}
		})
	}
}

func TestACP_InvalidRequests(t *testing.T) {
	c, stop, err := Launch(testBin, acp.FramingNewline, env(t), nil)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer stop()

	// An unknown method returns method-not-found.
	unknown, err := c.Call("bogus-method", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if unknown.Error == nil {
		t.Error("unknown method should return a JSON-RPC error")
	}

	// review without a workspace returns invalid params.
	noWs, err := c.Call("review", map[string]any{"mode": "report"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if noWs.Error == nil {
		t.Error("review without a workspace should return a structured error")
	}
}

// A child launched with no adapter starts, and a review turn is refused naming the launch flag.
func TestACP_NoAdapterRefusesAReviewTurnByName(t *testing.T) {
	ws := workspace(t)
	c, stop, err := Launch(testBin, acp.FramingNewline, env(t), nil)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer stop()
	rev, err := c.Call("review", map[string]any{"workspace": ws, "mode": "report", "_meta": panelMeta()})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if rev.Error == nil || !strings.Contains(rev.Error.Message, "--adapter") {
		t.Errorf("a review on an agent with no adapter must be refused naming --adapter, got %+v", rev)
	}
}

// Without --allow-writes, an apply turn over a subprocess is refused, naming the grant and the patch
// turn, and the workspace is unchanged.
func TestACP_ApplyWithoutAllowWritesIsRefused(t *testing.T) {
	ws := workspace(t)
	c, stop, err := launchAgent(t, acp.FramingNewline, env(t), false)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("server did not terminate cleanly: %v", err)
		}
	}()

	if init, err := c.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
		t.Fatalf("initialize: err=%v resp=%+v", err, init)
	}
	sn, err := c.Call("session/new", nil)
	if err != nil || sn.Error != nil {
		t.Fatalf("session/new: err=%v resp=%+v", err, sn)
	}
	sid, _ := sn.Result["sessionId"].(string)
	pr, err := c.Call("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "apply", "_meta": panelMeta()})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if pr.Error == nil {
		t.Fatalf("an apply turn without --allow-writes must be refused, got %+v", pr.Result)
	}
	if !strings.Contains(pr.Error.Message, "--allow-writes") || !strings.Contains(pr.Error.Message, "patch") {
		t.Errorf("the refusal must name --allow-writes and the patch turn: %q", pr.Error.Message)
	}
	b, rerr := os.ReadFile(filepath.Join(ws, "main.go"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(b), "// reviewmesh[") {
		t.Error("an apply capped to patch must not write to the live workspace")
	}
}

// canonical resolves v as the surface's run-handle check does, so two spellings of one directory
// compare equal.
func canonical(t *testing.T, v any) string {
	t.Helper()
	p, _ := v.(string)
	if c, err := filepath.EvalSymlinks(p); err == nil {
		return c
	}
	return p
}

// With --allow-writes, the two-turn write applies over a subprocess: the report turn returns runDir,
// and the apply turn keyed on it writes the set the report adjudicated.
func TestACP_AllowWritesHonorsApply(t *testing.T) {
	ws := workspace(t)
	c, stop, err := launchAgent(t, acp.FramingNewline, env(t), true)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("server did not terminate cleanly: %v", err)
		}
	}()

	init, err := c.Call("initialize", map[string]any{"protocolVersion": 1})
	if err != nil || init.Error != nil {
		t.Fatalf("initialize: err=%v resp=%+v", err, init)
	}
	if rm := promptRM(t, init.Result); rm["writes"] != "aimesh" {
		t.Errorf("initialize must disclose writes=aimesh with --allow-writes: %+v", rm)
	}
	sn, err := c.Call("session/new", nil)
	if err != nil || sn.Error != nil {
		t.Fatalf("session/new: err=%v resp=%+v", err, sn)
	}
	sid, _ := sn.Result["sessionId"].(string)
	rp, err := c.Call("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "report", "_meta": panelMeta()})
	if err != nil {
		t.Fatalf("session/prompt (report): %v", err)
	}
	if rp.Error != nil {
		t.Fatalf("session/prompt (report) error: %+v", rp.Error)
	}
	sourceRun, _ := promptRM(t, rp.Result)["runDir"].(string)
	if sourceRun == "" {
		t.Fatal("the report turn must hand back its runDir")
	}
	pr, err := c.Call("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "apply", "fromRun": sourceRun})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if pr.Error != nil {
		t.Fatalf("session/prompt error: %+v", pr.Error)
	}
	rm := promptRM(t, pr.Result)
	if rm["mode"] != "apply" {
		t.Errorf("effective mode = %v, want apply", rm["mode"])
	}
	if rm["modeDegraded"] != nil {
		t.Errorf("a granted apply must not degrade: %+v", rm)
	}
	// The write has its own run directory and names the run whose decisions it applied; sourceRunDir
	// is canonical, so compare canonically.
	if canonical(t, rm["sourceRunDir"]) != canonical(t, sourceRun) {
		t.Errorf("sourceRunDir = %v, want the report turn's runDir %q", rm["sourceRunDir"], sourceRun)
	}
	if rm["runDir"] == sourceRun {
		t.Error("the write's own runDir must not be the source run's — the receipt would be unfindable")
	}
	b, rerr := os.ReadFile(filepath.Join(ws, "main.go"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(b), "// reviewmesh[") {
		t.Errorf("a granted apply should have written the remediation to the workspace, got:\n%s", b)
	}
}

// Subprocess A persists a session in a shared AIMESH_HOME and exits; subprocess B resumes it and runs
// a session/prompt that omits workspace, using the restored cwd.
func TestACP_ResumeAcrossSubprocesses(t *testing.T) {
	home := t.TempDir()
	ws := workspace(t)

	// Instance A: create and persist a session, then exit.
	cA, stopA, err := launchAgent(t, acp.FramingNewline, envIn(t, home), false)
	if err != nil {
		t.Fatalf("launch A: %v", err)
	}
	if init, err := cA.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
		t.Fatalf("initialize A: err=%v resp=%+v", err, init)
	}
	sn, err := cA.Call("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	if err != nil || sn.Error != nil {
		t.Fatalf("session/new: err=%v resp=%+v", err, sn)
	}
	sid, _ := sn.Result["sessionId"].(string)
	if sid == "" {
		t.Fatalf("session/new returned no sessionId: %+v", sn.Result)
	}
	if err := stopA(); err != nil {
		t.Errorf("server A did not terminate cleanly: %v", err)
	}

	// Instance B, a fresh process with the same AIMESH_HOME: resume, then prompt without workspace.
	cB, stopB, err := launchAgent(t, acp.FramingNewline, envIn(t, home), false)
	if err != nil {
		t.Fatalf("launch B: %v", err)
	}
	defer func() {
		if err := stopB(); err != nil {
			t.Errorf("server B did not terminate cleanly: %v", err)
		}
	}()
	initB, err := cB.Call("initialize", map[string]any{"protocolVersion": 1})
	if err != nil || initB.Error != nil {
		t.Fatalf("initialize B: err=%v resp=%+v", err, initB)
	}
	ac, _ := initB.Result["agentCapabilities"].(map[string]any)
	sc, ok := ac["sessionCapabilities"].(map[string]any)
	if !ok {
		t.Fatalf("instance B must advertise sessionCapabilities (store wired), got %v", ac)
	}
	if _, ok := sc["resume"].(map[string]any); !ok {
		t.Errorf("sessionCapabilities.resume should be an empty object, got %v", sc["resume"])
	}
	rs, err := cB.Call("session/resume", map[string]any{"sessionId": sid})
	if err != nil || rs.Error != nil {
		t.Fatalf("session/resume: err=%v resp=%+v", err, rs)
	}
	if len(rs.Result) != 0 {
		t.Errorf("ResumeSessionResponse should be an empty object, got %+v", rs.Result)
	}
	pr, err := cB.Call("session/prompt", map[string]any{
		"sessionId": sid, "_meta": panelMeta(),
		"prompt": []any{map[string]any{"type": "text", "text": "review"}},
	})
	if err != nil || pr.Error != nil {
		t.Fatalf("resumed session/prompt: err=%v resp=%+v", err, pr)
	}
	if pr.Result["stopReason"] != "end_turn" {
		t.Errorf("resumed prompt stopReason = %v, want end_turn", pr.Result["stopReason"])
	}
	if rm := promptRM(t, pr.Result); rm["status"] != "stable" {
		t.Errorf("resumed prompt _meta.reviewmesh status = %v, want stable", rm["status"])
	}
}
