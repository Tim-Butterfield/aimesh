package shell

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// TestInvoke_WorkDir_Containment proves the shell adapter runs the CLI in the caller's isolated
// WorkDir (cmd.Dir), not the process cwd — the containment mechanism exploremesh relies on. A CLI that
// prints its working directory must report the isolated dir, and without WorkDir it must NOT.
func TestInvoke_WorkDir_Containment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX `sh -c pwd`")
	}
	work := t.TempDir()
	wantResolved, _ := filepath.EvalSymlinks(work) // macOS /var → /private/var

	a := New(Recipe{
		Name: "pwd-probe", Detect: "sh", Identity: core.IdentitySelfReport,
		BuildArgs: func(model.Call) []string { return []string{"-c", "pwd"} },
	}, "/bin/sh", 0)

	res, err := a.Invoke(context.Background(), model.Call{WorkDir: work, Prompt: "x"})
	if err != nil {
		t.Fatalf("invoke with WorkDir: %v", err)
	}
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(res.Stdout)))
	if got != wantResolved {
		t.Errorf("CLI ran in %q, want the isolated WorkDir %q — containment failed", got, wantResolved)
	}

	// Without WorkDir the CLI inherits the process cwd (the package dir), NOT the isolated dir.
	res2, err := a.Invoke(context.Background(), model.Call{Prompt: "x"})
	if err != nil {
		t.Fatalf("invoke without WorkDir: %v", err)
	}
	if strings.TrimSpace(string(res2.Stdout)) == strings.TrimSpace(string(res.Stdout)) {
		t.Error("without WorkDir the CLI should run in the process cwd, not the isolated dir")
	}
}
