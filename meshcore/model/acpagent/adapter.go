// Package acpagent is a GENERIC model adapter that drives an ACP-capable CLI as a client
// over stdio, reusing meshcore's ACP framing (meshcore/acp) as the transport. One code path
// supports every CLI that exposes an ACP server — Cursor (`agent acp`), Copilot
// (`copilot --acp --stdio`), Devin (`devin acp`), Gemini (`gemini --acp`), OpenCode
// (`opencode acp`), Qwen (`qwen --acp`) — parameterized only by a launch Recipe, so a new
// ACP CLI is one map entry, not a new package.
//
// Per call it: spawns `<bin> <acp-args>`, `initialize`s, opens a `session/new`, switches the
// session to a read-only mode (plan/ask) so the reviewer cannot edit, optionally selects the
// requested model, sends the prompt, and aggregates the streamed `agent_message_chunk`s into
// the reviewer payload. When the session authoritatively reports its active model
// (`currentModelId`) that becomes a cli_status-tier verified identity — stronger than a
// one-shot CLI's self-report. See docs/adapters.md and docs/acp.md.
package acpagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/acp"
	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// Recipe describes how to launch and drive one ACP-capable CLI as a client.
type Recipe struct {
	Name    string   // adapter name, e.g. "cursor-acp"
	Detect  string   // binary name for PATH lookup, e.g. "agent"
	ACPArgs []string // args that start the CLI's ACP server over stdio, e.g. {"acp"} / {"--acp"} / {"--acp","--stdio"}
}

// Instance is a fully-specified, USER-DEFINED ACP adapter to run: its Name (adapter key), the launch
// binary (Path override, else Detect looked up on PATH), and the Args that start that CLI's ACP server
// over stdio. There is NO fixed catalog — ACP is an open protocol, so instances come from config
// (meshcore/config/adapterlocations `acpAdapters`), and one generic Adapter drives any of them.
type Instance struct {
	Name   string
	Detect string
	Path   string
	Args   []string
}

// Adapter drives one ACP Recipe.
type Adapter struct {
	Recipe  Recipe
	Path    string        // configured binary path override ("" = look up Detect on PATH)
	Timeout time.Duration // per-call timeout (<=0 → default)
	// StartupTimeout bounds the ACP handshake (spawn → initialize → session/new) separately from the
	// per-call Timeout: a handshake that stalls past it is treated as a blocked first-run prompt and
	// the process is killed, instead of hanging for the full Timeout. <=0 → default (30s). Injectable
	// for tests.
	StartupTimeout time.Duration
}

func (a *Adapter) startupTimeout() time.Duration {
	if a.StartupTimeout > 0 {
		return a.StartupTimeout
	}
	return 30 * time.Second
}

// New builds an ACP adapter.
func New(r Recipe, path string, timeout time.Duration) *Adapter {
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	return &Adapter{Recipe: r, Path: path, Timeout: timeout}
}

// Registry builds one runnable ACP adapter per CONFIG-DEFINED instance, keyed by name — the ACP-side
// counterpart to shell.Registry, but driven by user-supplied instances rather than a fixed catalog.
// Both apps build the instance set from meshcore/config/adapterlocations and pass it here. A missing
// Detect falls back to the instance name for PATH lookup (used only when Path is empty).
func Registry(instances map[string]Instance, timeout time.Duration) map[string]model.Adapter {
	reg := make(map[string]model.Adapter, len(instances))
	for name, in := range instances {
		detect := in.Detect
		if detect == "" {
			detect = name
		}
		reg[name] = New(Recipe{Name: name, Detect: detect, ACPArgs: in.Args}, in.Path, timeout)
	}
	return reg
}

// Name returns the adapter's stable registry key (the instance name from config).
func (a *Adapter) Name() string { return a.Recipe.Name }

// Evidence is the ceiling this adapter may be classified on. An ACP session can
// authoritatively report its active model (`session/new` → `currentModelId`), so the ceiling
// is cli_status; a call that receives no model self-downgrades its Result to none.
func (a *Adapter) Evidence() core.IdentityEvidence { return core.EvidenceCLIStatus }

// DetectName is the binary name looked for on PATH — used for repair hints.
func (a *Adapter) DetectName() string { return a.Recipe.Detect }

func (a *Adapter) resolveBinary() (string, error) {
	if a.Path != "" {
		fi, err := os.Stat(a.Path)
		if err != nil {
			return "", fmt.Errorf("configured path %q not found", a.Path)
		}
		if fi.IsDir() {
			return "", fmt.Errorf("configured path %q is a directory, not a binary", a.Path)
		}
		if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
			return "", fmt.Errorf("configured path %q is not executable", a.Path)
		}
		return a.Path, nil
	}
	bin, err := exec.LookPath(a.Recipe.Detect)
	if err != nil {
		return "", fmt.Errorf("binary %q not found on PATH; install it or set adapters.%s.path", a.Recipe.Detect, a.Recipe.Name)
	}
	return bin, nil
}

// Available reports whether the binary is present (no model call, no ACP handshake).
func (a *Adapter) Available() (bool, string) {
	bin, err := a.resolveBinary()
	if err != nil {
		return false, err.Error()
	}
	if a.Path != "" {
		return true, "found at configured path " + bin
	}
	return true, "found on PATH at " + bin
}

// acpModel / acpMode mirror the shapes ACP returns in `session/new`.
type acpModel struct {
	ModelID string `json:"modelId"`
	Name    string `json:"name"`
}
type acpMode struct {
	ID string `json:"id"`
}

type sessionNewResult struct {
	SessionID string `json:"sessionId"`
	Models    struct {
		CurrentModelID  string     `json:"currentModelId"`
		AvailableModels []acpModel `json:"availableModels"`
	} `json:"models"`
	Modes struct {
		CurrentModeID  string    `json:"currentModeId"`
		AvailableModes []acpMode `json:"availableModes"`
	} `json:"modes"`
}

// Invoke drives the full ACP turn and normalizes a Result. A timeout returns ExitCode 124 +
// an error; a protocol/handshake failure (e.g. an account-gated `session/new`) returns a
// non-nil error so the Manager treats it as an adapter fault.
func (a *Adapter) Invoke(ctx context.Context, c model.Call) (res model.Result, err error) {
	bin, err := a.resolveBinary()
	if err != nil {
		return model.Result{ExitCode: 127, Stderr: []byte(err.Error())}, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	// Optional per-invocation diagnostic (opt-in via model.WithDebug), emitted on every return path.
	start := time.Now()
	if w := model.DebugWriter(ctx); w != nil {
		defer func() { emitDebug(w, a.Recipe.Name, bin, a.Recipe.ACPArgs, res, err, time.Since(start)) }()
	}

	// Spawn + handshake (initialize → session/new) under a startup-deadline watchdog. cwd is the
	// contained copy, or a fresh empty temp dir (never the process's real cwd) so an agentic CLI can't
	// wander — the task content is always inline in the prompt.
	cwd, cleanupCwd := resolveCwd(c.CopyRoot)
	defer cleanupCwd()

	s, _, oerr := a.openSession(ctx, bin, cwd, maxCapture)
	if oerr != nil {
		ec := 1
		switch ctx.Err() {
		case context.DeadlineExceeded:
			ec = 124
		case context.Canceled:
			ec = 125
		}
		return model.Result{ExitCode: ec, Stderr: []byte(oerr.Error())}, oerr
	}
	defer s.Close()
	cl, sn, stderr := s.cl, s.sn, s.stderr

	// Identity: the session's active model (authoritative), normalized to its base slug.
	actualModel := stripParams(sn.Models.CurrentModelID)

	// 3. read-only mode (containment is still the backstop).
	if modeID := pickReadOnlyMode(sn.Modes.AvailableModes); modeID != "" {
		_, _ = cl.call("session/set_mode", map[string]any{"sessionId": sn.SessionID, "modeId": modeID}, nil)
	}

	// 4. select the requested model when the session exposes a matching one.
	if want := string(c.ModelArg); want != "" && len(sn.Models.AvailableModels) > 0 {
		if pick := pickModel(sn.Models.AvailableModels, want); pick != "" {
			if _, err := cl.call("session/set_model", map[string]any{"sessionId": sn.SessionID, "modelId": pick}, nil); err == nil {
				actualModel = stripParams(pick)
			}
		}
	}

	// 5. prompt — aggregate the assistant's message chunks into the reviewer payload.
	var sb strings.Builder
	if _, err := cl.call("session/prompt", map[string]any{
		"sessionId": sn.SessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": c.Prompt}},
	}, func(u sessionUpdate) {
		if u.SessionUpdate == "agent_message_chunk" && u.Content.Type == "text" {
			sb.WriteString(u.Content.Text)
		}
	}); err != nil {
		return a.failResult(ctx, stderr, fmt.Errorf("%s: session/prompt: %w", a.Recipe.Name, err))
	}

	full := sb.String()
	res = model.Result{
		Stdout: []byte(full),
		// Agentic ACP CLIs narrate ("Searching the codebase…") and may fence their answer;
		// surface the embedded JSON object as the semantic Payload so a strict consumer gets a
		// clean body while Stdout keeps the full turn for audit. Nil when there is no JSON object
		// (a non-JSON use falls back to Stdout).
		Payload:     extractJSONObject(full),
		Stderr:      stderr.Bytes(),
		ExitCode:    0,
		ActualModel: actualModel,
		Evidence:    core.EvidenceCLIStatus,
	}
	if actualModel == "" { // no authoritative model → unverified, like a shell recipe
		res.Evidence = core.EvidenceNone
	}
	return res, nil
}

// extractJSONObject returns the first balanced top-level JSON object embedded in text — tolerating
// leading narration prose and ``` fences that agentic ACP agents emit around their answer — or nil
// if none is present. String-aware so braces inside string literals don't unbalance the scan.
func extractJSONObject(s string) []byte {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return []byte(s[start : i+1])
			}
		}
	}
	return nil
}

// emitDebug prints a per-invocation ACP diagnostic (launch argv, exit/identity, captured
// stderr + aggregated response) mirroring the shell adapter's --debug block.
func emitDebug(w io.Writer, name, bin string, acpArgs []string, res model.Result, err error, dur time.Duration) {
	fmt.Fprintf(w, "\n── acp adapter debug: %s ──\n", name)
	fmt.Fprintf(w, "  launch:   %s %s\n", bin, strings.Join(acpArgs, " "))
	fmt.Fprintf(w, "  exit:     %d   (%s)\n", res.ExitCode, dur.Round(time.Millisecond))
	if err != nil {
		fmt.Fprintf(w, "  error:    %v\n", err)
	}
	fmt.Fprintf(w, "  identity: model=%q evidence=%s\n", res.ActualModel, res.Evidence)
	fmt.Fprintf(w, "  stderr (%d bytes):\n%s", len(res.Stderr), indentCapped(res.Stderr))
	fmt.Fprintf(w, "  response (%d bytes):\n%s", len(res.Stdout), indentCapped(res.Stdout))
	fmt.Fprintf(w, "──\n")
}

// debugCap bounds how much of each captured stream the debug block prints.
const debugCap = 4 << 10

func indentCapped(b []byte) string {
	s := string(b)
	truncated := false
	if len(s) > debugCap {
		s, truncated = s[:debugCap], true
	}
	if strings.TrimSpace(s) == "" {
		return "    (empty)\n"
	}
	s = "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ") + "\n"
	if truncated {
		s += "    …(truncated)\n"
	}
	return s
}

var _ model.Adapter = (*Adapter)(nil)
var _ model.Prober = (*Adapter)(nil)

// acpSession is a live ACP session over a spawned CLI. Close terminates it.
type acpSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cl     *client
	stderr *cappedBuffer
	sn     sessionNewResult
}

// Close ends the session: a fire-and-forget `shutdown` NOTIFICATION (no id, no read — a blocking
// shutdown CALL could hang on a CLI that doesn't implement it), then EOF stdin, kill the group, reap.
func (s *acpSession) Close() {
	_ = s.cl.notify("shutdown", nil)
	_ = s.stdin.Close()
	_ = killProcessGroup(s.cmd.Process)
	_ = s.cmd.Wait()
}

// initializeParams advertises no filesystem/terminal capability so the agent answers from inline
// content and never reads the workspace.
func initializeParams() map[string]any {
	return map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}},
	}
}

// resolveCwd returns the session working directory and a cleanup func. A contained copy is used
// directly (no temp); otherwise a fresh EMPTY temp dir (never the process's real cwd) so an agentic
// CLI can't wander — removed by the returned func.
func resolveCwd(copyRoot string) (string, func()) {
	if copyRoot != "" {
		return copyRoot, func() {}
	}
	if tmp, err := os.MkdirTemp("", "acpcwd-"); err == nil {
		return tmp, func() { _ = os.RemoveAll(tmp) }
	}
	wd, _ := os.Getwd()
	return wd, func() {}
}

// openSession spawns the CLI and completes the handshake (initialize → session/new) under a
// startup-deadline WATCHDOG. The process stays bound to the OUTER ctx (so a real prompt can run for the
// full Timeout); a separate short deadline arms an AfterFunc that kills the process group if the
// handshake stalls — the signature of a first-run login/folder-trust prompt — disarmed the instant
// session/new returns so it can never affect the subsequent prompt. Returns the live session, the
// classified failure signal (if any), and an error; on error nothing is left running.
func (a *Adapter) openSession(ctx context.Context, bin, cwd string, stderrCap int) (*acpSession, clihint.Signal, error) {
	cmd := exec.CommandContext(ctx, bin, a.Recipe.ACPArgs...)
	cmd.Env = model.HardenedEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	stderr := &cappedBuffer{max: stderrCap}
	cmd.Stderr = stderr
	cmd.SysProcAttr = sysProcAttr()
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	s := &acpSession{cmd: cmd, stdin: stdin, stderr: stderr,
		cl: newClient(acp.NewFramer(acp.FramingNewline, stdout, stdin))}

	startupCtx, cancelStartup := context.WithTimeout(ctx, a.startupTimeout())
	stopWatch := context.AfterFunc(startupCtx, func() { _ = killProcessGroup(cmd.Process) })
	disarm := func() { stopWatch(); cancelStartup() }

	if _, err := s.cl.call("initialize", initializeParams(), nil); err != nil {
		disarm()
		sig, e := a.handshakeErr("initialize", ctx, startupCtx, stderr, err)
		s.Close()
		return nil, sig, e
	}
	snRaw, err := s.cl.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, nil)
	disarm() // handshake done — the watchdog must not affect the subsequent prompt
	if err != nil {
		sig, e := a.handshakeErr("session/new", ctx, startupCtx, stderr, err)
		s.Close()
		return nil, sig, e
	}
	_ = json.Unmarshal(snRaw, &s.sn)
	if s.sn.SessionID == "" {
		s.Close()
		return nil, "", fmt.Errorf("%s: session/new returned no sessionId", a.Recipe.Name)
	}
	return s, "", nil
}

// handshakeErr classifies a failed handshake: a captured prompt signature wins (an interactive prompt
// prints to stderr then hangs); then outer ctx cancel/timeout; then a startup-deadline breach (likely
// login/folder-trust); else the raw cause.
func (a *Adapter) handshakeErr(stage string, ctx, startupCtx context.Context, stderr *cappedBuffer, cause error) (clihint.Signal, error) {
	if sig := clihint.First(string(stderr.Bytes()), ""); sig != "" {
		return sig, fmt.Errorf("%s: %s blocked (%s) — run the CLI's own setup once (login / folder-trust), then retry", a.Recipe.Name, stage, sig)
	}
	switch {
	case ctx.Err() == context.Canceled:
		return "", context.Canceled
	case ctx.Err() == context.DeadlineExceeded:
		return "", fmt.Errorf("%s: %s: overall timeout after %s", a.Recipe.Name, stage, a.Timeout)
	case startupCtx.Err() == context.DeadlineExceeded:
		return "", fmt.Errorf("%s: %s: startup stalled after %s — the CLI may be waiting on login or a folder-trust prompt; complete its own setup once", a.Recipe.Name, stage, a.startupTimeout())
	default:
		return "", fmt.Errorf("%s: %s: %w", a.Recipe.Name, stage, cause)
	}
}

// failResult maps a post-handshake failure to a Result, distinguishing timeout/cancel from a fault.
func (a *Adapter) failResult(ctx context.Context, stderr *cappedBuffer, e error) (model.Result, error) {
	var errb []byte
	if stderr != nil {
		errb = stderr.Bytes()
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return model.Result{ExitCode: 124, Stderr: errb}, fmt.Errorf("%s: timed out after %s", a.Recipe.Name, a.Timeout)
	case ctx.Err() == context.Canceled:
		return model.Result{ExitCode: 125, Stderr: errb}, context.Canceled
	default:
		return model.Result{ExitCode: 1, Stderr: errb}, e
	}
}

// Probe implements model.Prober: a bounded ACP handshake (spawn → initialize → session/new) with NO
// prompt and NO tokens, reusing openSession so it exercises the exact path a real call takes. It always
// runs in a fresh empty temp cwd (never the user's tree). Diagnostic only — success does not prove a
// subsequent real prompt won't block on an approval the handshake never reaches.
func (a *Adapter) Probe(ctx context.Context) model.ProbeResult {
	bin, err := a.resolveBinary()
	if err != nil {
		return model.ProbeResult{OK: false, Stage: "resolve", Detail: err.Error()}
	}
	cwd, cleanup := resolveCwd("") // empty temp dir — never touch the user's project during a probe
	defer cleanup()
	s, sig, oerr := a.openSession(ctx, bin, cwd, 64<<10)
	if oerr != nil {
		return model.ProbeResult{OK: false, Stage: "handshake", Signal: sig, Detail: oerr.Error()}
	}
	defer s.Close()
	detail := "acp session ok"
	if m := stripParams(s.sn.Models.CurrentModelID); m != "" {
		detail += " (model: " + m + ")"
	}
	return model.ProbeResult{OK: true, Stage: "session_new", Detail: detail}
}

// stripParams drops a trailing ACP parameter suffix (`composer-2.5[fast=true]` → `composer-2.5`)
// so the reported identity compares to a base model slug.
func stripParams(s string) string {
	if i := strings.IndexAny(s, "[("); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// pickReadOnlyMode chooses the read-only session mode that best fits a one-shot Q&A/review over
// INLINE content. "ask" (pure question-answering, no edits AND no command/tool execution) is
// preferred over "plan": both prevent edits, but "plan" leaves the agent free to behave like a
// coding assistant — searching the workspace and narrating a plan instead of answering — whereas
// "ask" keeps it a chat responder that returns the requested payload. Containment is the backstop
// regardless.
func pickReadOnlyMode(modes []acpMode) string {
	have := make(map[string]bool, len(modes))
	for _, m := range modes {
		have[m.ID] = true
	}
	for _, cand := range []string{"ask", "plan"} {
		if have[cand] {
			return cand
		}
	}
	return ""
}

// pickModel finds the available ACP model id matching a requested base slug — exact on the
// base first, then a prefix (the requested slug is the base of a parameterized id).
func pickModel(models []acpModel, want string) string {
	wn := strings.ToLower(strings.TrimSpace(want))
	if wn == "" {
		return ""
	}
	for _, m := range models {
		if strings.ToLower(stripParams(m.ModelID)) == wn {
			return m.ModelID
		}
	}
	for _, m := range models {
		if strings.HasPrefix(strings.ToLower(m.ModelID), wn) {
			return m.ModelID
		}
	}
	return ""
}

// maxCapture bounds captured stderr so a runaway CLI cannot OOM the host.
const maxCapture = 32 << 20 // 32 MiB

// cappedBuffer captures up to max bytes and drops the rest (reporting full length so the
// child never sees a short write).
type cappedBuffer struct {
	buf []byte
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.buf); room > 0 {
		if len(p) <= room {
			c.buf = append(c.buf, p...)
		} else {
			c.buf = append(c.buf, p[:room]...)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf }
