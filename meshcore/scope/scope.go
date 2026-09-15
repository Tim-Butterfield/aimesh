// Package scope decides whether a path may be read or written, given a set of allowed roots, and
// refuses with a typed reason otherwise. Every surface is expected to route path parameters through
// one Resolver so confinement cannot diverge between surfaces.
//
// It enforces two rules:
//
//  1. Root confinement. A resolved path must lie inside an allowed root. Symlinks are resolved on
//     the longest existing prefix and `..` is applied to the resolved prefix, so neither a textual
//     escape nor an outward symlink passes. With no roots, every path is refused.
//
//  2. A denylist that applies inside roots. Every rule is matched against every path component,
//     after stripping the trailing dots and spaces Windows drops and, on Windows, any NTFS
//     alternate-data-stream suffix. It has two families.
//
//     Secrets (`.env*`, `.ssh`, `.aws`, key material, `.netrc`, ...) are refused for reads and
//     writes and are never overridable, because a read can put the bytes into a prompt.
//
//     Protected configuration (`.aimesh`, agent and IDE client configuration such as `.mcp.json`,
//     `.cursor` and `.claude`, and `.git`) is refused for writes, because files such as
//     `.git/hooks/*` and `.vscode/mcp.json` run code or redirect tools. An operator may waive
//     this family with Options.AllowProtectedWrites; no request parameter can.
//
// Checks reflect the filesystem at check time. meshcore/workspace therefore performs every read and
// write through an os.Root handle, so a component swapped to a symlink after a check cannot be
// followed out of the root.
package scope

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Op is the access a caller asked for.
type Op string

// Operations a Resolver authorizes.
const (
	OpRead  Op = "read"
	OpWrite Op = "write"
)

// Reason is the stable machine code for a refusal, suitable for branching and for audit records.
type Reason string

const (
	// ReasonNoRoots — the resolver has no allowed roots, so it fails closed.
	ReasonNoRoots Reason = "scope_no_roots_configured"
	// ReasonOutsideRoot — the canonical path is not inside any allowed root (covers a
	// `..` escape, an absolute path elsewhere, and a symlink pointing out of the root).
	ReasonOutsideRoot Reason = "scope_outside_root"
	// ReasonUnresolvable — the path could not be canonicalized (empty, or its existing
	// prefix could not be resolved).
	ReasonUnresolvable Reason = "scope_unresolvable"
	// ReasonReadDenied — the path matches the non-overridable READ denylist.
	ReasonReadDenied Reason = "scope_read_denied"
	// ReasonWriteDenied — the path matches the non-overridable WRITE denylist.
	ReasonWriteDenied Reason = "scope_write_denied"
)

// Denial is the typed refusal returned by ResolveRead / ResolveWrite. It carries the
// machine Reason, the rule that refused (for the denylist reasons), and a teaching
// message naming the rule.
type Denial struct {
	Op     Op
	Path   string // the path as the caller supplied it (never a resolved host path)
	Reason Reason
	Rule   string // the denylist rule that matched, e.g. ".env*" ("" for the root reasons)
	Detail string // extra context (e.g. the canonicalization error)
}

// Error renders a teaching message: what was refused, and which rule refused it.
func (d *Denial) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s refused for %q [%s]", d.Op, d.Path, d.Reason)
	if d.Rule != "" {
		fmt.Fprintf(&sb, ": protected path rule %s", d.Rule)
	}
	switch d.Reason {
	case ReasonNoRoots:
		sb.WriteString(": no allowed roots are configured (fail-closed)")
	case ReasonOutsideRoot:
		sb.WriteString(": the path resolves outside every allowed root")
	case ReasonWriteDenied:
		// Only the protected-configuration family can be waived, so only that refusal names the flag.
		if secretRule(normalizeComponent(filepath.Base(d.Path))) == "" && DeniedWriteWith(d.Path, true) == "" {
			sb.WriteString(". If this tree is yours and writing to it is the point, pass --allow-protected-paths (it never unlocks secrets)")
		}
	}
	if d.Detail != "" {
		sb.WriteString(": " + d.Detail)
	}
	return sb.String()
}

// AsDenial extracts a *Denial from an error chain.
func AsDenial(err error) (*Denial, bool) {
	if d, ok := errors.AsType[*Denial](err); ok {
		return d, true
	}
	return nil, false
}

// ReasonOf returns the machine Reason for an error produced by this package, or "".
func ReasonOf(err error) Reason {
	if d, ok := AsDenial(err); ok {
		return d.Reason
	}
	return ""
}

// Resolver confines path access to a fixed set of allowed roots. The zero value, like New with no
// roots, refuses every path and applies every denylist family.
type Resolver struct {
	roots []string // canonical, absolute
	opts  Options
}

// Options are operator-granted relaxations. The zero value is the strict resolver.
type Options struct {
	// AllowProtectedWrites waives the protected-configuration half of the write denylist. It
	// never waives the secret family or widens root confinement. It is an operator setting on
	// every surface and never a request parameter, since the requester is the model whose write
	// it would authorize.
	AllowProtectedWrites bool
}

// New returns a strict Resolver allowing the given roots. See NewWith for relaxations.
func New(roots ...string) (*Resolver, error) {
	return NewWith(Options{}, roots...)
}

// NewWith returns a Resolver allowing the given roots under opts. Each root is made
// absolute and canonicalized (symlinks resolved on its existing prefix); a root that
// cannot be canonicalized is an error, because a resolver built on an unresolvable root
// would silently confine nothing. Passing no roots is allowed and yields a fail-closed
// resolver — opts never makes a rootless resolver permissive.
func NewWith(opts Options, roots ...string) (*Resolver, error) {
	r := &Resolver{opts: opts}
	for _, root := range roots {
		c, err := canonical(root)
		if err != nil {
			return nil, fmt.Errorf("scope: allowed root %q: %w", root, err)
		}
		if !containsPath(r.roots, c) {
			r.roots = append(r.roots, c)
		}
	}
	return r, nil
}

// Roots returns the canonical allowed roots (a copy).
func (r *Resolver) Roots() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.roots...)
}

// ResolveRead canonicalizes path and returns it when it is inside an allowed root and
// not read-denied; otherwise it returns a *Denial.
func (r *Resolver) ResolveRead(path string) (string, error) {
	return r.resolve(OpRead, path)
}

// ResolveWrite canonicalizes path and returns it when it is inside an allowed root and
// not write-denied; otherwise it returns a *Denial. The write denylist is a superset of the read
// denylist; only its protected-configuration half is waived, by Options.AllowProtectedWrites.
func (r *Resolver) ResolveWrite(path string) (string, error) {
	return r.resolve(OpWrite, path)
}

func (r *Resolver) resolve(op Op, path string) (string, error) {
	if r == nil || len(r.roots) == 0 {
		return "", &Denial{Op: op, Path: path, Reason: ReasonNoRoots}
	}
	c, err := canonical(path)
	if err != nil {
		return "", &Denial{Op: op, Path: path, Reason: ReasonUnresolvable, Detail: err.Error()}
	}
	if !containedIn(r.roots, c) {
		return "", &Denial{Op: op, Path: path, Reason: ReasonOutsideRoot}
	}
	// The denylist is checked on the canonical path, so an innocuously named symlink to `.env`
	// is refused by what it resolves to.
	if op == OpWrite {
		if rule := DeniedWriteWith(c, r.opts.AllowProtectedWrites); rule != "" {
			return "", &Denial{Op: op, Path: path, Reason: ReasonWriteDenied, Rule: rule}
		}
		return c, nil
	}
	if rule := DeniedRead(c); rule != "" {
		return "", &Denial{Op: op, Path: path, Reason: ReasonReadDenied, Rule: rule}
	}
	return c, nil
}

// --- denylist ---
//
// Every rule is matched against every path component, not only the basename, so a denied
// directory denies its whole subtree (`some/.env/secret.txt`).

// deniedDirs are directory names whose contents are never written, wherever they appear: this
// tool's state directories (including older per-application names that existing machines may
// still hold), agent and IDE client configuration that can run code or redirect a tool, and
// `.git`, whose config and hooks run on the next git command.
var deniedDirs = map[string]string{
	".aimesh":      "~/.aimesh/** and ./.aimesh/**",
	".reviewmesh":  "~/.reviewmesh/** and ./.reviewmesh/** (legacy state location)",
	".exploremesh": "~/.exploremesh/** and ./.exploremesh/** (legacy state location)",
	".cursor":      ".cursor/**",
	".claude":      ".claude/**",
	".codex":       ".codex/**",
	".gemini":      ".gemini/**",
	".vscode":      ".vscode/** (includes .vscode/mcp.json)",
	".windsurf":    ".windsurf/**",
	".idea":        ".idea/**",
	".git":         ".git/** (includes .git/config)",
}

// deniedFiles are exact component names never written: client configuration stored as a single
// file.
var deniedFiles = map[string]string{
	".mcp.json":                  ".mcp.json",
	"claude_desktop_config.json": "claude_desktop_config.json",
}

// secretDirs are directory names whose whole subtree is credential material, denied for reads and
// writes. Bare `credentials` and `known_hosts` are not listed: `credentials` is a common source-tree
// name, and the real files live under `.aws` and `.ssh`, which are denied.
var secretDirs = map[string]string{
	".ssh":    ".ssh/**",
	".gnupg":  ".gnupg/**",
	".aws":    ".aws/** (includes .aws/credentials)",
	".azure":  ".azure/**",
	".gcloud": ".gcloud/**",
	".kube":   ".kube/**",
	".docker": ".docker/** (registry auth)",
}

// secretFiles are exact component names that hold plaintext credentials, denied for reads and
// writes.
var secretFiles = map[string]string{
	".netrc":           ".netrc",
	"_netrc":           "_netrc (Windows .netrc)",
	".npmrc":           ".npmrc (auth tokens)",
	".pypirc":          ".pypirc (auth tokens)",
	".pgpass":          ".pgpass",
	".git-credentials": ".git-credentials",
}

// keyMaterialGlobs are component patterns for private key material, denied for reads and writes.
var keyMaterialGlobs = []string{
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*",
	"*_rsa", "*_dsa", "*_ecdsa", "*_ed25519",
	"deploy_key*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.ppk",
}

// envPrefix is matched as a literal prefix, so `.envrc`, `.env-local` and `.env_production` are all
// covered.
const envPrefix = ".env"

// normalizeComponent lower-cases a path component and strips the trailing spaces and dots Windows
// drops when creating a file (`.git.` names `.git`), plus, on Windows, any stream suffix (see
// stripStream). Callers must handle "." and ".." first, since trimming would erase them.
func normalizeComponent(part string) string {
	return strings.ToLower(strings.TrimRight(stripStream(part), " ."))
}

// streamAliasing is true only on Windows. Stripping a stream suffix elsewhere would loosen the
// denylist, because `:` is an ordinary filename character there (`report:.pem` would stop matching
// `*.pem`).
var streamAliasing = runtime.GOOS == "windows"

// stripStream removes an NTFS alternate-data-stream suffix (everything from the first `:`), so
// `.git::$DATA` matches `.git`. A drive specifier such as "c:" becomes "", which callers skip. It
// does nothing outside Windows; see streamAliasing.
func stripStream(part string) string {
	if !streamAliasing {
		return part
	}
	if before, _, ok := strings.Cut(part, ":"); ok {
		return before
	}
	return part
}

// components splits a cleaned path into its components (slash-normalized).
func components(path string) []string {
	return strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
}

// secretRule returns the read and write denial rule for one normalized component, or "".
func secretRule(part string) string {
	if part == "" {
		return ""
	}
	if strings.HasPrefix(part, envPrefix) {
		return ".env*"
	}
	if rule, ok := secretDirs[part]; ok {
		return rule
	}
	if rule, ok := secretFiles[part]; ok {
		return rule
	}
	for _, g := range keyMaterialGlobs {
		if ok, _ := filepath.Match(g, part); ok {
			return "key material (" + g + ")"
		}
	}
	return ""
}

// DeniedRead returns the denylist rule refusing a read of path, or "" when allowed. path may be
// absolute or relative; every component is judged, so a file under a secret directory is denied.
func DeniedRead(path string) string {
	for _, part := range components(path) {
		if part == "" || part == "." || part == ".." {
			continue
		}
		if rule := secretRule(normalizeComponent(part)); rule != "" {
			return rule
		}
	}
	return ""
}

// DeniedWrite returns the denylist rule refusing a write to path under the strict rules, or "" when
// allowed. The write denylist is a superset of the read denylist.
func DeniedWrite(path string) string { return DeniedWriteWith(path, false) }

// DeniedWriteWith is DeniedWrite with the operator's protected-configuration waiver: with
// allowProtected set, only the secret family refuses. Reads have no such waiver.
func DeniedWriteWith(path string, allowProtected bool) string {
	for _, part := range components(path) {
		if part == "" || part == "." || part == ".." {
			continue
		}
		n := normalizeComponent(part)
		if rule := secretRule(n); rule != "" {
			return rule
		}
		if allowProtected {
			continue
		}
		if rule, ok := deniedDirs[n]; ok {
			return rule
		}
		if rule, ok := deniedFiles[n]; ok {
			return rule
		}
	}
	return ""
}

// --- canonicalization ---

// canonical turns path into an absolute, symlink-resolved path.
//
// It walks the components in order, resolving as far as the filesystem goes, so that:
//   - `..` is applied to the resolved prefix, as the OS would, so `root/link-to-elsewhere/../x`
//     is not misread as `root/x`;
//   - a path whose tail does not exist yet still canonicalizes via its longest existing prefix.
func canonical(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("empty path")
	}
	raw := path
	if !filepath.IsAbs(raw) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		// Not filepath.Join: Join cleans, collapsing `..` before symlinks on the prefix are
		// resolved.
		raw = wd + string(filepath.Separator) + raw
	}
	vol := filepath.VolumeName(raw)
	parts := strings.FieldsFunc(raw[len(vol):], func(r rune) bool { return r == '/' || r == '\\' })
	prefix := vol + string(filepath.Separator)

	i := 0
	for ; i < len(parts); i++ {
		switch parts[i] {
		case ".":
			continue
		case "..":
			resolved, err := filepath.EvalSymlinks(prefix)
			if err != nil {
				return "", err
			}
			prefix = filepath.Dir(resolved)
			continue
		}
		cand := filepath.Join(prefix, parts[i])
		if _, err := os.Lstat(cand); err != nil {
			break // this component and everything after it does not exist (yet)
		}
		prefix = cand
	}
	resolved, err := filepath.EvalSymlinks(prefix)
	if err != nil {
		return "", err
	}
	if i < len(parts) {
		resolved = filepath.Join(append([]string{resolved}, parts[i:]...)...)
	}
	return filepath.Clean(resolved), nil
}

// containedIn reports whether canonical path p is a root itself or sits beneath one.
// A root may be a single file (a caller may consent to exactly one path), in which case
// only that exact path is contained.
func containedIn(roots []string, p string) bool {
	for _, root := range roots {
		if equalPath(p, root) {
			return true
		}
		withSep := root
		if !strings.HasSuffix(withSep, string(filepath.Separator)) {
			withSep += string(filepath.Separator)
		}
		if hasPrefixPath(p, withSep) {
			return true
		}
	}
	return false
}

func containsPath(list []string, p string) bool {
	for _, e := range list {
		if equalPath(e, p) {
			return true
		}
	}
	return false
}

// equalPath and hasPrefixPath compare containment paths, folding case only on Windows. Folding on a
// case-sensitive filesystem would widen a root, so elsewhere a differently-cased path is refused.
// Denylist matching always folds case, because there folding is stricter.
func equalPath(a, b string) bool {
	if foldPaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func hasPrefixPath(p, prefix string) bool {
	if len(p) < len(prefix) {
		return false
	}
	if foldPaths {
		return strings.EqualFold(p[:len(prefix)], prefix)
	}
	return p[:len(prefix)] == prefix
}

// foldPaths is true only on Windows. It is a variable so tests can exercise the Windows comparison
// on any platform; see docs/security.md for the platform matrix.
var foldPaths = runtime.GOOS == "windows"
