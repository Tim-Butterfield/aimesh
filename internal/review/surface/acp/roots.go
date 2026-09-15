package acp

// This file decides which directories a call may read.
//
// An agent surface has no launch folder: an IDE can change folders, and several conversations can
// share one server. Every call declares the absolute directories it is about, and CallScope builds
// that call's resolver from exactly those paths. The operator's optional --root directories are a
// ceiling every declared path must lie inside.
//
// Two rules judge every root on its canonical form, so a symlink cannot alias past them: the read
// denylist (a protected path is never a root) and the degenerate-root rule (the filesystem root, a
// home directory or its parent, or a system tree is never a project). Only an operator's explicit
// --root with --allow-broad-root waives the degenerate rule.

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

// Reason codes for a --root refused at launch.
const (
	// ReasonDegenerateRoot refuses a --root that is never a project: the filesystem root, a home
	// directory or a system tree. The remedy is a narrower root or --allow-broad-root.
	ReasonDegenerateRoot = "acp_degenerate_root"
	// ReasonRootUnusable refuses a --root that does not exist, is not a directory, or cannot be resolved.
	ReasonRootUnusable = "acp_root_unusable"
	// ReasonRootDenied refuses a --root that is itself a protected path.
	ReasonRootDenied = "acp_root_denied"
)

// Reason codes for a path a call declares as its scope.
const (
	// ReasonCallPathRelative refuses a declared path that is not absolute; a server shares no working
	// directory with its caller.
	ReasonCallPathRelative = "scope_call_path_relative"
	// ReasonCallNoPath refuses a call that declared no path.
	ReasonCallNoPath = "scope_call_no_path"
	// ReasonOutsideCeiling refuses a declared path outside the operator's --root ceiling.
	ReasonOutsideCeiling = "scope_outside_root_ceiling"
)

// ValidateOptions says how ValidateRoot judges one candidate root.
type ValidateOptions struct {
	// Label names the path in refusals, e.g. "--root" or "workspace".
	Label string
	// AllowBroad waives the degenerate-root rule. Only an operator's explicit `--root` with
	// `--allow-broad-root` sets it; a path a call declares never does.
	AllowBroad bool
	// Home is the user's home directory; "" means os.UserHomeDir.
	Home string
}

// ValidateRoot judges one candidate root and returns its absolute form. It refuses a path that
// cannot be resolved or is not a directory, a protected path (by spelling and canonical form), and,
// unless AllowBroad is set, a location that is never a project. When AllowBroad waived that rule,
// broad says why the root was broad.
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

// ExpandRoots expands each --root value with the path grammar --adapter paths use: %VAR% on
// Windows, $VAR or ${VAR} elsewhere, and a leading ~. An undefined variable is an error.
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

// ResolveCeiling validates the operator's --root directories into the ceiling every call's scope
// must lie inside. With no --root it returns nil, meaning no ceiling.
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

// CallScope builds the resolver for one call from the paths it declared: its workspace and any
// extra roots. Every path must be absolute, pass ValidateRoot without the breadth waiver, and lie
// inside ceiling when one is set.
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

// degenerateRoot reports why dir is not a plausible project root, or "" when it is. It is an
// exact-match list: the filesystem or volume root, the home directory or its parent, and well-known
// system trees. Ordinary directories, including temp directories, are accepted, so new empty
// projects are not refused. dir is canonicalized again because canonicalRoot is idempotent.
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

// matchSystemRoot returns the system root abs is, or "". Windows entries are matched against the
// path with its volume stripped, so D:\Windows matches as well as C:\Windows.
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

// unixSystemRoots are directories that are never a project root. Matches are exact: a project
// under one of them is fine. The list covers OS trees (/etc, /usr, /var, /System, …), multi-user
// roots (/Users, /home, /root, /Users/Shared) and mount parents (/mnt, /media, /Volumes, /srv,
// /net), each of which would put large amounts of unrelated content in scope.
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

// windowsSystemRoots are the same families on Windows, without a drive letter; see matchSystemRoot.
var windowsSystemRoots = []string{
	`\Windows`, `\Windows\System32`, `\Windows\SysWOW64`,
	`\Program Files`, `\Program Files (x86)`, `\ProgramData`,
	`\Users`, `\Users\Public`, `\Documents and Settings`,
	`\$Recycle.Bin`, `\System Volume Information`,
}

// canonicalRoot returns p's absolute, symlink-free form, or p cleaned if it cannot be resolved.
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

// windowsPaths enables case folding and volume-stripped matching. It is a variable rather than a
// runtime.GOOS check so tests can exercise the Windows branch on any platform.
var windowsPaths = runtime.GOOS == "windows"

// sameRootPath compares two canonical paths, folding case only on Windows, as meshcore/scope does.
func sameRootPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if windowsPaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// userHome returns injected, or the OS home directory, or "".
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

// rootFault returns a usage fault (exit 2) with a machine reason for a launch-time root refusal.
func rootFault(reason, msg string) error {
	return fault.New(fault.Usage, msg).WithReason(reason)
}
