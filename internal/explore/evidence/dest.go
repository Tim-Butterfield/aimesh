package evidence

// This file checks the export destination and publishes the database atomically. The checks are:
//
//   - scope: the typed path is resolved with meshcore/scope using itself as the root, so the write denylist
//     (aimesh config, client configuration, .git, .env and key material) still applies;
//   - no overwrite: an existing destination or SQLite sidecar is refused unless Force is set;
//   - regular file only: a symlink, directory or device is refused, since resolution would otherwise follow
//     a final symlink;
//   - atomic replace: the database is built in a temporary file beside the destination, synced, then
//     renamed over it.
//
// Scope denials and non-regular destinations are Containment faults (halt M6); an existing destination
// without Force is a Policy fault.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Options control where an export may be written.
type Options struct {
	// Force allows replacing an existing destination and removing its stale SQLite sidecars.
	Force bool
}

// Reason codes for destination refusals raised by this package. Scope refusals carry the resolver's codes.
const (
	// ReasonDestinationExists means the destination exists and Force was not set.
	ReasonDestinationExists = "export_destination_exists"
	// ReasonDestinationNotRegular means the destination exists and is not a regular file.
	ReasonDestinationNotRegular = "export_destination_not_regular"
	// ReasonDestinationUnusable means the destination could not be resolved.
	ReasonDestinationUnusable = "export_destination_unusable"
)

// sidecarSuffixes are the files SQLite may create beside a database. They count as part of the destination,
// since a stale journal beside a new database would be read as its journal.
var sidecarSuffixes = []string{"-journal", "-wal", "-shm"}

// destination is an approved export target.
type destination struct {
	// typed is the path as the caller wrote it, used in messages.
	typed string
	// path is the resolved path the scope resolver approved.
	path string
	dir  string
}

// resolveDestination checks dbPath and returns the approved target without writing anything.
func resolveDestination(dbPath string, opts Options) (destination, error) {
	typed := strings.TrimSpace(dbPath)
	if typed == "" {
		return destination{}, fault.New(fault.Usage, "aimesh explore export: --sqlite needs a destination path")
	}

	// The typed path is the scope root; the write denylist still applies.
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

	// Lstat the typed path: the resolved path has already followed any final symlink.
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

	if !opts.Force {
		for _, p := range destinationSet(abs) {
			if _, serr := os.Lstat(p); serr == nil {
				return destination{}, existsFault(typed, p)
			}
		}
	}

	return destination{typed: typed, path: canon, dir: filepath.Dir(canon)}, nil
}

// destinationSet returns path and its SQLite sidecar paths.
func destinationSet(path string) []string {
	out := make([]string, 0, len(sidecarSuffixes)+1)
	out = append(out, path)
	for _, s := range sidecarSuffixes {
		out = append(out, path+s)
	}
	return out
}

// denialFault wraps a scope refusal as a Containment fault with halt M6 and the resolver's reason code. A
// denylist refusal also says that no flag lifts it.
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

// notRegularFault returns the refusal for a destination that is not a regular file.
func notRegularFault(typed, kind string) error {
	return fault.New(fault.Containment, fmt.Sprintf(
		"aimesh explore export: the destination %s is %s — refusing to write through it rather than following it "+
			"to whatever it names; give --sqlite a plain file path", typed, kind)).
		WithHalt("M6").WithReason(ReasonDestinationNotRegular)
}

// existsFault returns the refusal for an existing destination, naming the colliding file when it is a
// sidecar.
func existsFault(typed, collided string) error {
	what := "it"
	if collided != typed {
		what = filepath.Base(collided)
	}
	return fault.New(fault.Policy, fmt.Sprintf(
		"aimesh explore export: %s already exists (%s) — refusing to replace it. The evidence database is DERIVED, "+
			"so rebuilding it is safe by design, but the file is yours: pass --force to rebuild over it, or give "+
			"--sqlite a path that does not exist yet", typed, what)).
		WithReason(ReasonDestinationExists)
}

// writeAtomic builds the database in a temporary file in the destination's directory, syncs it and renames
// it over the destination. The temporary file shares the destination's filesystem so the rename is atomic.
// On failure the destination is untouched and the temporary file is removed.
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
	// The published database keeps os.CreateTemp's 0600 mode, since it holds prompts and model output.

	// The temporary file is also checked against the write denylist.
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
	// Sync before renaming so a crash cannot publish a name without its contents.
	if err := fsyncFile(tmp); err != nil {
		return Summary{}, fault.Wrap(fault.Config, "aimesh explore export: flush the temporary database to disk", err)
	}
	if err := os.Rename(tmp, dest.path); err != nil {
		return Summary{}, fault.Wrap(fault.Config, "aimesh explore export: publish the export to "+dest.typed, err)
	}
	committed = true
	// Remove sidecars left by a replaced database.
	for _, s := range sidecarSuffixes {
		_ = os.Remove(dest.path + s)
	}
	// Sync the directory so the rename is durable. This is best effort; Windows does not support it.
	if d, derr := os.Open(dest.dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return sum, nil
}

// tempPattern returns a hidden os.CreateTemp pattern named after the destination.
func tempPattern(path string) string {
	return "." + filepath.Base(path) + ".tmp-*"
}

// removeSet removes path and its sidecars, ignoring missing files.
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
