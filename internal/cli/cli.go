// Package cli is the aimesh command-line entry point. It owns the shared commands (init, doctor,
// agents-md, clean, mcp and version) and forwards `aimesh review …` and `aimesh explore …` to the
// domain CLIs, which keep their own flags, help and exit codes.
//
// Domain commands are grouped under a noun, and the review or exploration itself is the explicit
// `run` verb, so a path operand is never mistaken for a subcommand.
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
	// verbs maps each typed verb to the domain CLI's command name. Only run differs: it forwards
	// as the domain's own name (review or explore).
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
		// Asking for help is not a usage error: it prints to stdout and exits 0.
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

// runDomain rewrites `aimesh <noun> <verb> …` into the domain CLI's argv and forwards it. An unknown
// verb is refused here, in terms of the command tree the user typed.
func runDomain(d domain, args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(errw, "aimesh %s: a command is required\n\n%s", d.noun, usage)
		return int(fault.Usage)
	}
	switch args[0] {
	case "-h", "--help", "help":
		// Forward to the domain's own help, which documents its modes, flags and exit codes.
		return d.run([]string{"--help"}, out, errw)
	}
	inner, ok := d.verbs[args[0]]
	if !ok {
		fmt.Fprintf(errw, "aimesh %s: unknown command %q\n\n%s", d.noun, args[0], usage)
		return int(fault.Usage)
	}
	// A fresh slice, because appending onto args[1:] could write into the caller's backing array.
	forwarded := make([]string, 0, len(args))
	forwarded = append(forwarded, inner)
	forwarded = append(forwarded, args[1:]...)
	return d.run(forwarded, out, errw)
}

// runVersion prints the build version under the aimesh name, or the full build info with --json.
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
