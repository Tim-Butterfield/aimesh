// Package version holds the explore domain's build metadata, set with -ldflags at release time and
// otherwise read from the Go toolchain's build information.
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

// Get returns the build info. Values stamped with -ldflags take precedence; otherwise the toolchain's
// embedded build information is used, so `go install` from a tagged checkout reports that tag. The
// version is "dev" only when neither source has one.
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

// String returns the one-line version, such as "exploremesh 1.2.0". The rest of Info appears only in the
// JSON form.
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
