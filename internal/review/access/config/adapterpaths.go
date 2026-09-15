package config

import (
	"sort"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
)

// This file writes the shared `.aimesh/adapters.yaml`, the only place adapter binary paths and ACP
// instances are persisted. Each write is an atomic compare-and-swap read-modify-write through
// adapterlocations.Update, so a concurrent writer cannot clobber unrelated entries.

// ACPInstance is a user-defined ACP adapter instance.
type ACPInstance = adapterlocations.ACPInstance

// SetACPInstance adds or updates an ACP adapter instance in the adapters.yaml at locPath.
func SetACPInstance(locPath, name string, inst ACPInstance) error {
	return adapterlocations.Update(locPath, func(loc *adapterlocations.Locations) {
		loc.ACPAdapters[name] = inst
	})
}

// DeleteACPInstance removes an ACP adapter instance from the adapters.yaml at locPath and reports
// whether it was present.
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

// ACPInstanceNames returns the sorted ACP instance names at locPath, or nil when the file is missing
// or malformed.
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

// SharedUserLocationsPath returns the user-scope adapters.yaml path, anchored at AIMESH_HOME.
func SharedUserLocationsPath() (string, error) { return adapterlocations.UserLocationsPath() }

// SharedProjectLocationsPath returns the project-scope adapters.yaml path at the repository root
// found by walking up from cwd. ok is false outside a repository; callers must refuse rather than
// fall back to a cwd-relative path.
func SharedProjectLocationsPath(cwd string) (string, bool) {
	return adapterlocations.ProjectLocationsPath(cwd)
}

// SetSharedAdapterPath sets `adapters.<name>.path` in the adapters.yaml at locPath.
func SetSharedAdapterPath(locPath, name, binPath string) error {
	return adapterlocations.Update(locPath, func(loc *adapterlocations.Locations) {
		p := binPath
		loc.Adapters[name] = adapterlocations.Entry{Path: &p}
	})
}

// SharedAdapterNames returns the sorted names of adapters with an explicit path entry at locPath, or
// nil when the file is missing or malformed.
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

// CopySharedAdapterPaths copies every explicit path entry, including empty clears, from the src
// adapters.yaml into dst, preserving dst's other entries. It returns the sorted names copied; a src
// with no entries is a no-op.
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
