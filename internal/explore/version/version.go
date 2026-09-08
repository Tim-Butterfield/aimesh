// Package version holds build metadata, injected via -ldflags at release time
// and falling back to safe development values otherwise. It mirrors reviewmesh's
// version package exactly — both binaries answer `--version` the same way.
package version

import (
	"encoding/json"
	"runtime"

	"github.com/Tim-Butterfield/aimesh/meshcore/buildinfo"
)

// These are set at build time via:
//
//	-X github.com/Tim-Butterfield/aimesh/internal/explore/version.Version=<semver>
//	-X github.com/Tim-Butterfield/aimesh/internal/explore/version.Commit=<sha>
//	-X github.com/Tim-Butterfield/aimesh/internal/explore/version.Date=<iso8601>
//	-X github.com/Tim-Butterfield/aimesh/internal/explore/version.Dirty=<true|false>
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
	Dirty   = "true"
)

// Info is the machine-readable form of the build metadata.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get returns the current build info. The -ldflags-stamped values win when a release set them;
// otherwise the facts the Go toolchain embedded on its own are used, so a plain
// `go install ./cmd/aimesh` from a tagged checkout reports that tag (and a pseudo-version after
// it) instead of "dev". "dev" remains the answer only when neither source knows better.
func Get() Info {
	i := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		Dirty:     Dirty == "true",
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	if Version == "dev" {
		if f, ok := buildinfo.Read(); ok {
			i.Version, i.Dirty = f.Version, f.Dirty
			if f.Commit != "" {
				i.Commit = f.Commit
			}
			if f.Date != "" {
				i.Date = f.Date
			}
		}
	}
	return i
}

// String renders the single user-facing version line: "exploremesh <version>" (e.g.
// "exploremesh dev" for an unstamped local build, "exploremesh 1.2.0" for a release).
// The full build metadata (commit/date/dirty/go/os/arch) is retained in Info for the
// `--version --json` form and release stamping/audit use, but is NOT shown by default.
func (i Info) String() string {
	return "exploremesh " + i.Version
}

// JSON renders the machine-readable version object.
func (i Info) JSON() (string, error) {
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
