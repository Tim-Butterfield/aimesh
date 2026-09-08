package clihint

import "testing"

// TestForFailure pins the single derivation used by BOTH the human hint and the persisted
// audit signal: a text match wins; a watchdog kill with no match is a timeout; anything
// else is unclassified (the caller renders its own generic guidance).
func TestForFailure(t *testing.T) {
	cases := []struct {
		name           string
		stderr, stdout string
		exit           int
		want           Signal
	}{
		{"text match beats timeout", "Error: not logged in\n", "", TimeoutExitCode, LoginRequired},
		{"bare timeout", "", "", TimeoutExitCode, Timeout},
		{"timeout with unrelated noise", "connection reset\n", "", TimeoutExitCode, Timeout},
		{"non-timeout, no signal", "segmentation fault\n", "", 139, ""},
		{"folder trust", "please run with --skip-git-repo-check\n", "", 1, FolderTrust},
		{"model invalid", "unknown model \"gpt-9\"\n", "", 1, ModelInvalid},
		{"clean exit, nothing", "", "", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ForFailure(Failure{Stderr: tc.stderr, Stdout: tc.stdout, ExitCode: tc.exit})
			if got != tc.want {
				t.Errorf("ForFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestForFailure_TheEchoedPromptIsNotEvidence is the regression for the misclassification measured on
// 2026-08-11: a real codex seat failed on an unsupported model, and the run reported `folder_trust`
// and told the user to fix folder trust — because codex echoes the prompt to stderr, the prompt
// embedded this repo's own source, and that source says "trusted" a lot.
//
// The shape below is the real one: a large echo containing OUR vocabulary, with the adapter's actual
// error at the very end.
func TestForFailure_TheEchoedPromptIsNotEvidence(t *testing.T) {
	prompt := `You are a reviewer. Review the artifact(s) below.

	// TrustedRoots are the OUT-OF-BAND trusted roots of a surface whose caller is NOT a
	// human: the ACP surface's --root directories, established before any request arrived.
	trust, err := scope.New(req.TrustedRoots...)
	// do you trust this folder? is a question the CLI asks, not us`

	stderr := "OpenAI Codex v0.147.0\n--------\nmodel: gpt-5-codex\n--------\nuser\n" + prompt +
		"\n\nERROR: {\"type\":\"error\",\"error\":{\"message\":\"The 'gpt-5-codex' model is not supported on your account.\"}}\n"

	// Without the prompt, our own words win — this asserts the BUG still reproduces, so the test
	// cannot silently stop covering anything if the matchers change.
	if got := ForFailure(Failure{Stderr: stderr, ExitCode: 1}); got != FolderTrust {
		t.Fatalf("precondition: unsubtracted classification = %q, want %q (the echo should still mislead)", got, FolderTrust)
	}
	// With it, the adapter's own error is what gets classified.
	if got := ForFailure(Failure{Stderr: stderr, Prompt: prompt, ExitCode: 1}); got != ModelInvalid {
		t.Errorf("ForFailure with the prompt subtracted = %q, want %q — the real error is the only evidence left", got, ModelInvalid)
	}
}

// TestForFailure_EchoSubtractionKeepsTheAdaptersOwnLines: subtraction must not swallow a genuine
// signal that merely resembles the prompt's neighbourhood. Only exact (trimmed) lines go.
func TestForFailure_EchoSubtractionKeepsTheAdaptersOwnLines(t *testing.T) {
	prompt := "review this\nnot logged in is a phrase in the reviewed source\n"
	stderr := "review this\nError: you are not signed in\n"
	if got := ForFailure(Failure{Stderr: stderr, Prompt: prompt, ExitCode: 1}); got != LoginRequired {
		t.Errorf("ForFailure = %q, want %q — the adapter's own line was dropped along with the echo", got, LoginRequired)
	}
}

// TestForFailure_NoPromptIsUnchanged: a version probe sends nothing, so nothing is subtracted.
func TestForFailure_NoPromptIsUnchanged(t *testing.T) {
	if got := ForFailure(Failure{Stderr: "Error: not logged in\n", ExitCode: 1}); got != LoginRequired {
		t.Errorf("ForFailure = %q, want %q", got, LoginRequired)
	}
}

// TestTimeoutSignalIsCodeShaped keeps the persisted vocabulary machine-readable.
func TestTimeoutSignalIsCodeShaped(t *testing.T) {
	for _, s := range []Signal{FolderTrust, LoginRequired, ModelInvalid, UpdatePrompt, Timeout} {
		for _, r := range string(s) {
			if !(r >= 'a' && r <= 'z') && r != '_' {
				t.Errorf("Signal %q is not code-shaped", s)
				break
			}
		}
	}
}
