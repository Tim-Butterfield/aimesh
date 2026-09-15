package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/internal/mcpflags"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/app"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/setup"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/pathexpand"
)

// runMCP runs reviewmesh as a local MCP (Model Context Protocol) stdio server.
//
// The server is configured from its launch arguments alone — never from saved aimesh configuration:
// the adapters it may use (`--adapter`), whether aimesh may apply changes (`--allow-writes`), an
// optional `--root` ceiling, and the bounded-execution grants. Every run's panel, models and workspace
// come from the call.
//
// STDOUT PURITY is enforced here and nowhere else, because here is where it can be: the protocol
// stream is the process's real stdout, and the server spawns provider CLIs. Before serving,
// `os.Stdout` is REPOINTED at stderr and the captured original becomes the protocol stream. Any stray
// print — from this process, a library, or a child that inherited the descriptor — then lands on
// stderr instead of corrupting a JSON-RPC frame.
func runMCP(args []string, stdout, errw io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh review mcp")
	sh := mcpflags.Register(fs, mcp.DefaultWaitSeconds)
	adapters := launchflags.RegisterAdapters(fs)
	writes := launchflags.RegisterWrites(fs)
	build := RegisterMCPFlags(fs, sh, adapters, writes)
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	srv, code := build(errw)
	if srv == nil {
		return code
	}
	restore := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = restore }()
	if err := srv.Serve(os.Stdin, stdout); err != nil {
		fmt.Fprintln(errw, "aimesh review mcp:", err)
		return int(fault.Internal)
	}
	return int(fault.OK)
}

// RegisterMCPFlags declares reviewmesh's OWN mcp flags on fs and returns the builder for the
// configured server. It is exported for `aimesh mcp`, which registers both domains' flags on one
// flag set and then builds only the domains it was asked to serve.
//
// sh, adapters and writes are registered once by the command that owns fs and are passed in, so a
// command serving both domains never registers a name twice.
func RegisterMCPFlags(fs *flag.FlagSet, sh *mcpflags.Shared, adapters *launchflags.Adapters, writes *launchflags.Writes) func(errw io.Writer) (*mcp.Server, int) {
	var roots setFlags
	fs.Var(&roots, "root", "an absolute directory every call's declared workspace and roots must lie inside (repeatable). Without it, a call may declare any absolute directory that is not the filesystem root, a home directory, a system tree or a protected directory")
	allowBroadRoot := fs.Bool("allow-broad-root", false, "permit a --root that is normally refused as over-broad (/, a home directory, a system/shared tree)")
	// The OPERATOR opt-ins: given at launch, never as a tool parameter. This server's caller is a MODEL,
	// and a model must not be able to widen what containment admits or to name a command this process
	// executes.
	resolveWritePolicy := registerWritePolicyFlags(fs, "mcp")
	return func(errw io.Writer) (*mcp.Server, int) {
		return buildMCPServer(sh, adapters, writes, roots, *allowBroadRoot, resolveWritePolicy, errw)
	}
}

// buildMCPServer resolves the launch configuration and returns the configured server. A nil server
// means the int is the exit code to return.
func buildMCPServer(sh *mcpflags.Shared, adapters *launchflags.Adapters, writes *launchflags.Writes, roots []string,
	allowBroadRoot bool, resolveWritePolicy func(io.Writer) (writePolicy, bool), errw io.Writer) (*mcp.Server, int) {

	era, ok := sh.Era(errw, "reviewmesh mcp")
	if !ok {
		return nil, int(fault.Usage)
	}
	policy, pok := resolveWritePolicy(errw)
	if !pok {
		return nil, int(fault.Usage)
	}
	set, err := adapters.Resolve(pathexpand.OS())
	if err != nil {
		fmt.Fprintln(errw, "aimesh review mcp:", err)
		return nil, int(fault.CodeOf(err))
	}
	expanded, err := acp.ExpandRoots(roots, pathexpand.OS())
	if err != nil {
		fmt.Fprintln(errw, "aimesh review mcp: "+err.Error())
		return nil, int(fault.CodeOf(err))
	}
	ceiling, err := acp.ResolveCeiling(expanded, allowBroadRoot, "mcp", "")
	if err != nil {
		fmt.Fprintln(errw, "aimesh review mcp:", err)
		return nil, int(fault.CodeOf(err))
	}
	a, err := app.New(app.Options{Launch: &app.LaunchConfig{Adapters: set}})
	if err != nil {
		fmt.Fprintln(errw, "aimesh review mcp:", err)
		return nil, int(fault.CodeOf(err))
	}
	// Stated on stderr at launch as well as in `review_doctor`, because an operator reading a host log
	// should not have to call a tool to learn the server cannot run anything yet.
	if set.Empty() {
		fmt.Fprintf(errw, "aimesh review mcp: no adapter named — every review is refused until the host configuration adds --adapter <name> (or %s).\n", launchflags.EnvVar)
	}
	if writes.Allowed() {
		fmt.Fprintln(errw, "aimesh review mcp: --allow-writes: review_remediate can apply accepted findings to a workspace; each apply is still confirmed per call.")
	}
	srv := &mcp.Server{
		Manager:             a.Manager(),
		Config:              launchView{set: set},
		Adapters:            set,
		Ceiling:             ceiling,
		AllowWrites:         writes.Allowed(),
		VerifyCommands:      policy.VerifyCommands,
		VerifyTimeout:       policy.VerifyTimeout,
		VerifyBaseline:      policy.VerifyBaseline,
		AllowProtectedPaths: policy.AllowProtectedPaths,
		WaitSeconds:         *sh.WaitSeconds,
		TurnTimeout:         *sh.TurnTimeout,
		StrictSchema:        sh.Strict(),
		Framing:             *sh.Framing,
		Protocol:            era,
		Diagnostics:         errw,
	}
	sh.AnnounceStrict(errw, "reviewmesh mcp")
	return srv, int(fault.OK)
}

// launchView is the production implementation of mcp.Config: the named adapters and their availability
// at the moment of the call. It hands the MCP surface only logical facts; the projection types carry no
// path field at all.
type launchView struct {
	set launchflags.Set
}

func (v launchView) Adapters() []mcp.AdapterFact {
	defaults := config.Default().Adapters
	out := make([]mcp.AdapterFact, 0, len(v.set.Names()))
	for _, ad := range v.set.Adapters() {
		ok, why := v.set.Available(ad.Name)
		kind := "shell"
		if ad.Name == launchflags.FakeAdapter {
			kind = "fake"
		}
		fact := mcp.AdapterFact{
			Name: ad.Name, DisplayName: setup.AdapterDisplayName(ad.Name), Kind: kind,
			Available: ok, Source: string(ad.Source),
			IdentityEvidenceCapability: defaults[ad.Name].ModelIdentity,
		}
		if !ok {
			fact.Reason = why
		}
		out = append(out, fact)
	}
	return out
}

// Readiness reports each named adapter's availability. It starts no process and spends nothing. The
// server is ready when at least one named adapter can be started.
func (v launchView) Readiness() (bool, []mcp.ReadinessCheck) {
	if v.set.Empty() {
		return false, []mcp.ReadinessCheck{{
			Name: "adapters", OK: false,
			Detail: "no adapter was named at launch; add --adapter <name> (or " + launchflags.EnvVar + ") to the host configuration",
		}}
	}
	anyOK := false
	checks := make([]mcp.ReadinessCheck, 0, len(v.set.Names()))
	for _, a := range v.set.Adapters() {
		ok, why := v.set.Available(a.Name)
		anyOK = anyOK || ok
		detail := strings.TrimSpace(fmt.Sprintf("%s (named by %s)", why, a.Source))
		checks = append(checks, mcp.ReadinessCheck{Name: "adapter: " + a.Name, OK: ok, Detail: detail})
	}
	return anyOK, checks
}

// compile-time proof that the projection satisfies the surface's contract.
var _ mcp.Config = launchView{}
