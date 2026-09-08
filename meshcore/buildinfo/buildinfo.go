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
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

// Facts is what the toolchain recorded about the build, in the shape a version package reports.
type Facts struct {
	// Version is the human form: the tag exactly at a tag ("v0.1.0"), the nearest tag plus the
	// commit after one ("v0.1.0+3d6b10e"), "dev+3d6b10e" when no tag precedes the commit, and a
	// ".dirty" suffix when the tree was modified. It is semver build metadata, so it sorts as the
	// tag it names and never claims a version that does not exist.
	Version string
	// ModuleVersion is the toolchain's own string, untouched: the tag, or a pseudo-version such as
	// "v0.1.1-0.20260908022103-3d6b10e9b8fa" — which names the NEXT patch and encodes the commit
	// time, and is what `go version -m` prints.
	ModuleVersion string
	Commit        string // full VCS revision, or "" when not recorded
	Date          string // VCS commit time (RFC 3339), or "" when not recorded
	Dirty         bool   // the tree had uncommitted changes when the binary was built
}

// pseudoVersion matches the three pseudo-version shapes the toolchain produces:
//
//	v0.0.0-yyyymmddhhmmss-abcdefabcdef              no tag precedes the commit
//	vX.Y.Z-0.yyyymmddhhmmss-abcdefabcdef            the nearest tag is vX.Y.(Z-1)
//	vX.Y.Z-pre.0.yyyymmddhhmmss-abcdefabcdef        the nearest tag is the pre-release vX.Y.Z-pre
//
// Group 1 is everything before the timestamp; group 3 is the 12-hex-digit commit prefix.
var pseudoVersion = regexp.MustCompile(`^(.*?)[-.](\d{14})-([0-9a-f]{12})$`)

// displayVersion turns the toolchain's module version into the human form described on Facts.
func displayVersion(module string, dirty bool) string {
	if strings.HasSuffix(module, "+dirty") {
		dirty = true
		module = strings.TrimSuffix(module, "+dirty")
	}
	v := module
	if m := pseudoVersion.FindStringSubmatch(module); m != nil {
		base, hash := m[1], m[3][:7]
		switch {
		case base == "v0.0.0":
			v = "dev+" + hash
		case strings.HasSuffix(base, "-0"):
			v = decrementPatch(strings.TrimSuffix(base, "-0")) + "+" + hash
		case strings.HasSuffix(base, ".0"):
			v = strings.TrimSuffix(base, ".0") + "+" + hash
		default:
			v = base + "+" + hash
		}
	}
	if dirty {
		if strings.Contains(v, "+") {
			return v + ".dirty"
		}
		return v + "+dirty"
	}
	return v
}

// decrementPatch maps the "vX.Y.Z" a pseudo-version points AT back to the "vX.Y.(Z-1)" tag it
// came FROM. Anything that does not parse is returned unchanged rather than guessed at.
func decrementPatch(v string) string {
	i := strings.LastIndex(v, ".")
	if i < 0 {
		return v
	}
	n, err := strconv.Atoi(v[i+1:])
	if err != nil || n == 0 {
		return v
	}
	return v[:i+1] + strconv.Itoa(n-1)
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
	f := Facts{ModuleVersion: v}
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
	f.Dirty = f.Dirty || strings.HasSuffix(v, "+dirty")
	f.Version = displayVersion(v, f.Dirty)
	return f, true
}
