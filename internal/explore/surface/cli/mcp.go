package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/pathexpand"

	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/internal/mcpflags"

	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/mcp"
)

// runMCP runs exploremesh as a local MCP (Model Context Protocol) stdio server. Its adapters come from its
// launch arguments (`--adapter`) and nothing else; every call composes its own panel from them.
//
// STDOUT PURITY is enforced here and nowhere else, because here is where it can be: the protocol stream
// is the process's real stdout, and the server spawns provider CLIs. Before serving, `os.Stdout` is
// REPOINTED at stderr and the captured original becomes the protocol stream. Any stray print — from this
// process, from a library, or from a child that inherited the descriptor — then lands on stderr instead
// of corrupting a JSON-RPC frame. A stdio protocol server gets exactly one chance at this.
func runMCP(args []string, stdout, errw io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh explore mcp")
	// The era posture, the wait/turn budgets and strict schema are declared ONCE for both MCP servers —
	// same flag, same values, same meaning as `aimesh review mcp`.
	sh := mcpflags.Register(fs, mcp.DefaultWaitSeconds)
	adapters := launchflags.RegisterAdapters(fs)
	build := RegisterMCPFlags(fs, sh, adapters)
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	srv, code := build(errw)
	if srv == nil {
		return code
	}
	// The protocol stream is the `stdout` writer this surface was handed (the process's real stdout,
	// from main). Repointing the os.Stdout PACKAGE VARIABLE at stderr does not disturb it — that
	// writer was captured before — but it does send every subsequent stray print, from this process or
	// from a library that reaches for os.Stdout, to stderr instead of into a JSON-RPC frame.
	restore := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = restore }()
	if err := srv.Serve(os.Stdin, stdout); err != nil {
		fmt.Fprintf(errw, "aimesh explore mcp: %v\n", err)
		return int(fault.Internal)
	}
	return int(fault.OK)
}

// RegisterMCPFlags declares exploremesh's OWN mcp flags on fs and returns the builder for the server. It
// is exported for `aimesh mcp`, which registers both domains' flags on one flag set and then builds only
// the domains it was asked to serve.
//
// sh supplies the flags both domains share and adapters the shared `--adapter` set; both must already be
// registered on the same fs.
func RegisterMCPFlags(fs *flag.FlagSet, sh *mcpflags.Shared, adapters *launchflags.Adapters) func(errw io.Writer) (*mcp.Server, int) {
	noCapture := fs.Bool("no-capture", false, "do NOT write a run directory for each run (capture is on by default: an MCP run that left no disk record would be the one surface whose governance claims are uncheckable afterwards). It also turns off MCP `resources/*` for those runs — there is nothing on disk to publish")
	return func(errw io.Writer) (*mcp.Server, int) {
		return buildMCPServer(adapters, noCapture, sh, errw)
	}
}

// buildMCPServer resolves the launch adapters and returns the server. A nil server means the int is the
// exit code to return.
func buildMCPServer(adapters *launchflags.Adapters, noCapture *bool, sh *mcpflags.Shared, errw io.Writer) (*mcp.Server, int) {
	era, ok := sh.Era(errw, "exploremesh mcp")
	if !ok {
		return nil, int(fault.Usage)
	}
	set, err := adapters.Resolve(pathexpand.OS())
	if err != nil {
		fmt.Fprintf(errw, "aimesh explore mcp: %v\n", err)
		return nil, codeOf(err)
	}
	if set.Empty() {
		fmt.Fprintln(errw, noAdapterNotice("mcp"))
	}
	reg, err := launchRegistry(set)
	if err != nil {
		fmt.Fprintf(errw, "aimesh explore mcp: %v\n", err)
		return nil, codeOf(err)
	}
	srv := &mcp.Server{
		Explorer:       mcpExplorer{acp.NewPipelineExplorer(reg)},
		Adapters:       set,
		Config:         launchView{set: set},
		WaitSeconds:    *sh.WaitSeconds,
		TurnTimeout:    *sh.TurnTimeout,
		DisableCapture: *noCapture,
		StrictSchema:   sh.Strict(),
		Framing:        *sh.Framing,
		Protocol:       era,
		Diagnostics:    errw,
	}
	sh.AnnounceStrict(errw, "exploremesh mcp")
	return srv, int(fault.OK)
}

// mcpExplorer adapts the shared pipeline explorer to the MCP surface's seam. Both surfaces route to the
// same pipeline; only the transport differs.
type mcpExplorer struct{ acp.Explorer }
