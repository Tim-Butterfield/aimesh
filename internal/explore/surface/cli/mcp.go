package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	mdoctor "github.com/Tim-Butterfield/aimesh/meshcore/doctor"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"
	corefake "github.com/Tim-Butterfield/aimesh/meshcore/model/fake"

	"github.com/Tim-Butterfield/aimesh/internal/mcpflags"

	"github.com/Tim-Butterfield/aimesh/internal/explore/manager"
	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/mcp"
)

// runMCP runs exploremesh as a local MCP (Model Context Protocol) stdio server. It binds the SAME config
// a no-flag `explore` binds to — the default panel, the selectable profile set, and the configured
// adapter set an ad-hoc panel may compose from — and then serves tools until EOF.
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
	// The era posture, the wait/turn budgets, strict schema and the deep probe are declared ONCE for
	// both MCP servers — same flag, same values, same meaning as `aimesh review mcp`. Surface parity
	// is not decoration here: an operator rolling one server back to the legacy era should not have
	// to discover that the other spells it differently, and these texts state posture an operator is
	// agreeing to.
	sh := mcpflags.Register(fs, mcp.DefaultWaitSeconds)
	build := RegisterMCPFlags(fs, sh)
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

// RegisterMCPFlags declares exploremesh's OWN mcp flags on fs and returns the builder for the
// configured server. It is exported for `aimesh mcp`, which registers both domains' flags on one
// flag set and then builds only the domains it was asked to serve.
//
// The split is register-then-build rather than parse-argv-and-serve because the composed command
// owns the flag set (it has to, or the shared flags would be declared twice and panic) and owns the
// serving (there is one protocol stream, not two). What is left for a domain to own is exactly this:
// which flags are mine, and how do I turn them into my server.
//
// sh supplies the flags both domains share; it must already be registered on the same fs.
func RegisterMCPFlags(fs *flag.FlagSet, sh *mcpflags.Shared) func(errw io.Writer) (*mcp.Server, int) {
	rosterPath := fs.String("roster", "", "optional roster file (YAML/JSON); default is the discovered profile set (profiles.yaml, a migrated legacy roster.yaml, else the unconfigured default)")
	noCapture := fs.Bool("no-capture", false, "do NOT write a run directory for each run (capture is on by default: an MCP run that left no disk record would be the one surface whose governance claims are uncheckable afterwards). It also turns off MCP `resources/*` for those runs — there is nothing on disk to publish")
	return func(errw io.Writer) (*mcp.Server, int) {
		return buildMCPServer(rosterPath, noCapture, sh, errw)
	}
}

// buildMCPServer resolves the panel, the profile set and the adapters, runs the optional deep probe,
// and returns the configured server. A nil server means the int is the exit code to return.
func buildMCPServer(rosterPath *string, noCapture *bool, sh *mcpflags.Shared, errw io.Writer) (*mcp.Server, int) {
	era, ok := sh.Era(errw, "exploremesh mcp")
	if !ok {
		return nil, int(fault.Usage)
	}
	waitSeconds, turnTimeout, probeDeep := sh.WaitSeconds, sh.TurnTimeout, sh.ProbeDeep
	// The DEFAULT panel — what a call naming no panel runs.
	plan, err := loadPlan(*rosterPath)
	if err != nil {
		fmt.Fprintf(errw, "aimesh explore mcp: %v\n", err)
		return nil, codeOf(err)
	}
	var set profile.Set
	if *rosterPath == "" {
		cwd, cerr := os.Getwd()
		if cerr != nil {
			fmt.Fprintf(errw, "aimesh explore mcp: %v\n", cerr)
			return nil, int(fault.Internal)
		}
		if set, err = profile.Resolve(cwd); err != nil {
			fmt.Fprintf(errw, "aimesh explore mcp: %v\n", err)
			return nil, codeOf(err)
		}
	}
	paths, acpInsts, err := resolveAdapters()
	if err != nil {
		fmt.Fprintf(errw, "aimesh explore mcp: %v\n", err)
		return nil, codeOf(err)
	}
	reg, unknown := registry.Build(unionPlan(plan, set), paths, acpInsts, 0)
	if len(unknown) > 0 {
		// Fail closed BEFORE serving: an unrecognized adapter is a config error, not a silent substitute.
		fmt.Fprintf(errw, "aimesh explore mcp: unknown adapter(s) in the roster: %s — configure them (setup) or fix the roster\n", joinNames(unknown))
		return nil, int(fault.Config)
	}
	// The STARTUP-BOUND adapter set an ad-hoc panel may compose from: every adapter the OPERATOR
	// configured, resolved once, here. Widening the registry to cover it starts no process and spends
	// nothing — it is name resolution — and it is what makes compose-not-configure a real boundary
	// instead of an accident of which profiles happen to exist.
	adapterNames := configuredAdapterNames(acpInsts)
	wide, _ := registry.Build(namesPlan(adapterNames), paths, acpInsts, 0)
	for name, a := range wide {
		if _, have := reg[name]; !have {
			reg[name] = a
		}
	}

	// The deep probe, if the operator asked for it: ONE pass, at launch, before a single client request
	// has arrived. Its rows are then carried by every `doctor` tool result for the life of the process —
	// which is honest, because the answer ("does this CLI do real work in a throwaway directory?") is a
	// property of the CLI's own trust/auth posture and does not change per request.
	var deepChecks []mcp.ReadinessCheck
	if *probeDeep {
		fmt.Fprintln(errw, "aimesh explore mcp: running the DEEP readiness probe once at launch (this SPENDS real tokens; no interactive prompt will be answered for you)…")
		dctx, dcancel := context.WithTimeout(context.Background(), doctorDeepProbeTimeout)
		for _, ch := range mdoctor.ProbeAdaptersDeep(dctx, reg, planDeepSeats(plan)) {
			deepChecks = append(deepChecks, mcp.ReadinessCheck{Name: ch.Name, OK: ch.OK, Detail: ch.Detail})
			fmt.Fprintf(errw, "aimesh explore mcp: %s — %v — %s\n", ch.Name, ch.OK, ch.Detail)
		}
		dcancel()
	}

	srv := &mcp.Server{
		Explorer:       mcpExplorer{acp.NewPipelineExplorer(reg)},
		Plan:           plan,
		Profiles:       set,
		Adapters:       adapterNames,
		Config:         &cliConfig{plan: plan, set: set, adapters: reg, acpInsts: acpInsts, rosterPath: *rosterPath, deep: deepChecks},
		WaitSeconds:    *waitSeconds,
		TurnTimeout:    *turnTimeout,
		DisableCapture: *noCapture,
		StrictSchema:   sh.Strict(),
		// The server has always HONOURED a framing setting; until --framing moved to the shared
		// flags there was no way for an explore-only deployment to state one.
		Framing:     *sh.Framing,
		Protocol:    era,
		Diagnostics: errw,
	}
	sh.AnnounceStrict(errw, "exploremesh mcp")
	return srv, int(fault.OK)
}

// mcpExplorer adapts the shared pipeline explorer to the MCP surface's seam. Both surfaces route to the
// same pipeline; only the transport differs.
type mcpExplorer struct{ acp.Explorer }

// cliConfig is the production implementation of mcp.Config: the SANITIZED configuration projection the
// `list` and `doctor` tools report. It deliberately hands the MCP surface only logical facts — the
// projection types carry no path field at all — so the operator's environment cannot reach a third-party
// inference log through a tool result.
type cliConfig struct {
	plan       roster.Plan
	set        profile.Set
	adapters   map[string]model.Adapter
	acpInsts   map[string]acpagent.Instance
	rosterPath string
	// defaultMode is the mode a no-mode run would take. It is deliberately EMPTY on this surface: the MCP
	// `explore` tool makes `mode` required (a per-profile default would make the same call mean different
	// things on different installs), so there is no server-level default a readiness check could assume.
	// The canonicalizer check therefore reports a non-derivable pair as a caveat here rather than a failure.
	defaultMode string
	// deep holds the LAUNCH-TIME deep-probe rows (empty unless the operator passed --probe-deep). They
	// are reported, never recomputed: recomputing would let a peer spend by polling a tool that promises
	// to spend nothing.
	deep []mcp.ReadinessCheck
}

func (c *cliConfig) ProfileSet() profile.Set { return c.set }

// Adapters projects the configured adapters through the manager's read views, dropping every field that
// describes the machine (Path, ACPArgs) and keeping the ones that describe the CHOICE (identifier,
// readable name, kind, configured, declared identity-evidence tier).
func (c *cliConfig) Adapters() []mcp.AdapterFact {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	src := c.plan
	mgr, err := manager.New(cwd, roster.Roster{Explorers: src.Explorers, Collator: src.Collator, Canonicalizers: src.Canonicalizers})
	if err != nil {
		return nil
	}
	var out []mcp.AdapterFact
	for _, av := range mgr.AdapterViews() {
		kind := "shell"
		if av.IsACP {
			kind = "acp"
		}
		out = append(out, mcp.AdapterFact{
			Name: av.Name, DisplayName: av.DisplayName, Kind: kind,
			Configured: av.Configured, IdentityEvidenceCapability: av.ModelIdentity, SpecOnly: av.SpecOnly,
		})
	}
	// The hidden internal `fake` harness is absent from AdapterViews and stays absent here unless the
	// internal gate is on (tests / golden runs, never a user) — `list` must never advertise a name a user
	// cannot configure.
	if corefake.Enabled() {
		out = append(out, mcp.AdapterFact{Name: registry.FakeAdapter, DisplayName: "Fake", Kind: "fake", Configured: true})
	}
	return out
}

// Readiness runs the STATIC readiness checks — the same ones `exploremesh doctor` reports without
// --probe — and appends any LAUNCH-TIME deep-probe rows the operator asked for with `--probe-deep`.
//
// Nothing here starts a process or spends: the deep rows were computed once, before serving, on the
// operator's own command line. That is what keeps the `doctor` tool's readOnlyHint honest while still
// letting a peer see the one readiness fact a `--version` check cannot produce — whether the CLI does
// real work in the isolated directory a run actually uses.
func (c *cliConfig) Readiness() (bool, []mcp.ReadinessCheck) {
	var rep mdoctor.Report
	rep.Checks = append(rep.Checks, mdoctor.Check{
		Name: "roster: explorers >= 2", OK: len(c.plan.Explorers) >= 2,
		Detail: fmt.Sprintf("%d explorers", len(c.plan.Explorers)),
	})
	// The same canonicalizer readiness the CLI `doctor` reports — surface parity: which two identities hold
	// the merge-agreement rule, and whether they were named or will be derived.
	rep.Checks = append(rep.Checks, canonicalizerCheck(c.plan, c.defaultMode))
	names := make([]string, 0, len(c.adapters))
	required := map[string]bool{}
	for name := range c.adapters {
		names = append(names, name)
	}
	sortStrings(names)
	for _, e := range c.plan.Explorers {
		required[e.Adapter] = true
	}
	required[c.plan.Collator.Adapter] = true
	rep.Checks = append(rep.Checks, mdoctor.AdapterAvailability(c.adapters, names, required)...)
	rep.Finalize()
	out := make([]mcp.ReadinessCheck, 0, len(rep.Checks)+len(c.deep))
	for _, ch := range rep.Checks {
		out = append(out, mcp.ReadinessCheck{Name: ch.Name, OK: ch.OK, Detail: ch.Detail})
	}
	ok := rep.OK
	for _, ch := range c.deep {
		out = append(out, ch)
		if !ch.OK {
			ok = false
		}
	}
	return ok, out
}

func joinNames(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
