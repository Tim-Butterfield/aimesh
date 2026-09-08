package acp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// A1 — THE BYPASS. Running the degenerate rule ONLY on the inferred launch cwd would make
// it decorative: the documented flag walks straight past it, so `aimesh review acp --root /`
// would be accepted and grant whole-machine confinement. These are the reachable cases (both
// paths exist on every platform); the family-by-family coverage is in the matcher test below.
func TestResolveTrustedRoots_RefusesDegenerateExplicitRoots(t *testing.T) {
	home := t.TempDir()
	proj := projectDir(t, "proj")
	cases := map[string]string{
		"filesystem root": string(filepath.Separator),
		"home itself":     home,
		"home's parent":   filepath.Dir(home),
	}
	for name, root := range cases {
		t.Run(name, func(t *testing.T) {
			got, _, err := ResolveTrustedRoots(RootOptions{Explicit: []string{root}, Cwd: proj, Home: home})
			if err == nil {
				t.Fatalf("--root %q must NOT be accepted, got %v", root, got)
			}
			if fault.ReasonOf(err) != ReasonDegenerateRoot {
				t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonDegenerateRoot)
			}
			if !strings.Contains(err.Error(), "--allow-broad-root") {
				t.Errorf("error %q should name the explicit opt-in", err)
			}
		})
	}
	// The rule must not have become a blanket refusal: an ordinary explicit root still works.
	if got, _, err := ResolveTrustedRoots(RootOptions{Explicit: []string{proj}, Home: home}); err != nil {
		t.Fatalf("an ordinary --root must still be accepted: %v", err)
	} else if len(got) != 1 || got[0] != proj {
		t.Errorf("roots = %v, want [%s]", got, proj)
	}
}

// TestResolveTrustedRoots_ReportsWhereTheRootsCameFrom pins the PROVENANCE return.
//
// AGAINST THE OLD CODE this does not compile: the resolver returned only ([]string, error), and after
// it returned, `--root /repo` and a launch cwd of `/repo` were indistinguishable — the same []string,
// with nothing recording which consent produced it. That mattered the moment MCP's 2026-07-28
// revision removed the client's ability to narrow a server: an INFERRED root there would silently
// grant more filesystem authority than it did before, and a caller cannot fail closed on a
// distinction nobody carried.
func TestResolveTrustedRoots_ReportsWhereTheRootsCameFrom(t *testing.T) {
	home := t.TempDir()
	proj := projectDir(t, "proj")

	if _, src, err := ResolveTrustedRoots(RootOptions{Explicit: []string{proj}, Home: home}); err != nil {
		t.Fatalf("explicit root: %v", err)
	} else if src != RootsExplicit {
		t.Fatalf("source = %q for an explicit --root, want %q", src, RootsExplicit)
	}

	if _, src, err := ResolveTrustedRoots(RootOptions{Cwd: proj, Home: home}); err != nil {
		t.Fatalf("inferred cwd: %v", err)
	} else if src != RootsInferredCwd {
		t.Fatalf("source = %q for a launch-cwd fallback, want %q — nobody typed it, and a caller that has to fail closed on that must be able to tell", src, RootsInferredCwd)
	}

	// An error never comes back with a provenance claim: there are no roots to have a provenance.
	if _, src, err := ResolveTrustedRoots(RootOptions{NoDefault: true, Cwd: proj, Home: home}); err == nil {
		t.Fatal("--no-default-root with no --root must fail closed")
	} else if src != RootsNone {
		t.Fatalf("source = %q on a refusal, want %q", src, RootsNone)
	}
}

// TestResolveTrustedRoots_RefusesANonDirectoryRoot pins the shape check on `--root`. It was
// found unguarded by a test-strength sweep: deleting the `!fi.IsDir()` refusal left the suite
// green, and the consequence is not cosmetic — a trusted root is a CONTAINMENT BOUNDARY, and a
// file admitted as one is a boundary whose "inside" is the file's own directory as far as every
// prefix comparison downstream is concerned. The refusal is also the only place an operator is
// told that `--root ./notes.md` is a mistake rather than a very narrow scope.
func TestResolveTrustedRoots_RefusesANonDirectoryRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(file, []byte("not a project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := ResolveTrustedRoots(RootOptions{Explicit: []string{file}, Home: t.TempDir()})
	if err == nil {
		t.Fatalf("--root %q is a FILE and must be refused, got roots %v", file, got)
	}
	if fault.ReasonOf(err) != ReasonRootUnusable {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonRootUnusable)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("the refusal must say WHY (a trusted root is a project directory), got: %v", err)
	}
	// And the rule is about shape, not about the name: the containing directory is fine.
	if _, _, derr := ResolveTrustedRoots(RootOptions{Explicit: []string{dir}, Home: t.TempDir()}); derr != nil {
		t.Errorf("the containing directory must still be accepted: %v", derr)
	}
}

// A refusal an operator can override BY SAYING SO is not the same thing as silent acceptance.
// The opt-in covers explicit roots only.
func TestResolveTrustedRoots_AllowBroadRootIsAnExplicitOptIn(t *testing.T) {
	home := t.TempDir()
	root := string(filepath.Separator)
	// The filesystem root is `/` on unix, but on Windows `\` is DRIVE-RELATIVE rather than absolute
	// and resolves to the current volume's root (`C:\`). The assertion is that the broad root is
	// accepted and handed back, so it compares against the resolved form rather than the separator
	// that was typed — hardcoding `/` asserts a POSIX layout, not the behaviour under test.
	want, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve the filesystem root: %v", err)
	}
	got, _, err := ResolveTrustedRoots(RootOptions{Explicit: []string{root}, AllowBroadRoot: true, Home: home})
	if err != nil {
		t.Fatalf("--allow-broad-root must permit an explicit broad root: %v", err)
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("roots = %v, want [%s]", got, want)
	}
	// ...but it never reaches the INFERRED cwd: the inference's justification is "an IDE
	// started us in the project", which a degenerate cwd disproves, so there is nothing to
	// waive.
	if _, _, err := ResolveTrustedRoots(RootOptions{AllowBroadRoot: true, Cwd: root, Home: home}); err == nil {
		t.Fatal("--allow-broad-root must NOT make a degenerate launch cwd adoptable")
	} else if fault.ReasonOf(err) != ReasonDegenerateDefaultRoot {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonDegenerateDefaultRoot)
	}
	// ...and it never waives the non-overridable read denylist.
	ssh := filepath.Join(t.TempDir(), ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveTrustedRoots(RootOptions{Explicit: []string{ssh}, AllowBroadRoot: true}); err == nil {
		t.Fatal("--allow-broad-root must never promote a protected path to a trusted root")
	} else if fault.ReasonOf(err) != ReasonRootDenied {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonRootDenied)
	}
}

// A4 — PRE-CANONICAL DENIAL. Judging the path AS SPELLED would make a symlink with an innocuous
// name a one-step alias past either rule. Later requests would still fail closed, but the
// DOCUMENTED launch-time refusal would never fire — which is the whole promise.
func TestResolveTrustedRoots_JudgesTheCanonicalRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs developer mode/elevation on Windows")
	}
	home := t.TempDir()

	t.Run("symlink to the filesystem root", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "work")
		if err := os.Symlink(string(filepath.Separator), link); err != nil {
			t.Fatal(err)
		}
		if got, _, err := ResolveTrustedRoots(RootOptions{Explicit: []string{link}, Home: home}); err == nil {
			t.Fatalf("a root that RESOLVES to / must be refused, got %v", got)
		} else if fault.ReasonOf(err) != ReasonDegenerateRoot {
			t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonDegenerateRoot)
		}
	})

	t.Run("symlink to a protected directory", func(t *testing.T) {
		base := t.TempDir()
		ssh := filepath.Join(base, ".ssh")
		if err := os.MkdirAll(ssh, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "work") // no protected component in its own spelling
		if err := os.Symlink(ssh, link); err != nil {
			t.Fatal(err)
		}
		if got, _, err := ResolveTrustedRoots(RootOptions{Explicit: []string{link}, Home: home}); err == nil {
			t.Fatalf("a root that RESOLVES to a protected path must be refused, got %v", got)
		} else if fault.ReasonOf(err) != ReasonRootDenied {
			t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonRootDenied)
		}
	})

	t.Run("the launch cwd is judged canonically too", func(t *testing.T) {
		base := t.TempDir()
		ssh := filepath.Join(base, ".ssh")
		if err := os.MkdirAll(ssh, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "cwd")
		if err := os.Symlink(ssh, link); err != nil {
			t.Fatal(err)
		}
		if got, _, err := ResolveTrustedRoots(RootOptions{Cwd: link, Home: home}); err == nil {
			t.Fatalf("a cwd that RESOLVES to a protected path must be refused, got %v", got)
		} else if fault.ReasonOf(err) != ReasonRootDenied {
			t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonRootDenied)
		}
	})
}

// A4/A5 — EVERY degenerate family is pinned, one case per entry, so deleting any single entry
// from the list fails a named subtest rather than passing silently. Testing the matcher (a
// pure function over a canonical path) rather than the full launch is deliberate: `/mnt`,
// `/srv` and friends do not exist on every machine, and a test that only fires where the
// directory happens to exist would pin nothing on CI.
func TestMatchSystemRoot_CoversEveryDegenerateFamily(t *testing.T) {
	list := unixSystemRoots
	// wantPresent is written out IN FULL and independently of the implementation list, so
	// deleting ANY single entry from the rule fails a named assertion here. That is the point
	// the round-2 review made about the old test: it exercised only `/`, home and home's
	// parent, so the entire system-root loop could have been deleted and it would still have
	// passed. Additions are allowed (the loop below judges those); removals are not.
	wantPresent := []string{
		"/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libexec",
		"/etc", "/dev", "/proc", "/sys", "/boot", "/run",
		"/usr", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/local", "/usr/share",
		"/opt", "/var", "/var/tmp", "/var/log", "/tmp",
		"/private", "/private/var", "/private/tmp", "/private/etc",
		"/Library", "/System", "/Applications", "/Network",
		"/Users", "/Users/Shared", "/home", "/root",
		"/mnt", "/media", "/Volumes", "/srv", "/net", "/export",
	}
	if runtime.GOOS == "windows" {
		list = windowsSystemRoots
		wantPresent = []string{
			`\Windows`, `\Windows\System32`, `\Windows\SysWOW64`,
			`\Program Files`, `\Program Files (x86)`, `\ProgramData`,
			`\Users`, `\Users\Public`, `\Documents and Settings`,
			`\$Recycle.Bin`, `\System Volume Information`,
		}
	}
	have := map[string]bool{}
	for _, e := range list {
		have[e] = true
	}
	for _, want := range wantPresent {
		if !have[want] {
			t.Errorf("%q is missing from the degenerate-root list — that family is unprotected", want)
		}
	}
	for _, entry := range list {
		t.Run(entry, func(t *testing.T) {
			// The probe is CANONICAL, because that is what degenerateRoot passes (and on
			// macOS `/etc`, `/var` and `/tmp` are symlinks into `/private`).
			probe := canonicalRoot(entry)
			if runtime.GOOS == "windows" {
				probe = `C:` + entry
			}
			if got := matchSystemRoot(probe); got == "" {
				t.Errorf("matchSystemRoot(%q) = \"\", want a match — this family is unprotected", probe)
			}
		})
	}
	// It stays EXACT-match: a project UNDER a system tree is a legitimate root, and refusing
	// whole subtrees would be a false refusal with no security value.
	for _, under := range []string{"/usr/local/src/myproject", "/srv/git/myrepo", "/Volumes/Data/work"} {
		if runtime.GOOS == "windows" {
			continue
		}
		if got := matchSystemRoot(under); got != "" {
			t.Errorf("matchSystemRoot(%q) = %q, want \"\" (exact matches only)", under, got)
		}
	}
}

// The Windows entries must NOT hardcode the `C:` drive letter: `D:\Windows` — an ordinary
// shape on any machine with a second drive, a VHD, or a mapped volume — would pass straight
// through. The rule is matched against the volume-stripped path.
func TestMatchSystemRoot_WindowsIsDriveLetterAgnostic(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("volume-stripped matching only applies on Windows")
	}
	for _, p := range []string{`D:\Windows`, `E:\Program Files`, `Z:\Users`, `d:\windows`} {
		if got := matchSystemRoot(p); got == "" {
			t.Errorf("matchSystemRoot(%q) = \"\", want a match on any drive letter", p)
		}
	}
}
