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

// runMCP runs reviewmesh as a local MCP stdio server, configured only from its launch arguments:
// adapters, the write grant, an optional --root ceiling and the bounded-execution grants. Each call
// supplies its panel, models and workspace.
//
// Before serving, os.Stdout is pointed at stderr and the original becomes the protocol stream, so a
// stray print from this process or a child cannot corrupt a JSON-RPC frame.
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

// RegisterMCPFlags declares reviewmesh's own mcp flags on fs and returns the server builder. `aimesh
// mcp` registers both domains' flags on one flag set and builds only the domains it serves; sh,
// adapters and writes are registered once by the owner of fs.
func RegisterMCPFlags(fs *flag.FlagSet, sh *mcpflags.Shared, adapters *launchflags.Adapters, writes *launchflags.Writes) func(errw io.Writer) (*mcp.Server, int) {
	var roots setFlags
	fs.Var(&roots, "root", "an absolute directory every call's declared workspace and roots must lie inside (repeatable). Without it, a call may declare any absolute directory that is not the filesystem root, a home directory, a system tree or a protected directory")
	allowBroadRoot := fs.Bool("allow-broad-root", false, "permit a --root that is normally refused as over-broad (/, a home directory, a system/shared tree)")
	// Operator opt-ins are launch flags, never tool parameters, because the caller is a model.
	resolveWritePolicy := registerWritePolicyFlags(fs, "mcp")
	return func(errw io.Writer) (*mcp.Server, int) {
		return buildMCPServer(sh, adapters, writes, roots, *allowBroadRoot, resolveWritePolicy, errw)
	}
}

// buildMCPServer resolves the launch configuration and returns the server. When the server is nil, the
// int is the exit code.
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
	// Also stated on stderr, so a host log shows the server cannot run anything yet.
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

// launchView implements mcp.Config: the named adapters and their availability when asked. It exposes
// no paths.
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
