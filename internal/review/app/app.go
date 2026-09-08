// Package app wires the reviewmesh components together (dependency assembly) and
// is shared by the surface Clients. The deterministic fake adapter is wired only
// when the internal test-harness gate (meshcore fake.Enabled) is on.
package app

import (
	"cmp"
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/setup"
	"github.com/Tim-Butterfield/aimesh/internal/review/utility/doctor"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
)

// App holds the resolved configuration and wired components. The config snapshot (Cfg/Layers/
// Adapters) is guarded by mu so a web-UI write's ReloadConfig cannot race a concurrent read
// handler (net/http serves each request in its own goroutine). Read the snapshot only via the
// accessor methods; they hand back a consistent copy taken under the lock.
type App struct {
	Cfg         config.Config
	Layers      config.Layers // which config layers were discovered/loaded (for doctor)
	Adapters    map[string]model.Adapter
	ArtifactDir string
	TempBase    string

	mu         sync.RWMutex  // guards Cfg/Layers/Adapters (swapped wholesale by loadConfig)
	configPath string        // explicit --config path (retained so ReloadConfig re-reads it)
	scenario   fake.Scenario // resolved fake scenario (retained for adapter rebuild on reload)
}

// ArtifactSubdir is reviewmesh's component name under the shared `.aimesh/` state directory, so the two
// aimesh apps never interleave their state. It is the DOMAIN word rather than the binary name: the
// directory holds what the app owns (its config and its run output), and that outlives whatever the
// binary happens to be called — which, after the CLI unification, is `aimesh`.
const ArtifactSubdir = "review"

// Options configures wiring.
type Options struct {
	ConfigPath string
	// ArtifactDir is the base for run dirs. Empty resolves through localstate.RunDir — see New.
	ArtifactDir  string
	TempBase     string        // base for isolated copies ("" = OS temp)
	FakeScenario fake.Scenario // override; "" → REVIEWMESH_FAKE_SCENARIO → valid
}

// New loads config and wires the components. With no explicit ConfigPath it
// auto-discovers a project config under <repo-root>/.aimesh/review (YAML preferred, then JSON).
//
// THE RUN DIRECTORY IS NEVER CWD-RELATIVE. It used to default to `tmp/reviewmesh`, resolved against the
// process cwd — so a review run from anywhere inside a repository created that tree INSIDE the
// repository, and run artifacts embed verbatim copies of every file the reviewers were shown. Nothing
// excluded it: this repo's own protection was an UNANCHORED `tmp/` .gitignore rule, which covers only
// this checkout. It now resolves through localstate.RunDir, the same rule exploremesh already used:
// `<project .aimesh>/review/runs` when the state directory exists, else the OS temp directory.
func New(opts Options) (*App, error) {
	artifactDir := localstate.RunDir(ArtifactSubdir, cmp.Or(opts.ArtifactDir, os.Getenv("REVIEWMESH_ARTIFACT_DIR")))
	scenario := opts.FakeScenario
	if scenario == "" {
		scenario = fake.Scenario(os.Getenv("REVIEWMESH_FAKE_SCENARIO"))
	}
	a := &App{
		ArtifactDir: artifactDir,
		TempBase:    opts.TempBase,
		configPath:  opts.ConfigPath,
		scenario:    scenario,
	}
	if err := a.loadConfig(); err != nil {
		return nil, err
	}
	return a, nil
}

// loadConfig (re)composes the config layers and rebuilds the adapter registry from disk. It is
// the shared body of New and ReloadConfig so a config write can be reflected without a restart.
func (a *App) loadConfig() error {
	// Compose config layers: shipped defaults ← user/global (~/.aimesh/review) ← project
	// (<repo-root>/.aimesh/review) ← explicit (--config). Project config is never required.
	cwd, _ := os.Getwd()
	cfg, layers, err := config.LoadLayered(cwd, a.configPath)
	if err != nil {
		return err
	}
	// Supply the local Ollama model tag (if any) so the fully-local-ollama profile
	// resolves. No tag → the profile fails resolution with clear guidance.
	cfg = config.WithOllamaModel(cfg, os.Getenv("REVIEWMESH_OLLAMA_MODEL"))

	// Build the adapter registry: a shell adapter per documented recipe (plus, gated
	// below, the internal test-only `fake`). Real adapters are registered so a profile MAY select
	// them and doctor can report their binary status — but they are never chosen
	// automatically (no default AdapterPreference names them) and no model is called
	// unless a profile explicitly selects the adapter.
	paths := make(map[string]string, len(cfg.Adapters))
	for name, ad := range cfg.Adapters {
		paths[name] = ad.Path
	}
	adapters := shell.Registry(paths, 0)
	// Merge the generic ACP-client adapters. Unlike shell recipes these are USER-DEFINED instances
	// (config-driven, no fixed catalog): each `acpAdapters.<name>` entry from the shared adapters.yaml
	// becomes one adapter driven by the single generic acpagent.Adapter. A shell recipe never shares a
	// name with an ACP instance.
	maps.Copy(adapters, acpagent.Registry(acpInstances(layers), 0))
	// The deterministic `fake` adapter is a HIDDEN internal test harness: it enters the runnable
	// registry only when the internal gate (fake.Enabled — set by tests, the golden run, and the
	// ACP-validation safe-mode parent; never by users) is on. Without it, `fake` is simply not a
	// registered adapter, so nothing user-configurable can resolve to it.
	if fake.Enabled() {
		adapters["fake"] = fake.New(a.scenario)
	}

	// Swap the whole snapshot atomically under the write lock. loadConfig only *replaces* these
	// fields (never mutates the maps in place), so a reader holding an earlier snapshot is safe.
	a.mu.Lock()
	a.Cfg, a.Layers, a.Adapters = cfg, layers, adapters
	a.mu.Unlock()
	return nil
}

// acpInstances converts the config layer's user-defined ACP instances into the runnable-adapter
// instance set for acpagent.Registry (Detect falls back to the binary basename for PATH lookup).
func acpInstances(ly config.Layers) map[string]acpagent.Instance {
	out := make(map[string]acpagent.Instance, len(ly.ACPInstances))
	for name, in := range ly.ACPInstances {
		detect := name
		if in.Path != "" {
			detect = filepath.Base(in.Path)
		}
		out[name] = acpagent.Instance{Name: name, Detect: detect, Path: in.Path, Args: in.Args}
	}
	return out
}

// ReloadConfig re-reads the config from disk and rebuilds the adapter registry so that reads
// issued after a config write (e.g. a web-UI profile copy or adapter change) reflect the new
// state. The web-UI Client calls this after a successful mutation.
func (a *App) ReloadConfig() error { return a.loadConfig() }

// Manager returns a wired ReviewManager over a consistent config snapshot.
func (a *App) Manager() *run.Manager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return &run.Manager{
		Cfg: a.Cfg, Adapters: a.Adapters, ArtifactDir: a.ArtifactDir, TempBase: a.TempBase,
	}
}

// ManagerWithConfig returns a wired ReviewManager over an EXPLICIT config snapshot rather than the
// loaded one. It exists for a surface that must run on a config it derived at launch — today the
// MCP surface, whose `--allow-remediate` grants a policy CAPABILITY by editing its own snapshot, so
// that the Manager's write-authority resolution reads the same config the tool list was built from.
// The adapters, artifact dir and temp base are unchanged: a surface may narrow or widen POLICY, and
// may never introduce an adapter.
func (a *App) ManagerWithConfig(cfg config.Config) *run.Manager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return &run.Manager{
		Cfg: cfg, Adapters: a.Adapters, ArtifactDir: a.ArtifactDir, TempBase: a.TempBase,
	}
}

// Doctor runs the static readiness checks. profile (optional) additionally checks
// that profile's adapter availability; probe runs a safe no-model binary probe of
// the selected profile's adapters.
func (a *App) Doctor(workspace, profile string, probe bool) doctor.Report {
	return a.DoctorWith(workspace, profile, DoctorOptions{Probe: probe})
}

// DoctorOptions selects which OPT-IN probe ladders `doctor` runs on top of the static checks.
//
// Probe and ProbeDeep are separate fields, not two settings of one, because they cost differently:
// Probe is `<binary> --version` and is free; ProbeDeep is one REAL model invocation per required
// adapter and SPENDS. A caller must not be able to reach a spend by turning an existing boolean up.
type DoctorOptions struct {
	Probe     bool
	ProbeDeep bool
	// Ctx bounds the probe pass; nil → context.Background().
	Ctx context.Context
}

// DoctorWith is Doctor with the opt-in probe ladders selected explicitly.
func (a *App) DoctorWith(workspace, profile string, o DoctorOptions) doctor.Report {
	a.mu.RLock()
	in := doctor.Input{
		Config: a.Cfg, Layers: a.Layers, Adapters: a.Adapters, ArtifactDir: a.ArtifactDir,
		Workspace: workspace, Profile: profile, Probe: o.Probe, ProbeDeep: o.ProbeDeep, Ctx: o.Ctx,
	}
	a.mu.RUnlock()
	return doctor.Run(in)
}

// SetupManager returns a wired SetupManager writing progress to out. It also carries the
// resolved config + layers + artifact dir so its read-projection methods (config/profiles/
// adapters/privacy DTOs) can serve surfaces without exposing ResourceAccess.
func (a *App) SetupManager(out io.Writer) *setup.Manager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return &setup.Manager{
		Adapters: a.Adapters, Out: out,
		Cfg: a.Cfg, Layers: a.Layers, ArtifactDir: a.ArtifactDir,
	}
}
