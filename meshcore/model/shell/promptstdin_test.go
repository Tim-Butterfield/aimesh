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

// TestPromptOnStdin_CarriesAWorkspaceSizedPrompt is the regression test for a ceiling that was
// never the model's and never memory's: argv.
//
// `execve` bounds the whole argument vector, so a prompt passed as an argument is capped by the
// OPERATING SYSTEM — measured darwin/arm64 (ARG_MAX 1 MiB): 512 KiB execs, 1 MiB fails with
// "argument list too long", and Linux caps a single argument lower still (MAX_ARG_STRLEN,
// typically 128 KiB). The failure lands at exec time with EMPTY stderr, because the provider
// never starts, so there is nothing for clihint to classify either.
//
// This mattered the moment the collector's coverage caps came off: a real workspace produces a
// multi-megabyte prompt, and every provider recipe passed it as argv. The fake adapter never
// execs, so no existing test could have caught it.
//
// /bin/cat is the stand-in CLI: it echoes stdin, so a byte-exact round trip proves the whole
// prompt reached the process.
func TestPromptOnStdin_CarriesAWorkspaceSizedPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/cat as a stand-in CLI")
	}
	const size = 6 << 20 // the measured payload of a real review of this repo, and 6x ARG_MAX
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
	// cappedBuffer bounds what we CAPTURE, which is a separate concern from what we SEND; assert
	// on what arrived rather than on the full length.
	if got := len(res.Stdout); got == 0 || !strings.HasPrefix(string(res.Stdout), "xxxx") {
		t.Fatalf("the prompt did not reach the process: %d bytes back", got)
	}
}

// TestPromptOnArgv_HitsTheOSCeiling pins the reason the field exists. If this ever stops failing,
// the platform changed — not the argument that a prompt does not belong in argv.
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
	// The shape that makes this hard to diagnose in the field, pinned so it is not a surprise:
	// the CLI never ran, so there is no stderr for clihint to classify.
	if len(res.Stderr) != 0 {
		t.Errorf("expected empty stderr from a failed exec, got %q", res.Stderr)
	}
}

// TestPromptOnArgv_RefusesAnOptionLikePrompt pins the guard that argv-passing recipes need and that
// no prompt builder can be trusted to maintain from another package.
//
// A prompt beginning with `-` reaches an argv recipe's flag parser as an OPTION. Measured
// 2026-08-30, a `-----` ballot header cost a live shortlist run 3 of 5 seats (devin-cli exited 2 on
// its usage, cursor-cli exited 1) while the stdin recipes answered normally — a partial panel, a
// failed quorum, and a withheld ranking, with nothing in any single lane to say why.
//
// The guard converts that into one refusal before spend. It is deliberately NOT a normalization:
// silently rewriting a caller's prompt would hide the same defect one layer down.
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

	// A stdin recipe is UNAFFECTED: the constraint belongs to argv, not to prompts. Same prompt,
	// same guard, no refusal — /bin/cat echoes it back.
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
