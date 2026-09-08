package scope

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mkRoot builds a temp root and returns its CANONICAL path (t.TempDir on macOS is under
// /var → /private/var, so tests must compare against the resolved form).
func mkRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	c, err := canonical(dir)
	if err != nil {
		t.Fatalf("canonical(%q): %v", dir, err)
	}
	return c
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rawJoin joins path elements WITHOUT cleaning, so a test can hand the resolver a path
// that still contains `..` after a symlink component (filepath.Join would collapse it
// textually and defeat the very case under test).
func rawJoin(base string, parts ...string) string {
	return base + string(filepath.Separator) + strings.Join(parts, string(filepath.Separator))
}

func denialFor(t *testing.T, err error) *Denial {
	t.Helper()
	d, ok := AsDenial(err)
	if !ok {
		t.Fatalf("error %v is not a *Denial", err)
	}
	return d
}

// TestNoRoots_FailsClosed pins the fail-closed posture: a resolver with no configured
// roots refuses every path, for both operations — a caller that forgot to declare roots
// never gets unrestricted filesystem access.
func TestNoRoots_FailsClosed(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Roots()) != 0 {
		t.Fatalf("Roots() = %v, want empty", r.Roots())
	}
	for _, p := range []string{mkRoot(t), "relative/file.go", "/etc/hosts"} {
		if _, err := r.ResolveRead(p); ReasonOf(err) != ReasonNoRoots {
			t.Errorf("ResolveRead(%q) reason = %q, want %q", p, ReasonOf(err), ReasonNoRoots)
		}
		if _, err := r.ResolveWrite(p); ReasonOf(err) != ReasonNoRoots {
			t.Errorf("ResolveWrite(%q) reason = %q, want %q", p, ReasonOf(err), ReasonNoRoots)
		}
	}
	// The zero value behaves identically (no accidental "unconfigured = allow all").
	var zero Resolver
	if _, err := zero.ResolveWrite("/tmp/x"); ReasonOf(err) != ReasonNoRoots {
		t.Errorf("zero Resolver reason = %q, want %q", ReasonOf(err), ReasonNoRoots)
	}
	var nilR *Resolver
	if _, err := nilR.ResolveRead("/tmp/x"); ReasonOf(err) != ReasonNoRoots {
		t.Errorf("nil Resolver reason = %q, want %q", ReasonOf(err), ReasonNoRoots)
	}
}

// TestInsideRoot_Allowed covers the happy path: an existing file, a not-yet-existing
// file (a write target), and the root itself all resolve to canonical paths.
func TestInsideRoot_Allowed(t *testing.T) {
	root := mkRoot(t)
	write(t, filepath.Join(root, "pkg", "a.go"), "package pkg\n")
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{root, filepath.Join(root, "pkg"), filepath.Join(root, "pkg", "a.go"), filepath.Join(root, "pkg", "new", "b.go")}
	for _, p := range cases {
		got, err := r.ResolveRead(p)
		if err != nil {
			t.Errorf("ResolveRead(%q) = %v, want allowed", p, err)
			continue
		}
		if got != filepath.Clean(p) {
			t.Errorf("ResolveRead(%q) = %q, want %q", p, got, filepath.Clean(p))
		}
		if _, err := r.ResolveWrite(p); err != nil {
			t.Errorf("ResolveWrite(%q) = %v, want allowed", p, err)
		}
	}
}

// TestDotDotEscape_Refused pins the `..` escape: both a textual escape above the root
// and a sibling directory outside it are refused as outside-root.
func TestDotDotEscape_Refused(t *testing.T) {
	parent := mkRoot(t)
	root := filepath.Join(parent, "ws")
	sibling := filepath.Join(parent, "other")
	write(t, filepath.Join(root, "a.go"), "package a\n")
	write(t, filepath.Join(sibling, "secret.txt"), "s\n")

	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(root, "..", "other", "secret.txt"),
		filepath.Join(root, "..", ".."),
		sibling,
		filepath.Join(root, "a.go", "..", "..", "other"),
	} {
		if _, err := r.ResolveRead(p); ReasonOf(err) != ReasonOutsideRoot {
			t.Errorf("ResolveRead(%q) reason = %q, want %q", p, ReasonOf(err), ReasonOutsideRoot)
		}
	}
}

// TestSymlinkEscape_Refused is the core confinement test: a symlink INSIDE the root that
// points outside it must not admit the target — neither the link itself nor a path
// traversing it, and not a `..` applied after traversing it (which the OS resolves
// against the link's TARGET, not against the link's parent).
func TestSymlinkEscape_Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows; reparse-point handling is covered in meshcore/workspace")
	}
	parent := mkRoot(t)
	root := filepath.Join(parent, "ws")
	outside := filepath.Join(parent, "outside")
	write(t, filepath.Join(root, "a.go"), "package a\n")
	write(t, filepath.Join(outside, "secret.txt"), "s\n")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(root, "link"),
		filepath.Join(root, "link", "secret.txt"),
		filepath.Join(root, "link", "new-file.txt"), // a write target that does not exist yet
		// `..` AFTER a symlink component: the OS applies it to the link's TARGET, so this
		// lands outside the root. A textual Clean would wrongly read it as root/secret.txt.
		rawJoin(root, "link", "..", "secret.txt"),
	} {
		if _, err := r.ResolveRead(p); ReasonOf(err) != ReasonOutsideRoot {
			t.Errorf("ResolveRead(%q) reason = %q, want %q", p, ReasonOf(err), ReasonOutsideRoot)
		}
		if _, err := r.ResolveWrite(p); ReasonOf(err) != ReasonOutsideRoot {
			t.Errorf("ResolveWrite(%q) reason = %q, want %q", p, ReasonOf(err), ReasonOutsideRoot)
		}
	}
	// A symlink pointing back INSIDE the root stays allowed (it resolves inside).
	if err := os.Symlink(filepath.Join(root, "a.go"), filepath.Join(root, "inner")); err == nil {
		if _, err := r.ResolveRead(filepath.Join(root, "inner")); err != nil {
			t.Errorf("an in-root symlink must resolve: %v", err)
		}
	}
}

// TestSymlinkedRoot_Canonicalized pins that a root given via a symlink is canonicalized,
// so paths under the REAL directory are correctly recognized as inside it.
func TestSymlinkedRoot_Canonicalized(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	parent := mkRoot(t)
	realDir := filepath.Join(parent, "real")
	write(t, filepath.Join(realDir, "a.go"), "package a\n")
	link := filepath.Join(parent, "link-to-real")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	r, err := New(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Roots(); len(got) != 1 || got[0] != realDir {
		t.Fatalf("Roots() = %v, want [%s]", got, realDir)
	}
	if _, err := r.ResolveRead(filepath.Join(realDir, "a.go")); err != nil {
		t.Errorf("path under the real root must resolve: %v", err)
	}
}

// TestWriteDenylist_InsideRoot is the non-overridable part: these paths sit INSIDE the
// allowed root and are still refused for writing. Reads of the config trees stay allowed
// (they are not secrets) — only the key material / .env family is read-denied.
func TestWriteDenylist_InsideRoot(t *testing.T) {
	root := mkRoot(t)
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	writeDeniedOnly := []string{
		".aimesh/state.json",
		".reviewmesh/config.yaml",
		".exploremesh/profiles.yaml",
		"nested/.aimesh/adapters.yaml",
		".mcp.json",
		".cursor/rules.md",
		".claude/settings.json",
		".git/config",
		".git/hooks/pre-commit",
	}
	for _, rel := range writeDeniedOnly {
		p := filepath.Join(root, filepath.FromSlash(rel))
		_, err := r.ResolveWrite(p)
		d := denialFor(t, err)
		if d.Reason != ReasonWriteDenied {
			t.Errorf("ResolveWrite(%q) reason = %q, want %q", rel, d.Reason, ReasonWriteDenied)
		}
		if d.Rule == "" {
			t.Errorf("ResolveWrite(%q) denial carries no rule (the message must name the rule)", rel)
		}
		if _, err := r.ResolveRead(p); err != nil {
			t.Errorf("ResolveRead(%q) = %v, want allowed (only writes are denied here)", rel, err)
		}
	}
}

// TestReadAndWriteDenylist covers the secret family: refused for BOTH operations, so the
// content can never reach a prompt.
func TestReadAndWriteDenylist(t *testing.T) {
	root := mkRoot(t)
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		".env",
		".env.local",
		".env.production",
		"deploy/.env",
		"keys/id_rsa",
		"keys/id_rsa.pub",
		"keys/id_ed25519",
		"certs/server.pem",
		"certs/server.key",
		"certs/bundle.p12",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := r.ResolveRead(p); ReasonOf(err) != ReasonReadDenied {
			t.Errorf("ResolveRead(%q) reason = %q, want %q", rel, ReasonOf(err), ReasonReadDenied)
		}
		if _, err := r.ResolveWrite(p); ReasonOf(err) != ReasonWriteDenied {
			t.Errorf("ResolveWrite(%q) reason = %q, want %q", rel, ReasonOf(err), ReasonWriteDenied)
		}
	}
	// Near-misses that must stay allowed: the denylist must not swallow ordinary files.
	for _, rel := range []string{"environment.go", "env/config.go", "keyboard.go", "docs/keys.md", "a.pemx", "gitconfig"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := r.ResolveRead(p); err != nil {
			t.Errorf("ResolveRead(%q) = %v, want allowed", rel, err)
		}
		if _, err := r.ResolveWrite(p); err != nil {
			t.Errorf("ResolveWrite(%q) = %v, want allowed", rel, err)
		}
	}
}

// TestDenylistCaseFolding pins that denylist matching folds case — on a case-insensitive
// filesystem `.ENV` and `.Env` name the same file, so they must be refused too.
func TestDenylistCaseFolding(t *testing.T) {
	for _, name := range []string{".ENV", ".Env.Local", "ID_RSA", "Server.PEM"} {
		if DeniedRead(name) == "" {
			t.Errorf("DeniedRead(%q) = \"\", want a rule", name)
		}
	}
	for _, name := range []string{".AIMESH/x", ".Git/config", ".MCP.json"} {
		if DeniedWrite(filepath.FromSlash(name)) == "" {
			t.Errorf("DeniedWrite(%q) = \"\", want a rule", name)
		}
	}
}

// TestDenylistOnRelativePaths pins that the denylist helpers work on a workspace-relative
// path (the shape a containment/write layer holds), not only on absolute paths.
func TestDenylistOnRelativePaths(t *testing.T) {
	if got := DeniedWrite(filepath.FromSlash(".git/config")); got == "" {
		t.Error("DeniedWrite(.git/config) must refuse")
	}
	if got := DeniedRead(filepath.FromSlash("cfg/.env.local")); got == "" {
		t.Error("DeniedRead(cfg/.env.local) must refuse")
	}
	if got := DeniedWrite(filepath.FromSlash("pkg/main.go")); got != "" {
		t.Errorf("DeniedWrite(pkg/main.go) = %q, want allowed", got)
	}
}

// TestDenylistFollowsSymlinkTarget pins that the denylist is applied to what a path
// RESOLVES to: an innocuously named link pointing at `.env` is still read-denied.
func TestDenylistFollowsSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	root := mkRoot(t)
	write(t, filepath.Join(root, ".env"), "SECRET=1\n")
	if err := os.Symlink(filepath.Join(root, ".env"), filepath.Join(root, "notes.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveRead(filepath.Join(root, "notes.txt")); ReasonOf(err) != ReasonReadDenied {
		t.Errorf("reason = %q, want %q (the denylist must follow the resolved target)", ReasonOf(err), ReasonReadDenied)
	}
}

// TestUnresolvablePath pins the typed refusal for a path that cannot be canonicalized.
func TestUnresolvablePath(t *testing.T) {
	r, err := New(mkRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveRead("   "); ReasonOf(err) != ReasonUnresolvable {
		t.Errorf("reason = %q, want %q", ReasonOf(err), ReasonUnresolvable)
	}
}

// TestFileRoot confines to exactly one file: a caller may consent to a single path.
func TestFileRoot(t *testing.T) {
	dir := mkRoot(t)
	target := filepath.Join(dir, "only.go")
	write(t, target, "package only\n")
	write(t, filepath.Join(dir, "other.go"), "package other\n")
	r, err := New(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveRead(target); err != nil {
		t.Errorf("the file root itself must resolve: %v", err)
	}
	if _, err := r.ResolveRead(filepath.Join(dir, "other.go")); ReasonOf(err) != ReasonOutsideRoot {
		t.Error("a sibling of a single-file root must be refused")
	}
}

// TestSiblingPrefixIsNotContainment pins that root confinement is per PATH COMPONENT:
// `/x/ws-evil` must not count as inside `/x/ws` just because the string prefix matches.
func TestSiblingPrefixIsNotContainment(t *testing.T) {
	parent := mkRoot(t)
	root := filepath.Join(parent, "ws")
	evil := filepath.Join(parent, "ws-evil")
	write(t, filepath.Join(root, "a.go"), "package a\n")
	write(t, filepath.Join(evil, "a.go"), "package a\n")
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveRead(filepath.Join(evil, "a.go")); ReasonOf(err) != ReasonOutsideRoot {
		t.Errorf("reason = %q, want %q", ReasonOf(err), ReasonOutsideRoot)
	}
}

// TestMultipleRoots pins that any configured root admits, and everything else refuses.
func TestMultipleRoots(t *testing.T) {
	a, b := mkRoot(t), mkRoot(t)
	outside := mkRoot(t)
	r, err := New(a, b, a) // duplicate root is de-duplicated
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Roots()) != 2 {
		t.Fatalf("Roots() = %v, want 2 de-duplicated roots", r.Roots())
	}
	for _, root := range []string{a, b} {
		if _, err := r.ResolveWrite(filepath.Join(root, "x.go")); err != nil {
			t.Errorf("write under %q = %v, want allowed", root, err)
		}
	}
	if _, err := r.ResolveWrite(filepath.Join(outside, "x.go")); ReasonOf(err) != ReasonOutsideRoot {
		t.Error("a path under an unconfigured root must be refused")
	}
}

// TestNewRejectsUnresolvableRoot pins that a resolver is never built on a root that
// cannot be canonicalized — such a resolver would confine nothing while looking configured.
func TestNewRejectsUnresolvableRoot(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Error("New(\"\") must fail")
	}
	if _, err := New(filepath.Join(mkRoot(t), "missing", "deeper")); err != nil {
		// A not-yet-existing root canonicalizes through its existing prefix; that is fine.
		t.Errorf("a not-yet-existing root should canonicalize: %v", err)
	}
}

// TestDenialMessageNamesTheRule pins the teaching-error requirement: the message says
// what was refused and which rule refused it, and never leaks a resolved host path.
func TestDenialMessageNamesTheRule(t *testing.T) {
	root := mkRoot(t)
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ResolveWrite(filepath.Join(root, ".git", "config"))
	msg := err.Error()
	for _, want := range []string{"write", "scope_write_denied", ".git/**"} {
		if !strings.Contains(msg, want) {
			t.Errorf("denial message %q does not contain %q", msg, want)
		}
	}
	d := denialFor(t, err)
	if d.Op != OpWrite {
		t.Errorf("Op = %q, want %q", d.Op, OpWrite)
	}
}

// TestReasonsAreCodeShaped pins the machine contract: every Reason is a lower_snake code
// with no spaces or sentence punctuation, so it can be persisted and branched on.
func TestReasonsAreCodeShaped(t *testing.T) {
	for _, r := range []Reason{ReasonNoRoots, ReasonOutsideRoot, ReasonUnresolvable, ReasonReadDenied, ReasonWriteDenied} {
		s := string(r)
		if s == "" || strings.ContainsAny(s, " .:/\\") || s != strings.ToLower(s) {
			t.Errorf("Reason %q is not code-shaped", s)
		}
		if !strings.HasPrefix(s, "scope_") {
			t.Errorf("Reason %q should be namespaced with the scope_ prefix", s)
		}
	}
}
