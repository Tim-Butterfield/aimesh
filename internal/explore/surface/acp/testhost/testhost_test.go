package testhost

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
)

// testBin is the exploremesh binary built once for the whole package.
var testBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "exploremesh-acp-testhost")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	testBin = filepath.Join(dir, "exploremesh")
	if runtime.GOOS == "windows" {
		testBin += ".exe" // `go build -o` emits/executes exploremesh.exe on Windows
	}
	build := exec.Command("go", "build", "-o", testBin, "github.com/Tim-Butterfield/aimesh/cmd/aimesh")
	if out, berr := build.CombinedOutput(); berr != nil {
		fmt.Fprintf(os.Stderr, "build exploremesh: %v\n%s", berr, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// env isolates the child agent from the developer's machine: AIMESH_HOME (the durable ACP session store)
// points at a throwaway dir, the internal fake adapter is unlocked, and the agent is launched with it
// through AIMESH_ADAPTERS — the environment form of `--adapter fake`. Every prompt composes a panel of fake
// explorers and a fake collator, fully in-process. No real CLI is ever spawned.
func env(t *testing.T) []string {
	t.Helper()
	return envWithHome(t.TempDir())
}

func envWithHome(home string) []string {
	return append(os.Environ(),
		"AIMESH_HOME="+home,
		"AIMESH_INTERNAL_FAKE=1", // unlock the hidden internal fake harness for the child
		"AIMESH_ADAPTERS=fake",   // launch the child with the fake adapter
	)
}

// fakePanel is the panel every prompt in this file composes: two fake explorers and a fake collator.
func fakePanel() map[string]any {
	return map[string]any{
		"explorers": []any{
			map[string]any{"adapter": "fake", "model": "fake-a", "effort": "high"},
			map[string]any{"adapter": "fake", "model": "fake-b", "effort": "medium"},
		},
		"collator": map[string]any{"adapter": "fake", "model": "fake-c", "effort": "high"},
	}
}

// launch starts the agent in a THROWAWAY working directory, so nothing about this repository can reach it.
func launch(t *testing.T, framing string, environ []string) (*Client, func() error) {
	t.Helper()
	c, stop, err := LaunchIn(t.TempDir(), testBin, framing, environ, nil)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	return c, stop
}

// exploreTask is the minimal VALID `session/prompt` params: the ACP text prompt supplies the task
// PURPOSE, `_meta.exploremesh.criteria` the load-bearing criteria (exploremesh's criteria-via-_meta
// convention), and `_meta.exploremesh.panel` the required panel.
func exploreTask(sid string) map[string]any {
	return map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "compare Postgres and SQLite"}},
		"_meta": map[string]any{"exploremesh": map[string]any{"panel": fakePanel(),
			"criteria": []any{"cost", "latency"},
		}},
	}
}

// promptEM extracts the exploremesh result block from an ACP v1 PromptResponse (`_meta.exploremesh`),
// where status/mode/panel live (not at the ACP top level).
func promptEM(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	meta, _ := res["_meta"].(map[string]any)
	em, ok := meta["exploremesh"].(map[string]any)
	if !ok {
		t.Fatalf("PromptResponse missing _meta.exploremesh: %+v", res)
	}
	return em
}

// updEventType reads the exploremesh eventType from an ACP v1 SessionUpdate
// (update._meta.exploremesh.eventType).
func updEventType(upd map[string]any) string {
	meta, _ := upd["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	s, _ := em["eventType"].(string)
	return s
}

// The ACP v1 session flow over a real subprocess: initialize → session/new → session/prompt (fake
// panel) → shutdown — for BOTH framings.
func TestACP_SessionFlow(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			c, stop := launch(t, framing, env(t))
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
			// ACP v1 initialize result: integer protocolVersion 1 + agentCapabilities + agentInfo; no
			// exploremesh-internal capabilities/serverInfo top-level fields.
			if pv, ok := init.Result["protocolVersion"].(float64); !ok || pv != 1 {
				t.Errorf("initialize protocolVersion must be numeric 1, got %#v", init.Result["protocolVersion"])
			}
			if _, ok := init.Result["agentCapabilities"].(map[string]any); !ok {
				t.Errorf("initialize must include agentCapabilities: %+v", init.Result)
			}
			if ai, _ := init.Result["agentInfo"].(map[string]any); ai == nil || ai["name"] != "exploremesh" {
				t.Errorf("initialize agentInfo must name the agent: %+v", init.Result["agentInfo"])
			}
			if init.Result["capabilities"] != nil || init.Result["serverInfo"] != nil {
				t.Errorf("initialize must not carry top-level capabilities/serverInfo (ACP v1): %+v", init.Result)
			}

			sn, err := c.Call("session/new", nil)
			if err != nil || sn.Error != nil {
				t.Fatalf("session/new: err=%v resp=%+v", err, sn)
			}
			sid, _ := sn.Result["sessionId"].(string)
			if sid == "" {
				t.Fatalf("session/new returned no sessionId: %+v", sn.Result)
			}

			pr, err := c.Call("session/prompt", exploreTask(sid))
			if err != nil {
				t.Fatalf("session/prompt: %v", err)
			}
			if pr.Error != nil {
				t.Fatalf("session/prompt error: %+v", pr.Error)
			}
			// ACP v1 PromptResponse: top-level stopReason; exploremesh details under _meta.exploremesh.
			if pr.Result["stopReason"] != "end_turn" {
				t.Errorf("session/prompt stopReason = %v, want end_turn", pr.Result["stopReason"])
			}
			em := promptEM(t, pr.Result)
			if em["status"] != "complete" {
				t.Errorf("session/prompt status = %v, want complete", em["status"])
			}
			// The prompt named no mode, so the run used the default (map), and the echoed panel is the one
			// the prompt composed — the whole point of the echo is that a driver sees what actually ran.
			if em["mode"] != "map" {
				t.Errorf("session/prompt mode = %v, want map (the default)", em["mode"])
			}
			if em["purpose"] != "compare Postgres and SQLite" {
				t.Errorf("session/prompt purpose = %v, want the ACP prompt text", em["purpose"])
			}
			if n, _ := em["panelSize"].(float64); n < 2 {
				t.Errorf("panelSize = %v, want the 2-explorer demo panel", em["panelSize"])
			}
			if seats, _ := em["explorers"].([]any); len(seats) != 2 {
				t.Errorf("echoed explorers = %v, want the 2 composed seats", em["explorers"])
			}

			if sd, err := c.Call("shutdown", nil); err != nil || sd.Error != nil {
				t.Errorf("shutdown: err=%v resp=%+v", err, sd)
			}
		})
	}
}

// session/prompt streams ordered session/update progress notifications (JSON-RPC notifications, no id)
// tagged with the session id, then the terminal response — for both framings.
func TestACP_SessionPromptProgress(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			c, stop := launch(t, framing, env(t))
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

			pr, notes, err := c.CallCollecting("session/prompt", exploreTask(sid))
			if err != nil {
				t.Fatalf("session/prompt: %v", err)
			}
			if pr.Error != nil {
				t.Fatalf("session/prompt error: %+v", pr.Error)
			}
			if pr.Result["stopReason"] != "end_turn" {
				t.Errorf("session/prompt stopReason = %v, want end_turn", pr.Result["stopReason"])
			}
			if len(notes) == 0 {
				t.Fatal("expected session/update progress notifications, got none")
			}
			for _, n := range notes {
				if n.Method != "session/update" {
					t.Errorf("unexpected notification method %q", n.Method)
					continue
				}
				// ACP v1 SessionNotification: top-level params.sessionId + params.update (the SessionUpdate
				// variant). The exploremesh audit-event fields live under update._meta.exploremesh.
				if n.Params["sessionId"] != sid {
					t.Errorf("notification sessionId = %v, want %s", n.Params["sessionId"], sid)
				}
				upd, ok := n.Params["update"].(map[string]any)
				if !ok || upd["sessionUpdate"] != "agent_message_chunk" {
					t.Errorf("session/update must carry an ACP v1 update (agent_message_chunk), got %v", n.Params["update"])
					continue
				}
				if updEventType(upd) == "" {
					t.Errorf("session/update must preserve the structured exploremesh eventType: %v", upd)
				}
			}
			// CallCollecting returns the terminal response only after every preceding notification, so the
			// response is guaranteed to follow the progress stream.
			if sd, err := c.Call("shutdown", nil); err != nil || sd.Error != nil {
				t.Errorf("shutdown: err=%v resp=%+v", err, sd)
			}
		})
	}
}

// criteria-via-_meta is FAIL-CLOSED over a real subprocess: a prompt carrying no
// `_meta.exploremesh.criteria` is invalid-params rather than a run against invented criteria.
func TestACP_MissingCriteriaIsInvalidParams(t *testing.T) {
	c, stop := launch(t, acp.FramingNewline, env(t))
	defer stop()

	if init, err := c.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
		t.Fatalf("initialize: err=%v resp=%+v", err, init)
	}
	sn, err := c.Call("session/new", nil)
	if err != nil || sn.Error != nil {
		t.Fatalf("session/new: err=%v resp=%+v", err, sn)
	}
	sid, _ := sn.Result["sessionId"].(string)

	pr, err := c.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "explore something"}},
	})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if pr.Error == nil {
		t.Fatalf("a prompt with no criteria must be a structured error, got result %+v", pr.Result)
	}
}

// session/load is not implemented (loadSession:false; reconnect uses session/resume) →
// method-not-found, both framings.
func TestACP_SessionLoadMethodNotFound(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			c, stop := launch(t, framing, env(t))
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

func TestACP_InvalidRequests(t *testing.T) {
	c, stop := launch(t, acp.FramingNewline, env(t))
	defer stop()

	// unknown method → method-not-found error
	unknown, err := c.Call("bogus-method", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if unknown.Error == nil {
		t.Error("unknown method should return a JSON-RPC error")
	}

	// session/prompt for a session that was never created → invalid-params (clean, no crash)
	noSession, err := c.Call("session/prompt", exploreTask("s-never-created"))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if noSession.Error == nil {
		t.Error("session/prompt with an unknown sessionId should return a structured error")
	}
}

// Real CROSS-PROCESS durability: subprocess A (a shared AIMESH_HOME) creates + persists a session
// and exits; a SEPARATE subprocess B advertises sessionCapabilities.resume, resumes that session from the
// durable store, and runs a session/prompt on it — proving resume survives an agent restart, not just
// in-process state.
func TestACP_ResumeAcrossSubprocesses(t *testing.T) {
	// One home, shared by both subprocesses: it carries the durable session store the resume depends on
	// finding again after the restart.
	home := t.TempDir()
	envWith := func() []string { return envWithHome(home) }

	// Instance A: create + persist a session, then terminate.
	cA, stopA := launch(t, acp.FramingNewline, envWith())
	if init, err := cA.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
		t.Fatalf("initialize A: err=%v resp=%+v", err, init)
	}
	sn, err := cA.Call("session/new", map[string]any{"cwd": t.TempDir()})
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

	// Instance B (a fresh process, same AIMESH_HOME): resume + prompt.
	cB, stopB := launch(t, acp.FramingNewline, envWith())
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
	pr, err := cB.Call("session/prompt", exploreTask(sid))
	if err != nil || pr.Error != nil {
		t.Fatalf("resumed session/prompt: err=%v resp=%+v", err, pr)
	}
	if pr.Result["stopReason"] != "end_turn" {
		t.Errorf("resumed prompt stopReason = %v, want end_turn", pr.Result["stopReason"])
	}
	if em := promptEM(t, pr.Result); em["status"] != "complete" {
		t.Errorf("resumed prompt _meta.exploremesh status = %v, want complete", em["status"])
	}
}
