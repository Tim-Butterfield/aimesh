// Package clihint classifies a failed provider-CLI invocation's captured output into a typed Signal
// (folder trust, login, invalid model, update, quota), so a caller can render actionable guidance.
//
// It performs pure detection with no UI text and no I/O; callers own the wording. Detection runs over
// stderr and the structured message of a JSON error envelope on stdout, never raw stdout, which can
// carry model-generated or injected text. Some CLIs echo their prompt to stderr, and prompts embed
// workspace text, so a caller that sent a prompt must set Failure.Prompt; the echo is removed before
// matching.
package clihint

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Signal is a recognized class of CLI-invocation failure a caller can act on.
type Signal string

// Recognized signals.
const (
	FolderTrust   Signal = "folder_trust"   // the CLI refused an untrusted / non-git directory
	LoginRequired Signal = "login_required" // the CLI is not authenticated
	ModelInvalid  Signal = "model_invalid"  // the requested model is unknown/unsupported for the account
	UpdatePrompt  Signal = "update_prompt"  // the CLI wants to update (may block on a notifier)
	// QuotaExhausted — the provider refused on a rate limit, a usage window or exhausted credit. It is
	// recognized from the provider's own refusal, never predicted, and the wait time stays in the
	// provider's message, which callers pass through verbatim.
	QuotaExhausted Signal = "quota_exhausted"
	// Timeout is inferred from the watchdog exit status when the output matched nothing else, since a
	// CLI blocked on a first-run prompt typically prints nothing.
	Timeout Signal = "timeout"
)

// TimeoutExitCode is the conventional status a watchdog-killed process reports (coreutils
// `timeout` and the shell adapters mirror it).
const TimeoutExitCode = 124

// exitSignals maps an adapter name to exit statuses that identify a blocker on their own, a channel that
// survives reworded messages. CLIs commonly exit 1 for unrelated failures, including a mistyped flag, so
// only statuses measured to distinguish a cause are listed. See docs/adapters.md.
var exitSignals = map[string]map[int]Signal{
	// gemini-cli exits 55 with "not running in a trusted directory".
	"gemini-cli": {55: FolderTrust},
}

// ExitSignal returns the Signal a measured exit status identifies for an adapter, or "" when the pair
// identifies nothing.
func ExitSignal(adapter string, code int) Signal {
	return exitSignals[adapter][code]
}

// pattern binds a Signal to an anchored matcher that requires specific context (no bare status codes or
// single words), so incidental text does not misclassify.
type pattern struct {
	sig Signal
	re  *regexp.Regexp
}

var patterns = []pattern{
	// Each alternative requires the provider to state it hit a limit (a refusal verb, an API error
	// type, or the status envelopeMessage renders), since echoed workspace text may mention limits.
	{QuotaExhausted, regexp.MustCompile(`(?i)rate[ _-]?limit_?error|(?:rate|usage|quota|credit)[ _-]?limits? ?[^.\n]{0,30}(?:exceeded|reached|hit\b|resets?\b|try again|retry after)|exceeded your [^.\n]{0,25}(?:quota|limit|credits)|quota (?:exceeded|exhausted)|too many requests|out of (?:credits|quota)|insufficient (?:credits|quota)|api_error_status=(?:429|529)|(?:status|code)[^\n]{0,8}\b429\b`)},
	{FolderTrust, regexp.MustCompile(`(?i)trusted directory|--skip-git-repo-check|folder[ -]?trust|not inside a (?:trusted|git)|do you trust`)},
	{LoginRequired, regexp.MustCompile(`(?i)not (?:logged in|authenticated)|run [` + "`'\"]?" + `\S+ login|authentication required|please (?:log ?in|sign ?in)|you are not signed in`)},
	{ModelInvalid, regexp.MustCompile(`(?i)model \S+ (?:not found|not recognized|is unavailable)|unknown model|not supported on (?:your|a) .*account|invalid --?model`)},
	{UpdatePrompt, regexp.MustCompile(`(?i)update available|new version.{0,20}available|please update|npm i(?:nstall)? -g \S`)},
}

// precedence orders signals from most to least actionable: quota first (nothing else is worth acting on
// while the provider refuses), then trust and login blocks, then model configuration, then an update
// nudge.
var precedence = []Signal{QuotaExhausted, FolderTrust, LoginRequired, ModelInvalid, UpdatePrompt}

// Classify scans stderr plus the structured message extracted from a JSON error envelope on stdout and
// returns every matched Signal, ordered by precedence (deduplicated). Empty when nothing matches.
func Classify(stderr, stdout string) []Signal {
	text := stderr
	if msg := envelopeMessage(stdout); msg != "" {
		text += "\n" + msg
	}
	matched := map[Signal]bool{}
	for _, p := range patterns {
		if p.re.MatchString(text) {
			matched[p.sig] = true
		}
	}
	var out []Signal
	for _, s := range precedence {
		if matched[s] {
			out = append(out, s)
		}
	}
	return out
}

// First returns the highest-precedence Signal, or "" if none matched. It does not remove prompt echoes,
// so it suits only output from an invocation that sent no prompt, such as a `--version` probe;
// otherwise use ForFailure with Prompt set.
func First(stderr, stdout string) Signal {
	if h := Classify(stderr, stdout); len(h) > 0 {
		return h[0]
	}
	return ""
}

// Failure is one failed invocation's captured evidence.
type Failure struct {
	Stderr string
	Stdout string
	// Prompt is what was sent. It is removed from the captured output before matching. Empty means no
	// prompt was sent, or the caller cannot know what was.
	Prompt   string
	ExitCode int
	// Adapter is the recipe name the call ran under (such as "gemini-cli"), selecting the exit-status
	// table. Empty consults only the text.
	Adapter string
}

// ForFailure returns the most actionable Signal for a failed invocation, consulting in order:
//
//  1. the text, including an `api_error_status=<n>` rendered from the error envelope, with prompt
//     echoes removed;
//  2. the measured exit status for this adapter (see exitSignals);
//  3. the watchdog status, when the process was killed.
//
// Text outranks the exit table because the provider's message about this call is better evidence than a
// status measured against one CLI version. It returns "" when nothing is actionable. Every consumer
// derives the signal through it, so displayed and recorded signals cannot disagree.
func ForFailure(f Failure) Signal {
	if s := First(stripEcho(f.Stderr, f.Prompt), stripEcho(f.Stdout, f.Prompt)); s != "" {
		return s
	}
	if s := ExitSignal(f.Adapter, f.ExitCode); s != "" {
		return s
	}
	if f.ExitCode == TimeoutExitCode {
		return Timeout
	}
	return ""
}

// stripEcho drops every captured line that, trimmed, is also a line of the prompt. It works line by line
// because CLIs interleave their own output with the echo.
func stripEcho(captured, prompt string) string {
	if strings.TrimSpace(prompt) == "" || strings.TrimSpace(captured) == "" {
		return captured
	}
	sent := make(map[string]struct{})
	for line := range strings.SplitSeq(prompt, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			sent[t] = struct{}{}
		}
	}
	var b strings.Builder
	b.Grow(len(captured))
	for line := range strings.SplitSeq(captured, "\n") {
		if _, echoed := sent[strings.TrimSpace(line)]; echoed {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// envelopeMessage extracts a human message from a generic error envelope on stdout (e.g. claude-code
// `--output-format json` → {"is_error":true,"result":"…"}). It returns "" for anything that is not a
// single JSON object flagged as an error — so raw model prose (or an injected document) on stdout is
// never fed to the matchers.
func envelopeMessage(stdout string) string {
	s := strings.TrimSpace(stdout)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return ""
	}
	var env struct {
		IsError bool   `json:"is_error"`
		Error   string `json:"error"`
		Result  string `json:"result"`
		Message string `json:"message"`
		// APIErrorStatus is claude-code's upstream HTTP status (null on success), rendered as
		// `api_error_status=<n>` so the matchers see a status the provider itself reported.
		APIErrorStatus any `json:"api_error_status"`
	}
	if json.Unmarshal([]byte(s), &env) != nil {
		return ""
	}
	if !env.IsError && strings.TrimSpace(env.Error) == "" {
		return "" // not an error envelope — ignore (don't scan a successful result's prose)
	}
	var status string
	if env.APIErrorStatus != nil {
		status = "api_error_status=" + strings.TrimSuffix(strings.TrimSpace(fmt.Sprintf("%v", env.APIErrorStatus)), ".0")
	}
	for _, m := range []string{env.Result, env.Error, env.Message} {
		if strings.TrimSpace(m) != "" {
			return strings.TrimSpace(status + "\n" + m)
		}
	}
	return status
}
