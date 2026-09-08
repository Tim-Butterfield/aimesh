package evidence

// This file is the export's DESTINATION GUARD and its ATOMIC PUBLISH step — the two things that make
// `aimesh explore export --sqlite <path>` a governed write rather than an unguarded one.
//
// Without them the export would take any `--sqlite` value, `os.Remove` the file (plus its SQLite
// sidecars) and rewrite it: no scope resolver, no denylist, no confirmation, no
// atomicity. `aimesh explore export --sqlite ~/.aimesh/adapters.yaml --run <dir>` would then DELETE the
// shared adapter configuration — the exact file the non-overridable write denylist protects on every
// other surface in the repo — and a mistyped path would silently destroy whatever was already there.
// The app is documented as leaving the user's files alone; both cannot be true.
//
// Four properties are enforced here, and each one answers a specific way an unguarded export loses data:
//
//   - CONFINEMENT + THE DENYLIST (`meshcore/scope`). The consent model is the CLI's own and is stated
//     explicitly: a human typed this path at a terminal, so THAT PATH — and nothing else — is the
//     allowed root for this one operation. The resolver is therefore built with the typed path as its
//     root, which cannot widen anything. What the human's consent does NOT lift is the non-overridable
//     write denylist: `~/.aimesh/**` and `./.aimesh/**`, the per-app config trees, IDE/agent client
//     configuration, `.git/**`, `.env*` and key material stay refused no matter who asked. That is the
//     point of a denylist that is not configurable.
//   - NO SILENT CLOBBER. An existing destination is refused unless the caller passed `--force`. The
//     database is derived and a rebuild really is a rebuild — but the FILE is the user's, and a typo
//     must not be the thing that discovers that.
//   - ATOMIC REPLACE. The database is built into a temporary file in the destination's own directory,
//     fsynced, and then renamed over the destination. An export killed halfway can no longer leave a
//     truncated database where a valid one was: the rename is the only moment the destination changes,
//     and it either happened or it did not.
//   - A REGULAR FILE ONLY. A destination that is a directory, a symlink or a device is refused rather
//     than followed. The symlink case matters most: canonicalization RESOLVES a final symlink, so the
//     scope check alone would happily approve a link pointing at something the user never named.
//
// Refusals are typed into the SHARED aimesh halt taxonomy rather than into a new one:
//   - a scope denial → CONTAINMENT (exit 6), halt class M6, carrying the resolver's own machine reason
//     code — the same shape `reviewmesh`'s MCP surface uses for the same refusal;
//   - a non-regular destination → CONTAINMENT (exit 6) too: refusing to follow a link or write through
//     an inode nobody consented to is confinement, and one class keeps "the destination is not a thing
//     we may write" a single, teachable rule;
//   - an existing destination without `--force` → POLICY (exit 7). It is not a breach — it is a consent
//     gate the operator can lift with a flag, which is exactly what "halted by policy" means.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Options govern the DESTINATION of an export — never its content. The export is a pure function of the
// run directory; everything selectable about it is about where the bytes are allowed to land.
type Options struct {
	// Force permits REPLACING an existing destination (and clearing the stale SQLite sidecars beside
	// it). Without it an existing destination is refused. `--force` rather than `--yes` deliberately:
	// `--yes` is this repo's confirmation for a destructive CONFIG action (`setup --delete-profile
	// --yes`), whereas this is the ordinary "overwrite the output file" gate that `cp -f`/`ln -f` have
	// spelled `--force` for decades, and the refusal message names the flag so nobody has to guess.
	Force bool
}

// Machine reason codes for the refusals this package raises itself (the scope refusals carry the
// resolver's own codes). Code-shaped, so a caller can branch on them and an audit record can persist
// them without re-parsing prose.
const (
	// ReasonDestinationExists — the destination already exists and Force was not set.
	ReasonDestinationExists = "export_destination_exists"
	// ReasonDestinationNotRegular — the destination exists and is not a plain file.
	ReasonDestinationNotRegular = "export_destination_not_regular"
	// ReasonDestinationUnusable — the destination could not be canonicalized at all.
	ReasonDestinationUnusable = "export_destination_unusable"
)

// sidecarSuffixes are the files SQLite can leave BESIDE a database. They are part of the destination for
// every purpose here: a stale journal next to a fresh database is a corruption waiting to be blamed on
// the export, so they are covered by the no-clobber check and cleared on a successful publish.
var sidecarSuffixes = []string{"-journal", "-wal", "-shm"}

// destination is a checked, approved export target.
type destination struct {
	// typed is exactly what the caller supplied. Every message quotes THIS, never the resolved host
	// path — the user can only correct what they wrote.
	typed string
	// path is the canonical path the bytes are actually written to (symlinks resolved on the existing
	// prefix, `..` applied against the resolved prefix) — the path the scope resolver approved.
	path string
	dir  string
}

// resolveDestination applies the four checks above and returns the approved target. It touches nothing:
// a refusal must cost the user nothing, and must be reachable before any work has been done.
func resolveDestination(dbPath string, opts Options) (destination, error) {
	typed := strings.TrimSpace(dbPath)
	if typed == "" {
		return destination{}, fault.New(fault.Usage, "aimesh explore export: --sqlite needs a destination path")
	}

	// (1) CONFINEMENT + DENYLIST. The typed path IS the root: direct human consent, narrowed to the one
	// path that consent covers. The denylist still applies inside it, and nothing can lift that.
	res, err := scope.New(typed)
	if err != nil {
		return destination{}, fault.Wrap(fault.Containment,
			"aimesh explore export: the destination "+typed+" cannot be resolved, so it cannot be confined", err).
			WithHalt("M6").WithReason(ReasonDestinationUnusable)
	}
	canon, err := res.ResolveWrite(typed)
	if err != nil {
		return destination{}, denialFault(err)
	}

	abs, err := filepath.Abs(typed)
	if err != nil {
		return destination{}, fault.Wrap(fault.Containment,
			"aimesh explore export: the destination "+typed+" cannot be made absolute", err).
			WithHalt("M6").WithReason(ReasonDestinationUnusable)
	}

	// (2) A REGULAR FILE ONLY. Lstat the TYPED path, not the canonical one: canonicalization already
	// resolved a final symlink, and refusing the link rather than following it is the whole check.
	if fi, lerr := os.Lstat(abs); lerr == nil {
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			return destination{}, notRegularFault(typed, "a symbolic link")
		case fi.IsDir():
			return destination{}, notRegularFault(typed, "a directory")
		case !fi.Mode().IsRegular():
			return destination{}, notRegularFault(typed, "not a regular file (mode "+fi.Mode().String()+")")
		}
	} else if !os.IsNotExist(lerr) {
		return destination{}, fault.Wrap(fault.Config, "aimesh explore export: inspect the destination "+typed, lerr)
	}

	// (3) NO SILENT CLOBBER. The destination is the database AND its sidecars.
	if !opts.Force {
		for _, p := range destinationSet(abs) {
			if _, serr := os.Lstat(p); serr == nil {
				return destination{}, existsFault(typed, p)
			}
		}
	}

	return destination{typed: typed, path: canon, dir: filepath.Dir(canon)}, nil
}

// destinationSet is the database path plus every sidecar SQLite might have left beside it.
func destinationSet(path string) []string {
	out := make([]string, 0, len(sidecarSuffixes)+1)
	out = append(out, path)
	for _, s := range sidecarSuffixes {
		out = append(out, path+s)
	}
	return out
}

// denialFault types a scope refusal the way every other aimesh surface types one: a CONTAINMENT halt
// (exit 6) in class M6, carrying the resolver's own MACHINE reason code, with the resolver's message —
// which already names the rule that refused — as the cause. The appended sentence is the teaching half:
// a denylist refusal has no flag, and a reader who has just been told "refused" needs to know that
// before they go looking for one.
func denialFault(err error) error {
	msg := "aimesh explore export: the destination is refused by the workspace scope policy"
	if scope.ReasonOf(err) == scope.ReasonWriteDenied {
		msg = "aimesh explore export: the destination is refused by the NON-OVERRIDABLE write denylist — " +
			"no flag lifts it (not --force), because that denylist is what keeps configuration, credentials " +
			"and agent/IDE settings human-only; export somewhere else"
	}
	return fault.Wrap(fault.Containment, msg, err).
		WithHalt("M6").WithReason(string(scope.ReasonOf(err)))
}

// notRegularFault refuses a destination that is not a plain file, saying which kind it is.
func notRegularFault(typed, kind string) error {
	return fault.New(fault.Containment, fmt.Sprintf(
		"aimesh explore export: the destination %s is %s — refusing to write through it rather than following it "+
			"to whatever it names; give --sqlite a plain file path", typed, kind)).
		WithHalt("M6").WithReason(ReasonDestinationNotRegular)
}

// existsFault refuses to replace something that is already there, naming the flag that would allow it.
func existsFault(typed, collided string) error {
	what := "it"
	if collided != typed {
		// A sidecar collision needs saying out loud: the user is looking at `--sqlite x.db` and the thing
		// in the way is `x.db-wal`, which they will not otherwise think to look for.
		what = filepath.Base(collided)
	}
	return fault.New(fault.Policy, fmt.Sprintf(
		"aimesh explore export: %s already exists (%s) — refusing to replace it. The evidence database is DERIVED, "+
			"so rebuilding it is safe by design, but the file is yours: pass --force to rebuild over it, or give "+
			"--sqlite a path that does not exist yet", typed, what)).
		WithReason(ReasonDestinationExists)
}

// --- the atomic publish ---

// writeAtomic builds the database into a temporary file in the destination's OWN directory, fsyncs it,
// and renames it over the destination. Same directory on purpose: rename is only atomic within a
// filesystem, and a temp file in $TMPDIR could land on a different one, degrading the publish into a
// copy — which is exactly the truncation window this is here to close.
//
// Until the rename, the destination is untouched. Every failure path therefore leaves whatever was there
// intact, and removes the temporary file (and any sidecar of ITS own) rather than leaving debris.
func writeAtomic(runDir string, rec *runRecord, dest destination) (Summary, error) {
	if err := os.MkdirAll(dest.dir, 0o755); err != nil {
		return Summary{}, fault.Wrap(fault.Config, "aimesh explore export: create the destination directory "+dest.dir, err)
	}
	f, err := os.CreateTemp(dest.dir, tempPattern(dest.path))
	if err != nil {
		return Summary{}, fault.Wrap(fault.Config, "aimesh explore export: create a temporary file beside "+dest.typed, err)
	}
	tmp := f.Name()
	_ = f.Close()
	// The published database therefore inherits os.CreateTemp's 0600 rather than the 0644-and-umask a
	// direct SQLite create used to produce. That is left as it is, deliberately: it is strictly TIGHTER
	// than any previous outcome, it is what an evidence database full of prompts and model output should
	// be, and widening it back to 0644 here would ignore the operator's umask instead of respecting it.

	// The temp sibling is judged by the SAME non-overridable denylist as the destination. It sits in a
	// directory the human's typed path already named, so confinement is not in question — but the
	// denylist is checked on every path this code writes, without exception, rather than on the ones it
	// happens to think of as interesting.
	if rule := scope.DeniedWrite(tmp); rule != "" {
		removeSet(tmp)
		return Summary{}, fault.New(fault.Containment, fmt.Sprintf(
			"aimesh explore export: the temporary file for %s falls under protected path rule %s", dest.typed, rule)).
			WithHalt("M6").WithReason(string(scope.ReasonWriteDenied))
	}

	committed := false
	defer func() {
		if !committed {
			removeSet(tmp)
		}
	}()

	sum, err := build(runDir, rec, tmp)
	if err != nil {
		return Summary{}, err
	}
	// fsync BEFORE the rename: a rename that reaches the disk ahead of the file's own contents would
	// publish a name pointing at nothing after a crash, which is the failure this whole path exists to
	// prevent.
	if err := fsyncFile(tmp); err != nil {
		return Summary{}, fault.Wrap(fault.Config, "aimesh explore export: flush the temporary database to disk", err)
	}
	if err := os.Rename(tmp, dest.path); err != nil {
		return Summary{}, fault.Wrap(fault.Config, "aimesh explore export: publish the export to "+dest.typed, err)
	}
	committed = true
	// Sidecars of the PREVIOUS database at this path (only reachable with --force, which consented to
	// replacing the whole set): a stale journal beside the new file would be read as its journal.
	for _, s := range sidecarSuffixes {
		_ = os.Remove(dest.path + s)
	}
	// Directory fsync makes the rename itself durable. Best-effort: not every platform (Windows) allows
	// syncing a directory handle, and the export has already succeeded either way.
	if d, derr := os.Open(dest.dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return sum, nil
}

// tempPattern names the temporary file after its destination — dot-prefixed so it is hidden, and
// `.tmp-` suffixed so anything that survives a hard kill is obviously debris and obviously ours.
func tempPattern(path string) string {
	return "." + filepath.Base(path) + ".tmp-*"
}

// removeSet deletes a database path and every sidecar beside it, ignoring what is not there.
func removeSet(path string) {
	for _, p := range destinationSet(path) {
		_ = os.Remove(p)
	}
}

// fsyncFile forces the file's contents to stable storage.
func fsyncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
