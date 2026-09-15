package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// initRecord is the --json shape for `aimesh init`.
type initRecord struct {
	Home    string   `json:"home"`
	Mode    string   `json:"mode"`
	Actions []string `json:"actions"`
}

// runInit creates the .aimesh state directory. It chooses repository or folder mode from the current
// directory; --require-repo and --require-folder turn that choice into an assertion for scripts.
func runInit(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh init")
	requireRepo := fs.Bool("require-repo", false, "require a VCS repository; fail when there is none")
	requireFolder := fs.Bool("require-folder", false, "require a non-repository directory; fail when inside a repository")
	asJSON := fs.Bool("json", false, "print a machine-readable record instead of human-readable text")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	if *requireRepo && *requireFolder {
		fmt.Fprintln(errw, "aimesh init: --require-repo and --require-folder contradict each other — pass at most one")
		return int(fault.Usage)
	}

	mode, modeName := localstate.InitAuto, "auto"
	switch {
	case *requireRepo:
		mode, modeName = localstate.InitRepo, "repo"
	case *requireFolder:
		mode, modeName = localstate.InitFolder, "folder"
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errw, "aimesh init:", err)
		return int(fault.Internal)
	}
	res, err := localstate.Init(cwd, mode)
	if err != nil {
		fmt.Fprintln(errw, "aimesh init:", err)
		return int(fault.Usage)
	}

	if *asJSON {
		rec := initRecord{Home: res.Home, Mode: modeName, Actions: res.Actions}
		if rec.Actions == nil {
			rec.Actions = []string{}
		}
		b, merr := json.Marshal(rec)
		if merr != nil {
			return int(fault.Internal)
		}
		fmt.Fprintln(out, string(b))
		return int(fault.OK)
	}
	for _, a := range res.Actions {
		fmt.Fprintf(out, "  %s\n", a)
	}
	fmt.Fprintf(out, "aimesh: initialized %s\n", res.Home)
	return int(fault.OK)
}

// doctorRecord is the --json shape for `aimesh doctor`.
type doctorRecord struct {
	Ready      bool   `json:"ready"`
	Root       string `json:"root"`
	RootSource string `json:"rootSource"` // "vcs" | "cwd_no_marker"
	Home       string `json:"home"`
	HomeExists bool   `json:"homeExists"`
	Adapters   string `json:"adaptersPath"`
	AdaptersOK bool   `json:"adaptersPresent"`
}

// runDoctor reports shared readiness: the state root, the .aimesh directory and the shared adapters
// file. It creates nothing and exits 0 when not ready; --require-root makes a missing root fail.
// Adapter readiness is reported by the domain doctors.
func runDoctor(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh doctor")
	requireRoot := fs.Bool("require-root", false, "require a state root; fail when there is none")
	asJSON := fs.Bool("json", false, "print a machine-readable record instead of human-readable text")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errw, "aimesh doctor:", err)
		return int(fault.Internal)
	}

	rec := doctorRecord{Root: cwd, RootSource: "cwd_no_marker"}
	if root, ok := localstate.FindRoot(cwd); ok {
		rec.Root, rec.RootSource = root, "vcs"
	}
	rec.Home = filepath.Join(rec.Root, localstate.HomeDirName)
	if fi, serr := os.Stat(rec.Home); serr == nil && fi.IsDir() {
		rec.HomeExists = true
	}
	rec.Adapters = filepath.Join(rec.Home, "adapters.yaml")
	if _, serr := os.Stat(rec.Adapters); serr == nil {
		rec.AdaptersOK = true
	}
	rec.Ready = rec.HomeExists

	if *asJSON {
		b, merr := json.Marshal(rec)
		if merr != nil {
			return int(fault.Internal)
		}
		fmt.Fprintln(out, string(b))
	} else {
		fmt.Fprintf(out, "root:      %s (%s)\n", rec.Root, rec.RootSource)
		fmt.Fprintf(out, "state:     %s %s\n", rec.Home, presence(rec.HomeExists))
		fmt.Fprintf(out, "adapters:  %s %s\n", rec.Adapters, presence(rec.AdaptersOK))
		if rec.Ready {
			fmt.Fprintln(out, "ready:     yes")
		} else {
			fmt.Fprintln(out, "ready:     no — run `aimesh init` to create the state directory")
		}
		fmt.Fprintln(out, "\nadapter readiness is per-domain: `aimesh review doctor`, `aimesh explore doctor`")
	}

	// The assertion runs after the report, so a failing check still prints its diagnosis.
	if *requireRoot && rec.RootSource == "cwd_no_marker" {
		fmt.Fprintf(errw, "aimesh doctor: --require-root: no state root found at or above %s\n", cwd)
		return int(fault.Config)
	}
	return int(fault.OK)
}

func presence(ok bool) string {
	if ok {
		return "(present)"
	}
	return "(absent)"
}
