// Package app wires the review components together for the surfaces. The deterministic fake
// adapter is wired only under the internal test gate (fake.Enabled).
package app

import (
	"cmp"
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
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

// App holds the resolved configuration and wired components. The config snapshot (Cfg, Layers,
// Adapters) is guarded by mu so ReloadConfig cannot race a concurrent reader; use the accessor
// methods, which read it under the lock.
type App struct {
	Cfg         config.Config
	Layers      config.Layers // which config layers were discovered and loaded
	Adapters    map[string]model.Adapter
	ArtifactDir string
	TempBase    string

	mu         sync.RWMutex  // guards Cfg, Layers and Adapters, which loadConfig replaces wholesale
	configPath string        // explicit --config path, re-read by ReloadConfig
	scenario   fake.Scenario // fake scenario, kept for rebuilding adapters on reload
	launch     *LaunchConfig // non-nil when configured from launch arguments only (MCP, ACP)
	// artifactOverride is the explicit run-directory base (Options.ArtifactDir, else
	// REVIEWMESH_ARTIFACT_DIR); it wins over the per-workspace location.
	artifactOverride string
}

// LaunchConfig is the configuration an MCP or ACP server takes from its launch arguments. A server
// built from it reads no saved aimesh configuration: its adapters are exactly the named ones, every
// panel is composed per call, and run records are placed beside the workspace each run reviewed.
type LaunchConfig struct {
	Adapters launchflags.Set
}

// ArtifactSubdir is review's component directory under the shared `.aimesh/` state directory.
const ArtifactSubdir = "review"

// Options configures wiring.
type Options struct {
	ConfigPath string
	// ArtifactDir is the base for run dirs. Empty resolves through localstate.RunDir — see New.
	ArtifactDir  string
	TempBase     string        // base for isolated copies ("" = OS temp)
	FakeScenario fake.Scenario // override; "" → REVIEWMESH_FAKE_SCENARIO → valid
	// Launch, when set, builds the app from launch arguments alone (see LaunchConfig); ConfigPath is
	// then ignored.
	Launch *LaunchConfig
}

// New loads config and wires the components. Without ConfigPath it discovers a project config under
// <repo-root>/.aimesh/review.
//
// The run directory resolves through localstate.RunDir: `<project .aimesh>/review/runs` when the
// state directory exists, else the OS temp directory. It is never relative to cwd, because run
// artifacts contain copies of every reviewed file.
func New(opts Options) (*App, error) {
	override := cmp.Or(opts.ArtifactDir, os.Getenv("REVIEWMESH_ARTIFACT_DIR"))
	artifactDir := localstate.RunDir(ArtifactSubdir, override)
	if opts.Launch != nil {
		// A launch-configured server has no meaningful working directory: each run's record is placed
		// per workspace (see Manager), and this fixed base is only the fallback.
		artifactDir = localstate.RunDirFor("", ArtifactSubdir, override)
	}
	scenario := opts.FakeScenario
	if scenario == "" {
		scenario = fake.Scenario(os.Getenv("REVIEWMESH_FAKE_SCENARIO"))
	}
	a := &App{
		ArtifactDir:      artifactDir,
		TempBase:         opts.TempBase,
		configPath:       opts.ConfigPath,
		scenario:         scenario,
		launch:           opts.Launch,
		artifactOverride: override,
	}
	if err := a.loadConfig(); err != nil {
		return nil, err
	}
	return a, nil
}

// loadConfig composes the config layers and rebuilds the adapter registry. New and ReloadConfig
// share it.
func (a *App) loadConfig() error {
	if a.launch != nil {
		a.loadLaunch()
		return nil
	}
	cwd, _ := os.Getwd()
	cfg, layers, err := config.LoadLayered(cwd, a.configPath)
	if err != nil {
		return err
	}
	// Without a tag, the fully-local-ollama profile fails resolution with guidance.
	cfg = config.WithOllamaModel(cfg, os.Getenv("REVIEWMESH_OLLAMA_MODEL"))

	// Every shell recipe is registered so a profile may select it and doctor can report it; none
	// is chosen automatically.
	paths := make(map[string]string, len(cfg.Adapters))
	for name, ad := range cfg.Adapters {
		paths[name] = ad.Path
	}
	adapters := shell.Registry(paths, 0)
	// User-defined ACP instances from the shared adapters.yaml, each driven by acpagent.Adapter.
	maps.Copy(adapters, acpagent.Registry(acpInstances(layers), 0))
	if fake.Enabled() {
		adapters["fake"] = fake.New(a.scenario)
	}

	// Readers holding an earlier snapshot stay safe because the maps are replaced, never mutated.
	a.mu.Lock()
	a.Cfg, a.Layers, a.Adapters = cfg, layers, adapters
	a.mu.Unlock()
	return nil
}

// loadLaunch builds the configuration and adapter registry from launch arguments alone: shipped
// defaults, narrowed to the named adapters and their paths. No config layer or saved adapter path is
// read.
func (a *App) loadLaunch() {
	cfg := config.Default()
	named := make(map[string]config.Adapter, len(a.launch.Adapters.Names()))
	for _, ad := range a.launch.Adapters.Adapters() {
		entry := cfg.Adapters[ad.Name]
		entry.Path = ad.Path
		named[ad.Name] = entry
	}
	cfg.Adapters = named

	all := shell.Registry(a.launch.Adapters.Paths(), 0)
	adapters := make(map[string]model.Adapter, len(named))
	for name := range named {
		if ad, ok := all[name]; ok {
			adapters[name] = ad
		}
	}
	if fake.Enabled() && a.launch.Adapters.Has(launchflags.FakeAdapter) {
		adapters[launchflags.FakeAdapter] = fake.New(a.scenario)
	}

	a.mu.Lock()
	a.Cfg, a.Layers, a.Adapters = cfg, config.Layers{}, adapters
	a.mu.Unlock()
}

// artifactDirFor returns the run-directory base for a launch-configured app: the reviewed
// workspace's state directory when it has one, else the temp run directory. An explicit override
// wins.
func (a *App) artifactDirFor(workspace string) string {
	return localstate.RunDirIn(workspace, ArtifactSubdir, a.artifactOverride)
}

// acpInstances converts the user-defined ACP instances into acpagent.Registry instances. Detect is
// the binary's base name when a path is set, else the instance name.
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

// Manager returns a wired ReviewManager over a consistent config snapshot.
func (a *App) Manager() *run.Manager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.managerLocked(a.Cfg)
}

// managerLocked builds a Manager over cfg. The caller holds a.mu.
func (a *App) managerLocked(cfg config.Config) *run.Manager {
	m := &run.Manager{Cfg: cfg, Adapters: a.Adapters, ArtifactDir: a.ArtifactDir, TempBase: a.TempBase}
	if a.launch != nil {
		m.ArtifactDirFor = a.artifactDirFor
	}
	return m
}

// Doctor runs the static readiness checks. A non-empty profile is also checked, and probe runs a
// no-model binary probe of its adapters.
func (a *App) Doctor(workspace, profile string, probe bool) doctor.Report {
	return a.DoctorWith(workspace, profile, DoctorOptions{Probe: probe})
}

// DoctorOptions selects the opt-in probes doctor runs. Probe runs a free binary check; ProbeDeep
// makes one real model call per required adapter and spends tokens.
type DoctorOptions struct {
	Probe     bool
	ProbeDeep bool
	// Ctx bounds the probes; nil means context.Background().
	Ctx context.Context
}

// DoctorWith is Doctor with the probes selected explicitly.
func (a *App) DoctorWith(workspace, profile string, o DoctorOptions) doctor.Report {
	a.mu.RLock()
	in := doctor.Input{
		Config: a.Cfg, Layers: a.Layers, Adapters: a.Adapters, ArtifactDir: a.ArtifactDir,
		Workspace: workspace, Profile: profile, Probe: o.Probe, ProbeDeep: o.ProbeDeep, Ctx: o.Ctx,
	}
	a.mu.RUnlock()
	return doctor.Run(in)
}

// SetupManager returns a setup manager that writes progress to out and carries the current config
// snapshot for its read projections.
func (a *App) SetupManager(out io.Writer) *setup.Manager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return &setup.Manager{
		Adapters: a.Adapters, Out: out,
		Cfg: a.Cfg, Layers: a.Layers, ArtifactDir: a.ArtifactDir,
	}
}
