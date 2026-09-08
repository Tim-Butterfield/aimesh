package shell

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// These tests are HERMETIC: every "CLI" is a script this test writes, so nothing real is launched and
// nothing is spent. They cover the three outcomes the deep probe exists to tell apart —
//
//	(a) works-in-a-throwaway-directory
//	(b) refuses-an-untrusted-directory
//	(c) prompts-and-hangs
//
// — and, in (c), the rule that matters more than any classification: NOTHING IS AUTO-ANSWERED. The
// fake records every byte it receives on stdin; the assertion is that it received none.

// fakeCLI writes an executable script and returns its path.
func fakeCLI(t *testing.T, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake CLIs are POSIX shell scripts")
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// probeAdapter builds a shell adapter over a fake binary with a minimal recipe.
func probeAdapter(t *testing.T, bin string) *Adapter {
	t.Helper()
	return New(Recipe{
		Name:      "fake-cli",
		Detect:    filepath.Base(bin),
		BuildArgs: func(c model.Call) []string { return []string{"-p", c.Prompt} },
	}, bin, 60*time.Second)
}

// (a) WORKS IN A THROWAWAY DIRECTORY — and the directory really is a throwaway that resembles a run:
// the fake records its own cwd and that directory's contents.
func TestProbeDeep_WorksInAnIsolatedDirectory(t *testing.T) {
	cwdFile := filepath.Join(t.TempDir(), "cwd")
	lsFile := filepath.Join(t.TempDir(), "ls")
	t.Setenv("AIMESH_TEST_CWD", cwdFile)
	t.Setenv("AIMESH_TEST_LS", lsFile)
	bin := fakeCLI(t, "works", `
pwd > "$AIMESH_TEST_CWD"
ls -a > "$AIMESH_TEST_LS"
echo "AIMESH-DEEP-PROBE-OK"
`)
	a := probeAdapter(t, bin)

	res := a.ProbeDeep(context.Background(), model.DeepProbeSpec{ModelArg: "m1", Timeout: 30 * time.Second})
	if !res.OK {
		t.Fatalf("a CLI that answers must probe OK, got %+v", res)
	}
	if res.Stage != "invoke" {
		t.Errorf("stage = %q, want invoke (a real call was made, not a --version)", res.Stage)
	}
	if !strings.Contains(res.Detail, "token was echoed") {
		t.Errorf("detail should report the echoed probe token: %q", res.Detail)
	}
	if res.Signal != "" {
		t.Errorf("a working CLI must carry no blocker signal, got %q", res.Signal)
	}

	// The invocation ran somewhere that is NOT this process's cwd — the whole premise of the probe.
	ranIn := strings.TrimSpace(readFile(t, cwdFile))
	if ranIn == "" {
		t.Fatal("the fake CLI never recorded a working directory")
	}
	here, _ := os.Getwd()
	if ranIn == here {
		t.Errorf("the deep probe ran in the process cwd (%s); it must run in a fresh isolated directory", here)
	}
	// And that directory RESEMBLES A RUN: it holds a file, and (when git exists) it is a repository —
	// which is the difference between probing what a run does and probing an empty void.
	listing := readFile(t, lsFile)
	if !strings.Contains(listing, "README.md") {
		t.Errorf("the isolated probe directory must contain a representative file, got:\n%s", listing)
	}
	if _, err := os.Stat("/usr/bin/git"); err == nil {
		if !strings.Contains(listing, ".git") {
			t.Errorf("the isolated probe directory should be git-initialized so a trust-gating CLI is not failed by the probe's own austerity, got:\n%s", listing)
		}
	}
}

// (b) REFUSES AN UNTRUSTED DIRECTORY — exactly the `gemini-cli` shape: a non-zero exit whose only
// evidence is a line on stderr. The probe must classify it as folder_trust and report the HUMAN fix.
func TestProbeDeep_RefusesUntrustedDirectoryIsClassifiedWithTheHumanFix(t *testing.T) {
	bin := fakeCLI(t, "refuses", `
echo "Not inside a trusted directory and --skip-git-repo-check was not specified" >&2
exit 55
`)
	a := probeAdapter(t, bin)

	res := a.ProbeDeep(context.Background(), model.DeepProbeSpec{ModelArg: "m1", Timeout: 30 * time.Second})
	if res.OK {
		t.Fatalf("a CLI that refuses the directory must NOT probe OK: %+v", res)
	}
	if res.Signal != clihint.FolderTrust {
		t.Errorf("signal = %q, want %q", res.Signal, clihint.FolderTrust)
	}
	if !strings.Contains(res.Detail, "55") {
		t.Errorf("the detail must name the exit code the CLI actually gave: %q", res.Detail)
	}
	// The fix is a HUMAN step, and it says so.
	for _, must := range []string{"Nothing was auto-answered", "FIX:", "fresh isolated copy"} {
		if !strings.Contains(res.Detail, must) {
			t.Errorf("the detail must state %q; got:\n%s", must, res.Detail)
		}
	}
}

// (c) PROMPTS AND HANGS — the case that must never be "helped". The fake copies every byte it reads
// on stdin into a file and then blocks. The probe must time out, classify, and leave that file EMPTY.
func TestProbeDeep_PromptsAndHangsIsNeverAutoAnswered(t *testing.T) {
	stdinFile := filepath.Join(t.TempDir(), "stdin-capture")
	t.Setenv("AIMESH_TEST_STDIN", stdinFile)
	bin := fakeCLI(t, "prompts", `
printf 'Do you trust the files in this folder? [y/N] ' >&2
cat > "$AIMESH_TEST_STDIN"
sleep 30
`)
	a := probeAdapter(t, bin)

	start := time.Now()
	res := a.ProbeDeep(context.Background(), model.DeepProbeSpec{ModelArg: "m1", Timeout: 2 * time.Second})
	if res.OK {
		t.Fatalf("a CLI blocked on a prompt must NOT probe OK: %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("the deep probe must be bounded; it took %s", elapsed)
	}
	if !strings.Contains(res.Detail, "timed out") {
		t.Errorf("the detail must say it timed out: %q", res.Detail)
	}
	// The prompt text is a folder-trust signature, so it is classified rather than reported as a
	// mystery hang — that is the difference from the cheap probe, which can only guess.
	if res.Signal != clihint.FolderTrust {
		t.Errorf("signal = %q, want %q (the prompt itself is the evidence)", res.Signal, clihint.FolderTrust)
	}
	if !strings.Contains(res.Detail, "Nothing was auto-answered") {
		t.Errorf("the detail must state that nothing was answered on the operator's behalf: %q", res.Detail)
	}

	// THE RULE: not one byte was written to the CLI's stdin. A probe that answered "y" here would be
	// granting folder trust on the operator's behalf — the exact consent the prompt exists to collect.
	b, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("the fake CLI never got far enough to record its stdin: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("the deep probe wrote %d byte(s) to the CLI's stdin (%q) — it must NEVER answer a prompt", len(b), b)
	}
}

// A clean exit with NOTHING to show is not readiness. This is the other half of the gemini-cli lesson:
// "the process ended without complaining" and "the CLI did real work" are different claims.
func TestProbeDeep_SilentSuccessIsNotReadiness(t *testing.T) {
	bin := fakeCLI(t, "silent", `exit 0`)
	a := probeAdapter(t, bin)

	res := a.ProbeDeep(context.Background(), model.DeepProbeSpec{ModelArg: "m1", Timeout: 30 * time.Second})
	if res.OK {
		t.Fatalf("exit 0 with no answer must not be reported as ready: %+v", res)
	}
	if !strings.Contains(res.Detail, "NO answer") {
		t.Errorf("the detail must say the CLI produced no answer: %q", res.Detail)
	}
}

// A login blocker is classified separately and gets its own fix — the fix a human performs, never one
// this process performs for them.
func TestProbeDeep_LoginRequiredIsItsOwnClassification(t *testing.T) {
	bin := fakeCLI(t, "needslogin", `
echo "You are not signed in. Please log in first." >&2
exit 1
`)
	a := probeAdapter(t, bin)

	res := a.ProbeDeep(context.Background(), model.DeepProbeSpec{ModelArg: "m1", Timeout: 30 * time.Second})
	if res.OK || res.Signal != clihint.LoginRequired {
		t.Fatalf("want a login_required refusal, got %+v", res)
	}
	if !strings.Contains(res.Detail, "login command in your terminal") {
		t.Errorf("the fix must be the operator's own login step: %q", res.Detail)
	}
}

// A missing binary never reaches the isolated directory: it fails at `resolve`, exactly like the cheap
// probe, so the two report the same stage for the same problem.
func TestProbeDeep_MissingBinaryFailsAtResolve(t *testing.T) {
	a := New(Recipe{
		Name: "fake-cli", Detect: "definitely-not-on-path-aimesh",
		BuildArgs: func(c model.Call) []string { return nil },
	}, "", 60*time.Second)
	res := a.ProbeDeep(context.Background(), model.DeepProbeSpec{})
	if res.OK || res.Stage != "resolve" {
		t.Fatalf("want an unresolved binary at stage resolve, got %+v", res)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}
