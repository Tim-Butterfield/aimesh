package acp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// The degenerate rule refuses locations that are never a project; only an operator's --root can
// waive it.
func TestValidateRoot_RefusesDegenerateRoots(t *testing.T) {
	home := t.TempDir()
	proj := projectDir(t, "proj")
	cases := map[string]string{
		"filesystem root": string(filepath.Separator),
		"home itself":     home,
		"home's parent":   filepath.Dir(home),
	}
	for name, root := range cases {
		t.Run(name, func(t *testing.T) {
			got, _, err := ValidateRoot(root, ValidateOptions{Label: "--root", Home: home})
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
	// An ordinary root still works.
	if got, _, err := ValidateRoot(proj, ValidateOptions{Label: "--root", Home: home}); err != nil {
		t.Fatalf("an ordinary --root must still be accepted: %v", err)
	} else if got != proj {
		t.Errorf("root = %q, want %q", got, proj)
	}
}

// A path a call declares is refused without offering the operator's waiver: a caller cannot grant
// itself breadth.
func TestValidateRoot_ADeclaredPathIsNotOfferedTheWaiver(t *testing.T) {
	home := t.TempDir()
	_, _, err := ValidateRoot(home, ValidateOptions{Label: "declared path", Home: home})
	if fault.ReasonOf(err) != ReasonDegenerateRoot {
		t.Fatalf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonDegenerateRoot)
	}
	if strings.Contains(err.Error(), "--allow-broad-root") {
		t.Errorf("a declared-path refusal must not point a caller at an operator flag: %v", err)
	}
}

// A root is a containment boundary, so a file is refused as one.
func TestValidateRoot_RefusesANonDirectoryRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(file, []byte("not a project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := ValidateRoot(file, ValidateOptions{Label: "--root", Home: t.TempDir()})
	if err == nil {
		t.Fatalf("--root %q is a FILE and must be refused, got %q", file, got)
	}
	if fault.ReasonOf(err) != ReasonRootUnusable {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonRootUnusable)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("the refusal must say why, got: %v", err)
	}
	if _, _, derr := ValidateRoot(dir, ValidateOptions{Label: "--root", Home: t.TempDir()}); derr != nil {
		t.Errorf("the containing directory must still be accepted: %v", derr)
	}
}

// AllowBroad admits a broad root and says why, but never waives the denylist.
func TestValidateRoot_AllowBroadIsAnExplicitOptIn(t *testing.T) {
	home := t.TempDir()
	root := string(filepath.Separator)
	// On Windows `\` resolves to the current volume's root, so compare against the resolved form.
	want, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve the filesystem root: %v", err)
	}
	got, broad, err := ValidateRoot(root, ValidateOptions{Label: "--root", AllowBroad: true, Home: home})
	if err != nil {
		t.Fatalf("--allow-broad-root must permit an explicit broad root: %v", err)
	}
	if got != want || broad == "" {
		t.Errorf("root = %q broad = %q, want %q with a breadth reason", got, broad, want)
	}
	ssh := filepath.Join(t.TempDir(), ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ValidateRoot(ssh, ValidateOptions{Label: "--root", AllowBroad: true}); err == nil {
		t.Fatal("--allow-broad-root must never promote a protected path to a root")
	} else if fault.ReasonOf(err) != ReasonRootDenied {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonRootDenied)
	}
}

// Both rules judge the canonical root, so an innocuously named symlink cannot alias past them.
func TestValidateRoot_JudgesTheCanonicalRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs developer mode/elevation on Windows")
	}
	home := t.TempDir()

	t.Run("symlink to the filesystem root", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "work")
		if err := os.Symlink(string(filepath.Separator), link); err != nil {
			t.Fatal(err)
		}
		if got, _, err := ValidateRoot(link, ValidateOptions{Label: "--root", Home: home}); err == nil {
			t.Fatalf("a root that RESOLVES to / must be refused, got %q", got)
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
		if got, _, err := ValidateRoot(link, ValidateOptions{Label: "--root", Home: home}); err == nil {
			t.Fatalf("a root that RESOLVES to a protected path must be refused, got %q", got)
		} else if fault.ReasonOf(err) != ReasonRootDenied {
			t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonRootDenied)
		}
	})
}

// Every degenerate entry has its own subtest, so deleting one fails by name. The matcher is tested
// directly because /mnt, /srv and similar do not exist on every machine.
func TestMatchSystemRoot_CoversEveryDegenerateFamily(t *testing.T) {
	list := unixSystemRoots
	// wantPresent is written independently of the implementation list, so removing an entry fails here.
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
			// The probe is canonical, as in degenerateRoot; on macOS /etc, /var and /tmp are symlinks into
			// /private.
			probe := canonicalRoot(entry)
			if runtime.GOOS == "windows" {
				probe = `C:` + entry
			}
			if got := matchSystemRoot(probe); got == "" {
				t.Errorf("matchSystemRoot(%q) = \"\", want a match — this family is unprotected", probe)
			}
		})
	}
	// Matching is exact: a project under a system tree is a legitimate root.
	for _, under := range []string{"/usr/local/src/myproject", "/srv/git/myrepo", "/Volumes/Data/work"} {
		if runtime.GOOS == "windows" {
			continue
		}
		if got := matchSystemRoot(under); got != "" {
			t.Errorf("matchSystemRoot(%q) = %q, want \"\" (exact matches only)", under, got)
		}
	}
}

// The Windows entries are matched against the volume-stripped path, so any drive letter matches.
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
