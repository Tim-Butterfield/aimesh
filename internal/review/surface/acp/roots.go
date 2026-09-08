package acp

// TRUSTED ROOTS — who is allowed to say what this agent may read.
//
// The CLI's confinement model is "the path a human typed IS the consent": `reviewmesh review
// <dir>` makes <dir> the allowed root because a person named it. That reasoning does NOT
// transfer to ACP. Here the caller is a peer PROCESS: an editor extension, another agent, or
// anything else that can speak JSON-RPC on this machine. If a request's own `workspace` were
// treated as its own root, containment would be tautological — the peer could name any
// readable directory and have it copied and rendered into prompts that are shipped to
// external model CLIs.
//
// So the roots are established OUT OF BAND, before any request exists:
//
//   - `aimesh review acp --root <dir>` (repeatable) — the launching human's consent, exactly the
//     CLI's path argument moved to launch time.
//   - No `--root`: the process working directory at launch, because an IDE spawns its agents
//     in the project the user opened — CONSENT BY LAUNCH CONTEXT. That inference is only
//     honest when the cwd looks like a project, so a degenerate cwd (the filesystem root, the
//     home directory itself, a system directory) is REFUSED rather than adopted.
//   - `--no-default-root` declines the inference entirely: explicit roots only.
//
// Every request path is then judged against that set and may only NARROW it. Nothing a
// request contains can widen it.
//
// # The degenerate rule applies to EXPLICIT roots too
//
// Running the degenerate rule only on the inferred cwd would make it decorative: the
// documented flag walks straight past it, so `aimesh review acp --root /` would be accepted and
// grant whole-machine confinement — the exact condition the rule exists to prevent. Both
// the denylist and the degenerate rule judge EVERY root, explicit or inferred.
//
// An operator who genuinely wants a very broad root keeps a way to say so — but only by
// SAYING it: `--allow-broad-root` is an explicit, documented opt-in that applies to explicit
// `--root` values only. It never applies to the inferred cwd, because the inference's whole
// justification ("an IDE started us in the project") is what a degenerate cwd disproves.
//
// # Both rules are applied to the CANONICAL root
//
// A denylist or a degenerate-name rule that runs on the path AS SPELLED is one symlink away
// from silence: `--root ./work` pointing at `/` (or at `~/.ssh`) has no degenerate spelling
// and no protected component. The path is canonicalized FIRST and both rules are applied to
// the canonical form, so the refusal fires on the object rather than on the name. The path
// the operator typed is still what is returned and recorded — the resolver canonicalizes
// roots itself, so nothing downstream depends on this function doing it.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Stable MACHINE reason codes for a trusted-root refusal at launch (lower_snake, never
// sentences), so a host/wrapper can branch on them the way it branches on a run halt.
const (
	// ReasonNoTrustedRoot — `--no-default-root` was given with no `--root`.
	ReasonNoTrustedRoot = "acp_no_trusted_root"
	// ReasonDegenerateDefaultRoot — the launch cwd is not a plausible project root, so it is
	// NOT adopted as the default trusted root.
	ReasonDegenerateDefaultRoot = "acp_degenerate_default_root"
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

// RootSource is the PROVENANCE of a resolved trusted-root set: whether a human typed it or this
// process inferred it.
//
// It exists because the two are not the same consent, and the 2026-07-28 MCP revision makes the
// difference load-bearing. That revision deletes `roots/list`, so a client can no longer narrow a
// server the way it could under 2025-06-18 — which means the SAME launch configuration would grant
// strictly more filesystem authority to a client merely because it speaks a newer revision. The fix
// is to stop treating an inferred cwd as a trusted root on that era (see mcp.Server's waiver), and
// that is only expressible if the resolver says where its answer came from.
//
// A launch cannot tell the two apart after the fact: `--root /repo` and a launch cwd of `/repo`
// arrive as the same []string. Answering "explicit" for both would be a claim nothing checked, which
// is why the resolver returns this rather than a caller guessing.
type RootSource string

const (
	// RootsNone accompanies every error return: the resolver never returns roots with an error, and
	// never returns an empty set with a nil error.
	RootsNone RootSource = "none"
	// RootsExplicit means at least one `--root` survived validation. A human typed it.
	RootsExplicit RootSource = "explicit"
	// RootsInferredCwd means the set came from the launch working directory fallback. Nobody typed
	// it; it was inferred from where the process happened to start.
	RootsInferredCwd RootSource = "inferred_cwd"
)

// RootOptions is what the launching command knows about the trusted roots.
type RootOptions struct {
	// Surface names the launching subcommand ("acp" when empty). It appears only in the operator
	// notice a waived broad root prints, so `reviewmesh mcp` — which reuses this resolver unchanged,
	// because a peer-process caller is a peer-process caller whatever protocol it speaks — does not
	// print a notice attributing it to `acp`.
	Surface string
	// Explicit are the `--root` directories, in the order given.
	Explicit []string
	// NoDefault declines the launch-cwd default (`--no-default-root`).
	NoDefault bool
	// AllowBroadRoot is the explicit operator opt-in for an EXPLICIT `--root` that the
	// degenerate rule would otherwise refuse (`/`, a home directory, a system/shared tree).
	// It exists so the rule is a refusal an operator can override by SAYING SO, never a
	// silent acceptance — and it deliberately does NOT extend to the inferred launch cwd or
	// to the non-overridable read denylist, neither of which is an operator's to waive.
	AllowBroadRoot bool
	// AllowInferredRoot adopts the launch cwd even when it carries no PROJECT MARKER
	// (`--allow-inferred-root`). Without it, a marker-less cwd yields NO roots — not an error, so
	// the server still starts and still serves everything that consumes no trusted root.
	//
	// It does not reach the degenerate rule or the read denylist above: `/` and `~` stay refused
	// whatever this says, because "the operator meant this" is not evidence that a directory is a
	// project — it is only evidence that they accept the consequence for one that might not be.
	AllowInferredRoot bool
	// Cwd is the process working directory at launch ("" → os.Getwd). Injectable for tests.
	Cwd string
	// Home is the user's home directory ("" → os.UserHomeDir). Injectable for tests.
	Home string
}

// ResolveTrustedRoots returns the absolute trusted roots for an ACP server, THEIR PROVENANCE, or a
// typed *fault.Fault explaining why the launch must fail closed. It performs no I/O beyond
// stat-ing the candidate roots, and it never returns an empty root set with a nil error:
// "no roots" is always an error at launch, because a server with no roots can serve nothing
// but inline workspaces and the operator should learn that immediately, not on first request.
//
// The provenance return is not decoration. `--root /repo` and a launch cwd of `/repo` produce the
// same []string, so after this function nothing can tell them apart — and under MCP's 2026-07-28
// revision they must be told apart, because that revision removes the client's ability to narrow a
// server and an INFERRED root would therefore silently grant more authority than it did before. A
// caller that needs the distinction gets it here or not at all.
func ResolveTrustedRoots(o RootOptions) ([]string, RootSource, error) {
	var roots []string
	var broad []string // explicit roots the degenerate rule flagged and --allow-broad-root waived
	for _, raw := range o.Explicit {
		r := strings.TrimSpace(raw)
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, RootsNone, rootFault(ReasonRootUnusable, fmt.Sprintf("--root %q cannot be resolved: %v", raw, err))
		}
		// CANONICALIZE BEFORE JUDGING. Both rules below are about WHAT this root really is,
		// and a symlink is a one-step alias past either of them.
		canon := canonicalRoot(abs)
		fi, serr := os.Stat(canon)
		if serr != nil {
			return nil, RootsNone, rootFault(ReasonRootUnusable, fmt.Sprintf("--root %q cannot be used: %v", raw, serr))
		}
		if !fi.IsDir() {
			return nil, RootsNone, rootFault(ReasonRootUnusable, fmt.Sprintf("--root %q is not a directory (a trusted root is a project directory)", raw))
		}
		// A root that is itself protected is refused as a ROOT, not merely inside one: the
		// denylist exists so secrets never reach a prompt, and naming `~/.ssh` as the root is
		// the most direct way to ask for exactly that. Both the spelling and the canonical
		// form are judged, so neither a relative name nor a symlink alias walks past it.
		for _, form := range []string{abs, canon} {
			if rule := scope.DeniedRead(form); rule != "" {
				return nil, RootsNone, rootFault(ReasonRootDenied, fmt.Sprintf(
					"--root %q is a protected path (rule %s) and can never be a trusted root", raw, rule))
			}
		}
		// The degenerate rule applies to an EXPLICIT root as well. Checking only the inferred
		// cwd left the documented flag as a complete bypass: `--root /` granted whole-machine
		// confinement, which is precisely what the rule exists to prevent.
		if why := degenerateRoot(canon, o.Home); why != "" {
			if !o.AllowBroadRoot {
				return nil, RootsNone, rootFault(ReasonDegenerateRoot, fmt.Sprintf(
					"refusing --root %q as a trusted root: %s. Name the project directory instead; "+
						"if this breadth is genuinely intended, say so explicitly with `--allow-broad-root`", raw, why))
			}
			broad = append(broad, fmt.Sprintf("%s (%s)", abs, why))
		}
		roots = append(roots, abs)
	}
	// A waived breadth is a RECORDED fact, never a silent one: the operator opted in, and the
	// launch says out loud what was opted into.
	if len(broad) > 0 {
		surface := strings.TrimSpace(o.Surface)
		if surface == "" {
			surface = "acp"
		}
		fmt.Fprintf(os.Stderr, "aimesh review %s: --allow-broad-root: accepting %d broad trusted root(s): %s\n",
			surface, len(broad), strings.Join(broad, "; "))
	}
	if len(roots) > 0 {
		return roots, RootsExplicit, nil
	}
	if o.NoDefault {
		return nil, RootsNone, rootFault(ReasonNoTrustedRoot,
			"--no-default-root was given with no --root: this agent would have no trusted root and would refuse every request path; pass `--root <project-dir>` (repeatable)")
	}
	cwd := strings.TrimSpace(o.Cwd)
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, RootsNone, rootFault(ReasonRootUnusable, fmt.Sprintf("no --root was given and the working directory cannot be determined: %v", err))
		}
		cwd = wd
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, RootsNone, rootFault(ReasonRootUnusable, fmt.Sprintf("no --root was given and the working directory %q cannot be resolved: %v", cwd, err))
	}
	// Same order as above, and on the CANONICAL form for the same reason: a cwd that is a
	// symlink into a system tree has an innocuous spelling. `--allow-broad-root` deliberately
	// does NOT reach here — the inference's justification is that an IDE opened this project,
	// which a degenerate cwd disproves, so there is nothing to waive.
	canon := canonicalRoot(abs)
	if why := degenerateRoot(canon, o.Home); why != "" {
		return nil, RootsNone, rootFault(ReasonDegenerateDefaultRoot, fmt.Sprintf(
			"refusing to adopt the launch working directory %q as the trusted root: %s. "+
				"The default only holds when an IDE started this agent IN the project the user opened; "+
				"launch with `aimesh review acp --root <project-dir>` instead", abs, why))
	}
	for _, form := range []string{abs, canon} {
		if rule := scope.DeniedRead(form); rule != "" {
			return nil, RootsNone, rootFault(ReasonRootDenied, fmt.Sprintf(
				"refusing to adopt the launch working directory %q as the trusted root: it is a protected path (rule %s); launch with `aimesh review acp --root <project-dir>`", abs, rule))
		}
	}
	// THE INFERENCE MUST REST ON EVIDENCE, NOT ON WHERE THE PROCESS HAPPENED TO START.
	//
	// The justification for adopting a cwd at all is "a host opened this agent IN the project the
	// user opened". A PROJECT MARKER is what makes that checkable. Without one the directory is
	// merely where the process was spawned — and the degenerate-root rule above does not catch the
	// realistic over-grant, because `~/projects` or `~/src` is neither home nor home's parent, yet
	// adopting it hands a model read access across every project the user owns instead of one.
	//
	// This also replaces an era-conditional refusal that was keyed on the wrong variable. It refused
	// an inferred cwd on the newest MCP revision because that revision removes the client's ability
	// to narrow the server — but a LEGACY client that simply declines the roots capability is not
	// narrowing the server either, and it was granted the unnarrowed cwd silently. The risk was
	// never "which protocol revision"; it was "is there any reason to believe this directory is the
	// work". So the rule is now the same on both eras, and it asks that question directly.
	// NOT ADOPTING IS NOT THE SAME AS FAILING TO LAUNCH. A marker-less cwd yields NO roots and no
	// error: the server starts, every filesystem path is refused with `scope_no_roots_configured`
	// (which already names `--root`), and the paths that consume no trusted root — an inline
	// workspace — still work.
	//
	// Killing the process here instead would hand an MCP host "server disconnected" with the reason
	// on a stderr channel the stdio spec explicitly permits hosts to discard. A live server that
	// explains itself on the first call beats a dead one that explained itself where nobody looked.
	if !o.AllowInferredRoot && !hasProjectMarker(abs) {
		return nil, RootsNone, nil
	}
	return []string{abs}, RootsInferredCwd, nil
}

// projectMarkers are the files and directories whose presence makes "this directory is a project"
// an observation rather than an assumption. Ecosystem conventions only — each one exists on
// machines this tool has never seen, which is the same admission test the containment denylist
// uses. A marker-less tree is not refused outright; it needs `--root` or the explicit waiver.
var projectMarkers = []string{
	".git", ".hg", ".svn",
	"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "setup.py",
	"pom.xml", "build.gradle", "build.gradle.kts",
	"Gemfile", "composer.json", "CMakeLists.txt", "Makefile",
	".aimesh",
}

// ProjectMarkers returns the marker names, for a surface that has to TELL an operator which ones
// it looked for. A refusal that says "no project marker" without saying what one is has named a
// rule the reader cannot act on.
func ProjectMarkers() []string { return append([]string(nil), projectMarkers...) }

// hasProjectMarker reports whether dir carries any projectMarkers entry. Presence only — the
// content is never read, and a marker of the wrong TYPE (a `.git` file rather than a directory,
// as a git worktree uses) still counts, because it is equally good evidence.
func hasProjectMarker(dir string) bool {
	for _, m := range projectMarkers {
		if _, err := os.Lstat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
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
