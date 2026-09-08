package acpagent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestValidateCandidate_HappyPath drives the fake ACP server (this test binary) through the hint path:
// the winning args + the model the handshake reports come back on the Candidate.
func TestValidateCandidate_HappyPath(t *testing.T) {
	hint := []string{"-test.run=TestHelperACPServer", "--", "fakeacp", "composer-2.5[fast=true]", `{"findings":[]}`, "ok"}
	c := ValidateCandidate(context.Background(), os.Args[0], hint)
	if !c.OK {
		t.Fatalf("expected OK, got detail=%q", c.Detail)
	}
	if c.Model != "composer-2.5" {
		t.Errorf("Model = %q, want composer-2.5", c.Model)
	}
	if strings.Join(c.Args, " ") != strings.Join(hint, " ") {
		t.Errorf("Args = %v, want the hint that worked", c.Args)
	}
	if !strings.HasPrefix(c.Title, "ACP: ") {
		t.Errorf("Title = %q, want an \"ACP: \" prefix", c.Title)
	}
}

// TestDetectACPArgs exercises the --help heuristic against fake CLIs that print each flag shape.
func TestDetectACPArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLIs are POSIX")
	}
	mk := func(help string) string {
		p := filepath.Join(t.TempDir(), "cli")
		if err := os.WriteFile(p, []byte("#!/bin/sh\ncat <<'EOF'\n"+help+"\nEOF\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct{ help, want string }{
		{"Usage: foo --acp --stdio    run the agent client protocol over stdio", "--acp --stdio"},
		{"Usage: foo --acp            run the agent client protocol", "--acp"},
		{"Commands:\n  acp    start the agent client protocol server", "acp"},
		{"Usage: foo --help           just a normal CLI, no protocol here", ""},
	}
	// An explicit generous deadline: spawning a shell fake can take seconds on a loaded machine, and a
	// probe that runs out of time returns empty output, which this heuristic cannot distinguish from
	// "no ACP flag" — that made the test fail spuriously under load (observed at 7.26s against the
	// former hardcoded 5s cap). runHelp now honors the caller's deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, tc := range cases {
		got := strings.Join(DetectACPArgs(ctx, mk(tc.help)), " ")
		if got != tc.want {
			t.Errorf("DetectACPArgs for %q = %q, want %q", tc.help, got, tc.want)
		}
	}
}
