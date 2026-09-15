package clihint

import "testing"

// ForFailure is the one derivation behind both the human hint and the recorded signal: a text match
// wins, a watchdog kill with no match is a timeout, and anything else is unclassified.
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

// A prompt echoed to stderr is not evidence. codex echoes the prompt, and a prompt that embeds source
// mentioning "trusted" would otherwise classify an unsupported-model failure as folder trust. The
// fixture is a large echo with the adapter's real error at the end.
func TestForFailure_TheEchoedPromptIsNotEvidence(t *testing.T) {
	prompt := `You are a reviewer. Review the artifact(s) below.

	// TrustedRoots are the OUT-OF-BAND trusted roots of a surface whose caller is NOT a
	// human: the ACP surface's --root directories, established before any request arrived.
	trust, err := scope.New(req.TrustedRoots...)
	// do you trust this folder? is a question the CLI asks, not us`

	stderr := "OpenAI Codex v0.147.0\n--------\nmodel: gpt-5-codex\n--------\nuser\n" + prompt +
		"\n\nERROR: {\"type\":\"error\",\"error\":{\"message\":\"The 'gpt-5-codex' model is not supported on your account.\"}}\n"

	// Without the prompt the echo still misleads, so the test keeps covering the case if the matchers
	// change.
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
