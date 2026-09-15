package acp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

func TestCallScope_AMarkerLessAbsoluteDirectoryIsItsOwnRoot(t *testing.T) {
	ws := t.TempDir() // no .git, go.mod or other project marker
	file := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := CallScope([]string{ws}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("a declared workspace needs no project marker: %v", err)
	}
	if _, rerr := r.ResolveRead(file); rerr != nil {
		t.Fatalf("a file inside the declared workspace must be readable: %v", rerr)
	}
	other := t.TempDir()
	if _, rerr := r.ResolveRead(other); scope.ReasonOf(rerr) != scope.ReasonOutsideRoot {
		t.Fatalf("a path the call did not declare must be outside its scope: %v", rerr)
	}
}

func TestCallScope_ExtraRootsAreAdditive(t *testing.T) {
	ws, docs := t.TempDir(), t.TempDir()
	r, err := CallScope([]string{ws, docs}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Roots()) != 2 {
		t.Fatalf("roots = %v, want both declared paths", r.Roots())
	}
	if _, rerr := r.ResolveRead(docs); rerr != nil {
		t.Fatalf("an extra declared root must be readable: %v", rerr)
	}
}

func TestCallScope_Refusals(t *testing.T) {
	home := t.TempDir()
	secret := filepath.Join(t.TempDir(), ".ssh")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	notDir := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		paths  []string
		reason string
	}{
		{"relative path", []string{"project"}, ReasonCallPathRelative},
		{"no path", []string{" "}, ReasonCallNoPath},
		{"filesystem root", []string{filepath.VolumeName(home) + string(filepath.Separator)}, ReasonDegenerateRoot},
		{"home itself", []string{home}, ReasonDegenerateRoot},
		{"protected directory", []string{secret}, ReasonRootDenied},
		{"not a directory", []string{notDir}, ReasonRootUnusable},
		{"missing directory", []string{filepath.Join(home, "absent")}, ReasonRootUnusable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CallScope(tc.paths, nil, home)
			if err == nil {
				t.Fatalf("want refusal %q", tc.reason)
			}
			if r := fault.ReasonOf(err); r != tc.reason {
				t.Fatalf("reason = %q, want %q (err %v)", r, tc.reason, err)
			}
		})
	}
}

func TestCallScope_TheCeilingOnlyNarrows(t *testing.T) {
	ceiling := t.TempDir()
	inside := filepath.Join(ceiling, "proj")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CallScope([]string{inside}, []string{ceiling}, t.TempDir()); err != nil {
		t.Fatalf("a path inside the ceiling must be accepted: %v", err)
	}
	outside := t.TempDir()
	_, err := CallScope([]string{outside}, []string{ceiling}, t.TempDir())
	if fault.ReasonOf(err) != ReasonOutsideCeiling {
		t.Fatalf("a path outside the ceiling must be refused: %v", err)
	}
}

func TestResolveCeiling(t *testing.T) {
	home := t.TempDir()
	if roots, err := ResolveCeiling(nil, false, "mcp", home); err != nil || roots != nil {
		t.Fatalf("no --root is no ceiling: %v, %v", roots, err)
	}
	proj := t.TempDir()
	if roots, err := ResolveCeiling([]string{proj}, false, "mcp", home); err != nil || len(roots) != 1 {
		t.Fatalf("an ordinary --root is a ceiling: %v, %v", roots, err)
	}
	if _, err := ResolveCeiling([]string{home}, false, "mcp", home); fault.ReasonOf(err) != ReasonDegenerateRoot {
		t.Fatalf("a home-directory ceiling needs --allow-broad-root: %v", err)
	}
	if roots, err := ResolveCeiling([]string{home}, true, "mcp", home); err != nil || len(roots) != 1 {
		t.Fatalf("--allow-broad-root admits a broad ceiling: %v, %v", roots, err)
	}
}
