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

// `init` and `doctor` are SHARED, not per-domain, and each is ONE command with flags rather than a
// family of subcommands.
//
// They used to be spelled `init` / `repo init` / `folder init` in both CLIs — three commands for one
// operation, since all three called localstate.Init with nothing but a different mode. Subcommands
// should denote different operations and flags should denote modifiers of one (`git init --bare`,
// not `git bare init`); and `repo`/`folder` sitting at the top level alongside `review`/`explore`
// implied they were peer domains, which they are not. The aikit tool (github.com/Tim-Butterfield/aikit)
// made exactly this migration and states the reason for keeping the assertions available as flags:
// they are "for scripts and CI where doing the right thing silently is the wrong answer".

// initRecord is the --json shape for `aimesh init`.
type initRecord struct {
	Home    string   `json:"home"`
	Mode    string   `json:"mode"`
	Actions []string `json:"actions"`
}

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

// runDoctor reports SHARED readiness: is there a root, does the .aimesh state directory exist, is
// the shared adapters file present. It is read-only — it creates nothing, because a probe that
// initializes what it was asked to inspect cannot be run to find out whether initialization is
// needed.
//
// It REPORTS rather than blocking: with no root anywhere it prints ready=false and exits 0, since a
// read-only probe that errors tells a caller nothing about what to do next. --require-root turns
// that into an assertion for CI, where the absence of a root should fail the run.
//
// Adapter-level depth — probing binaries, guided repair, per-profile lane resolution — lives in the
// domain doctors (`aimesh review doctor`, `aimesh explore doctor`), which own the flags for it.
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

	// The assertion is evaluated AFTER the report: a caller that asked for a gate still gets the
	// diagnosis explaining what the gate tripped on.
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
