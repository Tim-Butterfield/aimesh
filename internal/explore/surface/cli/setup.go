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

// This file implements `aimesh explore setup`: record or clear a shell adapter's binary path, detect, add
// or remove a user-defined ACP adapter, and create or delete a profile. Every write goes through the
// manager, which validates it; this file only parses flags and renders results.
//
// There is no --scope flag. Adapter and ACP entries go to the user-scope shared ~/.aimesh/adapters.yaml,
// and profiles to profile.DefaultProfilesPath (the project scope inside a repository, else the user scope).

// argList collects a repeatable --acp-arg in order. Arguments are not comma-split, because a launch
// argument may contain a comma.
type argList []string

func (a *argList) String() string     { return strings.Join(*a, " ") }
func (a *argList) Set(v string) error { *a = append(*a, v); return nil }

// runSetup parses the setup flags, requires exactly one action, and runs it through the manager. Flag
// problems exit with a usage error before any configuration is read; a refused write exits with the
// refusal's own fault class.
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
	// There is no flag to choose the default profile: a run without flags uses the profile named `default`.
	deleteProfile := fs.String("delete-profile", "", "delete this profile (requires --yes; refused for the default profile and for the last remaining one)")
	yes := fs.Bool("yes", false, "confirm --delete-profile non-interactively")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}

	// --path alone names no action; say which flag is missing.
	if *binPath != "" && *adapter == "" && *acpAction == "" {
		fmt.Fprintln(stderr, "aimesh explore setup: --path requires --adapter (or --acp detect|add)")
		return int(fault.Usage)
	}

	// Exactly one action per invocation, so the resulting configuration state is unambiguous.
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
		fs.PrintDefaults()
		return int(fault.Usage)
	case actions > 1:
		fmt.Fprintln(stderr, "aimesh explore setup: name exactly ONE action per invocation")
		return int(fault.Usage)
	}

	// Per-action flag checks, before any configuration is read or written.
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

	// Every manager write returns (messages, error), so one renderer covers them all.
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

// setupManager returns a manager for the configuration a run without flags resolves in this directory,
// through the same resolveSource that run, doctor and list use.
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

// runSetupACP runs an ACP adapter action. detect and add launch the real CLI to confirm it speaks ACP,
// bounded by the manager's probe timeout, so no other command does this implicitly.
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
			// A failed probe exits with an adapter fault (4). Login and folder trust, the usual causes, are
			// fixed in the CLI itself.
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
		fmt.Fprintf(stdout, "  save it: aimesh explore setup --acp add --path %s\n", binPath)
		return int(fault.OK)
	case "add":
		// A failed handshake still saves, with a warning (see manager.SaveACP).
		return reporter(stdout, stderr)(mgr.SaveACP(name, title, binPath, args))
	default: // "remove" (validated by the caller)
		return reporter(stdout, stderr)(mgr.RemoveACP(name))
	}
}

// runSetupProfile creates or replaces a named profile from the `adapter=,model=[,effort=]` specs that
// `explore --explorer/--collator/--canonicalizer` accept. The specs are parsed here; the roster rules are
// checked by the manager, and a bad profile writes nothing. The profile is replaced whole, so omitting
// --canonicalizer saves a profile with none.
func runSetupProfile(mgr *manager.Manager, name string, explorerSpecs []string, collatorSpec string, canonicalizerSpecs []string, defaultMode string, stdout, stderr io.Writer) int {
	r, err := buildAdHocRoster(explorerSpecs, collatorSpec)
	if err != nil {
		fmt.Fprintf(stderr, "aimesh explore setup: --profile %s: %v\n", name, err)
		return codeOf(err)
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

// reporter returns a renderer for a manager write's (messages, error) result, so a call can be passed
// straight through as render(mgr.X(...)). It prints the messages on success, or the error on stderr,
// listing the blocking slots of a *BlockedError. The exit code comes from the error's fault class.
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
