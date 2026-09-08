package run

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// shellOK / shellFail are the smallest portable commands that succeed and fail. The verification path
// runs through the platform shell on purpose (an operator's command line has pipes and chains in it),
// so the tests exercise that same shell rather than a synthetic stub.
func shellOK() string   { return "exit 0" }
func shellFail() string { return "exit 3" }

func verifyDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "marker.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestVerify_RecordsWhatHappened: exit status, the output tail, and the working directory.
func TestVerify_RecordsWhatHappened(t *testing.T) {
	dir := verifyDir(t)
	res := runVerifyPass(context.Background(), dir, []string{shellOK(), shellFail(), "echo hello-from-copy"}, time.Minute)
	if len(res) != 3 {
		t.Fatalf("results = %d, want 3", len(res))
	}
	if !res[0].OK || res[0].ExitCode != 0 {
		t.Errorf("a succeeding command = %+v", res[0])
	}
	if res[1].OK || res[1].ExitCode != 3 {
		t.Errorf("a failing command must record its EXIT CODE, not merely that it failed: %+v", res[1])
	}
	if !strings.Contains(res[2].Output, "hello-from-copy") {
		t.Errorf("output was not captured: %q", res[2].Output)
	}
	for _, r := range res {
		if r.DurationMs < 0 {
			t.Errorf("duration = %d", r.DurationMs)
		}
	}
}

// TestVerify_RunsInTheDirectoryItIsGiven. The whole safety argument rests on this: the commands run in
// the containment copy, never in the live tree. A cwd bug here would run the user's build in their
// working directory.
func TestVerify_RunsInTheDirectoryItIsGiven(t *testing.T) {
	dir := verifyDir(t)
	cmd := "cat marker.txt"
	if runtime.GOOS == "windows" {
		cmd = "type marker.txt"
	}
	res := runVerifyPass(context.Background(), dir, []string{cmd}, time.Minute)
	if !res[0].OK || !strings.Contains(res[0].Output, "hi") {
		t.Fatalf("the command did not run in the directory it was given: %+v", res[0])
	}
}

// TestVerify_EveryCommandRunsEvenAfterAFailure. Stopping at the first failure would save wall clock
// and cost the feature its point: the two passes must describe the same command list or the
// per-command delta is not computable, and "the build broke so we never learned about the tests" is a
// report a reader would have to guess at.
func TestVerify_EveryCommandRunsEvenAfterAFailure(t *testing.T) {
	res := runVerifyPass(context.Background(), verifyDir(t), []string{shellFail(), shellOK()}, time.Minute)
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2 — a failure must not stop the pass", len(res))
	}
	if res[0].OK || !res[1].OK {
		t.Errorf("results = %+v", res)
	}
}

// TestVerify_ATimeoutIsNotAFailure. A wall clock is not a defect in the code, so a timeout is its own
// field and makes the two passes NOT COMPARABLE rather than red. Folding it into "failed" would be the
// easiest way for this feature to lie.
func TestVerify_ATimeoutIsNotAFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no portable sub-second sleep in cmd.exe")
	}
	res := runVerifyPass(context.Background(), verifyDir(t), []string{"sleep 30"}, 150*time.Millisecond)
	if !res[0].TimedOut {
		t.Fatalf("a command past its budget must be marked timedOut: %+v", res[0])
	}
	if res[0].OK {
		t.Error("a timed-out command did not pass")
	}
	// And the delta refuses to compare, rather than blaming the change.
	before := []review.VerificationResult{{Command: "x", OK: true}}
	if got := verificationDelta(before, res); got != DeltaNotComparable {
		t.Errorf("delta = %q, want %q — a timeout must not be reported as broken", got, DeltaNotComparable)
	}
}

// TestVerificationDelta_TheSignalIsTheDeltaNotAbsoluteGreen is the rule that decides whether this
// feature is usable on real repositories. A red suite before AND after is an ORDINARY outcome: the
// change did not make things worse. A tool that demanded green-before would refuse to run on exactly
// the repositories most in need of review.
func TestVerificationDelta_TheSignalIsTheDeltaNotAbsoluteGreen(t *testing.T) {
	r := func(ok ...bool) []review.VerificationResult {
		out := make([]review.VerificationResult, 0, len(ok))
		for i, o := range ok {
			out = append(out, review.VerificationResult{Command: string(rune('a' + i)), OK: o})
		}
		return out
	}
	cases := []struct {
		name          string
		before, after []review.VerificationResult
		want          string
	}{
		{"green stays green", r(true, true), r(true, true), DeltaUnchangedPass},
		{"red stays red is ORDINARY", r(false, true), r(false, true), DeltaUnchangedFail},
		{"the change fixed something", r(false), r(true), DeltaFixed},
		{"the change broke something", r(true, true), r(true, false), DeltaBroken},
		{"broken wins over fixed when both happen", r(true, false), r(false, true), DeltaBroken},
		{"no after pass at all", r(true), nil, DeltaBaselineOnly},
		{"different command counts", r(true), r(true, true), DeltaNotComparable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verificationDelta(tc.before, tc.after); got != tc.want {
				t.Errorf("delta = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerify_OutputIsBoundedAndTheTailIsKept. The tail is where a build tool puts its error; a clipped
// HEAD that read like a whole log would be worse than either.
func TestVerify_OutputIsBoundedAndTheTailIsKept(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no portable bulk-output one-liner in cmd.exe")
	}
	// Print far more than the bound, ending with a marker.
	cmd := "for i in $(seq 1 4000); do echo 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; done; echo THE-ACTUAL-ERROR"
	res := runVerifyPass(context.Background(), verifyDir(t), []string{cmd}, time.Minute)
	out := res[0].Output
	if len(out) > maxVerifyOutput+64 {
		t.Errorf("output is %d bytes, past the %d bound", len(out), maxVerifyOutput)
	}
	if !strings.Contains(out, "THE-ACTUAL-ERROR") {
		t.Error("the TAIL must survive the clip — that is where the error is")
	}
	if !strings.Contains(out, "clipped") {
		t.Error("a clipped log must SAY it was clipped, or it reads like a complete one")
	}
}

// TestVerify_AnUnstartableCommandIsAHostFact, not a red suite. The distinction matters because one is
// about the project and the other is about this machine.
func TestVerify_AnUnstartableCommandIsAHostFact(t *testing.T) {
	res := runVerifyPass(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"), []string{shellOK()}, time.Minute)
	r := res[0]
	if r.OK {
		t.Fatal("a command in a missing directory did not fail")
	}
	if r.Unstartable == "" && r.ExitCode == 0 {
		t.Errorf("an unstartable command must be recorded as such: %+v", r)
	}
}

// TestValidateVerifyCommands_CeilingAndBlanks. The ceiling is a spend/wall-clock bound of the same kind
// as the panel-seat cap: each command is a real process, run twice on a write run.
func TestValidateVerifyCommands_CeilingAndBlanks(t *testing.T) {
	got, err := ValidateVerifyCommands([]string{" make test ", "", "   ", "go build ./..."})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(got) != 2 || got[0] != "make test" {
		t.Errorf("blank entries must be dropped and the rest trimmed: %q", got)
	}
	over := make([]string, MaxVerifyCommands+1)
	for i := range over {
		over[i] = "true"
	}
	if _, err := ValidateVerifyCommands(over); err == nil {
		t.Fatalf("more than %d commands must be refused", MaxVerifyCommands)
	}
}

// TestVerify_BuildOutputNeverReachesTheLiveTree is the most important test in this file, and it is
// the one that decided where both passes run.
//
// `Access.CommitExpecting` re-enumerates the WHOLE containment copy and writes back every file that
// differs from live. A verification pass inside the copy the commit is derived from would therefore
// commit its own object files, binaries and coverage directories into the user's repository — and no
// denylist would catch that reliably, because build artifacts look exactly like source. The fix is
// structural: the baseline runs on a throwaway copy, and the after-pass runs only once the commit is
// already done.
//
// This drives a real apply through the governed write path with commands that WRITE, and requires the
// live tree to be free of them.
func TestVerify_BuildOutputNeverReachesTheLiveTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's command syntax is POSIX")
	}
	m, ws := remediateFixture(t, true)
	req := baseRequest(t, ws, review.ModeApply)
	// Two artifacts with the names a real toolchain would produce, written by BOTH passes — so the
	// test fails if either the baseline or the after-pass runs where the commit can see it.
	req.VerifyCommands = []string{"echo compiled > build-output.o && mkdir -p node_modules && echo x > node_modules/pkg.json"}
	req.VerifyTimeout = time.Minute

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if !out.Receipt.Committed {
		t.Fatalf("the fixture must actually commit, or this test proves nothing: %+v", out.Receipt)
	}
	// NON-VACUITY, and it is essential here: if the commands never ran, no artifact would exist and
	// the assertions below would pass while proving nothing at all. Both passes must have run and
	// succeeded — which means both really did create the files the live tree must not have.
	v := out.Verification
	if v == nil || len(v.Before) != 1 || len(v.After) != 1 || !v.Before[0].OK || !v.After[0].OK {
		t.Fatalf("both passes must have run and written their artifacts, or this test is vacuous: %+v", v)
	}
	for _, leaked := range []string{"build-output.o", filepath.Join("node_modules", "pkg.json")} {
		if _, serr := os.Stat(filepath.Join(ws, leaked)); serr == nil {
			t.Errorf("BUILD OUTPUT REACHED THE USER'S TREE: %s. A verification pass must never run where the commit can see it", leaked)
		}
	}
}

// TestVerify_RunsBothPassesOnAnApplyAndRecordsTheDelta: the wiring, end to end.
func TestVerify_RunsBothPassesOnAnApplyAndRecordsTheDelta(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := baseRequest(t, ws, review.ModeApply)
	req.VerifyCommands = []string{shellOK()}
	req.VerifyTimeout = time.Minute

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	v := out.Verification
	if v == nil {
		t.Fatal("an apply run with commands must carry the verification record — the pass is not wired in")
	}
	if len(v.Before) != 1 || len(v.After) != 1 {
		t.Fatalf("both passes must run on a write: %+v", v)
	}
	if v.Delta != DeltaUnchangedPass {
		t.Errorf("delta = %q, want %q", v.Delta, DeltaUnchangedPass)
	}
	if v.Note != VerificationNote {
		t.Error("the record must carry the fixed note")
	}
	// And it is written to the run directory, so the fact survives the process.
	if b, rerr := os.ReadFile(filepath.Join(out.RunDir, "remediation", "verification.json")); rerr != nil || !strings.Contains(string(b), DeltaUnchangedPass) {
		t.Errorf("verification.json: err=%v body=%s", rerr, string(b))
	}
}

// TestVerify_AFailingSuiteCommitsAnyway is the invariant, asserted where it would actually be broken.
//
// A red suite is the single most tempting thing to gate on, and gating is exactly what task #41
// removed for model identity. The commands here fail on BOTH passes and the run must be unaffected:
// committed, no halt, no changed exit path, and every finding still in place.
func TestVerify_AFailingSuiteCommitsAnyway(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := baseRequest(t, ws, review.ModeApply)
	req.VerifyCommands = []string{shellFail()}
	req.VerifyTimeout = time.Minute

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("a failing verification command must not halt the run: %v", err)
	}
	if !out.Receipt.Committed {
		t.Fatal("a failing verification command BLOCKED THE COMMIT — it is recorded, never enforced")
	}
	if out.Verification.Delta != DeltaUnchangedFail {
		t.Errorf("delta = %q, want %q — red before and after is an ordinary outcome", out.Verification.Delta, DeltaUnchangedFail)
	}
	if out.Applied() == 0 {
		t.Error("a failing suite reduced what was applied")
	}
}

// TestVerify_OffByDefault. A run that named no command executes nothing and records nothing, so the
// default is byte-identical to a build without the feature.
func TestVerify_OffByDefault(t *testing.T) {
	m, ws := remediateFixture(t, true)
	out, err := m.Remediate(context.Background(), baseRequest(t, ws, review.ModeApply))
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if out.Verification != nil {
		t.Errorf("a run with no --verify-cmd must carry no verification record: %+v", out.Verification)
	}
}

// TestVerificationReport_CarriesTheNote. Both misreadings of this block run in opposite directions —
// green invites "the change is fine", red invites "the findings were wrong" — so the sentence that
// says neither follows travels in the artifact rather than in each surface's prose.
func TestVerificationReport_CarriesTheNote(t *testing.T) {
	rep := newVerificationReport("/tmp/copy", []string{"make test"},
		[]review.VerificationResult{{Command: "make test", OK: true}},
		[]review.VerificationResult{{Command: "make test", OK: true}})
	if rep.Note != VerificationNote {
		t.Error("the report must carry the fixed note")
	}
	for _, phrase := range []string{"containment copy", "does NOT", "did not block"} {
		if !strings.Contains(rep.Note, phrase) {
			t.Errorf("the note must state %q: %s", phrase, rep.Note)
		}
	}
	if rep.Delta != DeltaUnchangedPass || len(rep.Commands) != 1 {
		t.Errorf("report = %+v", rep)
	}
}
