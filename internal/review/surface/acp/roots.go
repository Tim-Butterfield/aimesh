package acp

// ROOTS — which directories a call may read.
//
// An agent-driven surface has no single launch folder: an IDE can change folders after starting its
// agents, and several conversations can be active at once. So nothing is inferred from where this
// process started. Every call (an MCP tool call, an ACP turn) declares the absolute directories it is
// about, and CallScope builds that call's resolver from exactly those paths. The operator's optional
// `--root` directories are a CEILING every declared path must lie inside (ResolveCeiling).
//
// Two rules judge every root, declared or operator-named, and both on the CANONICAL form so a symlink
// cannot alias past them: the non-overridable read denylist (a protected path is never a root), and
// the degenerate-root rule (the filesystem root, a home directory or its parent, or a system/shared
// tree is never a project). An operator may waive the degenerate rule for an explicit `--root` with
// `--allow-broad-root`; a path a call declares can never waive it.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/pathexpand"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Stable MACHINE reason codes for a trusted-root refusal at launch (lower_snake, never
// sentences), so a host/wrapper can branch on them the way it branches on a run halt.
const (
	// ReasonDegenerateRoot — an EXPLICIT `--root` is a location that is never a project (the
	// filesystem root, a home directory, a system/shared tree). It is a separate code from
	// ReasonDegenerateDefaultRoot because the remedy differs: the operator named this root,
	// so the fix is to name a narrower one (or to opt in with `--allow-broad-root`), not to
	// switch from the cwd default.
	ReasonDegenerateRoot = "acp_degenerate_root"
	// ReasonRootUnusable — a `--root` does not exist, is not a directory, or cannot be resolved.
	ReasonRootUnusable = "acp_root_unusable"
	// ReasonRootDenied — a `--root` is itself a protected path (a secret directory / key
	// material); the non-overridable denylist refuses it as a root, not just inside one.
	ReasonRootDenied = "acp_root_denied"
)

// Reason codes for a path a CALL declares as its scope.
const (
	// ReasonCallPathRelative — a declared path is not absolute. A server shares no working directory
	// with its caller, so a relative path has no meaning to it.
	ReasonCallPathRelative = "scope_call_path_relative"
	// ReasonCallNoPath — the call declared no path at all.
	ReasonCallNoPath = "scope_call_no_path"
	// ReasonOutsideCeiling — a declared path is outside the operator's `--root` ceiling.
	ReasonOutsideCeiling = "scope_outside_root_ceiling"
)

// ValidateOptions says how ValidateRoot judges one candidate root.
type ValidateOptions struct {
	// Label names the path in refusals, e.g. "--root" or "workspace".
	Label string
	// AllowBroad waives the degenerate-root rule. Only an operator's explicit `--root` with
	// `--allow-broad-root` sets it; a path a call declares never does.
	AllowBroad bool
	// Home is the user's home directory ("" → os.UserHomeDir). Injectable for tests.
	Home string
}

// ValidateRoot judges one candidate root and returns its absolute form. It refuses a path that cannot
// be resolved or is not a directory, a protected path (on its spelling and its canonical form, so a
// symlink cannot alias past the rule), and — unless AllowBroad — a location that is never a project:
// the filesystem root, a home directory or its parent, or a system/shared tree. When AllowBroad waived
// the degenerate rule, broad names why the root was broad.
func ValidateRoot(raw string, o ValidateOptions) (abs, broad string, err error) {
	label := strings.TrimSpace(o.Label)
	if label == "" {
		label = "root"
	}
	r := strings.TrimSpace(raw)
	a, aerr := filepath.Abs(r)
	if r == "" || aerr != nil {
		return "", "", rootFault(ReasonRootUnusable, fmt.Sprintf("%s %q cannot be resolved", label, raw))
	}
	canon := canonicalRoot(a)
	fi, serr := os.Stat(canon)
	if serr != nil {
		return "", "", rootFault(ReasonRootUnusable, fmt.Sprintf("%s %q cannot be used: %v", label, raw, serr))
	}
	if !fi.IsDir() {
		return "", "", rootFault(ReasonRootUnusable, fmt.Sprintf("%s %q is not a directory", label, raw))
	}
	for _, form := range []string{a, canon} {
		if rule := scope.DeniedRead(form); rule != "" {
			return "", "", rootFault(ReasonRootDenied, fmt.Sprintf("%s %q is a protected path (rule %s) and can never be a root", label, raw, rule))
		}
	}
	if why := degenerateRoot(canon, o.Home); why != "" {
		if !o.AllowBroad {
			remedy := "name the project directory instead"
			if label == "--root" {
				remedy += "; if this breadth is genuinely intended, say so explicitly with `--allow-broad-root`"
			}
			return "", "", rootFault(ReasonDegenerateRoot, fmt.Sprintf("refusing %s %q as a root: %s. %s", label, raw, why, remedy))
		}
		broad = why
	}
	return a, broad, nil
}

// ExpandRoots expands each `--root` value with the same path grammar `--adapter` paths use (`%VAR%` on
// Windows, `$VAR`/`${VAR}` elsewhere, a leading `~`). An undefined variable is an error, never an empty
// expansion, so the launch refuses rather than bounding calls by a different directory.
func ExpandRoots(raw []string, env pathexpand.Env) ([]string, error) {
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if strings.TrimSpace(r) == "" {
			continue
		}
		p, err := pathexpand.Expand(strings.TrimSpace(r), env)
		if err != nil {
			return nil, fault.Wrap(fault.Usage, fmt.Sprintf("--root %q cannot be expanded", r), err).WithReason(fault.ReasonOf(err))
		}
		out = append(out, p)
	}
	return out, nil
}

// ResolveCeiling validates the operator's `--root` directories into the CEILING every call's scope must
// fall inside. No `--root` is no ceiling (nil, nil): nothing is inferred from the launch directory.
func ResolveCeiling(explicit []string, allowBroad bool, surface, home string) ([]string, error) {
	var roots, broad []string
	for _, raw := range explicit {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		abs, why, err := ValidateRoot(raw, ValidateOptions{Label: "--root", AllowBroad: allowBroad, Home: home})
		if err != nil {
			return nil, err
		}
		if why != "" {
			broad = append(broad, fmt.Sprintf("%s (%s)", abs, why))
		}
		roots = append(roots, abs)
	}
	if len(broad) > 0 {
		if strings.TrimSpace(surface) == "" {
			surface = "acp"
		}
		fmt.Fprintf(os.Stderr, "aimesh review %s: --allow-broad-root: accepting %d broad root ceiling(s): %s\n",
			surface, len(broad), strings.Join(broad, "; "))
	}
	return roots, nil
}

// CallScope builds the resolver for ONE call from the paths that call declared — its workspace and any
// extra roots. Every path must be absolute, must pass ValidateRoot with no breadth waiver, and must lie
// inside ceiling when the operator set one. The resolver's roots are exactly the declared paths, so each
// call is judged against its own scope and never against another call's.
func CallScope(paths, ceiling []string, home string) (*scope.Resolver, error) {
	var ceil *scope.Resolver
	if len(ceiling) > 0 {
		c, err := scope.New(ceiling...)
		if err != nil {
			return nil, fault.Wrap(fault.Containment, "the root ceiling cannot be resolved", err).WithHalt("M6").WithReason(ReasonRootUnusable)
		}
		ceil = c
	}
	var roots []string
	for _, raw := range paths {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			return nil, fault.New(fault.Usage, fmt.Sprintf("%q is not an absolute path; every path a call declares must be absolute, because this server does not share a working directory with its caller", raw)).
				WithReason(ReasonCallPathRelative)
		}
		abs, _, err := ValidateRoot(p, ValidateOptions{Label: "declared path", Home: home})
		if err != nil {
			return nil, fault.New(fault.Containment, err.Error()).WithHalt("M6").WithReason(fault.ReasonOf(err))
		}
		if ceil != nil {
			if _, derr := ceil.ResolveRead(abs); derr != nil {
				return nil, fault.New(fault.Containment, fmt.Sprintf("%q is outside the operator's --root ceiling; a call may declare only paths inside it", raw)).
					WithHalt("M6").WithReason(ReasonOutsideCeiling)
			}
		}
		roots = append(roots, abs)
	}
	if len(roots) == 0 {
		return nil, fault.New(fault.Usage, "the call declares no path; name the absolute workspace directory it is about").
			WithReason(ReasonCallNoPath)
	}
	r, err := scope.New(roots...)
	if err != nil || r == nil {
		return nil, fault.Wrap(fault.Containment, "the declared paths cannot be resolved", err).WithHalt("M6").WithReason(ReasonRootUnusable)
	}
	return r, nil
}

// degenerateRoot reports WHY dir is not a plausible project root, or "" when it is. The rule
// is deliberately a small, exact-match denylist of locations that are never a project: the
// filesystem/volume root, the user's home directory itself (or its parent — `/Users`,
// `/home`), and the well-known system/shared trees. It never rejects an ordinary directory (a
// temp dir IS a plausible root — that is how the web UI's isolated ACP validation runs),
// because a heuristic that guesses "this doesn't look like a project" would fail closed on
// new, still-empty projects.
//
// `dir` should already be canonical (both callers canonicalize first); canonicalRoot is
// applied again here because it is idempotent and this function must be safe to call
// directly.
func degenerateRoot(dir, home string) string {
	abs := canonicalRoot(dir)
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Sprintf("it cannot be read (%v)", err)
	}
	if !fi.IsDir() {
		return "it is not a directory"
	}
	if abs == filepath.VolumeName(abs)+string(filepath.Separator) {
		return "it is the filesystem root, so every readable file on this machine would be in scope"
	}
	if h := userHome(home); h != "" {
		hc := canonicalRoot(h)
		if sameRootPath(abs, hc) {
			return "it is your home directory itself, so every file you own would be in scope"
		}
		if sameRootPath(abs, filepath.Dir(hc)) {
			return "it is the parent of your home directory, so every user's files would be in scope"
		}
	}
	if sys := matchSystemRoot(abs); sys != "" {
		return fmt.Sprintf("it is the system/shared directory %s, not a project", sys)
	}
	return ""
}

// matchSystemRoot returns the system/shared root `abs` IS, or "".
//
// On Windows the entries are matched DRIVE-LETTER AGNOSTICALLY, against the path with its
// volume stripped. The previous list hardcoded `C:`, which made the rule a coincidence of
// where Windows happened to be installed: `D:\Windows` on any machine with a second drive
// (or any VHD/network mapping) passed straight through.
func matchSystemRoot(abs string) string {
	abs = filepath.Clean(abs)
	if windowsPaths {
		vol := filepath.VolumeName(abs)
		rest := filepath.Clean(abs[len(vol):])
		for _, s := range windowsSystemRoots {
			if strings.EqualFold(rest, filepath.Clean(s)) {
				return vol + s
			}
		}
		return ""
	}
	for _, s := range unixSystemRoots {
		if sameRootPath(abs, canonicalRoot(s)) {
			return s
		}
	}
	return ""
}

// unixSystemRoots are directories that are never a project root. EXACT matches only — a
// project UNDER one of them (a temp workspace under the OS temp dir, a checkout under
// `/srv/git`) is fine, and refusing whole subtrees would be a false refusal with no security
// value.
//
// Three families, all of them "naming this root puts an enormous amount of unrelated, often
// other-people's, content in scope":
//
//   - OS trees (`/etc`, `/usr`, `/var`, `/System`, …);
//   - multi-user roots (`/Users`, `/home`, `/root`, `/Users/Shared`) — `/Users/Shared` is
//     world-writable on macOS, so it is also a place an attacker can PLANT content;
//   - mount/volume parents (`/mnt`, `/media`, `/Volumes`, `/srv`, `/net`) — the whole point
//     of these is that arbitrary removable or network filesystems appear beneath them, so
//     "the root" is not a fixed set of files at all.
var unixSystemRoots = []string{
	"/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libexec",
	"/etc", "/dev", "/proc", "/sys", "/boot", "/run",
	"/usr", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/local", "/usr/share",
	"/opt", "/var", "/var/tmp", "/var/log", "/tmp",
	"/private", "/private/var", "/private/tmp", "/private/etc",
	"/Library", "/System", "/Applications", "/Network",
	"/Users", "/Users/Shared", "/home", "/root",
	"/mnt", "/media", "/Volumes", "/srv", "/net", "/export",
}

// windowsSystemRoots are the same families on Windows, written WITHOUT a drive letter: they
// are compared against the path's volume-stripped remainder (see matchSystemRoot).
var windowsSystemRoots = []string{
	`\Windows`, `\Windows\System32`, `\Windows\SysWOW64`,
	`\Program Files`, `\Program Files (x86)`, `\ProgramData`,
	`\Users`, `\Users\Public`, `\Documents and Settings`,
	`\$Recycle.Bin`, `\System Volume Information`,
}

// canonicalRoot resolves a path to its absolute, symlink-free form (best effort: an
// unresolvable path is returned cleaned, so comparisons still work on a non-existent path).
func canonicalRoot(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return resolved
	}
	return abs
}

// windowsPaths is true only on Windows: the filesystem is case-insensitive there, and the
// over-broad-root list has to be matched drive-letter agnostically against a volume-stripped
// path. It is a VAR — the pattern meshcore/scope's `foldPaths` and meshcore/workspace's
// `streamAliasing` already use — so that both Windows branches of the trusted-root rules can be
// exercised on any platform. This repository does not gate on Windows, so an inline
// `runtime.GOOS ==` would leave the branch that refuses `D:\Windows` compiled everywhere and
// executed nowhere. It tests the DECISION, not the filesystem; see docs/security.md's matrix.
var windowsPaths = runtime.GOOS == "windows"

// sameRootPath compares two canonical paths, folding case only on Windows — the same rule
// meshcore/scope uses.
func sameRootPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if windowsPaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// userHome returns the injected home or the OS's ("" when neither is available).
func userHome(injected string) string {
	if h := strings.TrimSpace(injected); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// rootFault types a launch-time root refusal as a USAGE fault (exit 2) carrying a stable
// machine reason: it is a wrong invocation, not a failed run.
func rootFault(reason, msg string) error {
	return fault.New(fault.Usage, msg).WithReason(reason)
}
