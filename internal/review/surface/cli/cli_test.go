package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// TestPrintIdentityCaveats proves the CLI review summary surfaces identity caveats (the run PASSED but
// a lane's model was weak/self-reported or not verified) — including the reported model for a weak
// signal — and prints nothing when there are none.
func TestPrintIdentityCaveats(t *testing.T) {
	var buf bytes.Buffer
	printIdentityCaveats(&buf, []review.IdentityCaveat{
		{Role: "reviewer", Adapter: "devin-cli", RequestedModel: "claude-opus-4-8-medium", Status: review.VerifUnknown, ReportedModel: ""},
		{Role: "verifier", Adapter: "agy-cli", RequestedModel: "gemini-3-1-pro-high", Status: review.VerifSelfReported, ReportedModel: "Gemini 3.1 Pro"},
	})
	out := buf.String()
	if !strings.Contains(out, "identity caveats (2)") {
		t.Errorf("summary must count caveats: %q", out)
	}
	if !strings.Contains(out, "reviewer lane") || !strings.Contains(out, "not verified") || !strings.Contains(out, "not reported") {
		t.Errorf("unknown caveat line missing detail: %q", out)
	}
	if !strings.Contains(out, "weak · self-reported") || !strings.Contains(out, "Gemini 3.1 Pro") {
		t.Errorf("self-report caveat must show weak state + the reported model: %q", out)
	}
	var empty bytes.Buffer
	printIdentityCaveats(&empty, nil)
	if empty.Len() != 0 {
		t.Errorf("no caveats must print nothing, got %q", empty.String())
	}
}

func TestHelp_IncludesSetupPathCapture(t *testing.T) {
	_, out, _ := run(t, "--help")
	if !strings.Contains(out, "setup --adapter") || !strings.Contains(out, "--path") {
		t.Errorf("help should document setup path capture:\n%s", out)
	}
}

func TestSetup_UserScopeWritesToInjectedHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AIMESH_HOME", home) // never touch the real home
	code, _, errs := run(t, "setup", "--scope", "user")
	if code != 0 {
		t.Fatalf("setup --scope user exit = %d (%s)", code, errs)
	}
	if _, err := os.Stat(filepath.Join(home, ".aimesh", "review", "config.yaml")); err != nil {
		t.Errorf("user-scope config not written to injected home: %v", err)
	}
}

func TestSetup_InteractiveRejectsConflictingFlags(t *testing.T) {
	code, _, errs := run(t, "setup", "--interactive", "--adapter", "claude-code", "--path", "/x")
	if code != int(fault.Usage) {
		t.Fatalf("code = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errs, "--interactive") {
		t.Errorf("guidance should mention --interactive: %q", errs)
	}
}

func TestHelp_IncludesInteractiveSetup(t *testing.T) {
	_, out, _ := run(t, "--help")
	if !strings.Contains(out, "setup --interactive") {
		t.Errorf("help should document the interactive wizard:\n%s", out)
	}
}

func TestDoctor_InteractiveRequiresFix(t *testing.T) {
	code, _, errs := run(t, "doctor", "--interactive")
	if code != int(fault.Usage) {
		t.Fatalf("code = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errs, "--interactive") {
		t.Errorf("guidance should mention --interactive: %q", errs)
	}
}

func TestSetup_PromoteRequiresProjectScope(t *testing.T) {
	code, _, errs := run(t, "setup", "--from", "user") // scope defaults to user
	if code != int(fault.Usage) {
		t.Fatalf("code = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errs, "--scope project") {
		t.Errorf("guidance should require --scope project: %q", errs)
	}
}

func TestSetup_PromoteRejectsUnknownFrom(t *testing.T) {
	code, _, errs := run(t, "setup", "--scope", "project", "--from", "bogus")
	if code != int(fault.Usage) {
		t.Fatalf("code = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errs, "--from") {
		t.Errorf("guidance should mention --from: %q", errs)
	}
}

func TestSetup_YesRequiresFrom(t *testing.T) {
	code, _, errs := run(t, "setup", "--yes")
	if code != int(fault.Usage) {
		t.Fatalf("code = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errs, "--from") {
		t.Errorf("guidance should mention --from: %q", errs)
	}
}

// chdirTo enters dir for the test and restores the original cwd on cleanup. Unlike
// t.Chdir, the restore uses the absolute saved path so it succeeds on Windows even when
// dir is a t.TempDir slated for removal: Windows cannot delete the process's current
// directory, and testing.Chdir's fd-relative restore (oldwd opened as ".") is a no-op
// there, which would leave cwd inside dir and fail both the RemoveAll and the restore.
func chdirTo(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func TestSetup_DefaultScopeIsUserNotProject(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("AIMESH_HOME", home)    // never touch the real home
	chdirTo(t, proj)                 // project scope would resolve to cwd
	code, _, errs := run(t, "setup") // no --scope → defaults to user/global
	if code != 0 {
		t.Fatalf("default setup exit = %d (%s)", code, errs)
	}
	if _, err := os.Stat(filepath.Join(home, ".aimesh", "review", "config.yaml")); err != nil {
		t.Errorf("default setup must write user/global config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".aimesh", "review", "config.yaml")); !os.IsNotExist(err) {
		t.Error("default setup must NOT create a project config")
	}
}

func TestSetup_ProjectScopeExplicitOnly(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	chdirTo(t, proj)
	code, _, errs := run(t, "setup", "--scope", "project")
	if code != 0 {
		t.Fatalf("setup --scope project exit = %d (%s)", code, errs)
	}
	if _, err := os.Stat(filepath.Join(proj, ".aimesh", "review", "config.yaml")); err != nil {
		t.Errorf("project-scope config not written to cwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".aimesh", "review", "config.yaml")); !os.IsNotExist(err) {
		t.Error("project scope must NOT touch user/global config")
	}
}

func TestSetup_RejectsUnknownScope(t *testing.T) {
	code, _, errs := run(t, "setup", "--scope", "bogus")
	if code != int(fault.Usage) {
		t.Errorf("unknown scope exit = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errs, "--scope") {
		t.Errorf("guidance should mention --scope: %q", errs)
	}
}

func TestSetup_PathRequiresAdapter(t *testing.T) {
	code, _, errs := run(t, "setup", "--path", "/bin/claude")
	if code != int(fault.Usage) {
		t.Errorf("--path without --adapter exit = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errs, "--adapter") {
		t.Errorf("guidance should mention --adapter: %q", errs)
	}
}

func TestSetup_AdapterRequiresPath(t *testing.T) {
	code, _, _ := run(t, "setup", "--adapter", "claude-code")
	if code != int(fault.Usage) {
		t.Errorf("--adapter without --path exit = %d, want %d", code, fault.Usage)
	}
}

func TestGitignore_NoAikitOrAnalysis(t *testing.T) {
	// Walk to the repo root (marked by go.work) — the reviewmesh app is now a nested
	// module (reviewmesh/go.mod), so go.mod no longer identifies the repo root.
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("repo root (go.work) not found from test cwd")
		}
		dir = parent
	}
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	s := string(b)
	if strings.Contains(s, ".aikit") {
		t.Error(".gitignore must not contain .aikit/ (managed via .git/info/exclude)")
	}
	if strings.Contains(s, "Analysis.md") {
		t.Error(".gitignore must not contain Analysis.md (it is deleted, not ignored)")
	}
}

func TestDiagSuffix(t *testing.T) {
	if got := diagSuffix(fault.New(fault.Model, "x").WithHalt("E")); got != " (Class E)" {
		t.Errorf("Class suffix = %q", got)
	}
	if got := diagSuffix(fault.New(fault.Containment, "x").WithHalt("M5")); got != " (M5)" {
		t.Errorf("mechanical suffix = %q", got)
	}
	if got := diagSuffix(errors.New("plain")); got != "" {
		t.Errorf("non-fault suffix = %q", got)
	}
}

// TestCLI_IdentityMismatchSucceedsAndTellsTheUser: the CLI exits 0 on an identity mismatch and SAYS SO
// in the human output. The user is the one who decides what an unexpected model means for their review;
// the tool's job is to make sure they cannot miss it.
func TestCLI_IdentityMismatchSucceedsAndTellsTheUser(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir()) // hermetic: never read the real home config
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", t.TempDir())
	t.Setenv("REVIEWMESH_FAKE_SCENARIO", "identity_mismatch")
	// The shipped `default` profile ships unconfigured; select the hidden fake profile to exercise
	// the deterministic fake adapter end-to-end.
	code, out, errs := run(t, "review", "--report", "--profile", "fake-smoke", ciWorkspace(t))
	if code != 0 {
		t.Errorf("exit = %d, want 0 — an identity mismatch is a caveat, not a failure; stderr=%q", code, errs)
	}
	if !strings.Contains(out+errs, "identity") {
		t.Errorf("the mismatch must be surfaced to the user, got stdout=%q stderr=%q", out, errs)
	}
}

// TestReview_FakeSmokeProfileIsEnvGated pins the hidden-internal posture of the fake harness: the
// shipped-but-hidden `fake-smoke` profile is NOT user-selectable — without AIMESH_INTERNAL_FAKE=1 it
// fails resolution with the same clean unknown-profile config error as any unrecognized name (no hint
// that it exists); with the gate it resolves and runs end-to-end.
func TestReview_FakeSmokeProfileIsEnvGated(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir()) // hermetic: never read the real home config
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", t.TempDir())
	t.Setenv("REVIEWMESH_FAKE_SCENARIO", "empty")
	ws := ciWorkspace(t)

	// WITHOUT the gate (Run called directly — the run() helper unlocks it): unknown profile.
	t.Setenv(fake.EnvVar, "") // explicitly NOT set: a user never configures the internal gate
	var o, e bytes.Buffer
	if code := Run([]string{"review", "--report", "--profile", "fake-smoke", ws}, &o, &e); code != int(fault.Config) {
		t.Fatalf("ungated fake-smoke exit = %d, want %d (config), stderr=%s", code, fault.Config, e.String())
	}
	if !strings.Contains(e.String(), `profile "fake-smoke" not found in config`) {
		t.Errorf("ungated fake-smoke must fail with the plain unknown-profile error, got: %q", e.String())
	}

	// WITH the gate: the hidden profile resolves and the deterministic run completes.
	code, _, errs := run(t, "review", "--report", "--profile", "fake-smoke", ws)
	if code != 0 {
		t.Errorf("gated fake-smoke exit = %d, want 0 (stderr=%s)", code, errs)
	}
}

func ciWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestCI_NoFindingsExit0(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir()) // hermetic: never read the real home config
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", t.TempDir())
	t.Setenv("REVIEWMESH_FAKE_SCENARIO", "empty")
	code, _, _ := run(t, "review", "--ci", "--profile", "fake-smoke", ciWorkspace(t))
	if code != 0 {
		t.Errorf("ci with no findings should exit 0, got %d", code)
	}
}

func TestCI_ForcesReportEvenWithApply(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir()) // hermetic: never read the real home config
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", t.TempDir())
	t.Setenv("REVIEWMESH_FAKE_SCENARIO", "valid")
	ws := ciWorkspace(t)
	// --ci must override --apply to report (no writes), regardless of config/flags
	code, _, _ := run(t, "review", "--ci", "--apply", "--profile", "fake-smoke", ws)
	if code != int(fault.Findings) {
		t.Errorf("ci exit = %d, want %d", code, fault.Findings)
	}
	b, _ := os.ReadFile(filepath.Join(ws, "main.go"))
	if strings.Contains(string(b), "// reviewmesh[") {
		t.Error("ci must not write to the workspace even with --apply")
	}
}

func TestCI_BlockingFindingsExit1(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir()) // hermetic: never read the real home config
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", t.TempDir())
	t.Setenv("REVIEWMESH_FAKE_SCENARIO", "valid")
	code, _, _ := run(t, "review", "--ci", "--profile", "fake-smoke", ciWorkspace(t))
	if code != int(fault.Findings) {
		t.Errorf("ci with findings should exit %d, got %d", fault.Findings, code)
	}
}

func TestParseSets(t *testing.T) {
	a, m, err := parseSets([]string{"reviewer.adapter=codex-cli", "author_remediator.model=local"})
	if err != nil {
		t.Fatalf("parseSets: %v", err)
	}
	if a[review.Role("reviewer")] != "codex-cli" {
		t.Errorf("adapter override = %v", a)
	}
	if m[review.Role("author_remediator")] != "local" {
		t.Errorf("model override = %v", m)
	}
	for _, bad := range []string{"nope", "reviewer.adapter=", "reviewer.frob=x", "=x"} {
		if _, _, e := parseSets([]string{bad}); e == nil {
			t.Errorf("parseSets(%q) should error", bad)
		}
	}
}

func run(t *testing.T, args ...string) (code int, out, errs string) {
	t.Helper()
	// Hermetic tests are the internal harness the hidden fake adapter exists for: unlock it
	// (fake + the hidden fake-smoke profile are env-gated and fail closed for users).
	t.Setenv(fake.EnvVar, "1")
	var o, e bytes.Buffer
	code = Run(args, &o, &e)
	return code, o.String(), e.String()
}

// TestList_JSON proves `list --json` emits a stable, parseable projection of adapters + profiles, that
// adapters carry the DECLARED identityEvidenceCapability (never "verified"), and that profiles carry
// their lanes.
func TestList_JSON(t *testing.T) {
	code, out, errs := run(t, "list", "--json")
	if code != int(fault.OK) {
		t.Fatalf("list --json exit=%d, stderr=%s", code, errs)
	}
	var view struct {
		Adapters []struct {
			Name                       string `json:"name"`
			IdentityEvidenceCapability string `json:"identityEvidenceCapability"`
		} `json:"adapters"`
		Profiles []struct {
			Name      string `json:"name"`
			IsDefault bool   `json:"isDefault"`
			Lanes     []struct {
				Role    string `json:"role"`
				Adapter string `json:"adapter"`
				Model   string `json:"model"`
			} `json:"lanes"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out)
	}
	if len(view.Adapters) == 0 {
		t.Fatal("list --json must report adapters")
	}
	var sawIdentity bool
	for _, a := range view.Adapters {
		if a.IdentityEvidenceCapability == "verified" {
			t.Errorf("adapter %q reports a live 'verified' — list must report only a DECLARED capability", a.Name)
		}
		if a.Name == "claude-code" && a.IdentityEvidenceCapability != "" {
			sawIdentity = true
		}
	}
	if !sawIdentity {
		t.Error("a shell recipe (claude-code) must carry a declared identityEvidenceCapability")
	}
	if len(view.Profiles) == 0 {
		t.Fatal("list --json must report profiles")
	}
}

// TestList_Human smoke-tests the human summary carries the Adapters + Profiles sections.
func TestList_Human(t *testing.T) {
	code, out, errs := run(t, "list")
	if code != int(fault.OK) {
		t.Fatalf("list exit=%d, stderr=%s", code, errs)
	}
	for _, want := range []string{"Adapters:", "Profiles:"} {
		if !strings.Contains(out, want) {
			t.Errorf("list human output missing %q:\n%s", want, out)
		}
	}
}

// TestInit_Folder_CreatesState smoke-tests `reviewmesh folder init` through the CLI dispatch: in a
// non-repo temp dir it creates .aimesh/temp and reports success.
func TestInit_Folder_CreatesState(t *testing.T) {
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	code, out, errs := run(t, "folder", "init")
	if code != int(fault.OK) {
		t.Fatalf("folder init exit=%d, stderr=%s", code, errs)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".aimesh", "temp")); err != nil || !fi.IsDir() {
		t.Fatalf(".aimesh/temp not created: err=%v", err)
	}
	if !strings.Contains(out, "initialized") {
		t.Errorf("expected confirmation, got %q", out)
	}
}

// TestHelp_ExitsZero_AndABadInvocationExitsUsage is the CROSS-APP PARITY pin, and the twin of
// exploremesh's test of the same name. Asking for help is not a usage error: `reviewmesh --help` must
// print to STDOUT and exit 0, exactly as `exploremesh --help` does. The two binaries drifted apart on
// precisely this — exploremesh exited 2 for a whole release, because help and "you invoked me wrong"
// shared one function that returned the usage code — so a script or a CI step that gated on `--help`
// succeeding failed against one binary and passed against the other. ALL THREE spellings are pinned
// here, because a pin on `--help` alone leaves `-h` and `help` free to rot away from it.
//
// The other half of the same table matters just as much: a bad invocation must STILL exit 2, its text
// must still go to stderr, and it must still name the command it did not recognize. A "fix" that made
// everything exit 0 would be the worse regression.
func TestHelp_ExitsZero_AndABadInvocationExitsUsage(t *testing.T) {
	for _, spelling := range []string{"-h", "--help", "help"} {
		code, out, errs := run(t, spelling)
		if code != int(fault.OK) {
			t.Errorf("%q exit %d, want 0 — asking for help is not a usage error", spelling, code)
		}
		// The RUNNABLE command, not the domain name: help that names a binary the user does not
		// have is help they cannot act on.
		if !strings.Contains(out, "aimesh review") {
			t.Errorf("%q must print the help text on STDOUT (a pipeable answer), got %q", spelling, out)
		}
		if errs != "" {
			t.Errorf("%q wrote to stderr: %q", spelling, errs)
		}
	}
	for _, args := range [][]string{nil, {"frobnicate"}, {"--not-a-flag"}} {
		code, out, errs := run(t, args...)
		if code != int(fault.Usage) {
			t.Errorf("%v exit %d, want %d — a bad invocation is still a usage error", args, code, fault.Usage)
		}
		if !strings.Contains(errs, "Usage:") {
			t.Errorf("%v must explain itself on STDERR, got %q", args, errs)
		}
		if out != "" {
			t.Errorf("%v wrote to stdout: %q", args, out)
		}
		if len(args) > 0 && !strings.Contains(errs, "unknown command") {
			t.Errorf("%v must name the token it did not recognize, got %q", args, errs)
		}
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, "--version")
	if code != 0 {
		t.Fatalf("version: code=%d", code)
	}
	got := strings.TrimRight(out, "\n")
	if !strings.HasPrefix(got, "reviewmesh ") || strings.Contains(got, "\n") {
		t.Errorf("--version must be a single \"reviewmesh <version>\" line, got %q", out)
	}
	for _, leaked := range []string{"commit", "os/arch", "go:", "(dirty)", "date"} {
		if strings.Contains(got, leaked) {
			t.Errorf("--version must not expose %q by default: %q", leaked, got)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, _ := run(t, "--version", "--json")
	if code != 0 || !strings.Contains(out, `"version"`) {
		t.Errorf("version --json: code=%d out=%q", code, out)
	}
}

func TestReviewNoPath_Exit2(t *testing.T) {
	code, _, _ := run(t, "review", "--report")
	if code != int(fault.Usage) {
		t.Errorf("code = %d, want %d", code, fault.Usage)
	}
}

func TestReviewConflictingModes_Exit2(t *testing.T) {
	code, _, _ := run(t, "review", "--report", "--apply", "x")
	if code != int(fault.Usage) {
		t.Errorf("code = %d, want %d", code, fault.Usage)
	}
}

func TestReview_IncludeHostReview_RejectedForApplyPatch(t *testing.T) {
	// Explicit apply/patch must be rejected — including when combined with --ci (which
	// would otherwise force report and silently accept a contradictory request).
	cases := [][]string{
		{"review", "--apply", "--include-host-review", "x"},
		{"review", "--patch", "--include-host-review", "x"},
		{"review", "--ci", "--apply", "--include-host-review", "x"},
		{"review", "--ci", "--patch", "--include-host-review", "x"},
	}
	for _, args := range cases {
		code, _, errs := run(t, args...)
		if code != int(fault.Usage) {
			t.Errorf("%v: code = %d, want %d (usage)", args, code, fault.Usage)
		}
		if !strings.Contains(errs, "report mode") {
			t.Errorf("%v: stderr = %q, want a report-mode usage error", args, errs)
		}
	}
}

func TestReview_IncludeHostReview_AcceptedForReport(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir()) // hermetic: never read the real home config
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", t.TempDir())
	code, out, errs := run(t, "review", "--report", "--include-host-review", "--profile", "fake-smoke", ciWorkspace(t))
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%q", code, errs)
	}
	if !strings.Contains(out, "review complete") {
		t.Errorf("stdout = %q", out)
	}
}

// splitArgsFixture builds a flag set shaped like `review run`'s — enough of it to cover the
// value-taking flags the old hand-maintained map had fallen behind on.
func splitArgsFixture(t *testing.T) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Bool("report", false, "")
	fs.Bool("apply", false, "")
	fs.String("mode", "", "")
	fs.String("changed-since", "", "")
	fs.String("changed-vs", "", "")
	fs.Int("max-parallel", 0, "")
	fs.Duration("verify-timeout", 0, "")
	var paths, cmds setFlags
	fs.Var(&paths, "path", "")
	fs.Var(&cmds, "verify-cmd", "")
	return fs
}

func TestSplitArgs_BothFlagForms(t *testing.T) {
	fs := splitArgsFixture(t)
	// flags after the positional
	f, p := splitArgs(fs, []string{"path", "--report"})
	if len(p) != 1 || p[0] != "path" || len(f) != 1 || f[0] != "--report" {
		t.Errorf("flags-after-positional: flags=%v positionals=%v", f, p)
	}
	// value-taking flag keeps its value together, positional after
	f2, p2 := splitArgs(fs, []string{"--mode", "patch", "path"})
	if len(p2) != 1 || p2[0] != "path" || len(f2) != 2 || f2[0] != "--mode" || f2[1] != "patch" {
		t.Errorf("value-flag split: flags=%v positionals=%v", f2, p2)
	}
}

// TestSplitArgs_EveryValueFlagKeepsItsValue is the regression this closes.
//
// These six flags were all absent from the hand-maintained map that used to answer "does this
// flag take a value". Their values were therefore classified as POSITIONALS, so the workspace
// path was lost and the parser blamed the flag ("flag needs an argument") instead. `--path` and
// `--changed-since` are two of the flags the help text pushes hardest, so this was reachable by
// following the documentation.
//
// The answer now comes from the flag set itself, which is why this test enumerates flags rather
// than a list: anything registered non-bool is covered by construction.
func TestSplitArgs_EveryValueFlagKeepsItsValue(t *testing.T) {
	fs := splitArgsFixture(t)
	for _, tc := range []struct{ flag, value string }{
		{"--path", "src"},
		{"--changed-since", "2h"},
		{"--changed-vs", "diff"},
		{"--max-parallel", "3"},
		{"--verify-cmd", "go test ./..."},
		{"--verify-timeout", "30s"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			f, p := splitArgs(fs, []string{tc.flag, tc.value, "workspace-path"})
			if len(p) != 1 || p[0] != "workspace-path" {
				t.Fatalf("the workspace path was lost: positionals=%v (flags=%v)", p, f)
			}
			if len(f) != 2 || f[0] != tc.flag || f[1] != tc.value {
				t.Errorf("flags=%v, want [%s %s]", f, tc.flag, tc.value)
			}
		})
	}
}

// TestSplitArgs_ABoolFlagDoesNotEatThePath is the other half: a bool must NOT consume the next
// token, or `--report .` would lose the workspace instead.
func TestSplitArgs_ABoolFlagDoesNotEatThePath(t *testing.T) {
	fs := splitArgsFixture(t)
	f, p := splitArgs(fs, []string{"--report", "."})
	if len(p) != 1 || p[0] != "." {
		t.Errorf("a bool flag swallowed the positional: flags=%v positionals=%v", f, p)
	}
	// `--flag=value` carries its own value and must not consume the next token either.
	f2, p2 := splitArgs(fs, []string{"--mode=patch", "."})
	if len(p2) != 1 || p2[0] != "." {
		t.Errorf("--flag=value swallowed the positional: flags=%v positionals=%v", f2, p2)
	}
}

func TestGatingCode(t *testing.T) {
	if gatingCode(false, 3) != 0 {
		t.Error("findings without gating should exit 0")
	}
	if gatingCode(true, 0) != 0 {
		t.Error("no findings with gating should exit 0")
	}
	if gatingCode(true, 2) != int(fault.Findings) {
		t.Error("findings with gating should exit 1")
	}
}

// (acp is implemented in Batch 3 and covered by internal/surface/acp tests;
// the CLI `acp` command reads os.Stdin so it is not invoked from a unit test here.)
