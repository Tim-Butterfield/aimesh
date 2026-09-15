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
	launch     *LaunchConfig // non-nil: configured from launch arguments only (MCP, ACP)
	// artifactOverride is the explicit run-directory base (Options.ArtifactDir, else
	// REVIEWMESH_ARTIFACT_DIR); it wins over the per-workspace location in launch mode too.
	artifactOverride string
}

// LaunchConfig is the configuration an MCP or ACP server takes from its launch arguments. A server
// built from it reads no saved aimesh configuration: its adapters are exactly the named ones, every
// panel is composed per call, and run records are placed beside the workspace each run reviewed.
type LaunchConfig struct {
	Adapters launchflags.Set
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
	// Launch, when set, builds the app from launch arguments alone (see LaunchConfig); ConfigPath is
	// then ignored.
	Launch *LaunchConfig
}

// New loads config and wires the components. With no explicit ConfigPath it
// auto-discovers a project config under <repo-root>/.aimesh/review (YAML preferred, then JSON).
//
// THE RUN DIRECTORY IS NEVER CWD-RELATIVE. A cwd-relative default would create a tree INSIDE whatever
// repository a review ran from, and run artifacts embed verbatim copies of every file the reviewers
// were shown. It resolves through localstate.RunDir, the same rule exploremesh uses:
// `<project .aimesh>/review/runs` when the state directory exists, else the OS temp directory.
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

// loadConfig (re)composes the config layers and rebuilds the adapter registry from disk. It is
// the shared body of New and ReloadConfig so a config write can be reflected without a restart.
func (a *App) loadConfig() error {
	if a.launch != nil {
		a.loadLaunch()
		return nil
	}
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

// artifactDirFor places a run's record for a launch-configured app: beside the workspace it reviewed
// when that workspace has a state directory, else in the temp run directory; an explicit override
// wins over both.
func (a *App) artifactDirFor(workspace string) string {
	return localstate.RunDirIn(workspace, ArtifactSubdir, a.artifactOverride)
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
