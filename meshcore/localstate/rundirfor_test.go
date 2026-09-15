package localstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunDirFor_UsesAnExistingProjectStateDirectory(t *testing.T) {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, HomeDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	got := RunDirFor(ws, "review", "")
	home, ok := ProjectHomeFor(ws)
	if !ok {
		t.Fatalf("ProjectHomeFor(%q) found no state directory", ws)
	}
	if want := filepath.Join(home, "review", RunsSubdir); got != want {
		t.Fatalf("RunDirFor = %q, want %q", got, want)
	}
}

func TestRunDirFor_FallsBackToTempAndCreatesNothing(t *testing.T) {
	ws := t.TempDir()
	got := RunDirFor(ws, "review", "")
	if got != TempRunDir("review") {
		t.Fatalf("RunDirFor without a state directory = %q, want the temp run dir %q", got, TempRunDir("review"))
	}
	if _, err := os.Stat(filepath.Join(ws, HomeDirName)); !os.IsNotExist(err) {
		t.Fatalf("RunDirFor must never create %s in the workspace (stat err %v)", HomeDirName, err)
	}
}

func TestRunDirFor_OverrideWins(t *testing.T) {
	if got := RunDirFor(t.TempDir(), "review", "/explicit/runs"); got != "/explicit/runs" {
		t.Fatalf("RunDirFor with override = %q", got)
	}
}

// RunDirIn never reaches a parent directory's state directory: a workspace below a repository root that
// holds `.aimesh` resolves to the temp run dir unless the workspace itself holds one.
func TestRunDirIn_DoesNotWalkToAParentStateDirectory(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{".git", HomeDirName, "sub"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sub := filepath.Join(root, "sub")
	if got := RunDirIn(sub, "review", ""); got != TempRunDir("review") {
		t.Fatalf("RunDirIn(sub) = %q, want the temp run dir — the parent's .aimesh is outside the declared directory", got)
	}
	if err := os.Mkdir(filepath.Join(sub, HomeDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(sub, HomeDirName, "review", RunsSubdir); RunDirIn(sub, "review", "") != want {
		t.Fatalf("RunDirIn(sub) = %q, want %q", RunDirIn(sub, "review", ""), want)
	}
	if got := RunDirIn(sub, "review", "/explicit"); got != "/explicit" {
		t.Fatalf("override must win, got %q", got)
	}
}

func TestRunDirFor_WorkspacesResolveIndependently(t *testing.T) {
	withState := t.TempDir()
	if err := os.Mkdir(filepath.Join(withState, HomeDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	without := t.TempDir()
	a, b := RunDirFor(withState, "review", ""), RunDirFor(without, "review", "")
	if a == b {
		t.Fatalf("two workspaces with different state resolved to the same run dir %q", a)
	}
}
