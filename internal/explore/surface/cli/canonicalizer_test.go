package cli

// CLI surface tests for the CANONICALIZER spec (design §4): `--canonicalizer` names the two identities
// that propose the canonicalization, `setup --canonicalizer` persists them on a profile, and the run
// record says which identities held the merge-agreement rule AND who chose them.
//
// The rule under test is compose-not-configure: a request may select and order identities the operator
// already configured, never introduce an adapter — and it is 0 or 2 entries, never 1.

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

// TestExplore_CanonicalizerFlagIsHonoredAndRecorded runs a dual-canonicalizer mode with an explicit pair
// and checks the run record: the identities that actually canonicalized are the ones named, and the
// provenance says `explicit`.
func TestExplore_CanonicalizerFlagIsHonoredAndRecorded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", dir)
	var out, errb bytes.Buffer
	code := Run([]string{"explore", "--purpose", "pick a datastore", "--criteria", "latency",
		"--mode", "shortlist", "--dump-run",
		"--canonicalizer", "adapter=fake,model=canon-one,effort=high",
		"--canonicalizer", "adapter=fake,model=canon-two"}, &out, &errb)
	if code != 0 {
		t.Fatalf("explore --canonicalizer exit %d, stderr: %s", code, errb.String())
	}
	// The pre-spend panel statement names them, so a caller sees what will canonicalize before paying.
	if !strings.Contains(errb.String(), "canonicalizers (explicit):") || !strings.Contains(errb.String(), "fake:canon-one:high") {
		t.Errorf("the pre-spend panel must state the explicit canonicalizers:\n%s", errb.String())
	}
	manifest := readJSON(t, filepath.Join(latestRunDir(t, dir), "manifest.json"))
	if got := manifest["canonicalizerProvenance"]; got != "explicit" {
		t.Errorf("manifest canonicalizerProvenance = %v, want %q", got, "explicit")
	}
	models := manifestCanonicalizerModels(t, manifest)
	if len(models) != 2 || models[0] != "canon-one" || models[1] != "canon-two" {
		t.Errorf("manifest canonicalizers = %v, want [canon-one canon-two] in the order named", models)
	}
}

// TestExplore_DerivedCanonicalizersAreRecordedAsDerived pins the OTHER half of the provenance: a run that
// named none records `derived` and the identities the host chose. Before this, a derived-and-arbitrary
// choice was indistinguishable from a deliberate one in every artifact a run produced.
func TestExplore_DerivedCanonicalizersAreRecordedAsDerived(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", dir)
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "pick a datastore", "--criteria", "latency",
		"--mode", "shortlist", "--dump-run"}, &out, &errb); code != 0 {
		t.Fatalf("explore exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "canonicalizers: derived") {
		t.Errorf("the pre-spend panel must state that the canonicalizers will be derived:\n%s", errb.String())
	}
	manifest := readJSON(t, filepath.Join(latestRunDir(t, dir), "manifest.json"))
	if got := manifest["canonicalizerProvenance"]; got != "derived" {
		t.Errorf("manifest canonicalizerProvenance = %v, want %q", got, "derived")
	}
	// Slot a is the collator's identity; slot b the first explorer by PREFERENCE order that differs from it
	// (the seeded profile's preference-first is fake-a, and the collator is fake-c).
	models := manifestCanonicalizerModels(t, manifest)
	if len(models) != 2 || models[0] != "fake-c" || models[1] != "fake-a" {
		t.Errorf("derived canonicalizers = %v, want [fake-c fake-a] (collator, then the preference-first explorer)", models)
	}
}

// TestExplore_CanonicalizerSpecRefusals pins every pre-spend refusal on this flag. Each one must cost
// nothing: they are usage/config errors raised before a single explorer token is spent.
func TestExplore_CanonicalizerSpecRefusals(t *testing.T) {
	base := []string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "shortlist"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"one entry is ambiguous", []string{"--canonicalizer", "adapter=fake,model=only-one"}, "which slot it fills"},
		{"two identical identities", []string{
			"--canonicalizer", "adapter=fake,model=twin,effort=high",
			"--canonicalizer", "adapter=fake,model=twin,effort=low"}, "independent"},
		{"three entries", []string{
			"--canonicalizer", "adapter=fake,model=a", "--canonicalizer", "adapter=fake,model=b",
			"--canonicalizer", "adapter=fake,model=c"}, "exactly 2"},
		{"malformed spec", []string{"--canonicalizer", "fake:some-model", "--canonicalizer", "adapter=fake,model=b"}, "want key=value"},
		{"missing model", []string{"--canonicalizer", "adapter=fake", "--canonicalizer", "adapter=fake,model=b"}, "adapter and model are required"},
		{"unconfigured adapter", []string{
			"--canonicalizer", "adapter=not-a-real-adapter,model=m",
			"--canonicalizer", "adapter=fake,model=b"}, "unknown adapter(s)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := Run(append(append([]string(nil), base...), c.args...), &out, &errb); code == 0 {
				t.Fatalf("a malformed canonicalizer spec must be refused before any spend; stdout:\n%s", out.String())
			}
			if !strings.Contains(errb.String(), c.want) {
				t.Errorf("refusal %q does not mention %q", errb.String(), c.want)
			}
		})
	}
}

// TestSetupAndList_CanonicalizersRoundTrip pins the CONFIG half: `setup --profile --canonicalizer` persists
// the pair, `list` reports it, and a run on that profile uses it without any flag.
func TestSetupAndList_CanonicalizersRoundTrip(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, localstate.HomeDirName, roster.ComponentName), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMESH_HOME", home)

	var out, errb bytes.Buffer
	code := Run([]string{"setup", "--profile", "default",
		"--explorer", "adapter=fake,model=fake-a,effort=high",
		"--explorer", "adapter=fake,model=fake-b,effort=medium",
		"--collator", "adapter=fake,model=fake-c,effort=high",
		"--canonicalizer", "adapter=fake,model=canon-x",
		"--canonicalizer", "adapter=fake,model=canon-y"}, &out, &errb)
	if code != 0 {
		t.Fatalf("setup --canonicalizer exit %d: %s", code, errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"list", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	var view listView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("list --json: %v", err)
	}
	if view.Roster.CanonicalizerSource != "explicit" || len(view.Roster.Canonicalizers) != 2 {
		t.Fatalf("list roster canonicalizers = %+v (source %q), want the saved pair", view.Roster.Canonicalizers, view.Roster.CanonicalizerSource)
	}
	if view.Roster.Canonicalizers[0].Model != "canon-x" || view.Roster.Canonicalizers[1].Model != "canon-y" {
		t.Errorf("list reported %+v, want canon-x then canon-y", view.Roster.Canonicalizers)
	}
	if len(view.Profiles.Profiles) != 1 || view.Profiles.Profiles[0].CanonicalizerSource != "explicit" {
		t.Errorf("the profile projection must carry the canonicalizer source: %+v", view.Profiles.Profiles)
	}

	// A run on that profile uses the SAVED pair with no flag at all.
	dir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", dir)
	out.Reset()
	errb.Reset()
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "shortlist", "--dump-run"}, &out, &errb); code != 0 {
		t.Fatalf("explore on the saved profile exit %d: %s", code, errb.String())
	}
	manifest := readJSON(t, filepath.Join(latestRunDir(t, dir), "manifest.json"))
	if got := manifest["canonicalizerProvenance"]; got != "explicit" {
		t.Errorf("a profile-supplied pair must record provenance %q, got %v", "explicit", got)
	}
	if models := manifestCanonicalizerModels(t, manifest); len(models) != 2 || models[0] != "canon-x" {
		t.Errorf("the run used %v, want the profile's saved canonicalizers", models)
	}
}

// TestSetup_RejectsHalfACanonicalizerSpec pins that the config surface refuses the same shape the run
// surface does — a profile is where a half-specified governance rule would persist unnoticed.
func TestSetup_RejectsHalfACanonicalizerSpec(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, localstate.HomeDirName, roster.ComponentName), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMESH_HOME", home)
	var out, errb bytes.Buffer
	code := Run([]string{"setup", "--profile", "default",
		"--explorer", "adapter=fake,model=fake-a", "--explorer", "adapter=fake,model=fake-b",
		"--collator", "adapter=fake,model=fake-c",
		"--canonicalizer", "adapter=fake,model=only-one"}, &out, &errb)
	if code == 0 {
		t.Fatal("setup accepted a one-entry canonicalizer spec")
	}
	if !strings.Contains(errb.String(), "which slot it fills") {
		t.Errorf("refusal %q must teach why one entry is ambiguous", errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, localstate.HomeDirName, roster.ComponentName, profile.FileName)); err == nil {
		t.Error("a refused setup must persist nothing")
	}
}

// TestDoctor_ReportsCanonicalizerResolution pins that `doctor` states which identities will hold the
// merge-agreement rule — the seat whose selection previously had no visible source anywhere.
func TestDoctor_ReportsCanonicalizerResolution(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"doctor"}, &out, &errb); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "roster: canonicalizers") {
		t.Fatalf("doctor must report the canonicalizer resolution:\n%s", s)
	}
	if !strings.Contains(s, "derived") || !strings.Contains(s, "fake:fake-a") {
		t.Errorf("doctor must name the identity slot b would resolve to:\n%s", s)
	}
}

// manifestCanonicalizerModels reads the manifest's recorded canonicalizer models, in order.
func manifestCanonicalizerModels(t *testing.T, manifest map[string]any) []string {
	t.Helper()
	raw, _ := manifest["canonicalizers"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		s, _ := m["model"].(string)
		out = append(out, s)
	}
	return out
}
