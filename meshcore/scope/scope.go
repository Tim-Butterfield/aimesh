// Package scope is meshcore's path-confinement resolver: it decides whether a path a
// caller was handed may be READ or WRITTEN, given a set of allowed roots, and refuses
// with a typed, machine-readable reason otherwise.
//
// It is domain-free — it knows nothing about artifacts, models, or surfaces. Every
// surface (CLI, agent protocol, MCP) is expected to funnel path-taking parameters
// through ONE Resolver so confinement behavior cannot diverge per surface.
//
// Two independent guarantees:
//
//  1. Root confinement. A resolved path must land inside one of the configured roots.
//     Canonicalization resolves symlinks on the longest EXISTING prefix of the path and
//     applies `..` against the RESOLVED prefix, so neither a textual `..` escape nor a
//     symlink pointing out of the root can smuggle a path past the check. With NO roots
//     configured the resolver fails CLOSED: every path is refused. A caller that
//     represents direct human consent (a CLI invocation naming a path) passes the
//     human-given path AS a root.
//
//  2. A denylist that applies EVEN INSIDE a root. Every rule is matched against EVERY
//     component of the path, so a denied directory really means its whole subtree
//     (`x/.env/secret.txt` is a secret), and each component is compared with its
//     trailing dots/spaces stripped, because Windows drops those when creating a file
//     (`.git /config` names the real `.git`), and — on Windows only — with any NTFS
//     alternate-data-stream suffix removed (`.git::$DATA` resolves to the real `.git`).
//
//     The denylist has TWO FAMILIES, and they are not equally strong:
//
//     SECRETS (`.env*`, `.ssh`, `.aws`, key material, `.netrc`, …) are refused for READS
//     as well as writes, and are NEVER overridable. A read is what puts the bytes into a
//     prompt, and a prompt goes to a vendor: that disclosure cannot be undone afterwards
//     by any amount of operator intent, so there is no flag for it. See secretRule.
//
//     PROTECTED CONFIG (`.aimesh` and the per-app state dirs, agent/IDE client config
//     like `.mcp.json`/`.cursor`/`.claude`, and `.git`) is refused for WRITES, because
//     `.git/hooks/**` and `.vscode/mcp.json` are code execution and tool redirection on
//     someone else's next command. This family IS overridable, by an OPERATOR only —
//     see Options.AllowProtectedWrites. It has to be: these are ordinary directories
//     that people legitimately own and legitimately ask a review to fix, and a tool that
//     cannot be pointed at `~/.claude/hooks` is refusing the user's own tree on the
//     user's own machine. What the override does NOT do is loosen the secret family or
//     root confinement, so an override still cannot read a key or escape a root.
//
//     Neither family is reachable from a request parameter on any surface. The override
//     is a launch/CLI act, which is what keeps "configuration stays human-only" true at
//     the filesystem layer rather than at the parameter layer.
//
// Honest limit: the checks are performed on the filesystem as it is at check time. This
// package resolves links on the existing prefix and re-checks on every call (nothing is
// cached), and the intended usage — validate immediately before each write — keeps the
// window minimal; it is not a substitute for kernel-enforced resolution. The layer that
// actually TOUCHES files (meshcore/workspace) therefore performs every read and write
// through an `os.Root` handle opened once on the workspace root, so a component swapped
// to a symlink after this resolver approved the path cannot be followed out of the root.
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

const (
	OpRead  Op = "read"
	OpWrite Op = "write"
)

// Reason is the stable MACHINE code for a refusal. Values are code-shaped (lower_snake,
// no spaces, no sentences) so a caller can branch on them and persist them in an audit
// record without re-parsing prose.
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
		// NAME THE WAY PAST IT — but only for the half that HAS one. A write refused by the
		// protected-config family is waivable by the operator; one refused by the secret family is
		// not, and offering a flag that will not help there would be worse than saying nothing.
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
	var d *Denial
	if errors.As(err, &d) {
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

// Resolver confines path access to a fixed set of allowed roots.
//
// The zero value (and New with no roots) is a valid, fail-CLOSED resolver: it refuses
// every path. That is deliberate — a caller that forgot to configure roots must not
// silently get unrestricted filesystem access. The zero value also has every denylist
// family in force: an override is something a caller must ASK for, never something it
// gets by leaving a field unset.
type Resolver struct {
	roots []string // canonical, absolute
	opts  Options
}

// Options are the resolver's operator-granted relaxations. The zero value is the strict
// resolver, so a caller that ignores this type entirely gets the safe behavior.
type Options struct {
	// AllowProtectedWrites waives the PROTECTED-CONFIG half of the write denylist —
	// `.git`, the agent/IDE client config dirs, and this tool's own state dirs. It does
	// NOT waive the secret family (see secretRule), which stays refused for reads and
	// writes alike, and it does not widen root confinement: an override still writes only
	// inside a root the operator named.
	//
	// It is an OPERATOR act on every surface — a flag on the CLI, a LAUNCH flag on
	// acp/mcp — and never a request parameter, because the party a per-call waiver would
	// hand it to is the model whose write it authorizes. `.git/hooks/**` and
	// `.vscode/mcp.json` execute code on the next command, so a model that could grant
	// itself this could arrange to run arbitrary code later.
	AllowProtectedWrites bool
}

// New returns a strict Resolver allowing the given roots — every denylist family in
// force. See NewWith for the operator-granted relaxations.
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
// not write-denied; otherwise it returns a *Denial. The write denylist is a superset of
// the read denylist. Its SECRET half is never overridable; its PROTECTED-CONFIG half is
// waived only when the operator built this resolver with Options.AllowProtectedWrites.
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
	// The denylist is checked on the CANONICAL path: a symlink named innocuously but
	// pointing at `.env` (or at a config tree) is refused by what it resolves to.
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

// --- the non-overridable denylist ---
//
// EVERY rule below is matched against EVERY component of the path, not against the
// basename. A basename-only secret rule is trivially bypassed: `some/.env/secret.txt`
// has an innocuous basename yet is a file INSIDE a secret directory, and reading it
// puts the secret into a prompt just the same. Component matching is also what makes a
// denied DIRECTORY mean "this subtree", which is the only reading that matches the
// documented claims (".git/**", ".env*").

// deniedDirs are directory NAMES whose contents are never WRITTEN, wherever they appear
// in a path. Matching on the name (rather than on `~/.aimesh` specifically) covers the
// user-home copy, the per-project copy, and any nested copy with one rule.
//
// The families are (a) this tool's own state/config, (b) agent/IDE client configuration
// that can execute code or redirect a tool at a different model/server (`.vscode/mcp.json`
// declares MCP servers; `.idea`/`.windsurf` carry run configurations), and (c) `.git`,
// where `.git/config` and `.git/hooks/**` are direct code execution on the next git
// command.
//
// `.reviewmesh`/`.exploremesh` are LEGACY state locations kept deliberately: nothing writes
// them any more, but anyone who ran an earlier build still has them on disk holding real
// configuration, and a rule we cannot prove is unnecessary is a rule worth keeping.
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

// deniedFiles are exact component names never WRITTEN (agent/IDE client configuration
// that lives as a single file rather than a directory).
var deniedFiles = map[string]string{
	".mcp.json":                  ".mcp.json",
	"claude_desktop_config.json": "claude_desktop_config.json",
}

// secretDirs are directory NAMES whose whole subtree is credential material: denied for
// BOTH reads and writes, wherever they appear. Naming the DIRECTORY rather than every
// file inside it is what covers `~/.aws/credentials`, `~/.ssh/id_ecdsa`, `~/.ssh/config`
// and `~/.ssh/known_hosts` with one rule that cannot be out-run by a new filename.
//
// Deliberately NOT included as standalone names: bare `credentials` and `known_hosts`.
// `credentials` is an extremely common ordinary source-tree identifier (a Go package, a
// docs page); denying it by name would make whole legitimate subtrees unreviewable while
// adding nothing — the real file lives in `.aws/`, which IS denied. `known_hosts` is host
// fingerprints, not key material (noise, not a secret), and the real one is in `.ssh/`.
var secretDirs = map[string]string{
	".ssh":    ".ssh/**",
	".gnupg":  ".gnupg/**",
	".aws":    ".aws/** (includes .aws/credentials)",
	".azure":  ".azure/**",
	".gcloud": ".gcloud/**",
	".kube":   ".kube/**",
	".docker": ".docker/** (registry auth)",
}

// secretFiles are exact component names that carry credentials in plaintext by design:
// denied for BOTH reads and writes.
var secretFiles = map[string]string{
	".netrc":           ".netrc",
	"_netrc":           "_netrc (Windows .netrc)",
	".npmrc":           ".npmrc (auth tokens)",
	".pypirc":          ".pypirc (auth tokens)",
	".pgpass":          ".pgpass",
	".git-credentials": ".git-credentials",
}

// keyMaterialGlobs are component patterns for private key material. These are denied for
// BOTH reads and writes: a read would put the secret into a prompt.
var keyMaterialGlobs = []string{
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*",
	"*_rsa", "*_dsa", "*_ecdsa", "*_ed25519",
	"deploy_key*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.ppk",
}

// envPrefix is matched LITERALLY as a prefix, so `.envrc`, `.env-local` and
// `.env_production` are covered — the previous `.env.` rule required a dot and let all
// three through while the package documented ".env*".
const envPrefix = ".env"

// normalizeComponent lower-cases a path component and strips the trailing spaces and dots
// that Windows silently drops when it creates a file: `.git ` and `.git.` both name `.git`
// on disk, so an exact match against the untrimmed component would miss the real target.
// On Windows it also strips an NTFS alternate-data-stream suffix (see stripStream).
// Callers must handle "." and ".." BEFORE calling this (trimming would erase them).
func normalizeComponent(part string) string {
	return strings.ToLower(strings.TrimRight(stripStream(part), " ."))
}

// streamAliasing is true only on Windows. It gates stripStream, because the aliasing it
// undoes is an NTFS behavior and the same transformation would be WRONG (and LOOSER)
// elsewhere: `:` is an ordinary filename character on unix, so a real file named
// `report:.pem` would stop matching the `*.pem` key-material rule if its suffix were cut.
var streamAliasing = runtime.GOOS == "windows"

// stripStream removes an NTFS alternate-data-stream suffix from a path component. On NTFS
// `x:stream:$DATA` and `x::$DATA` both resolve to the file `x`, so `.git::$DATA` is a write
// to the real `.git` directory while an exact lookup of the untrimmed component sees a name
// no rule matches. Everything from the FIRST `:` is dropped, which also covers the
// `name:stream` and `name:stream:$DATA` forms.
//
// A component that IS a drive specifier ("c:") normalizes to "" and is skipped by the
// callers, which is correct: a volume name is not a denylisted file name.
//
// This is deliberately Windows-only (see streamAliasing): on unix the same trim would
// silently WIDEN what is allowed rather than narrow it.
func stripStream(part string) string {
	if !streamAliasing {
		return part
	}
	if i := strings.IndexByte(part, ':'); i >= 0 {
		return part[:i]
	}
	return part
}

// components splits a cleaned path into its components (slash-normalized).
func components(path string) []string {
	return strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
}

// secretRule returns the read+write denial rule for ONE normalized component, or "".
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

// DeniedRead returns the denylist rule refusing a READ of path, or "" when allowed.
// It takes any path (absolute or relative) — only the path's own components matter — so
// callers holding a workspace-relative path can use it directly. EVERY component is
// judged: a file under a secret directory is a secret.
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

// DeniedWrite returns the denylist rule refusing a WRITE to path under the STRICT rules,
// or "" when allowed. The write denylist is a superset of the read denylist.
func DeniedWrite(path string) string { return DeniedWriteWith(path, false) }

// DeniedWriteWith is DeniedWrite with the operator's protected-config waiver applied.
//
// With allowProtected set, only the SECRET family still refuses — the one whose bytes
// would reach a vendor and could not be recalled. The protected-config family (`.git`,
// the client-config dirs, this tool's own state) is skipped, which is what lets an
// operator point a review at a tree they own that happens to wear one of those names.
//
// The waiver deliberately does NOT extend to reads: DeniedRead has no variant, because
// its whole membership is the secret family.
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
// It walks the path's components in order, resolving as far as the filesystem actually
// goes, so that:
//   - a `..` is applied to the RESOLVED prefix (matching what the OS would do), not to
//     the textual path — otherwise `root/link-to-elsewhere/../x` would be mis-read as
//     `root/x` and wrongly admitted;
//   - a path whose tail does not exist yet (a file about to be created) still
//     canonicalizes, via its longest existing prefix.
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
		// Deliberately NOT filepath.Join: Join cleans, which would collapse `..`
		// textually before any symlink on the prefix is resolved.
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

// equalPath / hasPrefixPath compare CONTAINMENT paths. They fold case only on Windows,
// where the filesystem itself is case-insensitive and an exact comparison would produce
// false refusals. Everywhere else the comparison is exact: folding case would be LOOSER
// (on a case-sensitive filesystem `/ROOT/x` and `/root/x` are different directories, and
// admitting one for the other would widen the root). A differently-cased path on a
// case-insensitive non-Windows filesystem is therefore refused — fail-closed, never
// fail-open. (Denylist matching folds case unconditionally: there, folding is stricter.)
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

// foldPaths is true only on Windows. It is held in a VAR, like streamAliasing above, so the
// Windows branch of containment path comparison can be exercised on any platform — this
// repository does not gate on Windows, so an inline `runtime.GOOS ==` here would be a
// containment branch that is compiled everywhere and executed nowhere. Substituting it tests
// the DECISION; it cannot test the filesystem underneath. See docs/security.md's platform
// matrix.
var foldPaths = runtime.GOOS == "windows"
