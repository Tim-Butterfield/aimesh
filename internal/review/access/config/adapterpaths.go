package config

import (
	"path/filepath"
	"sort"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// SharedLocationsPath is the adapters.yaml path under an explicit base dir's `.aimesh/` (e.g. an ACP
// config-snapshot home). For the ambient user/project scopes use SharedUserLocationsPath /
// SharedProjectLocationsPath instead.
func SharedLocationsPath(base string) string {
	return adapterlocations.FilePath(filepath.Join(base, localstate.HomeDirName))
}

// ACPInstance is a user-defined generic ACP adapter (re-exported so setup/webui code need not import
// meshcore/config/adapterlocations directly).
type ACPInstance = adapterlocations.ACPInstance

// SetACPInstance adds or updates a user-defined ACP adapter instance in the shared adapters.yaml at
// locPath, preserving every other entry (transactional CAS via adapterlocations.Update).
func SetACPInstance(locPath, name string, inst ACPInstance) error {
	return adapterlocations.Update(locPath, func(loc *adapterlocations.Locations) {
		loc.ACPAdapters[name] = inst
	})
}

// DeleteACPInstance removes a user-defined ACP adapter instance from the shared adapters.yaml at
// locPath. removed reports whether it was present; a missing file is a no-op. Transactional (CAS).
func DeleteACPInstance(locPath, name string) (removed bool, err error) {
	err = adapterlocations.Update(locPath, func(loc *adapterlocations.Locations) {
		_, ok := loc.ACPAdapters[name]
		removed = ok
		if ok {
			delete(loc.ACPAdapters, name)
		}
	})
	return removed, err
}

// ACPInstanceNames returns the ACP instance names defined at locPath, sorted. Empty for a
// missing/malformed file.
func ACPInstanceNames(locPath string) []string {
	loc, err := adapterlocations.Load(locPath)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(loc.ACPAdapters))
	for n := range loc.ACPAdapters {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// This file is the WRITE side of the shared adapter-location substrate: adapter binary PATHS are
// persisted to `.aimesh/adapters.yaml` (the path-only file shared with exploremesh) and NOWHERE else —
// config.yaml no longer carries them, so `.aimesh/adapters.yaml` is the single source of truth for
// paths. Each seam is a single atomic read-modify-write of the scope's adapters.yaml.

// SharedUserLocationsPath is the user-scope adapters.yaml write target (AIMESH_HOME-anchored).
func SharedUserLocationsPath() (string, error) { return adapterlocations.UserLocationsPath() }

// SharedProjectLocationsPath is the project-scope adapters.yaml write target, ROOT-anchored (walk-up
// from cwd). ok is false when cwd is not inside a repo — callers must BLOCK with guidance rather than
// silently fall back to a cwd-relative path.
func SharedProjectLocationsPath(cwd string) (string, bool) {
	return adapterlocations.ProjectLocationsPath(cwd)
}

// SetSharedAdapterPath sets `adapters.<name>.path` in the shared adapters.yaml at locPath, preserving
// every other entry. Uses adapterlocations.Update — a content-hash CAS read-modify-write — so a
// concurrent writer (the other app's UI, or the CLI) cannot clobber unrelated changes.
func SetSharedAdapterPath(locPath, name, binPath string) error {
	return adapterlocations.Update(locPath, func(loc *adapterlocations.Locations) {
		p := binPath
		loc.Adapters[name] = adapterlocations.Entry{Path: &p}
	})
}

// ClearSharedAdapterPath removes `adapters.<name>` from the shared adapters.yaml at locPath (the entry
// is path-only, so removing the key removes the override entirely — the effective path falls back to a
// lower layer, else PATH lookup). removed reports whether an entry was present. Transactional (CAS).
func ClearSharedAdapterPath(locPath, name string) (removed bool, err error) {
	err = adapterlocations.Update(locPath, func(loc *adapterlocations.Locations) {
		_, ok := loc.Adapters[name]
		removed = ok
		if ok {
			delete(loc.Adapters, name)
		}
	})
	return removed, err
}

// SharedAdapterNames returns the adapter names that have an explicit path entry in the shared
// adapters.yaml at locPath, sorted. Empty for a missing/malformed file.
func SharedAdapterNames(locPath string) []string {
	loc, err := adapterlocations.Load(locPath)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(loc.Adapters))
	for n, e := range loc.Adapters {
		if e.Path != nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// CopySharedAdapterPaths copies every explicit path entry from the src shared adapters.yaml into dst
// (preserving dst's other entries; a src clear "" copies as a clear), atomically. It returns the
// adapter names copied, in sorted order. A src with no entries is a no-op (nil, no error).
func CopySharedAdapterPaths(src, dst string) ([]string, error) {
	srcLoc, err := adapterlocations.Load(src)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(srcLoc.Adapters))
	for n, e := range srcLoc.Adapters {
		if e.Path != nil {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	err = adapterlocations.Update(dst, func(loc *adapterlocations.Locations) {
		for _, n := range names {
			p := *srcLoc.Adapters[n].Path
			loc.Adapters[n] = adapterlocations.Entry{Path: &p}
		}
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// SharedHasAdapterPath reports whether the shared adapters.yaml at locPath has an EXPLICIT entry for
// name (a non-nil path, including the empty-string "use PATH" clear — any presence is a saved
// override at this scope). A missing/malformed file is false.
func SharedHasAdapterPath(locPath, name string) bool {
	loc, err := adapterlocations.Load(locPath)
	if err != nil {
		return false
	}
	e, ok := loc.Adapters[name]
	return ok && e.Path != nil
}
