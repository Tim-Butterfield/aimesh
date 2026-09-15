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

// runMCP runs explorations as a local MCP stdio server. Its adapters come only from `--adapter`, and every
// call composes its own panel from them.
//
// The protocol stream is the process's real stdout, and the server spawns provider CLIs, so os.Stdout is
// pointed at stderr before serving: a stray print then lands on stderr instead of corrupting a JSON-RPC
// frame.
func runMCP(args []string, stdout, errw io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh explore mcp")
	// The protocol posture, wait and turn budgets, and strict schema flags are shared with `aimesh review mcp`.
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
	// stdout was captured from main, so pointing os.Stdout at stderr leaves the protocol stream intact and
	// sends later stray prints to stderr.
	restore := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = restore }()
	if err := srv.Serve(os.Stdin, stdout); err != nil {
		fmt.Fprintf(errw, "aimesh explore mcp: %v\n", err)
		return int(fault.Internal)
	}
	return int(fault.OK)
}

// RegisterMCPFlags declares the explore domain's own mcp flags on fs and returns the server builder. It is
// exported for `aimesh mcp`, which registers both domains' flags on one flag set and builds the domains it
// serves.
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
