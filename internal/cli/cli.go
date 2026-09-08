// Package cli is aimesh's single command-line entry point. It owns the SHARED commands (state
// initialization, readiness, the agent guide, version) and dispatches everything else to the two
// domain CLIs under their own noun: `aimesh review …` and `aimesh explore …`.
//
// WHY A DOMAIN NOUN RATHER THAN A FLAT MERGE. The two domain CLIs shared most of their command
// NAMES — doctor, list, setup, acp, mcp, init, repo, folder — while `review`/`config` and
// `explore`/`export` are unique to one. A flat merge would have to rename most of them. Grouping
// under the domain is the ordinary shape for a multi-domain tool (docker container ls, kubectl get
// pods, terraform state list) and it gives `aimesh review --help` a real scoped listing.
//
// WHY `run` IS SPELLED OUT. `aimesh review <path>` would put a free-form operand in the same slot
// as the verb names, so a directory called `setup`, `list`, `doctor`, `mcp` or `acp` would be
// silently taken as a subcommand — `aimesh review setup` would configure instead of reviewing
// ./setup, and reviewing it would require knowing to type `./setup`. git carried exactly that
// ambiguity in `git checkout <branch-or-path>` for fifteen years before splitting it into `switch`
// and `restore`; npm requires `npm run <script>` for the same reason. One extra word on the path
// that MCP, ACP and CI all invoke programmatically is a cheap way not to inherit that.
//
// This package is deliberately a THIN SHIM: it re-spells argv and calls the domain CLI's existing
// Run. The domains keep owning their own flags, help and exit codes, so the unification cannot
// quietly change what a command does.
package cli

import (
	"fmt"
	"io"
	"os"

	explorecli "github.com/Tim-Butterfield/aimesh/internal/explore/surface/cli"
	reviewcli "github.com/Tim-Butterfield/aimesh/internal/review/surface/cli"
	"github.com/Tim-Butterfield/aimesh/internal/review/version"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

const usage = `aimesh — provider-diverse, governed AI review and exploration.

usage:
  aimesh <command> [flags]
  aimesh review  <command> [flags]
  aimesh explore <command> [flags]

shared commands:
  init         prepare this directory for aimesh: create the VCS-excluded .aimesh/
               state directory. Adaptive — repo mode inside a repository, folder
               mode outside one.
               flags: --require-repo, --require-folder (turn the adaptive choice
               into an assertion, for scripts and CI), --json
  doctor       report whether this directory is ready for aimesh. Read-only: it
               creates nothing. Reports rather than blocking, so a probe that
               finds nothing still tells you what to do next.
               flags: --require-root (block when there is no root), --json
  agents-md    print the agent guide compiled into this binary
  clean        remove prior run artifacts, from BOTH the project .aimesh/ and the
               OS temp fallback an uninitialized tree writes to. With no selector
               it reports an inventory and removes nothing — a run directory holds
               that run's findings, decisions, patch and (for an apply) its undo,
               so the retention policy is yours to state.
               flags: --keep <n>, --older-than <7d|168h>, --all, --dry-run, --json
  mcp          run ONE MCP server carrying BOTH domains' tools (stdio) — the
               single entry a host config needs. Every flag is launch-time and
               set once; a caller varies the workspace, panel and waitSeconds
               per call, so one server serves many repos without a restart.
               flags: --only review|explore (serve one domain's tools), plus
               every flag of the domain(s) served — naming one whose domain
               --only excluded is refused, never ignored
  --version    print version ( --version --json for machine-readable )
  help         print this help on stdout and exit 0

review commands (aimesh review <command>):
  run          review a workspace — the review itself
  setup        write/update review configuration
  list         list configured adapters + profiles with their lanes
  doctor       adapter readiness for review (probing, guided repair)
  config       inspect/resolve the effective review configuration
  acp          run review as an ACP agent server (JSON-RPC 2.0 over stdio)
  mcp          run review as an MCP server (Model Context Protocol over stdio)

explore commands (aimesh explore <command>):
  run          run an exploration — the exploration itself
  setup        write/update exploration profiles
  list         list configured adapters + profiles
  doctor       adapter readiness for exploration
  export       export a captured run to SQLite
  acp          run exploration as an ACP agent server
  mcp          run exploration as an MCP server

State lives in ONE place: .aimesh/ — adapters.yaml shared by both domains, plus
review/ and explore/ for each domain's own config and run artifacts. AIMESH_HOME
overrides the user-scope base directory.

A command is required; running aimesh with no command prints this help and exits
non-zero. Per-domain flags and exit codes are documented by each domain's own
help: aimesh review --help, aimesh explore --help.
`

// domain describes one domain's slice of the command tree: the noun it is reached by, the verbs it
// accepts, and the Run it forwards to.
type domain struct {
	noun string
	// verbs maps the verb as typed to the command name the DOMAIN CLI already knows. Only `run`
	// differs — it is the domain's own name there (`review`, `explore`), which is exactly the
	// spelling this tree replaces.
	verbs map[string]string
	run   func(args []string, out, errw io.Writer) int
}

var reviewDomain = domain{
	noun: "review",
	verbs: map[string]string{
		"run":    "review",
		"setup":  "setup",
		"list":   "list",
		"doctor": "doctor",
		"config": "config",
		"acp":    "acp",
		"mcp":    "mcp",
	},
	run: reviewcli.Run,
}

var exploreDomain = domain{
	noun: "explore",
	verbs: map[string]string{
		"run":    "explore",
		"setup":  "setup",
		"list":   "list",
		"doctor": "doctor",
		"export": "export",
		"acp":    "acp",
		"mcp":    "mcp",
	},
	run: explorecli.Run,
}

// Run is the CLI entry point. It returns the process exit code.
func Run(args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, usage)
		return int(fault.Usage)
	}
	switch args[0] {
	case "-h", "--help", "help":
		// Asking for help is not a usage error: stdout, exit 0, so `aimesh --help | less` and any
		// CI script gating on the exit code behave.
		fmt.Fprint(out, usage)
		return int(fault.OK)
	case "--version", "version":
		return runVersion(args[1:], out)
	case "init":
		return runInit(args[1:], out, errw)
	case "doctor":
		return runDoctor(args[1:], out, errw)
	case "agents-md":
		return runAgentsMD(args[1:], out, errw)
	case "clean":
		return runClean(args[1:], out, errw)
	case "mcp":
		return runMCP(args[1:], out, errw)
	case reviewDomain.noun:
		return runDomain(reviewDomain, args[1:], out, errw)
	case exploreDomain.noun:
		return runDomain(exploreDomain, args[1:], out, errw)
	default:
		fmt.Fprintf(errw, "aimesh: unknown command %q\n\n%s", args[0], usage)
		return int(fault.Usage)
	}
}

// runDomain re-spells `aimesh <noun> <verb> …` as the argv the domain CLI already understands and
// forwards. An unknown verb is refused HERE, naming the domain, rather than passed down to produce
// an error that talks about a command tree the user did not type.
func runDomain(d domain, args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(errw, "aimesh %s: a command is required\n\n%s", d.noun, usage)
		return int(fault.Usage)
	}
	switch args[0] {
	case "-h", "--help", "help":
		// FORWARD, do not reprint the tree. `aimesh explore --help` asks for EXPLORE's help; answering
		// with the top level gives the user the page they just came from and hides the modes, the
		// mode-specific flags and the exit codes — which live in the domain's own curated usage and
		// exist nowhere else. The root must not restate them either, or the two drift.
		return d.run([]string{"--help"}, out, errw)
	}
	inner, ok := d.verbs[args[0]]
	if !ok {
		fmt.Fprintf(errw, "aimesh %s: unknown command %q\n\n%s", d.noun, args[0], usage)
		return int(fault.Usage)
	}
	// A FRESH slice: appending onto args[1:] could write into the caller's backing array.
	forwarded := make([]string, 0, len(args))
	forwarded = append(forwarded, inner)
	forwarded = append(forwarded, args[1:]...)
	return d.run(forwarded, out, errw)
}

// runVersion reports the build the binary was cut from. Both domains carry their own version
// package; they read the same module-level build info, so either answers for the whole tool.
//
// The NAME is printed here rather than taken from the domain package's String(): this binary is
// `aimesh`, and a version line naming one of its two domains misidentifies the thing the user just
// installed and ran. The domain packages keep their own String() for their own surfaces.
func runVersion(args []string, out io.Writer) int {
	info := version.Get()
	for _, a := range args {
		if a == "--json" {
			s, err := info.JSON()
			if err != nil {
				return int(fault.Internal)
			}
			fmt.Fprintln(out, s)
			return int(fault.OK)
		}
	}
	fmt.Fprintln(out, "aimesh "+info.Version)
	return int(fault.OK)
}

// Main is the process entry point used by cmd/aimesh.
func Main() int { return Run(os.Args[1:], os.Stdout, os.Stderr) }
