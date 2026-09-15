package shell

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// A workspace-sized prompt reaches the process on stdin. As an argument it would hit the OS argv
// limit (ARG_MAX is 1 MiB on darwin/arm64; Linux limits one argument to MAX_ARG_STRLEN, typically
// 128 KiB), failing at exec time with empty stderr. /bin/cat stands in for the CLI and echoes stdin.
func TestPromptOnStdin_CarriesAWorkspaceSizedPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/cat as a stand-in CLI")
	}
	const size = 6 << 20 // six times darwin's ARG_MAX
	prompt := strings.Repeat("x", size)

	a := New(Recipe{
		Name: "stdin-probe", Detect: "cat", Evidence: core.EvidenceNone,
		BuildArgs:     func(model.Call) []string { return nil },
		PromptOnStdin: true,
	}, "/bin/cat", 60*time.Second)

	res, err := a.Invoke(context.Background(), model.Call{Prompt: prompt})
	if err != nil {
		t.Fatalf("a %d-byte prompt on stdin failed: %v", size, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	// cappedBuffer bounds what is captured, not what is sent, so assert on what arrived rather than
	// on the full length.
	if got := len(res.Stdout); got == 0 || !strings.HasPrefix(string(res.Stdout), "xxxx") {
		t.Fatalf("the prompt did not reach the process: %d bytes back", got)
	}
}

// A 6 MiB argv element fails to exec, which is why PromptOnStdin exists.
func TestPromptOnArgv_HitsTheOSCeiling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/cat as a stand-in CLI")
	}
	prompt := strings.Repeat("x", 6<<20)

	a := New(Recipe{
		Name: "argv-probe", Detect: "cat", Evidence: core.EvidenceNone,
		BuildArgs: func(c model.Call) []string { return []string{c.Prompt} },
	}, "/bin/cat", 60*time.Second)

	res, err := a.Invoke(context.Background(), model.Call{Prompt: prompt})
	if err == nil {
		t.Fatal("a 6 MiB argv element execed; the ceiling this field works around is gone")
	}
	if !strings.Contains(err.Error(), "argument list too long") {
		t.Errorf("unexpected failure: %v", err)
	}
	// The CLI never ran, so there is no stderr for clihint to classify.
	if len(res.Stderr) != 0 {
		t.Errorf("expected empty stderr from a failed exec, got %q", res.Stderr)
	}
}

// An argv recipe refuses a prompt that begins with `-`, which the CLI's flag parser would read as an
// option. The refusal happens before spend, and the prompt is never rewritten.
func TestPromptOnArgv_RefusesAnOptionLikePrompt(t *testing.T) {
	argv := Recipe{
		Name: "argv-probe", Detect: "cat", Evidence: core.EvidenceNone,
		BuildArgs: func(c model.Call) []string { return []string{c.Prompt} },
	}
	// No binary is configured and none needs to be: the refusal precedes binary resolution, so it
	// reports the real cause instead of "cat not found" on a machine that happens to lack it.
	a := New(argv, "/nonexistent/cat", 60*time.Second)

	res, err := a.Invoke(context.Background(), model.Call{Prompt: "----- FROZEN DECISION INPUTS -----\nrank these"})
	if err == nil {
		t.Fatal("an option-like prompt was accepted by an argv recipe; the guard is gone")
	}
	if res.ExitCode != 126 {
		t.Errorf("want exit 126 (declined before spawn), got %d", res.ExitCode)
	}
	// The operator has to be able to act on this without reading adapter source.
	for _, want := range []string{"argv-probe", "parsed by the CLI as an option", "PromptOnStdin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must explain itself; missing %q in: %v", want, err)
		}
	}

	// A stdin recipe is unaffected: the same prompt is accepted and /bin/cat echoes it back.
	if runtime.GOOS != "windows" {
		stdin := argv
		stdin.Name, stdin.PromptOnStdin = "stdin-probe", true
		stdin.BuildArgs = func(model.Call) []string { return nil }
		res, err := New(stdin, "/bin/cat", 60*time.Second).
			Invoke(context.Background(), model.Call{Prompt: "----- still fine on stdin -----"})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("a stdin recipe must not be caught by an argv constraint: exit=%d err=%v", res.ExitCode, err)
		}
		if !strings.HasPrefix(string(res.Stdout), "-----") {
			t.Errorf("the dash-leading prompt must reach the process intact, got %q", res.Stdout)
		}
	}

	// An ordinary prompt still runs: the guard must not have become a blanket refusal.
	if runtime.GOOS != "windows" {
		res, err := New(argv, "/bin/echo", 60*time.Second).
			Invoke(context.Background(), model.Call{Prompt: "rank these candidates"})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("a normal argv prompt must still run: exit=%d err=%v", res.ExitCode, err)
		}
	}
}
