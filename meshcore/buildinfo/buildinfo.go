// Package buildinfo reads the version facts the Go toolchain embeds in a binary on its own, so a
// build that received no -ldflags can still say what it is.
//
// Since Go 1.24, `go build` and `go install` of a main package inside a VCS checkout stamp the main
// module's version from git tags (`v0.1.0` exactly at a tag, a pseudo-version such as
// `v0.1.1-0.20260908013429-f62b0bd69746` after one, with `+dirty` when the tree was modified) and
// record `vcs.revision`, `vcs.time` and `vcs.modified` as build settings. That is precisely the
// information a release stamps through -ldflags, so the two sources agree in shape and a version
// package can prefer the stamped values and fall back to these. Domain-free: it knows nothing about
// what the binary does.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Facts is what the toolchain recorded about the build, in the shape a version package reports.
type Facts struct {
	Version string // the main module's version, e.g. "v0.1.0"; never "(devel)" or ""
	Commit  string // full VCS revision, or "" when not recorded
	Date    string // VCS commit time (RFC 3339), or "" when not recorded
	Dirty   bool   // the tree had uncommitted changes when the binary was built
}

// Read returns the embedded facts and ok=true when the toolchain stamped a usable version.
// ok is false for a binary with no build info at all, for one whose main-module version is the
// "(devel)" placeholder (a `go test` binary, or a build outside any VCS checkout), and for an
// empty version — in each of those the caller's own default is the honest answer.
func Read() (Facts, bool) {
	return fromBuildInfo(debug.ReadBuildInfo())
}

func fromBuildInfo(bi *debug.BuildInfo, present bool) (Facts, bool) {
	if !present || bi == nil {
		return Facts{}, false
	}
	v := strings.TrimSpace(bi.Main.Version)
	if v == "" || v == "(devel)" {
		return Facts{}, false
	}
	f := Facts{Version: v}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			f.Commit = s.Value
		case "vcs.time":
			f.Date = s.Value
		case "vcs.modified":
			f.Dirty = s.Value == "true"
		}
	}
	// The toolchain also suffixes a modified tree's version with "+dirty"; the flag already says
	// so, and a version string that reads like a tag is what callers compare and display.
	f.Version = strings.TrimSuffix(f.Version, "+dirty")
	return f, true
}
