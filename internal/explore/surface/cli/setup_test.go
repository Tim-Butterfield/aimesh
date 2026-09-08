package cli

// The headless `setup` surface. Everything the config-only web workbench can do must be
// reachable from a terminal, through the SAME governed manager seams, on a machine with no built SPA.
//
// HERMETIC: every test runs under profileEnv (AIMESH_HOME + AIMESH_HOME in a temp dir, cwd in a
// non-repo temp dir), and NO test invokes a real CLI. `--acp detect` / `--acp add` deliberately launch
// the candidate binary — that is the only way to learn whether it speaks ACP — so they are covered by
// their usage guards only; the paths that touch config without launching anything (adapter paths, ACP
// remove, profiles) are exercised end-to-end.

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

// setup runs the setup surface and returns (exit code, stdout, stderr).
func setup(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(append([]string{"setup"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// stubBinary writes an executable file and returns its path. It is only ever STAT'd (the adapter-path
// seam validates existence/executability); nothing in these tests runs it.
func stubBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stub-cli")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// listAdapters returns `list --json`'s adapters keyed by name — the read-back path a user actually has,
// so a setup write is verified through the same projection the UI renders rather than by re-reading YAML.
func listAdapters(t *testing.T) map[string]listAdapter {
	t.Helper()
	var out, errb bytes.Buffer
	if code := Run([]string{"list", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("list --json exit %d, stderr: %s", code, errb.String())
	}
	var view listView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out.String())
	}
	byName := map[string]listAdapter{}
	for _, a := range view.Adapters {
		byName[a.Name] = a
	}
	return byName
}

// listProfilesView returns `list --json`'s profile section.
func listProfilesView(t *testing.T) listProfiles {
	t.Helper()
	var out, errb bytes.Buffer
	if code := Run([]string{"list", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("list --json exit %d, stderr: %s", code, errb.String())
	}
	var view listView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out.String())
	}
	return view.Profiles
}

// TestSetup_RequiresExactlyOneAction: setup WRITES config, so a call that did two things (or nothing) is
// refused before anything is touched rather than leaving the user unsure what state they are now in.
func TestSetup_RequiresExactlyOneAction(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	code, _, errs := setup(t)
	if code != 2 {
		t.Fatalf("a bare `setup` must be a usage error, got %d", code)
	}
	if !strings.Contains(errs, "nothing to do") {
		t.Errorf("guidance should name the available actions:\n%s", errs)
	}

	code, _, errs = setup(t, "--remove-adapter", "codex-cli", "--delete-profile", "base")
	if code != 2 {
		t.Fatalf("two actions must be a usage error, got %d", code)
	}
	if !strings.Contains(errs, "exactly ONE action") {
		t.Errorf("guidance should demand a single action:\n%s", errs)
	}
}

// TestSetup_ShapeGuards pins every usage guard that must fire BEFORE any config is read or written —
// including the two that stand in front of the ACP probes, which are the only paths that would launch a
// real CLI.
func TestSetup_ShapeGuards(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"adapter without path", []string{"--adapter", "claude-code"}, "--adapter requires --path"},
		{"path without adapter", []string{"--path", "/bin/true"}, "--path requires --adapter"},
		{"unknown acp action", []string{"--acp", "bogus"}, "unknown --acp action"},
		{"acp detect without path", []string{"--acp", "detect"}, "requires --path"},
		{"acp add without path", []string{"--acp", "add"}, "requires --path"},
		{"acp remove without name", []string{"--acp", "remove"}, "requires --name"},
		{"delete without confirmation", []string{"--delete-profile", "wide"}, "--yes"},
		{"orphan profile flags", []string{"--remove-adapter", "codex-cli", "--collator", "adapter=fake,model=z"}, "apply only with --profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errs := setup(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (usage)", code)
			}
			if !strings.Contains(errs, tc.want) {
				t.Errorf("error should contain %q:\n%s", tc.want, errs)
			}
		})
	}
}

// TestSetup_AdapterPathRoundTrip is the headless install's core loop: record a binary path, see it in
// `list`, then clear it. It proves the CLI writes the SAME shared adapters.yaml the workbench edits (the
// read-back goes through the manager projection `list` and the UI both render).
func TestSetup_AdapterPathRoundTrip(t *testing.T) {
	writeProfiles(t, profileEnv(t)) // a fake-only roster, so claude-code is unused and removable
	bin := stubBinary(t)

	code, out, errs := setup(t, "--adapter", "claude-code", "--path", bin)
	if code != 0 {
		t.Fatalf("recording an adapter path exit %d, stderr: %s", code, errs)
	}
	if !strings.Contains(out, "claude-code") {
		t.Errorf("setup should report what it recorded:\n%s", out)
	}
	if got := listAdapters(t)["claude-code"]; !got.Configured || got.Path != bin {
		t.Errorf("list should report claude-code configured at %q, got %+v", bin, got)
	}

	if code, _, errs = setup(t, "--remove-adapter", "claude-code"); code != 0 {
		t.Fatalf("clearing the adapter path exit %d, stderr: %s", code, errs)
	}
	if got := listAdapters(t)["claude-code"]; got.Path != "" {
		t.Errorf("the saved path should be gone, got %+v", got)
	}

	// Clearing a path that was never recorded is reported, never a phantom write.
	if code, _, errs = setup(t, "--remove-adapter", "claude-code"); code == 0 {
		t.Errorf("removing an unconfigured adapter should fail, stderr: %s", errs)
	}
}

// TestSetup_RemoveAdapterBlockedWhileInUse: the governed seam refuses to strip an adapter the roster
// still references, and the CLI LISTS the blocking slots — the user is told what to reconfigure, not
// merely that the removal was refused.
func TestSetup_RemoveAdapterBlockedWhileInUse(t *testing.T) {
	path := profileEnv(t)
	set := profile.Set{
		SchemaVersion:  profile.CurrentSchemaVersion,
		DefaultProfile: "real",
		Profiles: map[string]profile.Profile{
			"real": {
				Explorers: []roster.Explorer{
					{Adapter: "claude-code", Model: "sonnet"},
					{Adapter: "fake", Model: "fake-b"},
				},
				Collator: roster.Collator{Adapter: "fake", Model: "fake-c"},
			},
		},
	}
	if err := profile.Save(path, set); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := setup(t, "--adapter", "claude-code", "--path", stubBinary(t)); code != 0 {
		t.Fatalf("recording the path exit %d, stderr: %s", code, errs)
	}

	// A blocked removal is a CONFIGURATION refusal in the shared taxonomy (exit 3): the request was
	// well-formed, the config is simply in a state that forbids it.
	code, _, errs := setup(t, "--remove-adapter", "claude-code")
	if code != int(fault.Config) {
		t.Fatalf("removing an in-use adapter must fail with the config code %d, got %d", int(fault.Config), code)
	}
	if !strings.Contains(errs, "in use by") || !strings.Contains(errs, "explorer 1") {
		t.Errorf("the refusal should list the blocking roster slot(s):\n%s", errs)
	}
}

// TestSetup_ProfileLifecycle covers create → delete over the profile seams, including the two rules
// that keep a config usable: the default profile is PINNED (creating another profile never moves it —
// there is no set-default, as in reviewmesh) and it cannot be deleted.
func TestSetup_ProfileLifecycle(t *testing.T) {
	writeProfiles(t, profileEnv(t)) // `base` (default) + `wide`

	code, _, errs := setup(t, "--profile", "custom",
		"--explorer", "adapter=fake,model=c-one,effort=high",
		"--explorer", "adapter=fake,model=c-two",
		"--collator", "adapter=fake,model=c-collator",
		"--default-mode", "synthesize")
	if code != 0 {
		t.Fatalf("creating a profile exit %d, stderr: %s", code, errs)
	}
	view := listProfilesView(t)
	if view.DefaultProfile != "base" {
		t.Errorf("creating a profile must never move the default, got %q", view.DefaultProfile)
	}
	var found *listProfile
	for i, p := range view.Profiles {
		if p.Name == "custom" {
			found = &view.Profiles[i]
		}
	}
	if found == nil {
		t.Fatalf("the new profile is not listed: %+v", view.Profiles)
	}
	if found.DefaultMode != "synthesize" || len(found.Explorers) != 2 || found.Explorers[0].Model != "c-one" {
		t.Errorf("the profile did not round-trip as authored: %+v", *found)
	}

	// The DEFAULT profile cannot be deleted — a no-flag run always binds to it.
	if code, _, errs = setup(t, "--delete-profile", "base", "--yes"); code == 0 {
		t.Error("deleting the default profile must be refused")
	} else if !strings.Contains(errs, "default profile") {
		t.Errorf("the refusal should explain why:\n%s", errs)
	}

	// A non-default profile deletes cleanly.
	if code, _, errs = setup(t, "--delete-profile", "custom", "--yes"); code != 0 {
		t.Fatalf("deleting a non-default profile exit %d, stderr: %s", code, errs)
	}
	for _, p := range listProfilesView(t).Profiles {
		if p.Name == "custom" {
			t.Error("the deleted profile is still listed")
		}
	}

	// The removed set-default flag stays removed: naming it is an unknown-flag usage error.
	if code, _, _ = setup(t, "--set-default-profile", "base"); code != 2 {
		t.Errorf("--set-default-profile must no longer exist (want a usage error), got exit %d", code)
	}
}

// TestSetup_ProfileRejectsAnInvalidRoster: a profile below the 2-explorer minimum (or missing a
// collator) is refused at the surface, before the manager is asked to write anything.
func TestSetup_ProfileRejectsAnInvalidRoster(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	code, _, errs := setup(t, "--profile", "thin",
		"--explorer", "adapter=fake,model=only-one", "--collator", "adapter=fake,model=c")
	if code != 2 {
		t.Fatalf("a one-explorer profile must be a usage error, got %d", code)
	}
	if !strings.Contains(errs, "at least 2") {
		t.Errorf("the error should name the 2-explorer minimum:\n%s", errs)
	}
}

// TestSetup_ACPRemoveUnknown: removing a name that is not a saved ACP instance is a clear error. It
// launches nothing — only `detect`/`add` ever start a CLI — which is what makes it testable at all.
func TestSetup_ACPRemoveUnknown(t *testing.T) {
	writeProfiles(t, profileEnv(t))

	code, _, errs := setup(t, "--acp", "remove", "--name", "acp-nope")
	if code != int(fault.Config) {
		t.Fatalf("removing an unsaved ACP adapter must fail with the config code %d, got %d", int(fault.Config), code)
	}
	if !strings.Contains(errs, "acp-nope") {
		t.Errorf("the error should name the adapter:\n%s", errs)
	}
}

// TestSetup_ACPDetectValidatesThePathFirst: the binary path is checked (exists / not a directory / on
// POSIX executable) BEFORE the probe, so a typo'd path fails instantly instead of after a 90s launch
// attempt. It is also what makes `detect` safe to cover here — nothing is ever started.
func TestSetup_ACPDetectValidatesThePathFirst(t *testing.T) {
	dir := profileEnv(t)
	writeProfiles(t, dir)

	// A supplied path that does not resolve to a binary is a CONFIGURATION fault (exit 3) — the flag
	// shape was fine, the value names nothing runnable.
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	code, _, errs := setup(t, "--acp", "detect", "--path", missing)
	if code != int(fault.Config) {
		t.Fatalf("detect on a missing binary must fail with the config code %d, got %d", int(fault.Config), code)
	}
	if !strings.Contains(errs, "not found") {
		t.Errorf("the error should say the path was not found:\n%s", errs)
	}
	// A directory is not a binary either.
	code, _, errs = setup(t, "--acp", "add", "--path", t.TempDir())
	if code != int(fault.Config) {
		t.Fatalf("add with a directory must fail with the config code %d, got %d", int(fault.Config), code)
	}
	if !strings.Contains(errs, "directory") {
		t.Errorf("the error should say the path is a directory:\n%s", errs)
	}
}

// TestUsage_DocumentsSetup: a config surface nobody can discover is not a config surface.
func TestUsage_DocumentsSetup(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	s := out.String()
	for _, want := range []string{
		"setup --adapter", "--remove-adapter", "--acp detect", "--acp add", "--acp remove",
		"--delete-profile", "--acp-arg",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("usage does not document %q", want)
		}
	}
	// The set-default flags are REMOVED (the default is pinned to `default`, as in reviewmesh) —
	// usage must not advertise them.
	if strings.Contains(s, "--set-default") {
		t.Error("usage must not document a set-default flag — the runtime default is pinned to \"default\"")
	}
}
