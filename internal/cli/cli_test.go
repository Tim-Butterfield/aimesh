package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/agentguide"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// run executes the root CLI and returns (exit, stdout, stderr).
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// TestNoCommand_IsAUsageError: a bare invocation prints help to STDERR and exits non-zero, while
// asking for help prints to STDOUT and exits 0. Conflating the two is how `aimesh --help | less`
// and any CI script gating on the exit code break.
func TestHelpGoesToStdoutAndNoCommandIsAUsageError(t *testing.T) {
	code, out, errb := run(t)
	if code != int(fault.Usage) {
		t.Errorf("no command: exit = %d, want %d", code, fault.Usage)
	}
	if out != "" || !strings.Contains(errb, "usage:") {
		t.Errorf("no command must print usage on STDERR; stdout=%q stderr=%q", out, errb)
	}

	for _, a := range []string{"-h", "--help", "help"} {
		code, out, errb := run(t, a)
		if code != int(fault.OK) {
			t.Errorf("%s: exit = %d, want 0 — asking for help is not a usage error", a, code)
		}
		if !strings.Contains(out, "usage:") || errb != "" {
			t.Errorf("%s must print usage on STDOUT; stdout=%q stderr=%q", a, out, errb)
		}
	}
}

// `aimesh review --help` must answer with REVIEW's help, not the top-level tree the user just came
// from. The domain's curated usage is the ONLY place its modes, mode-specific flags and exit codes
// are documented — reprinting the root hides them, and restating them at the root would drift.
func TestDomainHelp_ForwardsToTheDomainNotTheRoot(t *testing.T) {
	_, rootOut, _ := run(t, "--help")

	for _, noun := range []string{"review", "explore"} {
		for _, flag := range []string{"-h", "--help", "help"} {
			code, out, _ := run(t, noun, flag)
			if code != int(fault.OK) {
				t.Errorf("%s %s: exit = %d, want 0", noun, flag, code)
			}
			if out == rootOut {
				t.Errorf("%s %s printed the ROOT usage verbatim — the domain's own help is unreachable", noun, flag)
			}
			if out == "" {
				t.Errorf("%s %s printed nothing", noun, flag)
			}
		}
	}
}

func TestUnknownCommand_NamesWhatWasTyped(t *testing.T) {
	code, _, errb := run(t, "nonesuch")
	if code != int(fault.Usage) {
		t.Errorf("exit = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errb, `unknown command "nonesuch"`) {
		t.Errorf("the refusal must echo what was typed: %q", errb)
	}
}

// An unknown verb is refused BY THE ROOT, naming the domain — not passed down to produce an error
// that talks about a command tree the user did not type.
func TestUnknownDomainVerb_IsRefusedByTheRootNamingTheDomain(t *testing.T) {
	for _, noun := range []string{"review", "explore"} {
		code, _, errb := run(t, noun, "nonesuch")
		if code != int(fault.Usage) {
			t.Errorf("%s nonesuch: exit = %d, want %d", noun, code, fault.Usage)
		}
		if !strings.Contains(errb, "aimesh "+noun+": unknown command") {
			t.Errorf("%s: the refusal must name the domain: %q", noun, errb)
		}
	}
	for _, noun := range []string{"review", "explore"} {
		if code, _, _ := run(t, noun); code != int(fault.Usage) {
			t.Errorf("%s with no verb: exit = %d, want %d", noun, code, fault.Usage)
		}
	}
}

// `run` is the ONLY verb that is renamed on the way down: it becomes the domain's own name, which is
// exactly the bare spelling this tree replaces. Everything else must pass through unchanged, or the
// root would silently redirect one command to another.
func TestVerbTables_OnlyRunIsRenamed(t *testing.T) {
	for _, d := range []domain{reviewDomain, exploreDomain} {
		if got := d.verbs["run"]; got != d.noun {
			t.Errorf("%s: run must forward as %q, got %q", d.noun, d.noun, got)
		}
		for verb, inner := range d.verbs {
			if verb == "run" {
				continue
			}
			if verb != inner {
				t.Errorf("%s: verb %q must pass through unchanged, got %q", d.noun, verb, inner)
			}
		}
	}
}

// Help that drifts from dispatch is worse than no help: it documents commands that do not exist, or
// hides ones that do. Every dispatchable verb must appear in the usage text.
func TestUsageListsEveryDispatchableVerb(t *testing.T) {
	for _, d := range []domain{reviewDomain, exploreDomain} {
		for verb := range d.verbs {
			if !strings.Contains(usage, "  "+verb+" ") {
				t.Errorf("usage does not document `aimesh %s %s`", d.noun, verb)
			}
		}
	}
	for _, shared := range []string{"init", "doctor", "agents-md"} {
		if !strings.Contains(usage, "  "+shared+" ") {
			t.Errorf("usage does not document the shared command %q", shared)
		}
	}
}

// --- init -------------------------------------------------------------------------------------

func TestInit_ContradictoryAssertionsAreRefused(t *testing.T) {
	code, _, errb := run(t, "init", "--require-repo", "--require-folder")
	if code != int(fault.Usage) {
		t.Errorf("exit = %d, want %d", code, fault.Usage)
	}
	if !strings.Contains(errb, "contradict") {
		t.Errorf("the refusal must say why: %q", errb)
	}
}

func TestInit_JSONRecordAndRepoAssertion(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	code, out, errb := run(t, "init", "--json", "--require-repo")
	if code != int(fault.OK) {
		t.Fatalf("init: exit = %d, stderr=%q", code, errb)
	}
	var rec initRecord
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("--json must emit one parseable record, got %q: %v", out, err)
	}
	if rec.Mode != "repo" {
		t.Errorf("mode = %q, want repo", rec.Mode)
	}
	if want := filepath.Join(repo, ".aimesh"); rec.Home != want {
		t.Errorf("home = %q, want %q", rec.Home, want)
	}
	if fi, err := os.Stat(rec.Home); err != nil || !fi.IsDir() {
		t.Errorf("init must actually create %s", rec.Home)
	}
}

// --require-folder inside a repository is the assertion doing its job: without it, running where you
// believed you were outside a repo silently produces the other mode.
func TestInit_RequireFolderRefusesInsideARepo(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	if code, _, _ := run(t, "init", "--require-folder"); code == int(fault.OK) {
		t.Error("--require-folder inside a repository must fail")
	}
}

// --- version ----------------------------------------------------------------------------------

// `aimesh --version` must name AIMESH. The root delegated the whole line to the review domain's
// version package, so the first command the quick start tells a new user to run answered
// "reviewmesh dev" — naming a binary that no longer exists, from the one that does.
func TestVersion_NamesTheBinaryNotADomain(t *testing.T) {
	for _, a := range []string{"--version", "version"} {
		code, out, errb := run(t, a)
		if code != int(fault.OK) {
			t.Errorf("%s: exit = %d, stderr=%q", a, code, errb)
		}
		if !strings.HasPrefix(out, "aimesh ") {
			t.Errorf("%s printed %q, want a line starting %q", a, out, "aimesh ")
		}
		for _, gone := range []string{"reviewmesh", "exploremesh"} {
			if strings.Contains(out, gone) {
				t.Errorf("%s names the retired binary %q: %q", a, gone, out)
			}
		}
	}

	// --json stays machine-readable and carries the build metadata, not the display name.
	code, out, _ := run(t, "--version", "--json")
	if code != int(fault.OK) {
		t.Fatalf("--version --json: exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--version --json must emit one parseable object, got %q: %v", out, err)
	}
	if _, ok := got["version"]; !ok {
		t.Errorf("--version --json = %q, want a version field", out)
	}
}

// --- doctor -----------------------------------------------------------------------------------

// doctor REPORTS: with no root it says ready=false and exits 0, because a read-only probe that
// errors tells a caller nothing about what to do next. --require-root turns that into a gate.
func TestDoctor_ReportsByDefaultAndGatesOnRequest(t *testing.T) {
	bare := t.TempDir() // no .git, no .aimesh
	t.Chdir(bare)

	code, out, _ := run(t, "doctor")
	if code != int(fault.OK) {
		t.Errorf("bare doctor: exit = %d, want 0 — it reports rather than blocking", code)
	}
	if !strings.Contains(out, "ready:     no") || !strings.Contains(out, "aimesh init") {
		t.Errorf("an unready report must name the remedy: %q", out)
	}

	code, _, errb := run(t, "doctor", "--require-root")
	if code != int(fault.Config) {
		t.Errorf("--require-root with no root: exit = %d, want %d", code, fault.Config)
	}
	if !strings.Contains(errb, "no state root found") {
		t.Errorf("the gate must say what it tripped on: %q", errb)
	}
}

func TestDoctor_CreatesNothing(t *testing.T) {
	bare := t.TempDir()
	t.Chdir(bare)
	if code, _, _ := run(t, "doctor"); code != int(fault.OK) {
		t.Fatal("doctor should report")
	}
	entries, err := os.ReadDir(bare)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("doctor is read-only but created %v — a probe that initializes what it inspects "+
			"cannot be used to find out whether initialization is needed", entries)
	}
}

func TestDoctor_JSONRecord(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	if code, _, errb := run(t, "init"); code != int(fault.OK) {
		t.Fatalf("init: %s", errb)
	}
	code, out, _ := run(t, "doctor", "--json")
	if code != int(fault.OK) {
		t.Fatalf("doctor --json: exit = %d", code)
	}
	var rec doctorRecord
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("--json must emit one parseable record, got %q: %v", out, err)
	}
	if !rec.Ready || !rec.HomeExists || rec.RootSource != "vcs" {
		t.Errorf("after init the report should be ready+rooted: %+v", rec)
	}
}

// --- agents-md --------------------------------------------------------------------------------

func TestAgentsMD_ServesTheEmbeddedGuideCleanly(t *testing.T) {
	t.Setenv(agentguide.EnvVar, "")
	code, out, errb := run(t, "agents-md")
	if code != int(fault.OK) {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	if !strings.HasPrefix(out, "# AGENTS.md") {
		t.Errorf("stdout must be the guide verbatim, got %q", out[:min(60, len(out))])
	}
	if errb != "" {
		t.Errorf("no override is active, so nothing belongs on stderr: %q", errb)
	}
	// The guide is load-bearing: it must actually carry the rules an agent gets wrong.
	for _, must := range []string{"Identity is recorded, never acted on", "containment copy", "denylist"} {
		if !strings.Contains(out, must) {
			t.Errorf("the embedded guide omits %q", must)
		}
	}
}

// An override is served VERBATIM on stdout — but disclosed on stderr, because with one active the
// "matches the binary" guarantee no longer holds and a reader who cannot tell has been misled.
func TestAgentsMD_OverrideIsServedAndDisclosed(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom.md")
	if err := os.WriteFile(custom, []byte("# CUSTOM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentguide.EnvVar, custom)

	code, out, errb := run(t, "agents-md")
	if code != int(fault.OK) {
		t.Fatalf("exit = %d", code)
	}
	if out != "# CUSTOM\n" {
		t.Errorf("stdout must be the override verbatim, got %q", out)
	}
	if !strings.Contains(errb, custom) || !strings.Contains(errb, "not the embedded guide") {
		t.Errorf("an active override must be disclosed on stderr: %q", errb)
	}
}

// An unreadable override is an ERROR. Falling back would hand the agent a document the operator did
// not choose while reporting success.
func TestAgentsMD_UnreadableOverrideIsAnErrorNamingTheRemedy(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.md")
	t.Setenv(agentguide.EnvVar, missing)

	code, out, errb := run(t, "agents-md")
	if code == int(fault.OK) {
		t.Fatal("an unreadable override must not succeed")
	}
	if out != "" {
		t.Errorf("nothing should be served: %q", out)
	}
	if !strings.Contains(errb, missing) || !strings.Contains(errb, "Unset "+agentguide.EnvVar) {
		t.Errorf("the error must name the path AND the remedy: %q", errb)
	}
}
