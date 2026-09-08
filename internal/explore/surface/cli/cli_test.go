package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// TestMain isolates the user-scope config: AIMESH_HOME points at a temp home so no test reads (or is
// affected by) the developer's real state, seeded with an all-`fake` default profile — the shipped
// default is deliberately UNCONFIGURED (nothing runs on a fresh install), so tests that explore
// without flags need an explicit hermetic panel. ONE home covers both the profiles and the shared
// adapters file now that they live under the same `.aimesh/` root.
//
// It ALSO leaves the repository, and that is not tidiness. Project scope BEATS user scope
// (profile.Discover) and is ROOT-ANCHORED — it walks up from the cwd to the VCS root. Run from
// inside this checkout, a single `<repo-root>/.aimesh/explore/profiles.yaml` therefore overrides
// the seed below, and because the shipped default profile is empty, every test that explores
// without flags fails with "profile is unconfigured". That file is one `aimesh init` away, it
// is untracked, and it has happened. Chdir to a directory that is inside no repository, so project
// scope resolves to nothing and the seed is what binds. TestSuiteIsIsolatedFromProjectScope pins it.
func TestMain(m *testing.M) {
	neutral, err := os.MkdirTemp("", "exploremesh-cli-cwd")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(neutral); err != nil {
		panic(err)
	}

	home, err := os.MkdirTemp("", "exploremesh-cli-home")
	if err != nil {
		panic(err)
	}
	dir := filepath.Join(home, localstate.HomeDirName, roster.ComponentName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	const fakeProfiles = `schemaVersion: 1
defaultProfile: default
profiles:
  default:
    explorers:
      - { adapter: fake, model: fake-a, effort: high }
      - { adapter: fake, model: fake-b, effort: medium }
    collator: { adapter: fake, model: fake-c, effort: high }
`
	if err := os.WriteFile(filepath.Join(dir, profile.FileName), []byte(fakeProfiles), 0o644); err != nil {
		panic(err)
	}
	_ = os.Setenv("AIMESH_HOME", home)

	// The seeded all-fake profile depends on the env-gated internal fake harness; unlock it for the
	// package (individual tests that pin the WITHOUT-gate posture override with t.Setenv).
	_ = os.Setenv("AIMESH_INTERNAL_FAKE", "1")

	code := m.Run()
	_ = os.RemoveAll(home)
	_ = os.RemoveAll(neutral)
	os.Exit(code)
}

// TestSuiteIsIsolatedFromProjectScope asserts the precondition every no-flag test in this package
// silently depends on: the suite's cwd is inside no repository, so root-anchored PROJECT scope
// resolves to nothing and TestMain's seeded USER-scope panel is what a run binds to.
//
// Without this the dependency is invisible. The suite passes on a clean checkout, then twenty tests
// fail on a machine where someone ran `exploremesh init` at the repo root — with an error about an
// unconfigured profile that says nothing about cwd, scope precedence, or the file responsible.
func TestSuiteIsIsolatedFromProjectScope(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := profile.ProjectProfilesPath(cwd); ok {
		t.Fatalf("the suite is running inside a repository (project profiles path %q would be consulted). "+
			"Project scope beats the user scope TestMain seeds, so a profiles.yaml there silently "+
			"overrides the test panel: TestMain must chdir outside any repo.", p)
	}
}

// TestExplore_FakeAdapterIsEnvGated pins the hidden-internal posture of the fake harness: the
// literal `fake` adapter is NOT user-configurable — without AIMESH_INTERNAL_FAKE=1 it fails closed
// with the SAME unknown-adapter config error as any other unrecognized name (and the guidance never
// advertises that a fake exists); with the gate it resolves and runs end-to-end.
func TestExplore_FakeAdapterIsEnvGated(t *testing.T) {
	args := []string{"explore", "--purpose", "p", "--criteria", "a",
		"--explorer", "adapter=fake,model=fake-a", "--explorer", "adapter=fake,model=fake-b",
		"--collator", "adapter=fake,model=fake-c"}

	// WITHOUT the gate (TestMain unlocks it package-wide; this test explicitly clears it):
	// the ad-hoc panel fails closed before spending, echoing the unknown name but never listing
	// `fake` among the configured choices.
	t.Setenv("AIMESH_INTERNAL_FAKE", "")
	var out, errb bytes.Buffer
	if code := Run(args, &out, &errb); code == 0 {
		t.Fatal("adapter=fake must fail closed without AIMESH_INTERNAL_FAKE")
	}
	errs := errb.String()
	if !strings.Contains(errs, "unknown adapter(s) in --explorer/--collator/--canonicalizer: fake") {
		t.Errorf("ungated fake must fail with the plain unknown-adapter error, got: %q", errs)
	}
	if _, choices, ok := strings.Cut(errs, "configured adapters are:"); ok && strings.Contains(choices, "fake") {
		t.Errorf("the configured-adapter guidance must not advertise the hidden fake key: %q", errs)
	}

	// WITH the gate: the same panel resolves and the deterministic pipeline completes.
	t.Setenv("AIMESH_INTERNAL_FAKE", "1")
	out.Reset()
	errb.Reset()
	if code := Run(args, &out, &errb); code != 0 {
		t.Fatalf("gated adapter=fake exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Exploration complete") {
		t.Errorf("gated fake panel should run end-to-end:\n%s", out.String())
	}
}

func TestExplore_EndToEnd_FakeAdapters(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"explore", "--purpose", "compare X and Y", "--criteria", "cost,scalability"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"Exploration complete", "Synthesis:", "Disagreements:"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	// The summary does not report a formulation source. Every mode is formulation-free, so the line
	// could only ever print one value while reading as a live choice between formulation regimes.
	if strings.Contains(s, "formulation:") {
		t.Errorf("the summary must not report a formulation source:\n%s", s)
	}
}

// TestExplore_DryRun covers the pre-spend disclosure end to end on the CLI: what it prints, the two things
// it must not let a reader assume, and the one flag it refuses to be combined with.
func TestExplore_DryRun(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "compare X and Y", "--criteria", "cost", "--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"dry run: nothing was spent",
		"blind panel",
		"model calls:",
		"exactly",            // one number, not a range — the difference from a review's shape
		"round-1 payload",    // what those calls would carry
		"pre-flight was NOT", // and the guarantee explore cannot make
		"Drop --dry-run",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the disclosure must mention %q:\n%s", want, s)
		}
	}
	// It explored nothing, so it must not read like a completed exploration.
	if strings.Contains(s, "Exploration complete") || strings.Contains(s, "Synthesis:") {
		t.Errorf("a dry run must not render as a finished exploration:\n%s", s)
	}

	// --json carries the machine-readable shape, including the full prompt the human summary points at.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"explore", "--purpose", "compare X and Y", "--criteria", "cost", "--dry-run", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("--dry-run --json exit %d, stderr: %s", code, errb.String())
	}
	var res struct {
		Shape *struct {
			ModelCalls int `json:"modelCalls"`
			Calls      []struct {
				Phase string `json:"phase"`
				Calls int    `json:"calls"`
			} `json:"calls"`
			Payload struct {
				Prompt      string `json:"prompt"`
				PayloadHash string `json:"payloadHash"`
			} `json:"payload"`
		} `json:"shape"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("--dry-run --json is not valid JSON: %v\n%s", err, out.String())
	}
	if res.Shape == nil {
		t.Fatalf("no shape in --dry-run --json:\n%s", out.String())
	}
	if res.Shape.ModelCalls <= 0 || len(res.Shape.Calls) == 0 {
		t.Errorf("shape prices nothing: %+v", res.Shape)
	}
	if !strings.Contains(res.Shape.Payload.Prompt, "compare X and Y") || res.Shape.Payload.PayloadHash == "" {
		t.Errorf("the payload must carry the exact prompt and its hash: %+v", res.Shape.Payload)
	}

	// --dump-run captures an exploration; a dry run performs none, so the pair is refused rather than
	// writing a run directory that records something that did not happen.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--dry-run", "--dump-run"}, &out, &errb); code == 0 {
		t.Fatal("--dry-run --dump-run must be refused")
	}
	if !strings.Contains(errb.String(), "cannot be combined with --dump-run") {
		t.Errorf("the refusal should name the combination:\n%s", errb.String())
	}
}

// TestExplore_Mode covers the --mode flag: an explicit "map" runs identically to the default, and an
// unknown mode is a usage error that lists the known modes.
func TestExplore_Mode(t *testing.T) {
	// (a) explicit --mode map succeeds.
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "map"}, &out, &errb); code != 0 {
		t.Fatalf("--mode map exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Synthesis:") {
		t.Errorf("explicit map mode should render the map synthesis:\n%s", out.String())
	}
	// (b) an unknown mode is rejected, listing the known modes.
	out.Reset()
	errb.Reset()
	code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "bogus"}, &out, &errb)
	if code == 0 {
		t.Fatal("an unknown --mode must be a usage error")
	}
	if !strings.Contains(errb.String(), "unknown mode") || !strings.Contains(errb.String(), "map") {
		t.Errorf("unknown-mode error should name the mode + list known modes:\n%s", errb.String())
	}
}

// TestExplore_SynthesizeMode runs the Synthesize mode end-to-end through the CLI on fake adapters: the
// human summary prints the composed artifact + provenance, and --json carries a valid SynthesizeOutput.
func TestExplore_SynthesizeMode(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "pick a datastore", "--criteria", "latency,cost", "--mode", "synthesize"}, &out, &errb); code != 0 {
		t.Fatalf("--mode synthesize exit %d, stderr: %s", code, errb.String())
	}
	for _, want := range []string{"Exploration complete", "Composed answer:", "Component provenance", "Minority report"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("synthesize summary missing %q:\n%s", want, out.String())
		}
	}
	// --json carries a valid SynthesizeOutput (non-empty artifact).
	out.Reset()
	errb.Reset()
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "synthesize", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("--mode synthesize --json exit %d, stderr: %s", code, errb.String())
	}
	var res struct {
		Output struct {
			Artifact            string `json:"artifact"`
			ComponentProvenance []any  `json:"componentProvenance"`
			MinorityReport      []any  `json:"minorityReport"`
		} `json:"Output"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if res.Output.Artifact == "" {
		t.Errorf("synthesize JSON result missing a composed artifact:\n%s", out.String())
	}
	if len(res.Output.ComponentProvenance) == 0 || len(res.Output.MinorityReport) == 0 {
		t.Error("synthesize JSON result missing provenance/minority report")
	}
}

func TestExplore_JSON_ProducesStructuredResult(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	var res struct {
		Formulation struct {
			Source string `json:"source"`
		} `json:"Formulation"`
		Output *struct {
			SynthesisSummary     string `json:"synthesisSummary"`
			DisagreementRegister []any  `json:"disagreementRegister"`
		} `json:"Output"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if res.Output == nil || res.Output.SynthesisSummary == "" {
		t.Error("JSON result missing a synthesis")
	}
	if len(res.Output.DisagreementRegister) == 0 {
		t.Error("JSON result missing a disagreement register")
	}
}

func TestExplore_MissingRequiredFlags(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p"}, &out, &errb); code == 0 {
		t.Fatal("missing --criteria should be a usage error")
	}
}

func TestDoctor_ReportsAdapterReadiness(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"doctor"}, &out, &errb)
	if code != 0 {
		t.Fatalf("doctor exit %d, stderr: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "adapter:") || !strings.Contains(s, "roster: explorers >= 2") {
		t.Errorf("doctor output missing expected checks:\n%s", s)
	}
}

// TestList_JSON proves `list --json` emits the adapters + resolved roster + available modes as a stable,
// parseable projection, and that adapters carry the DECLARED identityEvidenceCapability (never "verified").
func TestList_JSON(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"list", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("list --json exit %d, stderr: %s", code, errb.String())
	}
	var view struct {
		Adapters []struct {
			Name                       string `json:"name"`
			Configured                 bool   `json:"configured"`
			IsACP                      bool   `json:"isAcp"`
			IdentityEvidenceCapability string `json:"identityEvidenceCapability"`
		} `json:"adapters"`
		Roster struct {
			Explorers []struct {
				Adapter string `json:"adapter"`
				Model   string `json:"model"`
			} `json:"explorers"`
			Collator struct {
				Adapter string `json:"adapter"`
			} `json:"collator"`
		} `json:"roster"`
		Modes []string `json:"modes"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out.String())
	}
	if len(view.Adapters) == 0 {
		t.Fatal("list --json must report adapters")
	}
	// Find the shell recipes carry their declared evidence tier (never the string "verified"), and that
	// `fake` is present + configured (the built-in demo roster's adapter).
	var sawFake, sawShellIdentity bool
	for _, a := range view.Adapters {
		if a.IdentityEvidenceCapability == "verified" {
			t.Errorf("adapter %q reports a live 'verified' — list must report only a DECLARED capability", a.Name)
		}
		if a.Name == "fake" && a.Configured {
			sawFake = true
		}
		if a.Name == "claude-code" && a.IdentityEvidenceCapability != "" {
			sawShellIdentity = true
		}
	}
	if !sawFake {
		t.Error("list --json must include a configured `fake` adapter")
	}
	if !sawShellIdentity {
		t.Error("a shell recipe (claude-code) must carry a declared identityEvidenceCapability")
	}
	// The default demo roster has 2 explorers + a collator, all `fake`.
	if len(view.Roster.Explorers) < 2 || view.Roster.Collator.Adapter == "" {
		t.Errorf("list --json roster should have >=2 explorers + a collator: %+v", view.Roster)
	}
	// Modes reflect the mode registry.
	if len(view.Modes) == 0 {
		t.Error("list --json must report the available modes")
	}
}

// TestList_Human smoke-tests the human summary carries the adapters/roster/modes sections.
func TestList_Human(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"list"}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, errb.String())
	}
	for _, want := range []string{"Adapters:", "Roster:", "explorers:", "collator:", "Modes:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list human output missing %q:\n%s", want, out.String())
		}
	}
}

// TestExplore_AdHoc_EndToEnd runs a one-off roster built from --explorer/--collator identifiers end-to-end
// on fakes, bypassing --roster.
func TestExplore_AdHoc_EndToEnd(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{
		"explore", "--purpose", "compare X and Y", "--criteria", "cost",
		"--explorer", "adapter=fake,model=a",
		"--explorer", "adapter=fake,model=b",
		"--collator", "adapter=fake,model=c",
		"--mode", "map",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("ad-hoc explore exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Exploration complete") {
		t.Errorf("ad-hoc explore did not complete:\n%s", out.String())
	}
}

// TestExplore_AdHoc_UnknownAdapter fails closed on an unknown adapter, naming the configured adapters.
func TestExplore_AdHoc_UnknownAdapter(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{
		"explore", "--purpose", "p", "--criteria", "a",
		"--explorer", "adapter=fake,model=a",
		"--explorer", "adapter=not-a-real-adapter,model=b",
		"--collator", "adapter=fake,model=c",
	}, &out, &errb)
	if code == 0 {
		t.Fatal("an unknown ad-hoc adapter must be a fail-closed error")
	}
	if !strings.Contains(errb.String(), "not-a-real-adapter") || !strings.Contains(errb.String(), "configured adapters are") {
		t.Errorf("error should name the unknown adapter + list configured adapters:\n%s", errb.String())
	}
}

// TestExplore_AdHoc_MutuallyExclusiveWithRoster rejects --explorer together with --roster.
func TestExplore_AdHoc_MutuallyExclusiveWithRoster(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{
		"explore", "--purpose", "p", "--criteria", "a",
		"--explorer", "adapter=fake,model=a",
		"--explorer", "adapter=fake,model=b",
		"--collator", "adapter=fake,model=c",
		"--roster", "/tmp/does-not-matter.yaml",
	}, &out, &errb)
	if code == 0 {
		t.Fatal("--explorer + --roster must be a usage error")
	}
	if !strings.Contains(errb.String(), "cannot be combined with --roster") {
		t.Errorf("error should explain the mutual exclusion:\n%s", errb.String())
	}
}

// TestParseSlot_ColonInValue proves the k=v parser keeps a ':' inside a value intact (a model tag like
// llama3:8b) — it splits fields on ',' and each field on the FIRST '=' only, never on ':'.
func TestParseSlot_ColonInValue(t *testing.T) {
	e, err := parseSlot("adapter=ollama,model=llama3:8b,effort=high")
	if err != nil {
		t.Fatalf("parseSlot error: %v", err)
	}
	if e.Adapter != "ollama" || e.Model != "llama3:8b" || e.Effort != "high" {
		t.Errorf("parsed slot wrong: %+v (model must keep its ':')", e)
	}
	// Missing required fields is an error.
	if _, err := parseSlot("adapter=fake"); err == nil {
		t.Error("parseSlot should require both adapter and model")
	}
}

// --- profiles + --count (design §7) ---

// profileEnv isolates a profile-aware test from BOTH the developer's real config and this repo:
// AIMESH_HOME + AIMESH_HOME point at a temp home and the cwd moves to a NON-repo temp dir, so
// project-scope resolution finds nothing (it would otherwise anchor at the aimesh repo root) and a run
// binds only to what the test persists. It returns the user-scope profiles path (the write target).
func profileEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	t.Setenv("AIMESH_HOME", home)
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	target, err := profile.UserProfilesPath()
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// writeProfiles persists a 2-profile set: `base` (2 explorers, no defaultMode) as the DEFAULT, and
// `wide` (3 explorers in a deliberate preference order, defaultMode synthesize).
func writeProfiles(t *testing.T, path string) {
	t.Helper()
	set := profile.Set{
		SchemaVersion:  profile.CurrentSchemaVersion,
		DefaultProfile: "base",
		Profiles: map[string]profile.Profile{
			"base": {
				Explorers: []roster.Explorer{
					{Adapter: "fake", Model: "base-a", Effort: "high"},
					{Adapter: "fake", Model: "base-b", Effort: "low"},
				},
				Collator: roster.Collator{Adapter: "fake", Model: "base-c"},
			},
			"wide": {
				Explorers: []roster.Explorer{
					{Adapter: "fake", Model: "z-first", Effort: "high"},
					{Adapter: "fake", Model: "a-second", Effort: "low"},
					{Adapter: "fake", Model: "m-third"},
				},
				Collator:    roster.Collator{Adapter: "fake", Model: "wide-c"},
				DefaultMode: "synthesize",
			},
		},
	}
	if err := profile.Save(path, set); err != nil {
		t.Fatalf("save profiles: %v", err)
	}
}

// TestExplore_ProfileAndCount runs a named profile with a --count subset end-to-end on fakes and asserts
// printSelected names the source + the selected/full counts + the "not run" subset note (design §7: a
// count subset must never be implicit, because a reorder changes WHICH explorers it selects).
func TestExplore_ProfileAndCount(t *testing.T) {
	writeProfiles(t, profileEnv(t))
	var out, errb bytes.Buffer
	code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "wide", "--count", "2", "--mode", "map"}, &out, &errb)
	if code != 0 {
		t.Fatalf("explore --profile wide --count 2 exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Exploration complete") {
		t.Errorf("run did not complete:\n%s", out.String())
	}
	e := errb.String()
	if !strings.Contains(e, `panel: 2 of 3 explorer(s) from profile "wide"`) {
		t.Errorf("printSelected must name the source + selected/full counts:\n%s", e)
	}
	// The SELECTED explorers are the top 2 by preference (z-first, a-second) — printed in canonical
	// attribution order — and the unselected one is reported as not run.
	if !strings.Contains(e, "fake:z-first:high") || !strings.Contains(e, "fake:a-second:low") {
		t.Errorf("selected panel should be the top 2 by preference order:\n%s", e)
	}
	if strings.Contains(e, "m-third") {
		t.Errorf("the unselected explorer must not appear in the panel:\n%s", e)
	}
	if !strings.Contains(e, "1 configured explorer(s) were NOT run") {
		t.Errorf("a count subset must print the not-run note:\n%s", e)
	}
	if !strings.Contains(e, "collator: fake:wide-c") {
		t.Errorf("panel should name the profile's collator:\n%s", e)
	}
}

// TestExplore_NoFlags_UsesDefaultProfile: a no-flag run binds to the persisted DEFAULT profile, with
// every explorer selected (no subset note).
func TestExplore_NoFlags_UsesDefaultProfile(t *testing.T) {
	writeProfiles(t, profileEnv(t))
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	e := errb.String()
	if !strings.Contains(e, `panel: 2 of 2 explorer(s) from profile "base" (default)`) {
		t.Errorf("a no-flag run must bind to the default profile:\n%s", e)
	}
	if strings.Contains(e, "were NOT run") {
		t.Errorf("a full-panel run must print no subset note:\n%s", e)
	}
}

// TestExplore_ProfileDefaultMode: a profile's defaultMode supplies the mode when --mode is omitted, and
// an explicit --mode overrides it (precedence explicit > profile default > the app default map, §7).
func TestExplore_ProfileDefaultMode(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	// (a) --mode omitted → the `wide` profile's synthesize.
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "wide"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Composed answer:") {
		t.Errorf("the profile's defaultMode (synthesize) should have applied:\n%s", out.String())
	}

	// (b) an explicit --mode wins over the profile's defaultMode.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "wide", "--mode", "map"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	// Map renders the synthesis block; synthesize renders "Composed answer:" — so the rendering names
	// which mode actually ran.
	if !strings.Contains(out.String(), "Synthesis:") || strings.Contains(out.String(), "Composed answer:") {
		t.Errorf("an explicit --mode must override the profile defaultMode:\n%s", out.String())
	}
}

// TestExplore_ProfileMutualExclusions: --profile cannot be combined with --roster or with the ad-hoc
// --explorer/--collator identifiers — each is a usage error before any spend.
func TestExplore_ProfileMutualExclusions(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "wide", "--roster", "/tmp/nope.yaml"}, &out, &errb); code == 0 {
		t.Fatal("--roster + --profile must be a usage error")
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Errorf("error should explain the mutual exclusion:\n%s", errb.String())
	}

	out.Reset()
	errb.Reset()
	code := Run([]string{
		"explore", "--purpose", "p", "--criteria", "a", "--profile", "wide",
		"--explorer", "adapter=fake,model=a",
		"--explorer", "adapter=fake,model=b",
		"--collator", "adapter=fake,model=c",
	}, &out, &errb)
	if code == 0 {
		t.Fatal("--explorer + --profile must be a usage error")
	}
	if !strings.Contains(errb.String(), "--roster or --profile") {
		t.Errorf("error should explain the ad-hoc mutual exclusion:\n%s", errb.String())
	}
}

// TestExplore_UnknownProfile names the configured profiles rather than silently falling back.
func TestExplore_UnknownProfile(t *testing.T) {
	writeProfiles(t, profileEnv(t))
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "nope"}, &out, &errb); code == 0 {
		t.Fatal("an unknown --profile must fail")
	}
	if !strings.Contains(errb.String(), "nope") || !strings.Contains(errb.String(), "base, wide") {
		t.Errorf("error should name the bad profile + list the configured ones:\n%s", errb.String())
	}
}

// TestExplore_CountOutOfRange: a --count above the profile size and a --count below 2 both fail clearly
// BEFORE any spend — the count is never clamped (requested = executed, §7). A non-numeric value is a
// usage error too.
func TestExplore_CountOutOfRange(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "base", "--count", "5"}, &out, &errb); code == 0 {
		t.Fatal("a --count above the profile size must fail")
	}
	if !strings.Contains(errb.String(), "never clamped") {
		t.Errorf("the out-of-range error should say the count is not clamped:\n%s", errb.String())
	}
	if strings.Contains(out.String(), "Exploration complete") {
		t.Error("an out-of-range count must fail before any exploration runs")
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--count", "1"}, &out, &errb); code == 0 {
		t.Fatal("--count 1 must fail — a panel needs 2+ explorers")
	}
	if !strings.Contains(errb.String(), "at least 2") {
		t.Errorf("the <2 error should say a panel needs 2+ explorers:\n%s", errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--count", "two"}, &out, &errb); code == 0 {
		t.Fatal("a non-numeric --count must fail")
	}
	if !strings.Contains(errb.String(), `want an integer or "all"`) {
		t.Errorf("the invalid-count error should state the accepted forms:\n%s", errb.String())
	}
}

// TestExplore_CountAll selects every explorer explicitly (the same panel as omitting --count).
func TestExplore_CountAll(t *testing.T) {
	writeProfiles(t, profileEnv(t))
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "wide", "--count", "all", "--mode", "map"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), `panel: 3 of 3 explorer(s) from profile "wide"`) {
		t.Errorf(`--count all must select every explorer:\n%s`, errb.String())
	}
}

// TestDoctor_Profile checks a NAMED profile's readiness (the full configured panel, no --count), and
// rejects --roster + --profile together.
func TestDoctor_Profile(t *testing.T) {
	writeProfiles(t, profileEnv(t))
	var out, errb bytes.Buffer
	if code := Run([]string{"doctor", "--profile", "wide"}, &out, &errb); code != 0 {
		t.Fatalf("doctor --profile wide exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "3 explorers") {
		t.Errorf("doctor must check the profile's FULL panel:\n%s", out.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"doctor", "--profile", "wide", "--roster", "/tmp/nope.yaml"}, &out, &errb); code == 0 {
		t.Fatal("doctor --roster + --profile must be a usage error")
	}
}

// TestList_Profiles proves `list` surfaces the configured profile set (design §8) in BOTH projections:
// the default profile's name and, per profile, its name/isDefault/defaultMode/ordered explorers +
// collator. The legacy top-level `roster` field keeps reporting the DEFAULT profile's roster.
func TestList_Profiles(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	var out, errb bytes.Buffer
	if code := Run([]string{"list", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("list --json exit %d, stderr: %s", code, errb.String())
	}
	var view struct {
		Roster struct {
			Explorers []struct {
				Model string `json:"model"`
			} `json:"explorers"`
		} `json:"roster"`
		Profiles struct {
			DefaultProfile string `json:"defaultProfile"`
			Profiles       []struct {
				Name        string `json:"name"`
				IsDefault   bool   `json:"isDefault"`
				DefaultMode string `json:"defaultMode"`
				Explorers   []struct {
					Adapter string `json:"adapter"`
					Model   string `json:"model"`
					Effort  string `json:"effort"`
				} `json:"explorers"`
				Collator struct {
					Model string `json:"model"`
				} `json:"collator"`
			} `json:"profiles"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out.String())
	}
	if view.Profiles.DefaultProfile != "base" {
		t.Errorf("defaultProfile = %q, want base", view.Profiles.DefaultProfile)
	}
	if len(view.Profiles.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2 (sorted by name)", len(view.Profiles.Profiles))
	}
	base, wide := view.Profiles.Profiles[0], view.Profiles.Profiles[1]
	if base.Name != "base" || !base.IsDefault || base.DefaultMode != "" {
		t.Errorf("base profile projection wrong: %+v", base)
	}
	if wide.Name != "wide" || wide.IsDefault || wide.DefaultMode != "synthesize" {
		t.Errorf("wide profile projection wrong: %+v", wide)
	}
	// The profile's explorers keep their AUTHORED (preference) order — what --count selects from.
	if len(wide.Explorers) != 3 || wide.Explorers[0].Model != "z-first" || wide.Explorers[2].Model != "m-third" {
		t.Errorf("profile explorers must be in preference order: %+v", wide.Explorers)
	}
	if wide.Collator.Model != "wide-c" {
		t.Errorf("profile collator wrong: %+v", wide.Collator)
	}
	// The legacy top-level `roster` still reports the DEFAULT profile's roster (additive-only projection).
	if len(view.Roster.Explorers) != 2 || view.Roster.Explorers[0].Model != "base-a" {
		t.Errorf("top-level roster must stay the default profile's roster: %+v", view.Roster.Explorers)
	}

	// The human summary carries the same section.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"list"}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, errb.String())
	}
	for _, want := range []string{
		"Profiles (default: base):", "base (default)", "wide; mode: synthesize",
		"explorers (preference order):", "fake:z-first:high", "fake:wide-c",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list human output missing %q:\n%s", want, out.String())
		}
	}
}

func TestUnknownCommand_Usage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"frobnicate"}, &out, &errb); code == 0 {
		t.Fatal("unknown command should be non-zero")
	}
}

// TestInit_Folder_CreatesState smoke-tests `folder init` end-to-end through the CLI dispatch: in a
// non-repo temp dir it must create .aimesh/temp and report success.
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

	var out, errb bytes.Buffer
	if code := Run([]string{"folder", "init"}, &out, &errb); code != 0 {
		t.Fatalf("folder init exit %d, stderr: %s", code, errb.String())
	}
	if fi, err := os.Stat(dir + "/.aimesh/temp"); err != nil || !fi.IsDir() {
		t.Fatalf(".aimesh/temp not created: err=%v", err)
	}
	if !strings.Contains(out.String(), "initialized") {
		t.Errorf("init output missing confirmation:\n%s", out.String())
	}
}
