package clihint

import (
	"strings"
	"testing"
)

// TestQuota_ClassifiedFromWhatProvidersActuallySay. Quota is the one failure whose correct response
// is to WAIT: told anything else — or told nothing — a user goes looking for a configuration fault
// that is not there. It is surfaced exactly like folder-trust or an update nudge, as a peer signal
// through the same hint path, because to the person running it they are the same kind of event: the
// provider is refusing to proceed until something outside aimesh changes.
func TestQuota_ClassifiedFromWhatProvidersActuallySay(t *testing.T) {
	for _, tc := range []struct{ name, stderr string }{
		{"anthropic api error type", `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`},
		{"claude usage window", "Claude usage limit reached. Your limit will reset at 3:00 PM."},
		{"openai 429", "Error: 429 Too Many Requests"},
		{"http status form", "request failed with status code 429"},
		{"quota wording", "You have exceeded your current quota, please check your plan and billing details."},
		{"credits", "You are out of credits. Add credits to continue."},
		{"retry-after", "rate limit exceeded, retry after 60s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ForFailure(Failure{Stderr: tc.stderr, ExitCode: 1}); got != QuotaExhausted {
				t.Errorf("signal = %q, want %q\n  from: %s", got, QuotaExhausted, tc.stderr)
			}
		})
	}
}

// TestQuota_FromTheStructuredEnvelope: claude-code reports the upstream HTTP status in its own
// `api_error_status` field. A status the PROVIDER set cannot be planted by reviewed source the way a
// phrase can, so it is the strongest channel available and is matched structurally.
func TestQuota_FromTheStructuredEnvelope(t *testing.T) {
	env := `{"is_error":true,"api_error_status":429,"result":"upstream request failed","type":"result"}`
	if got := ForFailure(Failure{Stdout: env, ExitCode: 1}); got != QuotaExhausted {
		t.Errorf("signal = %q, want %q", got, QuotaExhausted)
	}
	// A SUCCESSFUL envelope carries api_error_status: null and must classify as nothing at all —
	// the shape measured on a real run, kept here so a passing call can never be read as a failure.
	ok := `{"is_error":false,"api_error_status":null,"result":"{\"findings\":[]}","type":"result"}`
	if got := ForFailure(Failure{Stdout: ok, ExitCode: 0}); got != "" {
		t.Errorf("a successful envelope classified as %q", got)
	}
}

// TestQuota_FromEachCLIsOwnStructuredStatus. The structured channel is not claude-only, which is what
// the 2026-08-12 measurement established: forcing a bad model showed codex printing
// `ERROR: {"type":"error","status":400,...}` and gemini surfacing `code: 404` from its own error
// object. Those are the same field a 429 arrives in, so the strongest channel covers all three CLIs.
//
// The strings below are the MEASURED wire shapes (claude 2.1.227, codex-cli 0.147.0, gemini 0.54.4),
// with the status changed to the rate-limit one — the part of the shape that varies by failure.
func TestQuota_FromEachCLIsOwnStructuredStatus(t *testing.T) {
	for _, tc := range []struct{ name, stderr, stdout string }{
		{
			name:   "codex ERROR envelope on stderr",
			stderr: `ERROR: {"type":"error","status":429,"error":{"type":"rate_limit_error","message":"Rate limit reached for this account."}}`,
		},
		{
			name:   "gemini error object in its node dump",
			stderr: "    at retryWithBackoff (file:///…/chunk.js:307231:31)\nAn unexpected critical error occurred:[object Object] {\n  code: 429\n}",
		},
		{
			name:   "claude api_error_status in its json envelope",
			stdout: `{"is_error":true,"terminal_reason":"api_error","api_error_status":429,"result":"upstream request failed","type":"result"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ForFailure(Failure{Stderr: tc.stderr, Stdout: tc.stdout, ExitCode: 1}); got != QuotaExhausted {
				t.Errorf("signal = %q, want %q — the provider reported the status itself", got, QuotaExhausted)
			}
		})
	}
}

// TestQuota_DoesNotFireOnOurOwnVocabulary is the #49 lesson applied before the fact rather than
// after it. Reviewed source reaches these matchers whenever echo subtraction is imperfect, and this
// project's own code and docs discuss rate limiting, quotas and HTTP statuses at length. A matcher
// that fired on the WORDS rather than on a provider refusing would misreport a real failure as a
// quota wait — the exact defect shape that told a user to fix folder trust that was never broken.
func TestQuota_DoesNotFireOnOurOwnVocabulary(t *testing.T) {
	for _, text := range []string{
		"// QuotaExhausted — the provider refused on a rate limit, a usage window, or exhausted credit.",
		"the rate limit signal is classified from captured output, never predicted in advance",
		"see docs/adapters.md for how a rate limit is surfaced to the caller",
		"const maxRetries = 429 // arbitrary",
		"line 429: unexpected token",
		"quota accounting is deliberately absent from this repo",
	} {
		if got := ForFailure(Failure{Stderr: text, ExitCode: 1}); got == QuotaExhausted {
			t.Errorf("classified our own prose as a quota refusal:\n  %s", text)
		}
	}
}

// TestQuota_OutranksTheOtherSignals: a provider that is refusing to serve makes every other
// classification moot, and acting on one of them sends the user to debug a fault that is not there.
// The measured case for this is codex, which prints its whole prompt to stderr — so a quota refusal
// arrives mixed with whatever vocabulary the reviewed source happens to contain.
func TestQuota_OutranksTheOtherSignals(t *testing.T) {
	mixed := strings.Join([]string{
		"warning: a new version is available, please update",
		"error: rate limit exceeded, retry after 60s",
	}, "\n")
	if got := ForFailure(Failure{Stderr: mixed, ExitCode: 1}); got != QuotaExhausted {
		t.Errorf("signal = %q, want %q — quota must win", got, QuotaExhausted)
	}
	all := Classify(mixed, "")
	if len(all) < 2 || all[0] != QuotaExhausted {
		t.Errorf("Classify = %v, want quota first with the update nudge still recorded", all)
	}
}
