package testhost

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
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

// env isolates the child agent from the developer's real config: AIMESH_HOME (the app-private
// profiles + the durable ACP session store) and AIMESH_HOME (the shared adapters.yaml) both point at
// throwaway dirs. The shipped default profile is deliberately UNCONFIGURED (nothing runs on a fresh
// install), so the hermetic home gets an explicit all-`fake` test profile written into it — 2 `fake`
// explorers + a `fake` collator, fully in-process. No real CLI is ever spawned.
func env(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	writeFakeProfiles(t, home)
	return append(os.Environ(),
		"AIMESH_HOME="+home,
		"AIMESH_INTERNAL_FAKE=1", // unlock the hidden internal fake harness for the child
	)
}

// writeFakeProfiles writes the deterministic all-fake test profile set into explore's component
// directory under home (the user-scope profiles.yaml the child resolves via AIMESH_HOME).
func writeFakeProfiles(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, localstate.HomeDirName, roster.ComponentName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const y = `schemaVersion: 1
defaultProfile: default
profiles:
  default:
    explorers:
      - { adapter: fake, model: fake-a, effort: high }
      - { adapter: fake, model: fake-b, effort: medium }
    collator: { adapter: fake, model: fake-c, effort: high }
`
	if err := os.WriteFile(filepath.Join(dir, "profiles.yaml"), []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
}

// launch starts the agent in a THROWAWAY working directory. The child's project-scope config resolution
// is root-anchored (it walks up from its cwd), so launching in the repo would bind it to the aimesh
// checkout's `.aimesh` instead of the hermetic home above.
func launch(t *testing.T, framing string, environ []string) (*Client, func() error) {
	t.Helper()
	c, stop, err := LaunchIn(t.TempDir(), testBin, framing, environ, nil)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	return c, stop
}

// exploreTask is the minimal VALID `session/prompt` params: the ACP text prompt supplies the task
// PURPOSE and `_meta.exploremesh.criteria` the load-bearing criteria (exploremesh's criteria-via-_meta
// convention). Anything else about the run comes from the agent's bound config, never from the wire.
func exploreTask(sid string) map[string]any {
	return map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "compare Postgres and SQLite"}},
		"_meta": map[string]any{"exploremesh": map[string]any{
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
			// The prompt named no mode, so the run used the default (map), and the echoed panel is the
			// bound default profile's — the whole point of the echo is that a driver sees what actually ran.
			if em["mode"] != "map" {
				t.Errorf("session/prompt mode = %v, want map (the default)", em["mode"])
			}
			if em["purpose"] != "compare Postgres and SQLite" {
				t.Errorf("session/prompt purpose = %v, want the ACP prompt text", em["purpose"])
			}
			if n, _ := em["panelSize"].(float64); n < 2 {
				t.Errorf("panelSize = %v, want the 2-explorer demo panel", em["panelSize"])
			}
			if sel, _ := em["explorersSelected"].(float64); sel != 2 {
				t.Errorf("explorersSelected = %v, want 2", em["explorersSelected"])
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
	// One home, shared by both subprocesses: it carries the panel the agent binds to AND the durable
	// session store the resume depends on finding again after the restart.
	home := t.TempDir()
	writeFakeProfiles(t, home) // the shipped default is unconfigured; the agent needs a panel to bind
	envWith := func() []string {
		return append(os.Environ(),
			"AIMESH_HOME="+home,
			"AIMESH_INTERNAL_FAKE=1", // unlock the hidden internal fake harness for the children
		)
	}

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
