package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

// TestWindows_FilesystemSafety exercises the Windows-specific reparse/junction and
// path-safety behavior. It compiles on every OS but runs only on Windows (skips
// elsewhere). From the meshcore module root on a Windows machine:
//
//	go test ./workspace -run Windows -v
//
// This is the ONLY way the Windows path is ever executed: `make windows-build`
// cross-compiles it but runs nothing, and the gate does not include either. So the
// Windows behavior is coded-for and unverified until someone runs the line above —
// which is why the root README states that as a limitation rather than implying
// Windows support is tested. Junction creation (`mklink /J`) does not require
// administrator rights; symlink creation may, so those sub-tests skip cleanly when
// not permitted.
func TestWindows_FilesystemSafety(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only: run `go test ./workspace -run Windows -v` on a Windows machine")
	}

	mklinkJunction := func(t *testing.T, link, target string) bool {
		t.Helper()
		if err := exec.Command("cmd", "/c", "mklink", "/J", link, target).Run(); err != nil {
			t.Logf("mklink /J unavailable: %v", err)
			return false
		}
		return true
	}

	t.Run("junction direct target refused", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		writeFile(t, filepath.Join(real, "a.txt"), "x\n")
		junction := filepath.Join(base, "jx")
		if !mklinkJunction(t, junction, real) {
			t.Skip("could not create a junction")
		}
		if _, err := New(t.TempDir()).Copy(junction, true, "t"); err == nil {
			t.Error("Copy must refuse a junction/reparse-point target")
		}
	})

	t.Run("junction skipped during copy", func(t *testing.T) {
		base := t.TempDir()
		live := filepath.Join(base, "repo")
		writeFile(t, filepath.Join(live, "main.go"), "package main\n")
		outside := filepath.Join(base, "outside")
		writeFile(t, filepath.Join(outside, "secret.txt"), "SECRET\n")
		if !mklinkJunction(t, filepath.Join(live, "linkdir"), outside) {
			t.Skip("could not create a junction")
		}
		h, err := New(t.TempDir()).Copy(live, true, "t")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(h.Root, "linkdir")); !os.IsNotExist(err) {
			t.Error("a junction must not be copied into the isolated copy")
		}
	})

	t.Run("drive-letter and UNC edits refused", func(t *testing.T) {
		ws := New(t.TempDir())
		h, _ := ws.Copy(t.TempDir(), false, "t")
		for _, p := range []string{`C:..\escape.txt`, `\\server\share\x.txt`, `C:\abs.txt`} {
			if err := ws.ApplyEdit(h, core.Edit{File: p, Replacement: "y"}); err == nil {
				t.Errorf("ApplyEdit must refuse %q", p)
			}
		}
	})

	t.Run("case-insensitive excluded matching", func(t *testing.T) {
		if !IsExcluded(`Pkg\NODE_MODULES\m.js`) || !IsExcluded(`.GIT\config`) {
			t.Error("excluded matching must be case-insensitive on Windows")
		}
	})
}
