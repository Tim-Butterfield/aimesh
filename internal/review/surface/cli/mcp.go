package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/mcpflags"
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/app"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/setup"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// runMCP runs reviewmesh as a local MCP (Model Context Protocol) stdio server.
//
// Two things differ from `exploremesh mcp`, and both come from what this server can do:
//
//   - TRUSTED ROOTS ARE MANDATORY. exploremesh reads no files; this server takes paths, so the
//     consent a CLI path argument carries has to be given at LAUNCH instead. The resolution is the
//     ACP surface's, unchanged (`--root`, else the validated launch cwd, else a refusal): one
//     model, so the two agent surfaces cannot diverge in what a peer may read.
//   - REMEDIATION IS A CONFIG CAPABILITY, not a flag that overrides config. `--allow-remediate`
//     GRANTS `allowRemediate` to the `mcp` surface in this process's config snapshot, and the
//     ceiling is then computed from that config (config.SurfaceCeiling). An operator who prefers it
//     permanent writes the same capability into their config file; the code path is identical. That
//     is what keeps `surfaces.defaultModeBySurface.mcp: report` an authoritative ceiling instead of
//     a default some flag can step around.
//
// STDOUT PURITY is enforced here and nowhere else, because here is where it can be: the protocol
// stream is the process's real stdout, and the server spawns provider CLIs. Before serving,
// `os.Stdout` is REPOINTED at stderr and the captured original becomes the protocol stream. Any
// stray print — from this process, a library, or a child that inherited the descriptor — then lands
// on stderr instead of corrupting a JSON-RPC frame.
func runMCP(args []string, stdout, errw io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh review mcp")
	sh := mcpflags.Register(fs, mcp.DefaultWaitSeconds)
	build := RegisterMCPFlags(fs, sh)
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
// flag set and then builds only the domains it was asked to serve. See exploremesh's namesake for
// why the seam is register-then-build.
//
// sh supplies the flags both domains share; it must already be registered on the same fs.
func RegisterMCPFlags(fs *flag.FlagSet, sh *mcpflags.Shared) func(errw io.Writer) (*mcp.Server, int) {
	// --framing is declared by internal/mcpflags: it is transport posture shared by both domains,
	// not a review setting. See that package for what classifying it here used to cost.
	framing := sh.Framing
	var roots setFlags
	fs.Var(&roots, "root", "trusted workspace root an MCP call may review (repeatable; default: the launch working directory)")
	noDefaultRoot := fs.Bool("no-default-root", false, "do not adopt the launch working directory as a trusted root (explicit --root only)")
	allowBroadRoot := fs.Bool("allow-broad-root", false, "permit an explicit --root that is normally refused as over-broad (/, a home directory, a system/shared tree)")
	// The INFERRED-CWD WAIVER. Under the 2026-07-28 revision there is no `roots/list`, so a client can
	// no longer narrow this server the way it could under 2025-06-18 — which means the same launch
	// would grant a modern client strictly MORE filesystem authority than a legacy one. The default is
	// therefore to refuse an inferred launch cwd as a trusted root on that era; this flag is the
	// operator's explicit opt-in, in the same family as --allow-broad-root and --allow-remediate.
	//
	// Its blast radius is bounded by the degenerate-root rule, which still refuses /, a home
	// directory, home's parent and the system trees EVEN WITH THIS SET. So it can only ever admit a
	// plausible project directory — the same consent the CLI already accepts from a human-typed path.
	//
	// It is disclosed in `doctor` and the readiness projection, not only here: a host launches its
	// servers from a config file and MAY discard stderr entirely (the stdio transport says so in as
	// many words), and a security-relevant setting visible only on a discarded channel is disclosed in
	// name only.
	allowInferredRoot := fs.Bool("allow-inferred-root", false, "accept the launch working directory as a trusted root even under MCP revisions that removed the client's ability to narrow this server (2026-07-28 and later). Default: an INFERRED root is refused on those revisions and every filesystem path is denied until `--root <project-dir>` is given. The over-broad denylist still applies, so this can only admit a plausible project directory. Its state is reported by the `doctor` tool")
	allowRemediate := fs.Bool("allow-remediate", false, "grant this server the `"+config.CapabilityAllowRemediate+"` capability: the review_remediate tool becomes available (and is then LISTED to the client). Without it the tool does not exist on this server. Each call must still pass allowWrite: true.")
	// The OPERATOR opt-ins, in the same family as --allow-remediate and --allow-broad-root: given at
	// launch, never as a tool parameter. This server's caller is a MODEL, and a model must not be
	// able to widen what containment admits or to name a command this process executes.
	resolveWritePolicy := registerWritePolicyFlags(fs, "mcp")
	return func(errw io.Writer) (*mcp.Server, int) {
		return buildMCPServer(mcpFlagValues{
			framing: framing, roots: &roots, noDefaultRoot: noDefaultRoot, allowBroadRoot: allowBroadRoot,
			allowInferredRoot: allowInferredRoot, allowRemediate: allowRemediate, resolveWritePolicy: resolveWritePolicy,
		}, sh, errw)
	}
}

// mcpFlagValues carries the domain-specific flag pointers from registration to build. It is a struct
// rather than eight closure captures so the builder's signature says what it depends on.
type mcpFlagValues struct {
	framing           *string
	roots             *setFlags
	noDefaultRoot     *bool
	allowBroadRoot    *bool
	allowInferredRoot *bool
	allowRemediate    *bool
	// resolveVerify returns the operator's bounded-execution settings, or ok=false having already
	// explained the refusal. It is a closure rather than three pointers because the validation
	// (the command ceiling, the inert-knob check) belongs with the flags it validates.
	resolveWritePolicy func(io.Writer) (writePolicy, bool)
}

// buildMCPServer resolves the trusted roots, the config snapshot and the capability grant, runs the
// optional deep probe, and returns the configured server. A nil server means the int is the exit
// code to return.
func buildMCPServer(f mcpFlagValues, sh *mcpflags.Shared, errw io.Writer) (*mcp.Server, int) {
	framing, roots := f.framing, *f.roots
	noDefaultRoot, allowBroadRoot, allowInferredRoot := f.noDefaultRoot, f.allowBroadRoot, f.allowInferredRoot
	allowRemediate := f.allowRemediate

	era, ok := sh.Era(errw, "reviewmesh mcp")
	if !ok {
		return nil, int(fault.Usage)
	}
	policy, pok := f.resolveWritePolicy(errw)
	if !pok {
		return nil, int(fault.Usage)
	}
	probeDeep, waitSeconds, turnTimeout := sh.ProbeDeep, sh.WaitSeconds, sh.TurnTimeout
	strict := sh.Strict()
	// TRUSTED ROOTS, resolved before anything else: a server that cannot establish a root would
	// refuse every request path, and the operator should learn that at launch rather than on the
	// first call.
	trustedRoots, rootSource, rerr := acp.ResolveTrustedRoots(acp.RootOptions{
		Surface: "mcp", Explicit: roots, NoDefault: *noDefaultRoot, AllowBroadRoot: *allowBroadRoot,
		AllowInferredRoot: *allowInferredRoot,
	})
	if rerr != nil {
		fmt.Fprintln(errw, "aimesh review mcp: "+rerr.Error())
		return nil, int(fault.CodeOf(rerr))
	}
	// The stderr line is a SUPPLEMENT to the disclosure, never the disclosure — see the flag's own
	// comment and the readiness entry the server adds.
	if rootSource == acp.RootsInferredCwd && *allowInferredRoot {
		fmt.Fprintf(errw, "aimesh review mcp: --allow-inferred-root: the launch working directory is accepted as a trusted root even though it carries no project marker. This is reported by the `doctor` tool.\n")
	}
	// ZERO ROOTS IS A LIVE STATE, NOT A FAILURE — but it is one the operator should hear about at
	// launch rather than discover through a refused call, so say it here as well as in `doctor`.
	if rootSource == acp.RootsNone {
		fmt.Fprintf(errw, "aimesh review mcp: no trusted root. The launch working directory carries no project marker (%s), so it was not adopted — nothing suggests it is the project rather than wherever this process was started. Filesystem paths will be refused; `inlineWorkspace` still works. Fix with `--root <project-dir>`, or `--allow-inferred-root` to adopt the cwd anyway. Reported by the `doctor` tool.\n", strings.Join(acp.ProjectMarkers(), ", "))
	}
	a, err := app.New(app.Options{})
	if err != nil {
		fmt.Fprintln(errw, "aimesh review mcp:", err)
		return nil, int(fault.CodeOf(err))
	}
	// The config snapshot this server runs on. The capability grant is a CONFIG edit (in memory,
	// for this process), so the Manager's own resolution sees exactly the policy the tool list was
	// built from — there is no second place where "may this write?" is decided.
	cfg := a.Cfg
	if *allowRemediate {
		cfg = config.WithSurfaceCapability(cfg, "mcp", config.CapabilityAllowRemediate)
	}
	// The write-authority POLICY CEILING for the MCP surface. An absent or unrecognized value fails
	// CLOSED at `report`: this surface never widens itself on a malformed policy.
	policyCeiling := review.ModeReport
	switch m := review.Mode(cfg.Surfaces.DefaultModeBySurface["mcp"]); m {
	case review.ModeReport, review.ModePatch, review.ModeApply:
		policyCeiling = m
	}
	granted := cfg.Surfaces.HasCapability("mcp", config.CapabilityAllowRemediate)
	if granted {
		// A write-capable server says so at launch, on stderr, every time. An operator who did not
		// mean to enable it should not have to read a tool list to find out.
		fmt.Fprintf(errw, "aimesh review mcp: REMEDIATION ENABLED (capability %s on the `mcp` surface): the review_remediate tool is available and can write to %d trusted root(s). Each call must also pass allowWrite: true.\n",
			config.CapabilityAllowRemediate, len(trustedRoots))
	}

	mgr := setup.Manager{Cfg: cfg, Layers: a.Layers, Adapters: a.Adapters, ArtifactDir: a.ArtifactDir, Out: io.Discard}
	// The deep probe, if the operator asked for it: ONE pass, at launch, before a single client
	// request has arrived. Its rows are then carried by every `doctor` tool result for the life of the
	// process — which is honest, because the answer ("does this CLI work in a throwaway directory?")
	// is a property of the CLI's trust/auth posture and does not change per request.
	var deepChecks []mcp.ReadinessCheck
	if *probeDeep {
		fmt.Fprintln(errw, "aimesh review mcp: running the DEEP readiness probe once at launch (this SPENDS real tokens; no interactive prompt will be answered for you)…")
		dctx, dcancel := context.WithTimeout(context.Background(), doctorDeepProbeTimeout)
		rep := a.DoctorWith("", "", app.DoctorOptions{ProbeDeep: true, Ctx: dctx})
		dcancel()
		for _, ch := range rep.Checks {
			if strings.HasPrefix(ch.Name, "probe-deep: ") {
				deepChecks = append(deepChecks, mcp.ReadinessCheck{Name: ch.Name, OK: ch.OK, Detail: ch.Detail})
				fmt.Fprintf(errw, "aimesh review mcp: %s — %v — %s\n", ch.Name, ch.OK, ch.Detail)
			}
		}
	}
	srv := &mcp.Server{
		Manager:             a.ManagerWithConfig(cfg),
		Config:              &mcpConfig{app: a, mgr: &mgr, deep: deepChecks},
		Adapters:            configuredAdapterNames(&mgr),
		Roots:               trustedRoots,
		RootSource:          rootSource,
		AllowInferredRoot:   *allowInferredRoot,
		PolicyCeiling:       policyCeiling,
		AllowRemediate:      granted,
		VerifyCommands:      policy.VerifyCommands,
		VerifyTimeout:       policy.VerifyTimeout,
		VerifyBaseline:      policy.VerifyBaseline,
		AllowProtectedPaths: policy.AllowProtectedPaths,
		WaitSeconds:         *waitSeconds,
		TurnTimeout:         *turnTimeout,
		StrictSchema:        strict,
		Framing:             *framing,
		Protocol:            era,
		Diagnostics:         errw,
	}
	sh.AnnounceStrict(errw, "reviewmesh mcp")
	return srv, int(fault.OK)
}

// configuredAdapterNames is the STARTUP-BOUND adapter set an ad-hoc panel may compose from: every
// adapter the OPERATOR configured, resolved once, here. It is what makes compose-not-configure a
// real boundary instead of an accident of which profiles happen to exist.
func configuredAdapterNames(mgr *setup.Manager) []string {
	var names []string
	for _, av := range mgr.AdapterViews() {
		if av.Implemented {
			names = append(names, av.Name)
		}
	}
	if fake.Enabled() {
		names = append(names, "fake")
	}
	return names
}

// mcpConfig is the production implementation of mcp.Config: the SANITIZED configuration projection
// the `list` and `doctor` tools report. It hands the MCP surface only logical facts — the
// projection types carry no path field at all — so the operator's environment cannot reach a
// third-party inference log through a tool result. reviewmesh's own `list --json` DOES report
// adapter paths; that is a human on the machine asking, which is a different question.
type mcpConfig struct {
	app *app.App
	mgr *setup.Manager
	// deep holds the LAUNCH-TIME deep-probe rows (empty unless the operator passed --probe-deep).
	// They are reported, never recomputed: recomputing would let a peer spend by polling a tool that
	// promises to spend nothing.
	deep []mcp.ReadinessCheck
}

func (c *mcpConfig) Adapters() []mcp.AdapterFact {
	var out []mcp.AdapterFact
	for _, av := range c.mgr.AdapterViews() {
		kind := "shell"
		if av.IsACP {
			kind = "acp"
		}
		out = append(out, mcp.AdapterFact{
			Name: av.Name, DisplayName: av.DisplayName, Kind: kind,
			Configured: av.Configured, IdentityEvidenceCapability: av.ModelIdentity, SpecOnly: av.SpecOnly,
		})
	}
	// The hidden internal `fake` harness is absent from AdapterViews and stays absent here unless
	// the internal gate is on (tests / golden runs, never a user): `list` must never advertise a
	// name a user cannot configure.
	if fake.Enabled() {
		out = append(out, mcp.AdapterFact{
			Name: "fake", DisplayName: setup.AdapterDisplayName("fake"), Kind: "fake", Configured: true,
			IdentityEvidenceCapability: string(review.EvidenceInvocationTag),
		})
	}
	return out
}

func (c *mcpConfig) Profiles() []mcp.ProfileFact {
	var out []mcp.ProfileFact
	for _, pv := range c.mgr.ProfileViews() {
		p := mcp.ProfileFact{Name: pv.Name, IsDefault: pv.IsDefault}
		for _, s := range pv.Reviewers {
			p.Reviewers = append(p.Reviewers, mcp.SeatFact{Adapter: s.Adapter, Model: s.Model})
		}
		for _, l := range pv.Lanes {
			p.Lanes = append(p.Lanes, mcp.SeatFact{Role: l.Role, Adapter: l.Adapter, Model: l.Model})
		}
		out = append(out, p)
	}
	return out
}

// Catalog projects the model catalog: the keys a composed `panel[].model` may name, and what each
// resolves to. Same projection the CLI's `list` renders, re-mapped through this surface's own types
// so no path or launch detail can arrive here by inheriting a field.
func (c *mcpConfig) Catalog() []mcp.CatalogFact {
	var out []mcp.CatalogFact
	for _, cv := range c.mgr.CatalogViews() {
		f := mcp.CatalogFact{
			Key: cv.Key, Provider: cv.Provider, CanonicalModel: cv.CanonicalModel,
			AdapterDefault: cv.AdapterDefault,
		}
		for _, b := range cv.Adapters {
			f.Adapters = append(f.Adapters, mcp.CatalogBind{Adapter: b.Adapter, ModelArg: b.ModelArg, Effort: b.Effort})
		}
		out = append(out, f)
	}
	return out
}

func (c *mcpConfig) DefaultProfile() string {
	for _, pv := range c.mgr.ProfileViews() {
		if pv.IsDefault {
			return pv.Name
		}
	}
	return ""
}

// Readiness runs the STATIC readiness checks — the same ones `reviewmesh doctor` reports without
// --probe. No process is started and nothing is spent, which is what lets the `doctor` tool
// honestly carry readOnlyHint. The live probe stays CLI-only on purpose: it starts provider CLIs.
func (c *mcpConfig) Readiness() (bool, []mcp.ReadinessCheck) {
	rep := c.app.Doctor("", "", false)
	out := make([]mcp.ReadinessCheck, 0, len(rep.Checks)+len(c.deep))
	for _, ch := range rep.Checks {
		out = append(out, mcp.ReadinessCheck{Name: ch.Name, OK: ch.OK, Detail: ch.Detail})
	}
	// The launch-time deep-probe rows, if the operator opted in. They are the only readiness facts
	// here that a REAL invocation produced — `--version` cannot tell a client whether the CLI will
	// work in the isolated directory a run actually uses. Appended verbatim; nothing is re-run, so the
	// tool still starts no process and spends nothing.
	ok := rep.OK
	for _, ch := range c.deep {
		out = append(out, ch)
		if !ch.OK {
			ok = false
		}
	}
	return ok, out
}

// compile-time proof that the projection satisfies the surface's contract.
var _ mcp.Config = (*mcpConfig)(nil)
