// Package clihint classifies a failed provider-CLI invocation's captured output into a typed Signal
// (folder-trust / update / login / model-invalid), so a caller can render actionable guidance when a
// CLI blocks on an interactive first-run prompt or a fixable misconfiguration.
//
// It is PURE detection — no UI prose, no I/O — so meshcore stays substrate-neutral and boundary-clean;
// the caller (doctor, reviewmesh CLI, the web UI) owns the human wording. Detection runs over stderr
// and the STRUCTURED message extracted from a JSON error envelope on stdout — never raw stdout, because
// a failed call's stdout can carry arbitrary model-generated (or prompt-injected) text that could plant
// a false "run … login" signature.
//
// THE SAME HAZARD EXISTS ON STDERR, and it is not hypothetical. Several provider CLIs echo the prompt
// they were given to stderr before doing anything (codex prints the whole thing under a banner). Our
// prompts embed the reviewed source, so whatever vocabulary that source uses becomes input to these
// matchers. Measured on 2026-08-11: a review of this repo's own internal/review/manager/run package
// produced 90 KB of codex stderr that was mostly our prompt echoed back, containing our own words
// `TrustedRoots` and "trusted" — and the classifier returned folder_trust for a call whose real error,
// at the very end, was "The 'gpt-5-codex' model is not supported when using Codex with a ChatGPT
// account." The user was told to fix folder trust, which was never broken.
//
// So a caller that SENT a prompt must say so (Failure.Prompt) and the echo is subtracted before any
// matching. The defence is the same one the stdout rule already states; it simply had to be extended to
// the other stream. Note the failure mode is worst when aimesh reviews itself, because that is when the
// reviewed text is densest in this project's own governance vocabulary.
package clihint

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Signal is a recognized class of CLI-invocation failure a caller can act on.
type Signal string

const (
	FolderTrust   Signal = "folder_trust"   // the CLI refused an untrusted / non-git directory
	LoginRequired Signal = "login_required" // the CLI is not authenticated
	ModelInvalid  Signal = "model_invalid"  // the requested model is unknown/unsupported for the account
	UpdatePrompt  Signal = "update_prompt"  // the CLI wants to update (may block on a notifier)
	// QuotaExhausted — the provider refused on a rate limit, a usage window, or exhausted credit.
	//
	// It is REPORTED, never predicted. aimesh cannot see a subscription's plan, its window, or what
	// else is drawing on it, so any advance accounting would be a number invented from nothing — the
	// same mistake as a coverage cap chosen without evidence. What it CAN do is recognise the
	// provider's own refusal when it arrives and say so plainly, because this is the one failure
	// class where the correct response is to wait rather than to debug: told anything else, a user
	// goes looking for a configuration fault that does not exist.
	//
	// Rate limits and usage-window exhaustion are ONE signal deliberately. They differ only in how
	// long the wait is, and that duration is in the provider's own message, which the caller passes
	// through verbatim — paraphrasing a reset time we might get wrong would be worse than quoting it.
	QuotaExhausted Signal = "quota_exhausted"
	// Timeout is NOT text-detected: it is inferred from the process outcome (the
	// conventional watchdog exit status) when the output matched nothing else. A CLI
	// that blocks on an interactive first-run prompt typically prints nothing at all,
	// so the timeout itself is the only available signal — and it is still actionable
	// ("run it once directly to finish setup").
	Timeout Signal = "timeout"
)

// TimeoutExitCode is the conventional status a watchdog-killed process reports (coreutils
// `timeout` and the shell adapters mirror it).
const TimeoutExitCode = 124

// exitSignals maps an ADAPTER NAME to the process exit statuses MEASURED to identify a blocker on
// their own. It is a channel that survives a reworded error message, which prose cannot.
//
// IT HAS ONE ROW, AND THAT IS THE MEASUREMENT RESULT — not an unfinished table. Every combination
// below was forced against the installed CLIs on 2026-08-12 (claude 2.1.227, codex-cli 0.147.0,
// gemini 0.54.4, darwin/arm64), each in a directory chosen to isolate the blocker:
//
//	CLI     untrusted dir   bogus model   unknown flag   discriminating?
//	claude  0 (no wall)     1             1              no — model error and usage error share 1
//	codex   1               1             2              no — trust and model error share 1
//	gemini  55              1             1              YES for 55; model error and usage share 1
//
// So exactly one status in the matrix names a cause: gemini's 55 for an untrusted directory. Every
// other failure exits 1, which each CLI also uses for a mistyped flag — a code that means "something
// went wrong" classifies nothing, and mapping it would manufacture the same false confidence #49 was
// about. Login could not be measured without logging the user out, and quota is not forceable on
// demand; both are recorded here when a real run produces one.
//
// A row is added ONLY from a measurement, with its date and CLI version. An unmeasured guess here is
// worse than an empty table: prose at least describes the failure that actually happened.
var exitSignals = map[string]map[int]Signal{
	// gemini-cli 0.54.4, measured 2026-08-12 (twice, in two different untrusted directories) and again
	// by the earlier real-token campaign that first recorded it in geminiRecipe: it exits 55 with
	// "not running in a trusted directory". The prose is also matched today; this is what keeps the
	// classification working when Google rewords it.
	"gemini-cli": {55: FolderTrust},
}

// ExitSignal returns the Signal a MEASURED exit status identifies for an adapter, or "" when that
// adapter/status pair says nothing. Exported so a caller can classify a failure it holds only the
// status for, and so the table is testable without reaching through ForFailure.
func ExitSignal(adapter string, code int) Signal {
	return exitSignals[adapter][code]
}

// pattern binds a Signal to an ANCHORED matcher — each requires specific context (no bare status codes
// or single words) so an entitlement error or incidental text does not misclassify.
type pattern struct {
	sig Signal
	re  *regexp.Regexp
}

var patterns = []pattern{
	// Every alternative needs the provider to be SAYING it hit a limit, not merely mentioning one.
	// A bare "rate limit" or a bare 429 is vocabulary this project's own source and docs contain, and
	// reviewed source reaches these matchers whenever the echo subtraction is imperfect — the defect
	// recorded in the package comment. So each branch pairs the noun with a refusal verb, an API error
	// type (`rate_limit_error`), or the structured status envelopeMessage synthesizes.
	{QuotaExhausted, regexp.MustCompile(`(?i)rate[ _-]?limit_?error|(?:rate|usage|quota|credit)[ _-]?limits? ?[^.\n]{0,30}(?:exceeded|reached|hit\b|resets?\b|try again|retry after)|exceeded your [^.\n]{0,25}(?:quota|limit|credits)|quota (?:exceeded|exhausted)|too many requests|out of (?:credits|quota)|insufficient (?:credits|quota)|api_error_status=(?:429|529)|(?:status|code)[^\n]{0,8}\b429\b`)},
	{FolderTrust, regexp.MustCompile(`(?i)trusted directory|--skip-git-repo-check|folder[ -]?trust|not inside a (?:trusted|git)|do you trust`)},
	{LoginRequired, regexp.MustCompile(`(?i)not (?:logged in|authenticated)|run [` + "`'\"]?" + `\S+ login|authentication required|please (?:log ?in|sign ?in)|you are not signed in`)},
	{ModelInvalid, regexp.MustCompile(`(?i)model \S+ (?:not found|not recognized|is unavailable)|unknown model|not supported on (?:your|a) .*account|invalid --?model`)},
	{UpdatePrompt, regexp.MustCompile(`(?i)update available|new version.{0,20}available|please update|npm i(?:nstall)? -g \S`)},
}

// precedence orders signals from most to least actionable when a caller wants a single one: quota
// outranks everything (nothing else is worth acting on while the provider is refusing to serve, and
// acting on anything else sends the user to debug a fault that is not there), then an auth / trust
// block (the CLI is waiting on YOU), then a config problem (model), then a soft update nudge.
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

// First returns the highest-precedence Signal, or "" if none matched. A caller handling a timeout with
// no signal ("" return) should render its own generic "the CLI may be waiting on login or a folder-trust
// prompt" guidance.
//
// IT DOES NO ECHO SUBTRACTION, so it is safe ONLY for output produced by an invocation that sent no
// prompt — a `--version` probe. Anything that sent a prompt must go through ForFailure with Prompt set;
// see the package comment for what happens otherwise.
func First(stderr, stdout string) Signal {
	if h := Classify(stderr, stdout); len(h) > 0 {
		return h[0]
	}
	return ""
}

// Failure is one failed invocation's captured evidence. It is a struct rather than a parameter list
// because Prompt is the field a caller must not silently omit: leaving it empty is a claim that nothing
// was sent, and that claim is exactly what makes the classification trustworthy.
type Failure struct {
	Stderr string
	Stdout string
	// Prompt is what we SENT. It is subtracted from the captured output before matching, because a CLI
	// that echoes its prompt otherwise feeds our own words back to the matchers (see the package
	// comment). Empty means no prompt was sent — true for a version probe and for a caller
	// reconstructing a failure it did not itself issue, where nothing can be subtracted honestly.
	Prompt   string
	ExitCode int
	// Adapter is the recipe name the call ran under (e.g. "gemini-cli"). It selects the MEASURED
	// exit-status table, so a failure whose message a provider has since reworded is still classified
	// from its process status. Empty means the caller does not know which CLI failed, and only the
	// text channels are consulted — never a wrong table.
	Adapter string
}

// ForFailure returns the single most actionable Signal for a FAILED invocation, consulting three
// channels in descending order of how specific each is to THIS failure:
//
//  1. the text — the provider's own account of what went wrong, including the structured HTTP status
//     it reported (rendered as `api_error_status=<n>`), with our prompt echo subtracted first;
//  2. the MEASURED exit status for this adapter (see exitSignals) — less specific, because it was
//     measured once against one version, but it is the channel that survives a reworded message;
//  3. the watchdog status, when nothing else matched and the process was killed.
//
// Text outranks the exit table deliberately. The two agree wherever both speak today, so the order
// only decides a disagreement — and there, a message the provider emitted about this call is better
// evidence than a status this repo measured months earlier. Putting the code first would let a stale
// row overrule a provider saying plainly that something else went wrong.
//
// It returns "" when the failure carries no actionable signal — a caller should then render its own
// generic guidance rather than guess.
//
// It exists so every consumer (the CLI's next-step hint, the audit record, an agent
// surface) derives the signal ONE way; a signal shown to a human and a signal persisted
// for a machine must not be able to disagree.
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

// stripEcho drops every line of captured output that is also a line of the prompt we sent.
//
// LINE-WISE rather than one substring removal, because a CLI interleaves its own output with the echo
// (codex frames it with a banner and a `user` marker), so a single contiguous match would usually fail
// and silently leave the whole echo in place — the failure mode this exists to prevent, restored.
//
// Trimmed comparison absorbs re-indentation. It can drop a genuine error line that happens to be
// byte-identical to a prompt line after trimming; that is the right trade, because a line we sent is
// evidence about our own input either way, and the alternative is classifying on our own text.
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
		// APIErrorStatus is claude-code's own field for the upstream HTTP status (null on success).
		// It is the STRUCTURED form of the thing the prose matchers have to guess at, so it is
		// rendered into the text as `api_error_status=<n>` and matched there — a status the provider
		// itself reported cannot be planted by reviewed source the way a phrase can.
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
