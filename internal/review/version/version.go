// Package version holds build metadata, injected via -ldflags at release time
// and falling back to safe development values otherwise.
package version

import (
	"encoding/json"
	"runtime"

	"github.com/Tim-Butterfield/aimesh/meshcore/buildinfo"
)

// These are set at build time via:
//
//	-X github.com/Tim-Butterfield/aimesh/internal/review/version.Version=<semver>
//	-X github.com/Tim-Butterfield/aimesh/internal/review/version.Commit=<sha>
//	-X github.com/Tim-Butterfield/aimesh/internal/review/version.Date=<iso8601>
//	-X github.com/Tim-Butterfield/aimesh/internal/review/version.Dirty=<true|false>
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

// Get returns the current build info. Values stamped with -ldflags win; otherwise the Go toolchain's
// embedded build info is used, so a plain `go install` from a tagged checkout reports that tag.
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

// String renders the user-facing version line, "reviewmesh <version>". The full metadata is available
// through JSON.
func (i Info) String() string {
	return "reviewmesh " + i.Version
}

// JSON renders the machine-readable version object.
func (i Info) JSON() (string, error) {
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
