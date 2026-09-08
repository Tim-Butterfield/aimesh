// Package shell is the generic command-exec ModelAccess adapter: it runs a real
// reviewer CLI per a Recipe (binary detection, configured-path-vs-PATH lookup,
// argv construction, timeout/cancel, stdout/stderr capture, identity extraction).
// Concrete per-CLI recipes (devin/claude/codex/agy/ollama/gemini) live in
// recipes.go. Real authentication and exact identity parsing are verify-on-provision.
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

// Egress says where a call's CONTENT goes — the single most consequential fact about running an
// adapter, and the one a user has to know BEFORE the call rather than after.
//
// It lives on the recipe because the recipe is what decides it: this struct names the binary that is
// executed, so it is the only place that can honestly say which party ends up holding the prompt. It
// was prose in docs/security.md, correct but unreachable from code, so anything wanting to state a
// run's destinations had to re-derive them from a table a reader cross-referenced by hand.
type Egress struct {
	// Destination names the party that receives the content, in the words a user would recognise
	// ("Anthropic", "OpenAI"). Empty when unknown, which is not the same as none.
	Destination string
	// Local is true when nothing leaves the machine. It is the distinction that actually matters,
	// and it is deliberately a separate field rather than a magic Destination value: code that
	// branches on "did anything leave" must not have to string-match.
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
	// Egress is where this recipe's content goes. Every code-owned recipe declares one; see the
	// type. A recipe that left it zero would claim an unknown destination, which is why the
	// registry-level lookup treats "unset" and "user-defined" as the same honest answer.
	Egress Egress
	// Evidence is the identity-evidence tier this recipe currently PRODUCES (not its
	// target). Recipes whose extraction is uncaptured set EvidenceNone so the verifier
	// treats them as unverified (verify-on-provision) until a stronger parser is added.
	Evidence core.IdentityEvidence
	// Env is this recipe's PER-CLI environment, merged ONTO model.HardenedEnv() at spawn time
	// (model.HardenedEnvWith): one appended `K=V` per entry, so the recipe's value wins over both the
	// inherited environment and the hardened base. It is an override map, never a from-scratch
	// environment — the inherited variables a provider CLI authenticates with are preserved.
	//
	// It exists because non-interactive hints are NOT universally safe. `CI=1` in particular suppresses
	// or reshapes several CLIs' stderr chrome, which is where some recipes read their identity — so it
	// is set here, per recipe, only where it cannot cost the run a signal it actually records. Nil for
	// a recipe that needs nothing extra.
	Env map[string]string
	// BuildArgs constructs the argv (excluding the binary) for a call. When PromptOnStdin is set it
	// must NOT include model.Call.Prompt — the adapter delivers it separately.
	BuildArgs func(c model.Call) []string
	// PromptOnStdin delivers the prompt on the CLI's STANDARD INPUT instead of as an argv element.
	//
	// It exists because argv is not a pipe: `execve` bounds the whole argument vector, so a prompt
	// passed as an argument is capped by the OPERATING SYSTEM at a size that has nothing to do with
	// the model's context or with available memory. Measured on darwin/arm64 (ARG_MAX 1 MiB): a
	// single 512 KiB argument execs, and a 1 MiB one fails with "argument list too long" before the
	// CLI starts. Linux is stricter still per argument (MAX_ARG_STRLEN, typically 128 KiB) whatever
	// its total allows. A workspace of any size therefore could not be reviewed through argv, and
	// the failure lands at exec time with EMPTY stderr — nothing for clihint to classify, because
	// the provider never ran.
	//
	// Standard input has no such bound: it is a stream, so the limit becomes memory, which is what
	// the limit should have been all along. Verified 2026-08-12 against the installed CLIs, each
	// with a real call under read-only sandboxing: `codex exec` documents "instructions are read
	// from stdin", `claude -p` reads a piped prompt, and `gemini` appends `-p` to stdin input.
	//
	// A recipe that leaves this false keeps the argv form, and keeps the OS ceiling with it.
	PromptOnStdin bool
	// ParseIdentity extracts the actual answering model from the output. May be nil, in which case the
	// call records NO identity — never the requested model, which would be our own argument echoed back.
	ParseIdentity func(stdout, stderr []byte, c model.Call) string
	// ParsePayload extracts the SEMANTIC content when the CLI wraps it in an envelope
	// (e.g. claude-code's `result` field). May be nil (no wrapping → Stdout is the payload).
	// Returns nil/empty when there is nothing to unwrap (e.g. fake/non-envelope output).
	ParsePayload func(stdout, stderr []byte, c model.Call) []byte
	// Discovery is the OPTIONAL model-listing mechanism (see discovery.go). Nil = the
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

// Evidence reports the identity-evidence tier this adapter's recipe PRODUCES (not its target). It
// lets a Client surface project ACP-validation readiness (whether a lane can be model-identity
// verified) WITHOUT invoking the adapter. Satisfies the optional `interface{ Evidence() … }`.
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

// Probe implements model.Prober: a safe, no-model `<bin> --version` probe under a short timeout. It
// never authenticates, sends no prompt, and answers no interactive prompt; a captured login/folder-trust
// signature (or a hang) is classified via clihint. A version probe cannot exercise auth/trust/model, so
// success here does NOT prove a real review call won't block — it is a diagnostic. Real model identity is
// verified on spawned review calls, not here.
func (a *Adapter) Probe(ctx context.Context) model.ProbeResult {
	bin, err := a.resolveBinary()
	if err != nil {
		return model.ProbeResult{OK: false, Stage: "resolve", Detail: err.Error()}
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "--version")
	// The SAME environment a real call gets (base + this recipe's overrides), so the probe is
	// representative: a CLI that only misbehaves under the recipe's env would otherwise probe clean.
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
// element. It is the exact inverse of PromptOnStdin by the BuildArgs contract: a recipe that sets
// PromptOnStdin must NOT put the prompt in argv, and one that leaves it false keeps the argv form.
func (r Recipe) promptReachesArgv() bool { return !r.PromptOnStdin }

// checkPromptNotOptionLike refuses, before anything is spawned, a prompt that an argv-passing CLI
// would parse as an OPTION instead of as the prompt.
//
// argv is not a byte channel. A prompt handed to a CLI as a positional argument (ollama, cursor-cli)
// or as an option VALUE (devin-cli and agy-cli, each `-p <prompt>`) reaches that CLI's flag parser
// before it reaches its model, so a prompt whose first byte is `-` is consumed as a flag and the CLI
// exits on an unknown option having never seen it. None of the four argv recipes passes a `--`
// end-of-options separator, and `--` is not universally honored, so the constraint is real.
//
// Measured 2026-08-30: a ballot header opening `-----` cost a live shortlist run 3 of 5 seats —
// devin-cli exited 2 printing its usage, cursor-cli exited 1 echoing prompt text — while the three
// PromptOnStdin recipes (codex-cli, claude-code, gemini-cli) answered normally. Quorum failed and
// every ranked row came back withheld.
//
// The reason this belongs at the adapter rather than in each prompt builder is that the failure is
// silent exactly where it is expensive: each affected lane looks like an ordinary non-zero exit, so
// a governed run degrades to a partial panel instead of stopping, and the tally then reflects
// whichever providers happen to read their prompt from stdin. Refusing here turns a distributed,
// provider-shaped bias into ONE legible cause at the boundary that owns the constraint. It costs a
// stdin recipe nothing: the check applies only where the prompt actually meets a flag parser.
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
	// BEFORE resolving a binary or spending anything: a prompt this recipe would hand to a flag
	// parser as an option is refused outright (see checkPromptNotOptionLike). 126 is the shell's
	// "found but not executable" — the nearest honest code for a call the host declined to run.
	// The check is pure argument inspection, so it answers identically with no CLI installed.
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
	// inherited env + the universal hardened hints + THIS recipe's own overrides (appended last, so a
	// recipe wins; auth-bearing inherited variables are never dropped).
	cmd.Env = model.HardenedEnvWith(a.Recipe.Env)
	// The prompt goes down a PIPE, not into argv, wherever the recipe supports it — see
	// Recipe.PromptOnStdin for the measured reason. strings.Reader, so the bytes are not copied
	// again on the way out.
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
	// With no parser there is NO identity — not an assumed one. Echoing back the model we requested
	// would report a "confirmation" manufactured from our own argument, which is precisely the defect
	// that got codex-cli's banner demoted (see codexRecipe). An empty ActualModel is the honest answer
	// and costs nothing: identity is recorded, never enforced (../../../docs/model-identity.md).
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

// WithDebug is retained for the apps' existing wiring; it delegates to model.WithDebug so shell and
// the ACP adapter share ONE debug writer key (enable --debug once, every adapter emits). Resolved
// binary + argv, exit code, duration, extracted identity + evidence tier, and captured stderr/stdout
// go to w. Off by default; for troubleshooting an adapter/provider and bringing up a new adapter.
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
