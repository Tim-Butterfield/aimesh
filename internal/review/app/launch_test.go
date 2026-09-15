package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

func TestLaunchMode_OnlyNamedAdaptersExist(t *testing.T) {
	t.Setenv(localstate.HomeEnvVar, t.TempDir())
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", "")
	a, err := New(Options{Launch: &LaunchConfig{Adapters: launchflags.NewSet(
		launchflags.Adapter{Name: "codex-cli", Path: "/opt/codex", Source: launchflags.SourceFlag},
	)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Adapters) != 1 || a.Adapters["codex-cli"] == nil {
		t.Fatalf("registry = %v, want exactly the named adapter", keys(a.Adapters))
	}
	if len(a.Cfg.Adapters) != 1 || a.Cfg.Adapters["codex-cli"].Path != "/opt/codex" {
		t.Fatalf("config adapters = %+v, want exactly the named adapter with its launch path", a.Cfg.Adapters)
	}
	if a.Cfg.Adapters["codex-cli"].ModelIdentity == "" {
		t.Fatal("a named adapter keeps its shipped identity-evidence declaration")
	}
}

func TestLaunchMode_IgnoresSavedConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv(localstate.HomeEnvVar, home)
	dir := filepath.Join(home, localstate.HomeDirName, ArtifactSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A malformed saved config fails the CLI's layered load; a launch-configured app never reads it.
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("::: not yaml :::\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{Launch: &LaunchConfig{Adapters: launchflags.NewSet()}})
	if err != nil {
		t.Fatalf("a launch-configured app must not read saved configuration: %v", err)
	}
	if a.Layers.UserLoaded || a.Layers.ProjectLoaded {
		t.Fatalf("layers = %+v, want none loaded", a.Layers)
	}
	if len(a.Adapters) != 0 {
		t.Fatalf("no adapter was named, so none may exist: %v", keys(a.Adapters))
	}
}

func TestLaunchMode_RunRecordsFollowTheWorkspace(t *testing.T) {
	t.Setenv(localstate.HomeEnvVar, t.TempDir())
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", "")
	a, err := New(Options{Launch: &LaunchConfig{Adapters: launchflags.NewSet()}})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, localstate.HomeDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	m := a.Manager()
	if m.ArtifactDirFor == nil {
		t.Fatal("a launch-configured manager must place run records per workspace")
	}
	home, _ := localstate.ProjectHomeFor(ws)
	if got, want := m.ArtifactDirFor(ws), filepath.Join(home, ArtifactSubdir, localstate.RunsSubdir); got != want {
		t.Fatalf("run dir = %q, want %q", got, want)
	}
	if got := m.ArtifactDirFor(t.TempDir()); got != localstate.TempRunDir(ArtifactSubdir) {
		t.Fatalf("a workspace without state must use the temp run dir, got %q", got)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
