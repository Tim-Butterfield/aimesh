package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cwd inference rests on EVIDENCE now. Its justification has always been "a host started this
// agent IN the project the user opened"; a project marker is what makes that checkable instead of
// assumed. These tests pin both directions, because a rule that only ever accepts is not a rule.

func TestInferredCwd_AdoptedWhenItLooksLikeAProject(t *testing.T) {
	home := t.TempDir()
	for _, marker := range []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml", ".aimesh"} {
		t.Run(marker, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			// A marker counts by PRESENCE, and either shape counts: `.git` is a directory in a normal
			// clone and a FILE in a worktree, and both are equally good evidence.
			target := filepath.Join(dir, marker)
			if marker == ".git" || marker == ".aimesh" {
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			roots, src, err := ResolveTrustedRoots(RootOptions{Cwd: dir, Home: home})
			if err != nil {
				t.Fatalf("a directory carrying %s must be adopted: %v", marker, err)
			}
			if len(roots) != 1 || src != RootsInferredCwd {
				t.Errorf("roots=%v source=%q, want the cwd adopted and reported as inferred", roots, src)
			}
		})
	}
}

// TestInferredCwd_NotAdoptedWithoutAMarker is the half that closes the over-grant the
// degenerate-root rule cannot see. `~/projects` is neither home nor home's parent, so nothing above
// catches it — yet adopting it hands a model read access across every project the user owns
// instead of one.
//
// NOT ADOPTED, not failed: the result is zero roots and NO ERROR. A server with no roots still
// starts and still serves everything that consumes no trusted root; killing the process instead
// would hand an MCP host "server disconnected" with the reason on a channel hosts may discard.
func TestInferredCwd_NotAdoptedWithoutAMarker(t *testing.T) {
	home := t.TempDir()
	bare := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(filepath.Join(bare, "some-repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots, src, err := ResolveTrustedRoots(RootOptions{Cwd: bare, Home: home})
	if err != nil {
		t.Fatalf("a marker-less cwd must not be a LAUNCH failure, only a non-adoption: %v", err)
	}
	if len(roots) != 0 {
		t.Errorf("roots = %v, want none: a marker-less directory was adopted on the strength of being the cwd", roots)
	}
	if src != RootsNone {
		t.Errorf("source = %q, want %q", src, RootsNone)
	}
}

// TestInferredCwd_TheWaiverAdoptsAMarkerLessCwd. The refusal has to have a way past it, or an
// operator working in a tree that carries no convention marker is simply stuck.
func TestInferredCwd_TheWaiverAdoptsAMarkerLessCwd(t *testing.T) {
	home := t.TempDir()
	bare := filepath.Join(t.TempDir(), "notaproject")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	roots, src, err := ResolveTrustedRoots(RootOptions{Cwd: bare, Home: home, AllowInferredRoot: true})
	if err != nil {
		t.Fatalf("--allow-inferred-root must adopt a marker-less cwd: %v", err)
	}
	if len(roots) != 1 || src != RootsInferredCwd {
		t.Errorf("roots=%v source=%q, want the cwd adopted and still reported as inferred", roots, src)
	}
}

// TestTheWaiverDoesNotReachTheDegenerateRule. "The operator meant this" is evidence that they
// accept the consequence for a directory that might not be a project; it is not evidence that
// `$HOME` is one. The broad-root rule stands regardless.
func TestTheWaiverDoesNotReachTheDegenerateRule(t *testing.T) {
	home := t.TempDir()
	if _, _, err := ResolveTrustedRoots(RootOptions{Cwd: home, Home: home, AllowInferredRoot: true}); err == nil {
		t.Fatal("--allow-inferred-root admitted the HOME directory as a trusted root")
	} else if !strings.Contains(err.Error(), "--root") {
		t.Errorf("the refusal does not name the way out: %v", err)
	}
}

// TestExplicitRootNeedsNoMarker. The marker test exists to justify an INFERENCE. A human who typed
// `--root` has supplied the thing the marker was standing in for, so requiring one there would
// refuse a deliberate instruction — the failure mode this whole pass is removing.
func TestExplicitRootNeedsNoMarker(t *testing.T) {
	home := t.TempDir()
	bare := filepath.Join(t.TempDir(), "notaproject")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	roots, src, err := ResolveTrustedRoots(RootOptions{Explicit: []string{bare}, Home: home})
	if err != nil {
		t.Fatalf("an explicit --root must not need a marker: %v", err)
	}
	if len(roots) != 1 || src != RootsExplicit {
		t.Errorf("roots=%v source=%q, want the explicit root honoured", roots, src)
	}
}
