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

// launchTrusting starts the child agent with `ws` as its TRUSTED root — the operator step a
// real ACP host performs (`aimesh review acp --root <project>`). Over ACP a workspace path in a
// request is not its own consent, so a driver that will send one must authorize it at launch;
// an unlisted path is refused before any run (see the acp package's scope tests).
func launchTrusting(t *testing.T, framing, ws string, env []string) (*Client, func() error, error) {
	t.Helper()
	return LaunchWith(testBin, Options{Framing: framing, Roots: []string{ws}, Env: env})
}

func workspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// fakeProfileHome creates an isolated AIMESH_HOME whose config points defaultProfile at the
// shipped-but-hidden fake profile — the deterministic, fully-local profile the child `reviewmesh acp`
// runs. (The shipped `default` profile now ships UNCONFIGURED, so a bare home would not resolve.)
func fakeProfileHome(t *testing.T) string {
	t.Helper()
	return fakeProfileHomeWith(t, "")
}

// fakeProfileHomeWith is fakeProfileHome with extra top-level config appended — for tests that
// must exercise a NON-seed policy (e.g. widening `surfaces.defaultModeBySurface.acp`). Everything
// it does not set still comes from the shipped seed.
func fakeProfileHomeWith(t *testing.T, extraYAML string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".aimesh", "review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("schemaVersion: 1\ndefaultProfile: fake-smoke\n"+extraYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func env(t *testing.T) []string {
	t.Helper()
	return envIn(t, fakeProfileHome(t))
}

// envIn is env with an explicit AIMESH_HOME (so a test can supply its own config layer).
func envIn(t *testing.T, home string) []string {
	t.Helper()
	return append(os.Environ(),
		"REVIEWMESH_ARTIFACT_DIR="+t.TempDir(),
		"REVIEWMESH_FAKE_SCENARIO=valid", // deterministic 1-finding report
		"AIMESH_HOME="+home,              // never touch the real home (durable session store)
		"AIMESH_INTERNAL_FAKE=1",         // unlock the hidden internal fake harness for the child
	)
}

// promptRM extracts the reviewmesh result block from an ACP v1 PromptResponse result
// (`_meta.reviewmesh`), where status/mode/findings/runDir live (not at the ACP top level).
func promptRM(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	meta, _ := res["_meta"].(map[string]any)
	rm, ok := meta["reviewmesh"].(map[string]any)
	if !ok {
		t.Fatalf("PromptResponse missing _meta.reviewmesh: %+v", res)
	}
	return rm
}

// updEventType reads the reviewmesh eventType from an ACP v1 SessionUpdate
// (update._meta.reviewmesh.eventType).
func updEventType(upd map[string]any) string {
	meta, _ := upd["_meta"].(map[string]any)
	rm, _ := meta["reviewmesh"].(map[string]any)
	s, _ := rm["eventType"].(string)
	return s
}

// The minimal required ACP flow over a real subprocess, for both framings.
func TestACP_ReportFlow(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			ws := workspace(t)
			c, stop, err := launchTrusting(t, framing, ws, env(t))
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
			// ACP v1 initialize result: integer protocolVersion 1 + agentCapabilities; no
			// reviewmesh-internal capabilities/serverInfo top-level fields.
			if pv, ok := init.Result["protocolVersion"].(float64); !ok || pv != 1 {
				t.Errorf("initialize protocolVersion must be numeric 1, got %#v", init.Result["protocolVersion"])
			}
			if _, ok := init.Result["agentCapabilities"].(map[string]any); !ok {
				t.Errorf("initialize must include agentCapabilities: %+v", init.Result)
			}
			if init.Result["capabilities"] != nil || init.Result["serverInfo"] != nil {
				t.Errorf("initialize must not carry top-level capabilities/serverInfo (ACP v1): %+v", init.Result)
			}

			rev, err := c.Call("review", map[string]any{"workspace": ws, "mode": "report"})
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
			// audit artifacts written under the run dir
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

// The ACP v1 session flow over a real subprocess: initialize → session/new →
// session/prompt (report-mode, fake adapter) → shutdown — for BOTH framings.
func TestACP_SessionFlow(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			ws := workspace(t)
			c, stop, err := launchTrusting(t, framing, ws, env(t))
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
			pr, err := c.Call("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "report"})
			if err != nil {
				t.Fatalf("session/prompt: %v", err)
			}
			if pr.Error != nil {
				t.Fatalf("session/prompt error: %+v", pr.Error)
			}
			// ACP v1 PromptResponse: top-level stopReason; reviewmesh details under _meta.reviewmesh.
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

// session/prompt streams ordered session/update progress notifications (JSON-RPC
// notifications, no id) tagged with the session id, then the terminal response — for both
// framings. initialize advertises session.update.
func TestACP_SessionPromptProgress(t *testing.T) {
	for _, framing := range []string{acp.FramingNewline, acp.FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			ws := workspace(t)
			c, stop, err := launchTrusting(t, framing, ws, env(t))
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
			// The ACP v1 initialize result no longer advertises a reviewmesh `session` block;
			// session/update progress streaming is verified by the ordered notifications below.

			sn, err := c.Call("session/new", nil)
			if err != nil || sn.Error != nil {
				t.Fatalf("session/new: err=%v resp=%+v", err, sn)
			}
			sid, _ := sn.Result["sessionId"].(string)
			if sid == "" {
				t.Fatalf("session/new returned no sessionId: %+v", sn.Result)
			}

			pr, notes, err := c.CallCollecting("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "report"})
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
				// ACP v1 SessionNotification: top-level params.sessionId + params.update (the
				// SessionUpdate variant). reviewmesh eventType lives under update._meta.reviewmesh.
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
			// CallCollecting returns the terminal response only after every preceding
			// notification, so the response is guaranteed to follow the progress stream.
			if sd, err := c.Call("shutdown", nil); err != nil || sd.Error != nil {
				t.Errorf("shutdown: err=%v resp=%+v", err, sd)
			}
		})
	}
}

// session/load is not implemented (loadSession:false; reconnect uses session/resume) →
// method-not-found, both framings.
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

// session/prompt with host-mediated inline content (no workspace path): the Client
// materializes it to a temp workspace and reviews it end-to-end — both framings.
func TestACP_InlineWorkspace(t *testing.T) {
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
			sn, err := c.Call("session/new", nil)
			if err != nil || sn.Error != nil {
				t.Fatalf("session/new: err=%v resp=%+v", err, sn)
			}
			sid, _ := sn.Result["sessionId"].(string)
			pr, err := c.Call("session/prompt", map[string]any{
				"sessionId": sid, "mode": "report",
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

	// unknown method → method-not-found error
	unknown, err := c.Call("bogus-method", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if unknown.Error == nil {
		t.Error("unknown method should return a JSON-RPC error")
	}

	// review with no workspace → invalid-params error (clean, no crash)
	noWs, err := c.Call("review", map[string]any{"mode": "report"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if noWs.Error == nil {
		t.Error("review without a workspace should return a structured error")
	}
}

// REGRESSION (security): over a REAL `reviewmesh acp` subprocess on the SHIPPED seed config, a
// host that advertises no `fs` capabilities at all — so the connection ceiling stays `apply` —
// and asks for `mode: "apply"` runs in REPORT mode. That is the seed's
// `surfaces.defaultModeBySurface.acp: report` write-authority ceiling; before that entry existed
// the ACP surface fell through to `apply` and a host could reach LIVE workspace writes with no
// config opt-in. The cap must be explicit (a `mode_degraded` warn plus modeDegraded /
// requestedMode in the echo), and the workspace must be left untouched.
func TestACP_SeedPolicyCapsApplyToReport(t *testing.T) {
	ws := workspace(t)
	c, stop, err := launchTrusting(t, acp.FramingNewline, ws, env(t))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("server did not terminate cleanly: %v", err)
		}
	}()

	// No clientCapabilities: nothing narrows the connection, so only config policy can cap.
	if init, err := c.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
		t.Fatalf("initialize: err=%v resp=%+v", err, init)
	}
	sn, err := c.Call("session/new", nil)
	if err != nil || sn.Error != nil {
		t.Fatalf("session/new: err=%v resp=%+v", err, sn)
	}
	sid, _ := sn.Result["sessionId"].(string)
	pr, notes, err := c.CallCollecting("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "apply"})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if pr.Error != nil {
		t.Fatalf("session/prompt error: %+v", pr.Error)
	}
	rm := promptRM(t, pr.Result)
	if rm["mode"] != "report" {
		t.Errorf("effective mode = %v, want report (shipped acp surface ceiling)", rm["mode"])
	}
	if rm["modeDegraded"] != true || rm["requestedMode"] != "apply" {
		t.Errorf("the cap must be reported, not silent: %+v", rm)
	}
	var warned bool
	for _, n := range notes {
		if upd, ok := n.Params["update"].(map[string]any); ok && updEventType(upd) == "mode_degraded" {
			warned = true
		}
	}
	if !warned {
		t.Error("expected a mode_degraded session/update warning")
	}
	b, rerr := os.ReadFile(filepath.Join(ws, "main.go"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(b), "// reviewmesh[") {
		t.Error("a policy-capped ACP run must not write to the live workspace")
	}
}

// canonical resolves a path the way the surface's run-handle check does, so two spellings of one
// directory compare equal.
func canonical(t *testing.T, v any) string {
	t.Helper()
	p, _ := v.(string)
	if c, err := filepath.EvalSymlinks(p); err == nil {
		return c
	}
	return p
}

// The ACP ceiling is config-visible POLICY, not a hard-coded refusal: a user who widens
// `surfaces.defaultModeBySurface.acp` to `apply` gets apply over a write-capable connection —
// the run is not degraded and it does reach the live workspace.
func TestACP_WidenedPolicyHonorsApply(t *testing.T) {
	ws := workspace(t)
	home := fakeProfileHomeWith(t, "surfaces:\n  defaultModeBySurface:\n    acp: apply\n")
	c, stop, err := launchTrusting(t, acp.FramingNewline, ws, envIn(t, home))
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
	// TURN ONE — the report. Its RESPONSE carries the `runDir`, which is the two-phase write rule's
	// handle (D5) AND, now, the address of the decision set turn two applies. The handle is no
	// longer a token the surface merely records: it is resolved against this agent's own artifact
	// directory, so a fabricated one is refused before any work starts.
	rp, err := c.Call("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "report"})
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
	// TURN TWO — the write, keyed on that handle. It applies the set turn one adjudicated.
	pr, err := c.Call("session/prompt", map[string]any{"sessionId": sid, "workspace": ws, "mode": "apply", "fromRun": sourceRun})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if pr.Error != nil {
		t.Fatalf("session/prompt error: %+v", pr.Error)
	}
	rm := promptRM(t, pr.Result)
	if rm["mode"] != "apply" {
		t.Errorf("effective mode = %v, want apply (config widened the acp ceiling)", rm["mode"])
	}
	if rm["modeDegraded"] != nil {
		t.Errorf("a widened policy must not degrade: %+v", rm)
	}
	// TWO runs, TWO handles: the write has its own run directory (its journal and receipt live
	// there) and names the run whose decisions it applied. Across a real process boundary, which is
	// the point of this harness.
	//
	// `sourceRunDir` is the CANONICAL directory — the run the write actually read, symlinks
	// resolved — so it is compared canonically rather than by string. On macOS every temp path is
	// reached through a symlinked `/var`, so a byte comparison here would test the platform.
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
		t.Errorf("an honored apply should have written the remediation to the workspace, got:\n%s", b)
	}
}

// Real CROSS-PROCESS durability: subprocess A (a shared AIMESH_HOME) creates + persists a
// session and exits; a SEPARATE subprocess B advertises sessionCapabilities.resume, resumes
// that session from the durable store, and runs a session/prompt that omits `workspace` using
// the restored cwd — proving resume survives an agent restart, not just in-process state.
func TestACP_ResumeAcrossSubprocesses(t *testing.T) {
	home := fakeProfileHome(t)
	ws := workspace(t)
	envWith := func() []string {
		return append(os.Environ(),
			"REVIEWMESH_ARTIFACT_DIR="+t.TempDir(),
			"REVIEWMESH_FAKE_SCENARIO=valid",
			"AIMESH_HOME="+home,      // shared durable session store across both subprocesses
			"AIMESH_INTERNAL_FAKE=1", // unlock the hidden internal fake harness for the children
		)
	}

	// Instance A: create + persist a session, then terminate.
	cA, stopA, err := launchTrusting(t, acp.FramingNewline, ws, envWith())
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

	// Instance B (a fresh process, same AIMESH_HOME): resume + prompt without workspace.
	cB, stopB, err := launchTrusting(t, acp.FramingNewline, ws, envWith())
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
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "review"}},
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
