package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
)

func TestDoctor_UsesConfiguredAdapterPath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := shell.Recipes()["claude-code"]
	cfg := config.WithExampleProfiles(config.Default())

	// configured path (binary not on PATH) → claude-code reported available
	reg := map[string]model.Adapter{"fake": fake.New(fake.Valid), "claude-code": shell.New(rec, bin, time.Minute)}
	rep := Run(Input{Config: cfg, Adapters: reg, ArtifactDir: t.TempDir(), Profile: "claude-code-smoke"})
	if !rep.OK || !strings.Contains(rep.String(), "configured path") {
		t.Errorf("configured path should make claude-code available:\n%s", rep.String())
	}

	// invalid configured path → doctor fails, naming the adapter
	reg2 := map[string]model.Adapter{"fake": fake.New(fake.Valid), "claude-code": shell.New(rec, "/no/such/bin", time.Minute)}
	rep2 := Run(Input{Config: cfg, Adapters: reg2, ArtifactDir: t.TempDir(), Profile: "claude-code-smoke"})
	if rep2.OK {
		t.Error("invalid configured path should fail doctor")
	}
	if !strings.Contains(rep2.String(), "claude-code") {
		t.Error("failing check should name the adapter")
	}
}

func registry() map[string]model.Adapter {
	return map[string]model.Adapter{"fake": fake.New(fake.Valid)}
}

// stubAdapter is an adapter with a fixed availability, for doctor visibility tests.
type stubAdapter struct {
	name  string
	avail bool
}

func (s stubAdapter) Name() string { return s.name }
func (s stubAdapter) Available() (bool, string) {
	if s.avail {
		return true, "present"
	}
	return false, "not installed"
}
func (s stubAdapter) Invoke(context.Context, model.Call) (model.Result, error) {
	return model.Result{}, nil
}

// probeStub additionally implements model.Prober (for doctor --probe tests).
type probeStub struct {
	stubAdapter
	probeOK bool
}

func (p probeStub) Probe(context.Context) model.ProbeResult {
	if p.probeOK {
		return model.ProbeResult{OK: true, Stage: "version", Detail: "probe ok: stub 1.0"}
	}
	return model.ProbeResult{OK: false, Stage: "version", Detail: "probe timed out — native login may be required"}
}

func TestDoctor_DefaultConfig_OK(t *testing.T) {
	// The shipped `default` profile is now UNCONFIGURED (Doctor flags it); the shipped-but-hidden
	// fake profile is the ready one, so target it here to exercise the all-green path. The hidden
	// profile resolves only under the internal test-harness gate.
	t.Setenv(fake.EnvVar, "1")
	rep := Run(Input{Config: config.Default(), Adapters: registry(), ArtifactDir: t.TempDir(), Workspace: t.TempDir(), Profile: config.FakeProfile})
	if !rep.OK {
		t.Errorf("doctor not OK with the shipped fake profile:\n%s", rep.String())
	}
}

// A fresh install targeting the shipped (unconfigured) `default` profile must be FLAGGED, not OK —
// the honest "configure your adapters" posture.
func TestDoctor_UnconfiguredDefaultProfile_Flagged(t *testing.T) {
	rep := Run(Input{Config: config.Default(), Adapters: registry(), ArtifactDir: t.TempDir(), Profile: "default"})
	if rep.OK {
		t.Errorf("doctor must flag the unconfigured shipped default profile:\n%s", rep.String())
	}
}

func TestDoctor_MissingDefaultProfile_Fails(t *testing.T) {
	cfg := config.Default()
	cfg.DefaultProfile = "nope"
	rep := Run(Input{Config: cfg, Adapters: registry(), ArtifactDir: t.TempDir()})
	if rep.OK {
		t.Error("doctor should fail when defaultProfile is unresolvable")
	}
}

func TestDoctor_OptInCloudMissing_StillOK(t *testing.T) {
	t.Setenv(fake.EnvVar, "1") // the hidden fake profile resolves only under the internal gate
	reg := map[string]model.Adapter{"fake": fake.New(fake.Valid), "ollama": stubAdapter{"ollama", false}}
	rep := Run(Input{Config: config.Default(), Adapters: reg, ArtifactDir: t.TempDir(), Profile: config.FakeProfile})
	if !rep.OK {
		t.Errorf("a missing opt-in adapter must not fail doctor:\n%s", rep.String())
	}
}

func TestDoctor_RequestedProfileOverridesBrokenDefault(t *testing.T) {
	// The default profile is "broken" (its adapter unavailable) but the user requested a working
	// (fake) profile → doctor must not fail on the unused default.
	t.Setenv(fake.EnvVar, "1") // the hidden fake profile resolves only under the internal gate
	cfg := config.WithOllamaModel(config.WithExampleProfiles(config.Default()), "x:tag")
	cfg.DefaultProfile = "fully-local-ollama"
	reg := map[string]model.Adapter{"fake": fake.New(fake.Valid), "ollama": stubAdapter{"ollama", false}}
	rep := Run(Input{Config: cfg, Adapters: reg, ArtifactDir: t.TempDir(), Profile: config.FakeProfile})
	if !rep.OK {
		t.Errorf("requested working profile should pass despite a broken default:\n%s", rep.String())
	}
}

func TestDoctor_Probe(t *testing.T) {
	cfg := config.WithOllamaModel(config.WithExampleProfiles(config.Default()), "x:tag")
	// passing probe → doctor OK with a probe check
	reg := map[string]model.Adapter{"fake": fake.New(fake.Valid), "ollama": probeStub{stubAdapter{"ollama", true}, true}}
	rep := Run(Input{Config: cfg, Adapters: reg, ArtifactDir: t.TempDir(), Profile: "fully-local-ollama", Probe: true})
	if !rep.OK || !strings.Contains(rep.String(), "probe: ollama") {
		t.Errorf("expected a passing probe check:\n%s", rep.String())
	}
	// failing probe → doctor not OK
	reg2 := map[string]model.Adapter{"fake": fake.New(fake.Valid), "ollama": probeStub{stubAdapter{"ollama", true}, false}}
	rep2 := Run(Input{Config: cfg, Adapters: reg2, ArtifactDir: t.TempDir(), Profile: "fully-local-ollama", Probe: true})
	if rep2.OK {
		t.Error("a failing probe should fail doctor")
	}
}

func TestDoctor_SelectedOllamaProfileUnavailable_NotReady(t *testing.T) {
	cfg := config.WithOllamaModel(config.WithExampleProfiles(config.Default()), "x:tag")
	reg := map[string]model.Adapter{"fake": fake.New(fake.Valid), "ollama": stubAdapter{"ollama", false}}
	rep := Run(Input{Config: cfg, Adapters: reg, ArtifactDir: t.TempDir(), Profile: "fully-local-ollama"})
	if rep.OK {
		t.Error("doctor should not be OK when an explicitly selected profile's adapter is unavailable")
	}
}
