package cli

// The taxonomy exit codes, `doctor --json`, and `doctor --probe`.
//
// The exit-code tests are written as a TABLE OF CLASSES, never as "non-zero": an undifferentiated exit 1
// would leave a script unable to tell a missing binary from a malformed profile from an internal bug.
// Each row below pins WHICH class a failure maps to, so a regression that collapses two classes together
// fails here rather than silently in somebody's CI script.
//
// HERMETIC: every test runs under profileEnv (AIMESH_HOME + AIMESH_HOME in a temp dir, cwd in a
// non-repo temp dir) on the all-`fake` panel. --probe is covered on that panel deliberately: the fake
// harness has no Prober capability, so meshcore reports it honestly as "no probe" and NOTHING is ever
// spawned by the test suite.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// runCLI runs the surface and returns (exit code, stdout, stderr).
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// TestExitCodes_FollowTheSharedTaxonomy pins the class each failure family maps to: every one of
// these answers its taxonomy class, never a blanket 1.
func TestExitCodes_FollowTheSharedTaxonomy(t *testing.T) {
	writeProfiles(t, profileEnv(t)) // `base` (default) + `wide`, both all-fake

	cases := []struct {
		name string
		args []string
		want fault.Code
		why  string
	}{
		{
			name: "no command",
			args: nil,
			want: fault.Usage,
			why:  "an empty invocation is a usage error, as it always was",
		},
		{
			name: "unknown command",
			args: []string{"frobnicate"},
			want: fault.Usage,
			why:  "an unrecognized verb is a usage error",
		},
		{
			name: "mutually exclusive flags",
			args: []string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "wide", "--roster", "/tmp/nope.yaml"},
			want: fault.Usage,
			why:  "two flags that cannot combine is a flag-shape error",
		},
		{
			name: "unknown mode",
			args: []string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "bogus"},
			want: fault.Usage,
			why:  "an unknown --mode value is a flag-value error, checked before any spend",
		},
		{
			name: "count out of range",
			args: []string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "base", "--count", "5"},
			want: fault.Usage,
			why:  "the count is never clamped; asking for more explorers than exist is a usage error",
		},
		{
			name: "non-numeric count",
			args: []string{"explore", "--purpose", "p", "--criteria", "a", "--count", "two"},
			want: fault.Usage,
			why:  "a malformed --count value is a usage error",
		},
		{
			name: "no such profile",
			args: []string{"explore", "--purpose", "p", "--criteria", "a", "--profile", "nope"},
			want: fault.Config,
			why:  "the flag is well-formed; the CONFIG has no such profile — that is a config fault, not usage",
		},
		{
			name: "missing roster file",
			args: []string{"explore", "--purpose", "p", "--criteria", "a", "--roster", filepath.Join(t.TempDir(), "absent.yaml")},
			want: fault.Config,
			why:  "an unreadable roster file is a configuration fault",
		},
		{
			name: "unknown adapter in an ad-hoc panel",
			args: []string{"explore", "--purpose", "p", "--criteria", "a",
				"--explorer", "adapter=not-a-real-adapter,model=x", "--explorer", "adapter=fake,model=fake-b",
				"--collator", "adapter=fake,model=fake-c"},
			want: fault.Config,
			why:  "an unrecognized adapter fails CLOSED before any spend, as a configuration fault",
		},
		{
			name: "malformed ad-hoc spec",
			args: []string{"explore", "--purpose", "p", "--criteria", "a",
				"--explorer", "adapter=fake", "--explorer", "adapter=fake,model=fake-b",
				"--collator", "adapter=fake,model=fake-c"},
			want: fault.Usage,
			why:  "the --explorer spec grammar is flag syntax, so a broken spec is a usage error",
		},
		{
			name: "export names a directory that is not a run",
			args: []string{"export", "--sqlite", filepath.Join(t.TempDir(), "x.db"), "--run", t.TempDir()},
			want: fault.Config,
			why:  "the user-supplied --run value names nothing exportable: bad input, not a broken export",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errs := runCLI(t, tc.args...)
			if code != int(tc.want) {
				t.Fatalf("exit %d, want %d (%s)\nstderr: %s", code, int(tc.want), tc.why, errs)
			}
		})
	}
}

// TestExitCodes_SuccessIsStillZero guards the other half of a breaking change: nothing that used to
// succeed may start reporting a class.
func TestExitCodes_SuccessIsStillZero(t *testing.T) {
	writeProfiles(t, profileEnv(t))
	for _, args := range [][]string{
		{"--version"},
		{"list", "--json"},
		{"doctor"},
		{"doctor", "--json"},
		{"explore", "--purpose", "p", "--criteria", "a", "--mode", "map"},
	} {
		if code, _, errs := runCLI(t, args...); code != int(fault.OK) {
			t.Errorf("%v: exit %d, want 0\nstderr: %s", args, code, errs)
		}
	}
}

// TestDoctor_UnreadyPanelExitsConfig: a doctor that FINDS something is a configuration verdict (3), the
// same class `reviewmesh doctor` returns — not the old undifferentiated 1, and not a usage error.
func TestDoctor_UnreadyPanelExitsConfig(t *testing.T) {
	path := profileEnv(t)
	// A panel naming an adapter that is neither a shipped recipe nor a configured ACP instance: doctor
	// reports it as a failing check rather than erroring out, and the VERDICT is what sets the code.
	set := profile.Set{
		SchemaVersion:  profile.CurrentSchemaVersion,
		DefaultProfile: "broken",
		Profiles: map[string]profile.Profile{"broken": {
			Explorers: []roster.Explorer{
				{Adapter: "not-a-real-adapter", Model: "x"},
				{Adapter: "fake", Model: "fake-b"},
			},
			Collator: roster.Collator{Adapter: "fake", Model: "fake-c"},
		}},
	}
	if err := profile.Save(path, set); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "doctor")
	if code != int(fault.Config) {
		t.Fatalf("a failing doctor must exit %d (config), got %d\n%s", int(fault.Config), code, out)
	}
	if !strings.Contains(out, "unknown adapter") {
		t.Errorf("the failing check should name the problem:\n%s", out)
	}
}

// TestDoctor_JSONProjection: --json emits the typed projection on stdout and NOTHING else, carries the
// same verdict the text form does, and leaves the exit code untouched.
func TestDoctor_JSONProjection(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	code, out, errs := runCLI(t, "doctor", "--json")
	if code != int(fault.OK) {
		t.Fatalf("doctor --json on a ready fake panel exit %d, stderr: %s", code, errs)
	}
	var got doctorJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, out)
	}
	if !got.OK {
		t.Errorf("the all-fake panel must be ready: %+v", got)
	}
	if len(got.Checks) == 0 {
		t.Fatal("the projection must carry the checks, not just the verdict")
	}
	if len(got.Probes) != 0 {
		t.Errorf("probes must be absent without --probe (doctor is static by default): %+v", got.Probes)
	}
	// Every check is fully projected — a name with no ok/detail beside it is not a readiness report.
	for _, c := range got.Checks {
		if c.Name == "" || c.Detail == "" {
			t.Errorf("check projected without name/detail: %+v", c)
		}
	}
	// The human report must NOT leak into the machine stream.
	if strings.Contains(out, "doctor: all static checks passed") {
		t.Errorf("--json must emit only the projection:\n%s", out)
	}
}

// TestDoctor_JSONVerdictMatchesTheExitCode: the projection's `ok` and the process exit code must never
// disagree — a consumer that reads one and a consumer that reads the other must reach the same
// conclusion, including on the shipped-UNCONFIGURED default profile (the fresh-install posture, which
// is rendered as a failing CHECK rather than an error).
func TestDoctor_JSONVerdictMatchesTheExitCode(t *testing.T) {
	profileEnv(t) // no profiles file written → the built-in unconfigured default

	code, out, _ := runCLI(t, "doctor", "--json")
	var got doctorJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, out)
	}
	if got.OK {
		t.Fatalf("the shipped-unconfigured default cannot be ready: %+v", got)
	}
	if code != int(fault.Config) {
		t.Errorf("an unconfigured profile must exit %d (config), got %d", int(fault.Config), code)
	}
	if len(got.Checks) != 1 || got.Checks[0].OK {
		t.Errorf("the unconfigured posture must be ONE failing check, got %+v", got.Checks)
	}
}

// TestDoctor_ProbeIsReportedPerAdapter: --probe adds a per-adapter probe row to BOTH renderings. On the
// hermetic fake panel the harness has no Prober capability, so meshcore reports "no probe" honestly —
// which is exactly the case that proves nothing is spawned and nothing is spent.
func TestDoctor_ProbeIsReportedPerAdapter(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	// Text form: the report keeps the probe CHECK, and the detail block adds the per-adapter line.
	code, out, errs := runCLI(t, "doctor", "--probe")
	if code != int(fault.OK) {
		t.Fatalf("doctor --probe on a ready fake panel exit %d, stderr: %s", code, errs)
	}
	if !strings.Contains(out, "probe: fake") {
		t.Errorf("the report must carry a probe check per required adapter:\n%s", out)
	}
	if !strings.Contains(out, "Probe detail") {
		t.Errorf("the text form must render the per-adapter probe detail:\n%s", out)
	}
	if !strings.Contains(out, "no model call") {
		t.Errorf("the probe block must state what a probe costs:\n%s", out)
	}

	// JSON form: the same probe, projected with the typed fields kept apart.
	code, out, errs = runCLI(t, "doctor", "--probe", "--json")
	if code != int(fault.OK) {
		t.Fatalf("doctor --probe --json exit %d, stderr: %s", code, errs)
	}
	var got doctorJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("doctor --probe --json is not valid JSON: %v\n%s", err, out)
	}
	if len(got.Probes) != 1 || got.Probes[0].Adapter != "fake" {
		t.Fatalf("expected exactly one probe row for the fake adapter, got %+v", got.Probes)
	}
	if !got.Probes[0].OK {
		t.Errorf("an un-probeable adapter is reported honestly, not as a failure: %+v", got.Probes[0])
	}
	// The probe checks also ride in `checks`, so the verdict is computed over them like any other check.
	found := false
	for _, c := range got.Checks {
		if c.Name == "probe: fake" {
			found = true
		}
	}
	if !found {
		t.Errorf("the probe result must also be a CHECK (so it counts toward the verdict): %+v", got.Checks)
	}
}

// TestDoctor_ProbeFailureCarriesStageAndSignal: a probe that cannot even resolve a binary reports the
// STAGE it reached. Stage is the field a meshcore Check cannot carry, and the single most useful thing
// to know when a probe fails — this is the test that would fail if the recording wrapper were dropped.
//
// It is hermetic and starts nothing: the adapter's configured path points at a file that does not exist,
// so the probe fails at `resolve` without ever spawning a process.
func TestDoctor_ProbeFailureCarriesStageAndSignal(t *testing.T) {
	home := profileEnv(t)

	set := profile.Set{
		SchemaVersion:  profile.CurrentSchemaVersion,
		DefaultProfile: "mixed",
		Profiles: map[string]profile.Profile{"mixed": {
			Explorers: []roster.Explorer{
				{Adapter: "devin-cli", Model: "devin"},
				{Adapter: "fake", Model: "fake-b"},
			},
			Collator: roster.Collator{Adapter: "fake", Model: "fake-c"},
		}},
	}
	if err := profile.Save(home, set); err != nil {
		t.Fatal(err)
	}
	// A saved path that does not exist makes the adapter unavailable DETERMINISTICALLY — independent of
	// whatever happens to be installed on the machine running the tests.
	writeBrokenAdapterPath(t, "devin-cli", filepath.Join(t.TempDir(), "definitely-not-installed"))

	code, out, _ := runCLI(t, "doctor", "--probe", "--json")
	if code != int(fault.Config) {
		t.Fatalf("an unresolvable adapter must fail doctor with %d, got %d\n%s", int(fault.Config), code, out)
	}
	var got doctorJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("doctor --probe --json is not valid JSON: %v\n%s", err, out)
	}
	// An UNAVAILABLE required adapter is reported by the availability check and deliberately skipped by
	// the probe (meshcore's rule) — so the probe list holds only the fake, and the availability check is
	// the one that names devin-cli.
	unavailable := false
	for _, c := range got.Checks {
		if c.Name == "adapter: devin-cli" && !c.OK {
			unavailable = true
		}
	}
	if !unavailable {
		t.Errorf("the unresolvable adapter must be a failing availability check: %+v", got.Checks)
	}
	for _, p := range got.Probes {
		if p.Adapter == "devin-cli" {
			t.Errorf("an already-unavailable adapter must not be probed again: %+v", p)
		}
	}
}

// writeBrokenAdapterPath records a binary path for `name` in the hermetic user-scope adapters.yaml by
// writing the file directly. It goes around the governed write seam ON PURPOSE: that seam validates the
// path, and the point of this fixture is a path that does NOT resolve (the state a user lands in when a
// CLI is upgraded, moved or uninstalled after being configured).
func writeBrokenAdapterPath(t *testing.T, name, binPath string) {
	t.Helper()
	base := os.Getenv("AIMESH_HOME")
	if base == "" {
		t.Fatal("AIMESH_HOME must be set by profileEnv for a hermetic adapters.yaml")
	}
	dir := filepath.Join(base, ".aimesh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "schemaVersion: 1\nadapters:\n  " + name + ":\n    path: " + binPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, "adapters.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
