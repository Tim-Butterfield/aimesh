package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/manager"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// This file is exploremesh's HEADLESS configuration surface (`setup`) — the CLI path to everything the
// config-only web workbench can do: record/clear a shell adapter's binary path, detect/add/remove a
// user-defined ACP adapter, and create / delete a profile. Before it, the ONLY supported
// ways to configure exploremesh were the built web UI (`ui`, which needs `make ui-build-exploremesh`)
// and hand-editing YAML — so a headless install had no supported path at all, and NEITHER app could add
// an ACP adapter from a terminal.
//
// Every write goes through the SAME governed manager seams the workbench uses (ConfigureAdapterPath /
// RemoveAdapter / DetectACP / SaveACP / RemoveACP / SaveProfile / DeleteProfile), so
// this surface can neither bypass a validation rule nor drift from the UI: it renders, it does not decide.
//
// NO --scope FLAG (unlike `reviewmesh setup`): exploremesh's manager owns its write targets — adapter
// and ACP entries always go to the USER-scope shared ~/.aimesh/adapters.yaml (the single seam adapter
// locations live in, shared with reviewmesh), and profiles go to profile.DefaultProfilesPath (project
// scope inside a repo, else the user scope). There is nothing for a scope flag to select.

// argList collects a repeatable --acp-arg (order preserved). ACP launch args are passed one flag at a
// time rather than comma-split: an argument may legitimately contain a comma, and a mangled launch line
// fails looking like a broken CLI rather than a broken flag.
type argList []string

func (a *argList) String() string     { return strings.Join(*a, " ") }
func (a *argList) Set(v string) error { *a = append(*a, v); return nil }

// runSetup parses the setup surface, enforces EXACTLY ONE action, and dispatches it to a manager seam.
// Flag/shape problems are usage errors (exit 2, before any config is touched); a refused or failed write
// exits on the REFUSAL'S OWN CLASS from the shared taxonomy (a rejected profile/adapter is a config
// fault, an unreachable ACP binary an adapter fault), exactly as the equivalent `reviewmesh setup` does.
func runSetup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cliflags.Style(fs, "aimesh explore setup")
	adapter := fs.String("adapter", "", "record a binary path for this shell adapter (e.g. claude-code); use with --path")
	binPath := fs.String("path", "", "full path to a binary (use with --adapter, or with --acp detect|add)")
	removeAdapter := fs.String("remove-adapter", "", "clear this shell adapter's saved binary path (refused while the roster uses it)")
	acpAction := fs.String("acp", "", "user-defined ACP adapter action: detect (probe only) | add | remove")
	name := fs.String("name", "", "with --acp add: the instance key (default: derived from the binary); with --acp remove: the key to remove")
	title := fs.String("title", "", "with --acp add: the display title (default \"ACP: <binary>\")")
	var acpArgs argList
	fs.Var(&acpArgs, "acp-arg", "with --acp detect/add: one launch argument to try (repeatable; default --acp)")
	profileName := fs.String("profile", "", "create or replace this profile from >=2 --explorer specs + a --collator spec")
	var explorerSpecs slotSpecs
	fs.Var(&explorerSpecs, "explorer", "with --profile: an explorer as adapter=<n>,model=<m>[,effort=<e>] (repeatable; needs >=2). Slice ORDER is the preference order --count selects the top-N from")
	collatorSpec := fs.String("collator", "", "with --profile: the collator as adapter=<n>,model=<m>[,effort=<e>]")
	var canonicalizerSpecs slotSpecs
	fs.Var(&canonicalizerSpecs, "canonicalizer", "with --profile: a canonicalizer identity as adapter=<n>,model=<m>[,effort=<e>] (repeatable; supply exactly 2 or none). Two INDEPENDENT identities propose the canonicalization; omitted, the host derives them from the collator + the panel's preference order")
	defaultMode := fs.String("default-mode", "", "with --profile: the mode a run uses when --mode is omitted (known modes: "+strings.Join(mode.Names(), ", ")+")")
	// There is deliberately NO --set-default / --set-default-profile: as in reviewmesh, a no-flag run
	// always binds to the profile named `default` — save the panel you want as `default` instead.
	deleteProfile := fs.String("delete-profile", "", "delete this profile (requires --yes; refused for the default profile and for the last remaining one)")
	yes := fs.Bool("yes", false, "confirm --delete-profile non-interactively")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}

	// --path names no action by itself; say what it is missing rather than falling through to the generic
	// "nothing to do" (a bare `setup --path /x` is almost always a forgotten --adapter).
	if *binPath != "" && *adapter == "" && *acpAction == "" {
		fmt.Fprintln(stderr, "aimesh explore setup: --path requires --adapter (or --acp detect|add)")
		return int(fault.Usage)
	}

	// EXACTLY ONE action per invocation. Setup writes config; a call that quietly did two things (or
	// nothing) is the kind of surface that makes a user unsure what state they are now in.
	actions := 0
	for _, selected := range []bool{
		*adapter != "", *removeAdapter != "", *acpAction != "",
		*profileName != "", *deleteProfile != "",
	} {
		if selected {
			actions++
		}
	}
	switch {
	case actions == 0:
		fmt.Fprintln(stderr, "aimesh explore setup: nothing to do — name an action: --adapter/--path, --remove-adapter, --acp detect|add|remove, --profile or --delete-profile")
		fs.PrintDefaults() // the flags go with the guidance: a bare `setup` is someone looking for the surface
		return int(fault.Usage)
	case actions > 1:
		fmt.Fprintln(stderr, "aimesh explore setup: name exactly ONE action per invocation")
		return int(fault.Usage)
	}

	// Per-action shape guards, all BEFORE any config is read or written.
	switch {
	case *adapter != "" && *binPath == "":
		fmt.Fprintf(stderr, "aimesh explore setup: --adapter requires --path (e.g. aimesh explore setup --adapter %s --path /full/path/to/binary)\n", *adapter)
		return int(fault.Usage)
	case *deleteProfile != "" && !*yes:
		fmt.Fprintf(stderr, "aimesh explore setup: deleting the profile %q discards its explorers + collator — re-run with --yes to confirm\n", *deleteProfile)
		return int(fault.Usage)
	}
	if *acpAction != "" {
		switch *acpAction {
		case "detect", "add":
			if *binPath == "" {
				fmt.Fprintf(stderr, "aimesh explore setup: --acp %s requires --path (the ACP-capable CLI binary to launch)\n", *acpAction)
				return int(fault.Usage)
			}
		case "remove":
			if *name == "" {
				fmt.Fprintln(stderr, "aimesh explore setup: --acp remove requires --name (the saved instance key; `list` names them)")
				return int(fault.Usage)
			}
		default:
			fmt.Fprintf(stderr, "aimesh explore setup: unknown --acp action %q (want detect, add or remove)\n", *acpAction)
			return int(fault.Usage)
		}
	}
	if *profileName == "" && (len(explorerSpecs) > 0 || *collatorSpec != "" || len(canonicalizerSpecs) > 0 || *defaultMode != "") {
		fmt.Fprintln(stderr, "aimesh explore setup: --explorer/--collator/--canonicalizer/--default-mode apply only with --profile")
		return int(fault.Usage)
	}

	mgr, err := setupManager()
	if err != nil {
		fmt.Fprintf(stderr, "aimesh explore setup: %v\n", err)
		return codeOf(err)
	}

	// Every seam returns the same (messages, error) pair, so one renderer covers all of them.
	render := reporter(stdout, stderr)
	switch {
	case *adapter != "":
		return render(mgr.ConfigureAdapterPath(*adapter, *binPath))
	case *removeAdapter != "":
		return render(mgr.RemoveAdapter(*removeAdapter))
	case *acpAction != "":
		return runSetupACP(mgr, *acpAction, *name, *title, *binPath, acpArgs, stdout, stderr)
	case *profileName != "":
		return runSetupProfile(mgr, *profileName, explorerSpecs, *collatorSpec, canonicalizerSpecs, *defaultMode, stdout, stderr)
	default: // *deleteProfile != "" (guarded above)
		return render(mgr.DeleteProfile(*deleteProfile))
	}
}

// setupManager binds a manager to the config a no-flag run resolves for this folder: the SAME
// resolveSource path `explore`/`doctor`/`ui`/`list` use, so setup edits exactly what a run reads.
func setupManager() (*manager.Manager, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	r, _, _, err := resolveSource("", "")
	if err != nil {
		return nil, err
	}
	return manager.New(cwd, r)
}

// runSetupACP drives the user-defined ACP adapter flow over the manager's governed seams. There is no
// fixed ACP catalog: the user points the generic ACP driver at an ACP-capable CLI, the launch args are
// auto-detected and confirmed by a REAL handshake, and the instance lands in the shared adapters.yaml.
//
// detect and add LAUNCH THE REAL CLI (that is the only way to know whether it speaks ACP), which is why
// they are explicit verbs a user opts into rather than something any other command does implicitly. Both
// are bounded by the manager's own probe timeout plus the acpagent startup watchdog.
func runSetupACP(mgr *manager.Manager, action, name, title, binPath string, args []string, stdout, stderr io.Writer) int {
	switch action {
	case "detect":
		res, err := mgr.DetectACP(binPath, args)
		if err != nil {
			fmt.Fprintf(stderr, "aimesh explore setup: %v\n", err)
			return codeOf(err)
		}
		fmt.Fprintf(stdout, "ACP probe: %s\n", binPath)
		if !res.OK {
			// A failed probe is reported as an ADAPTER fault (exit 4 — a script must be able to branch on
			// it) WITHOUT declaring the CLI unusable: mid-login and folder-trust are the common causes, and
			// both are fixed in that CLI, not here. `aimesh review setup --acp detect` exits the same way.
			fmt.Fprintf(stdout, "  handshake: FAILED — %s\n", res.Detail)
			fmt.Fprintln(stdout, "  next step: complete the CLI's own login / folder-trust setup, then re-run detect (or `setup --acp add` anyway and re-save later).")
			return int(fault.Adapter)
		}
		fmt.Fprintln(stdout, "  handshake: OK")
		fmt.Fprintf(stdout, "  launch args: %s\n", strings.Join(res.Args, " "))
		if res.Model != "" {
			fmt.Fprintf(stdout, "  reported model: %s\n", res.Model)
		}
		if res.SuggestedTitle != "" {
			fmt.Fprintf(stdout, "  suggested title: %s\n", res.SuggestedTitle)
		}
		fmt.Fprintf(stdout, "  save it: exploremesh setup --acp add --path %s\n", binPath)
		return int(fault.OK)
	case "add":
		// A handshake failure does NOT block the save (the manager warns instead), so a CLI that is
		// mid-login can still be recorded and re-saved later to capture its model.
		return reporter(stdout, stderr)(mgr.SaveACP(name, title, binPath, args))
	default: // "remove" (validated by the caller)
		return reporter(stdout, stderr)(mgr.RemoveACP(name))
	}
}

// runSetupProfile creates or replaces a named profile from the same STRUCTURED `adapter=,model=[,effort=]`
// specs `explore --explorer/--collator/--canonicalizer` accept. The specs are parsed (and the >=2-explorer /
// collator-present / 0-or-2-canonicalizer pre-conditions checked) before the manager is asked to write; the
// deeper roster rules — unique triples, non-empty fields, a known default mode — are the manager's and
// profile.Save's, so a bad profile is refused with nothing written. To make a panel the one a no-flag
// run binds to, save it as the profile named `default` (the runtime default is fixed, as in reviewmesh).
//
// A save is WHOLE (see manager.SaveProfile): omitting --canonicalizer writes a profile with none, which is
// the honest reading of "create or replace this profile from these specs".
func runSetupProfile(mgr *manager.Manager, name string, explorerSpecs []string, collatorSpec string, canonicalizerSpecs []string, defaultMode string, stdout, stderr io.Writer) int {
	r, err := buildAdHocRoster(explorerSpecs, collatorSpec)
	if err != nil {
		fmt.Fprintf(stderr, "aimesh explore setup: --profile %s: %v\n", name, err)
		return codeOf(err) // the spec grammar's own errors carry fault.Usage → exit 2
	}
	cs, cerr := buildCanonicalizers(canonicalizerSpecs)
	if cerr != nil {
		fmt.Fprintf(stderr, "aimesh explore setup: --profile %s: %v\n", name, cerr)
		return codeOf(cerr)
	}
	render := reporter(stdout, stderr)
	return render(mgr.SaveProfile(name, profile.Profile{
		Explorers: r.Explorers, Collator: r.Collator, Canonicalizers: cs, DefaultMode: defaultMode,
	}))
}

// reporter returns the renderer for a manager seam's (messages, error) outcome: its messages on success,
// or the failure on stderr. It is a closure so a seam call can be passed straight through
// (render(mgr.X(...))) — Go only permits that when the call's results are the whole argument list.
//
// A *BlockedError (an adapter still referenced by the roster) additionally LISTS the blocking slots, so
// the user is told what to reconfigure rather than only that the removal was refused.
//
// The exit code always comes from the ERROR (codeOf), never from a literal here: the manager already
// classified every refusal it can produce, so this renderer stays a renderer.
func reporter(stdout, stderr io.Writer) func([]string, error) int {
	return func(msgs []string, err error) int {
		if err != nil {
			if blocked, ok := errors.AsType[*manager.BlockedError](err); ok {
				fmt.Fprintf(stderr, "aimesh explore setup: %s\n", blocked.Message)
				for _, u := range blocked.UsedBy {
					fmt.Fprintf(stderr, "  in use by: %s\n", u.Role)
				}
				return codeOf(err)
			}
			fmt.Fprintf(stderr, "aimesh explore setup: %v\n", err)
			return codeOf(err)
		}
		for _, m := range msgs {
			fmt.Fprintln(stdout, m)
		}
		return int(fault.OK)
	}
}
