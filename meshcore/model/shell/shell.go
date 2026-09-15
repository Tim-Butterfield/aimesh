// Package shell is the command-exec model adapter: it runs a provider CLI according to a Recipe
// (binary lookup, argv construction, timeout and cancellation, output capture, identity extraction).
// The per-CLI recipes live in recipes.go.
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// Egress says where a call's content goes. It lives on the recipe because the recipe names the
// binary that receives the prompt.
type Egress struct {
	// Destination names the party that receives the content ("Anthropic", "OpenAI"). Empty when
	// unknown, which is not the same as none.
	Destination string
	// Local is true when nothing leaves the machine. It is a separate field so code can check it
	// without matching Destination strings.
	Local bool
	// Note carries the qualification a destination needs when naming one party would overstate it —
	// a gateway that routes onward, or a CLI whose destination is whatever the operator pointed it
	// at. Empty when the destination stands on its own.
	Note string
}

// Recipe describes how to drive one CLI (see docs/adapters.md).
type Recipe struct {
	Name     string // adapter name, e.g. "codex-cli"
	Detect   string // binary name for PATH lookup, e.g. "codex"
	Identity core.IdentityMethod
	// Egress is where this recipe's content goes. A zero Egress means the destination is unknown.
	Egress Egress
	// Evidence is the identity-evidence tier this recipe produces. A recipe without a captured
	// extraction sets EvidenceNone, so its calls are treated as unverified.
	Evidence core.IdentityEvidence
	// Env holds this recipe's environment overrides, merged onto model.HardenedEnv at spawn time
	// (model.HardenedEnvWith). Hints such as CI=1 are set per recipe because some CLIs change the
	// output identity parsing reads under them. Nil when nothing extra is needed.
	Env map[string]string
	// BuildArgs constructs the argv (excluding the binary) for a call. When PromptOnStdin is set it
	// must not include model.Call.Prompt; the adapter delivers it separately.
	BuildArgs func(c model.Call) []string
	// PromptOnStdin delivers the prompt on standard input instead of in argv. The OS bounds argv
	// size (on Linux, typically 128 KiB per argument), so a large prompt fails at exec time with
	// no output; standard input is bounded only by memory.
	PromptOnStdin bool
	// ParseIdentity extracts the actual answering model from the output. May be nil, in which case the
	// call records NO identity — never the requested model, which would be our own argument echoed back.
	ParseIdentity func(stdout, stderr []byte, c model.Call) string
	// ParsePayload extracts the SEMANTIC content when the CLI wraps it in an envelope
	// (e.g. claude-code's `result` field). May be nil (no wrapping → Stdout is the payload).
	// Returns nil/empty when there is nothing to unwrap (e.g. fake/non-envelope output).
	ParsePayload func(stdout, stderr []byte, c model.Call) []byte
	// Discovery is the optional model-listing mechanism (see discovery.go). Nil = the
	// CLI documents no non-interactive way to enumerate models (claude/devin/gemini).
	Discovery *Discovery
}

// Adapter runs a Recipe.
type Adapter struct {
	Recipe  Recipe
	Path    string        // configured binary path override ("" = look up Detect on PATH)
	Timeout time.Duration // per-call timeout (<=0 → default)
}

// New builds a shell adapter.
func New(r Recipe, path string, timeout time.Duration) *Adapter {
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	return &Adapter{Recipe: r, Path: path, Timeout: timeout}
}

// Registry builds one runnable adapter per known recipe, keyed by adapter name. This is the
// single adapter-discovery point: both applications construct their adapter set from it, so a
// recipe added to Recipes() is picked up by every app with no per-app change. paths[name] is an
// optional configured binary-path override for that adapter (absent or "" → look up the recipe's
// Detect name on PATH). A nil paths map is fine (all adapters resolve via PATH).
func Registry(paths map[string]string, timeout time.Duration) map[string]model.Adapter {
	recipes := Recipes()
	reg := make(map[string]model.Adapter, len(recipes))
	for name, r := range recipes {
		reg[name] = New(r, paths[name], timeout)
	}
	return reg
}

// Name returns the adapter's stable registry key (the recipe name, e.g. "claude-code").
func (a *Adapter) Name() string { return a.Recipe.Name }

// Evidence reports the identity-evidence tier this adapter's recipe produces, so callers can report
// verification readiness without invoking the adapter.
func (a *Adapter) Evidence() core.IdentityEvidence { return a.Recipe.Evidence }

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

// DetectName is the binary name this adapter looks for on PATH (e.g. "claude" for
// the claude-code adapter) — used to render repair hints.
func (a *Adapter) DetectName() string { return a.Recipe.Detect }

// Available reports whether the binary is present and the specific reason if not
// (configured-path missing / directory / not-executable, or PATH not found) — no
// model call. Authentication remains the CLI's own concern.
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

// Probe implements model.Prober with a bounded `<bin> --version` run. It sends no prompt and answers
// no interactive prompt; a login or folder-trust signature, or a hang, is classified via clihint.
// Success proves only that the binary starts, not that a real call will succeed.
func (a *Adapter) Probe(ctx context.Context) model.ProbeResult {
	bin, err := a.resolveBinary()
	if err != nil {
		return model.ProbeResult{OK: false, Stage: "resolve", Detail: err.Error()}
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "--version")
	// The same environment as a real call, so recipe-specific behavior shows up in the probe.
	cmd.Env = model.HardenedEnvWith(a.Recipe.Env)
	// Separate bounded buffers for stdout/stderr — os/exec pumps them on distinct
	// goroutines, so a single shared buffer would data-race (and could OOM unbounded).
	stdout := &cappedBuffer{max: 64 << 10}
	stderr := &cappedBuffer{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = sysProcAttr()
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process) }
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	sig := clihint.First(string(stderr.Bytes()), string(stdout.Bytes()))
	if pctx.Err() == context.DeadlineExceeded {
		return model.ProbeResult{OK: false, Stage: "version", Signal: sig, Detail: "probe timed out — the CLI may be waiting on native login or a folder-trust prompt; complete its own setup first"}
	}
	if runErr != nil {
		return model.ProbeResult{OK: false, Stage: "version", Signal: sig, Detail: fmt.Sprintf("probe failed: %v", runErr)}
	}
	text := string(stdout.Bytes())
	if strings.TrimSpace(text) == "" {
		text = string(stderr.Bytes()) // some CLIs print --version to stderr
	}
	line := strings.TrimSpace(text)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return model.ProbeResult{OK: true, Stage: "version", Detail: "probe ok: " + line}
}

var _ model.Prober = (*Adapter)(nil)

// promptReachesArgv reports whether this recipe hands model.Call.Prompt to the CLI as an argv
// element: the inverse of PromptOnStdin under the BuildArgs contract.
func (r Recipe) promptReachesArgv() bool { return !r.PromptOnStdin }

// checkPromptNotOptionLike refuses, before anything is spawned, a prompt that an argv-passing CLI
// would parse as an option. A prompt passed as a positional argument or option value reaches the
// CLI's flag parser, so a leading `-` yields a usage error that looks like an ordinary non-zero exit.
// Checking at the adapter reports one clear cause instead. Recipes that use stdin are unaffected.
func checkPromptNotOptionLike(r Recipe, prompt string) error {
	if !r.promptReachesArgv() || !strings.HasPrefix(prompt, "-") {
		return nil
	}
	return fmt.Errorf("%s: refusing to call: this recipe passes the prompt as an argv element, and a "+
		"prompt beginning with '-' is parsed by the CLI as an option rather than as the prompt — the "+
		"call would produce a usage error, not an answer. Fix the prompt's first character (a letter "+
		"or '=' is safe), or give this recipe PromptOnStdin. Prompt head: %.80q", r.Name, prompt)
}

// Invoke runs the CLI under a timeout, captures output, and normalizes a Result.
// A non-zero exit is reported via Result.ExitCode (not a Go error) so the caller
// classifies it; a timeout returns ExitCode 124 plus an error.
func (a *Adapter) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	// An option-like prompt is refused before resolving a binary (see checkPromptNotOptionLike).
	// 126 is the shell's "found but not executable" code, the nearest match for a declined call.
	if err := checkPromptNotOptionLike(a.Recipe, c.Prompt); err != nil {
		return model.Result{ExitCode: 126, Stderr: []byte(err.Error())}, err
	}
	bin, err := a.resolveBinary()
	if err != nil {
		return model.Result{ExitCode: 127, Stderr: []byte(err.Error())}, err
	}
	// derive from the caller's context so a run-level cancel also stops the CLI
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	argv := a.Recipe.BuildArgs(c)
	cmd := exec.CommandContext(ctx, bin, argv...)
	if c.WorkDir != "" {
		cmd.Dir = c.WorkDir // containment: run the CLI in the caller's isolated dir, not the process cwd
	}
	// Inherited environment, hardened hints, then this recipe's overrides, which win.
	cmd.Env = model.HardenedEnvWith(a.Recipe.Env)
	// The prompt goes through standard input where the recipe supports it; see
	// Recipe.PromptOnStdin.
	if a.Recipe.PromptOnStdin {
		cmd.Stdin = strings.NewReader(c.Prompt)
	}
	stdout := &cappedBuffer{max: maxCapture}
	stderr := &cappedBuffer{max: maxCapture}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Run the CLI in its own process group and, on timeout/cancel, signal the group
	// (best-effort) then force-kill the child after a short grace period. Full
	// process-tree termination of grandchildren is OS-specific (verify-on-provision).
	cmd.SysProcAttr = sysProcAttr()
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process) }
	cmd.WaitDelay = 5 * time.Second

	var res model.Result
	start := time.Now()
	// Optional per-invocation diagnostic (opt-in via WithDebug), emitted on every return path.
	defer func() {
		if w := debugWriter(ctx); w != nil {
			emitDebug(w, a.Recipe.Name, bin, argv, res, time.Since(start))
		}
	}()

	runErr := cmd.Run()
	res = model.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ctx.Err() == context.DeadlineExceeded {
		res.ExitCode = 124
		return res, fmt.Errorf("%s: timed out after %s", a.Recipe.Name, a.Timeout)
	}
	if ctx.Err() == context.Canceled {
		res.ExitCode = 125
		return res, context.Canceled // run-level cancellation
	}
	if runErr != nil {
		if ee, ok := errors.AsType[*exec.ExitError](runErr); ok {
			res.ExitCode = ee.ExitCode() // non-zero exit: caller classifies
		} else {
			res.ExitCode = 1
			return res, runErr // could not start / I/O error
		}
	}
	// With no parser the identity stays empty rather than echoing the requested model, which would
	// be a confirmation made from our own argument. Identity is recorded, never enforced
	// (docs/model-identity.md).
	if a.Recipe.ParseIdentity != nil {
		res.ActualModel = a.Recipe.ParseIdentity(res.Stdout, res.Stderr, c)
	}
	// Report the evidence tier; if nothing was extracted, there is no usable evidence.
	res.Evidence = a.Recipe.Evidence
	if res.ActualModel == "" {
		res.Evidence = core.EvidenceNone
	}
	// Unwrap an envelope into the semantic payload, if the recipe wraps one.
	if a.Recipe.ParsePayload != nil {
		res.Payload = a.Recipe.ParsePayload(res.Stdout, res.Stderr, c)
	}
	return res, nil
}

var _ model.Adapter = (*Adapter)(nil)

// --- optional per-invocation debug (opt-in via WithDebug) ---

// WithDebug enables per-invocation debug output to w by delegating to model.WithDebug, so every
// adapter shares one debug writer. The output covers the binary and argv, exit code, duration,
// extracted identity and evidence tier, and captured streams. Debug output is off by default.
func WithDebug(ctx context.Context, w io.Writer) context.Context { return model.WithDebug(ctx, w) }

func debugWriter(ctx context.Context) io.Writer { return model.DebugWriter(ctx) }

// debugCap bounds how much of each captured stream the debug block prints (the streams may be many
// MiB; a truncated head is enough to diagnose auth/flag/output problems).
const debugCap = 4 << 10 // 4 KiB

func emitDebug(w io.Writer, name, bin string, argv []string, res model.Result, dur time.Duration) {
	fmt.Fprintf(w, "\n── adapter debug: %s ──\n", name)
	fmt.Fprintf(w, "  bin:      %s\n", bin)
	fmt.Fprintf(w, "  argv:     %s %s\n", bin, strings.Join(argvForDebug(argv), " "))
	fmt.Fprintf(w, "  exit:     %d   (%s)\n", res.ExitCode, dur.Round(time.Millisecond))
	fmt.Fprintf(w, "  identity: model=%q evidence=%s\n", res.ActualModel, res.Evidence)
	fmt.Fprintf(w, "  stderr (%d bytes):\n%s", len(res.Stderr), indentCapped(res.Stderr))
	fmt.Fprintf(w, "  stdout (%d bytes):\n%s", len(res.Stdout), indentCapped(res.Stdout))
	fmt.Fprintf(w, "──\n")
}

// argvForDebug truncates any very long arg (e.g. an inlined prompt) so the argv line stays readable.
func argvForDebug(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if len(a) > 300 {
			out[i] = a[:300] + fmt.Sprintf("…(+%d bytes)", len(a)-300)
		} else {
			out[i] = a
		}
	}
	return out
}

// debugANSI strips terminal control/escape sequences (CSI, incl. private `?` modes) so a diagnostic
// dump of a CLI's captured stderr/stdout stays readable (e.g. ollama's spinner escapes).
var debugANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func indentCapped(b []byte) string {
	s := debugANSI.ReplaceAllString(string(b), "")
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

// maxCapture bounds per-stream output so a runaway CLI cannot OOM the host.
const maxCapture = 32 << 20 // 32 MiB

// cappedBuffer captures up to max bytes and silently drops the rest (reporting the
// full length as written so the child never sees a short write / SIGPIPE).
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) <= room {
			return c.buf.Write(p)
		}
		_, _ = c.buf.Write(p[:room])
	}
	c.truncated = true
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }
