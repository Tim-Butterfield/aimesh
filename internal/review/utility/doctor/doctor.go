// Package doctor performs static, local readiness checks for reviewmesh. Batch 1 requires no
// cloud auth or real model calls; cloud adapters are reported as not-verified (verify-on-
// provision). It composes meshcore's domain-free readiness primitives (meshcore/doctor) with
// roster-aware checks (profile/lane resolvability) over the typed config.Config.
package doctor

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	mdoctor "github.com/Tim-Butterfield/aimesh/meshcore/doctor"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// Check and Report are re-exported from meshcore/doctor so existing reviewmesh callers keep using
// `doctor.Report` / `doctor.Check` unchanged. The probe capability is now `model.Prober`.
type (
	Check  = mdoctor.Check
	Report = mdoctor.Report
)

// Input is what doctor needs for static checks.
type Input struct {
	Config      config.Config
	Layers      config.Layers            // which config layers were discovered/loaded
	Adapters    map[string]model.Adapter // registered adapters (fake + shell recipes)
	ArtifactDir string
	Workspace   string // optional path under review
	Profile     string // optional: also check this profile's readiness (e.g. fully-local-ollama)
	// Resolved, when set, is the plan the CALLER already resolved for Profile — so readiness is
	// judged against THE LANES THAT WILL ACTUALLY RUN rather than re-derived from the profile.
	//
	// Without it this check re-resolved from config alone, which meant it could not see a `--set`
	// role override or a composed `--reviewer` panel. A run whose plan resolved perfectly was then
	// halted Class A by a preflight that had examined a different plan:
	//
	//	preflight failed: profile: default ready (no adapter resolvable for role "author_remediator")
	//
	// ...with that exact role overridden on the command line. The check was answering "is the
	// PROFILE ready", while the question the run needed answered is "is what I am about to run
	// ready" — and those diverge the moment an invocation overrides anything.
	//
	// Optional and additive: `aimesh review doctor` calls this standalone with no plan in hand and
	// keeps its existing behaviour, which is the right answer there — asking after the profile is
	// exactly what a bare `doctor` means.
	Resolved *review.RunPlan
	// ResolvedSeats is the resolved reviewer PANEL for Resolved, when the caller has one. The plan's
	// `Lanes[reviewer]` is only seat 1, so a panel whose seats sit on different adapters would
	// otherwise have the rest of them go unchecked.
	ResolvedSeats []review.LaneResolution
	Probe         bool // run a safe no-model binary probe for the selected profile's adapters
	// ProbeDeep additionally runs the DEEP probe: one REAL, bounded model invocation per required
	// adapter, in a throwaway isolated directory, through the same path a run takes. It SPENDS REAL
	// TOKENS, which is why it is a separate field from Probe rather than a stronger setting of it —
	// `--probe` is documented everywhere as free, and quietly making a free flag cost money would be
	// the worst possible way to add this. ProbeDeep implies Probe (the cheap ladder still runs first,
	// so a resolve/version failure is reported before anything is spent).
	ProbeDeep bool
	// Ctx bounds the (opt-in) adapter probe so a cancel/timeout propagates; nil → context.Background().
	Ctx context.Context
}

// layerDetail describes a config layer for doctor output.
func layerDetail(path string, loaded bool, fallback string) string {
	if loaded && path != "" {
		return "loaded: " + path
	}
	if path != "" {
		return "absent: " + path
	}
	return "absent: " + fallback
}

// Run executes the static checks.
func Run(in Input) Report {
	var r Report
	add := func(name string, ok bool, detail string) {
		r.Checks = append(r.Checks, Check{Name: name, OK: ok, Detail: detail})
	}

	// Config layers loaded (precedence: shipped defaults ← user/global ← project ←
	// explicit). These are informational (always OK) so users can see what was merged.
	add("config: shipped defaults loaded", true, "built-in seed (defaultProfile=default)")
	add("config: user/global config", true, layerDetail(in.Layers.UserPath, in.Layers.UserLoaded, "~/.aimesh/review/config.yaml"))
	add("config: project config (optional)", true, layerDetail(in.Layers.ProjectPath, in.Layers.ProjectLoaded, "<repo-root>/.aimesh/review/config.yaml"))
	if in.Layers.ExplicitPath != "" {
		add("config: --config override", true, layerDetail(in.Layers.ExplicitPath, in.Layers.ExplicitLoaded, in.Layers.ExplicitPath))
	}

	// Shared adapter-location layers (`.aimesh/adapters.yaml`) — the PATH-ONLY substrate shared with
	// exploremesh. Informational, like the config layers.
	add("adapters: user shared locations", true, layerDetail(in.Layers.SharedUserPath, in.Layers.SharedUserLoaded, "~/.aimesh/adapters.yaml"))
	if in.Layers.SharedProjectHasRoot {
		add("adapters: project shared locations", true, layerDetail(in.Layers.SharedProjectPath, in.Layers.SharedProjectLoaded, in.Layers.SharedProjectPath))
	}
	// A shadowed adapter path is a saved path that a HIGHER-precedence layer overrode — the user may
	// think a path is active when a different one takes effect. Surface it as a non-fatal warning.
	if len(in.Layers.AdapterPathShadowed) > 0 {
		add("adapters: no shadowed saved paths", false,
			fmt.Sprintf("a higher layer overrides the saved path for: %s (see `reviewmesh config` for the effective source)", strings.Join(in.Layers.AdapterPathShadowed, ", ")))
	}

	// Count only profiles the user can actually SEE. The shipped `fake-smoke` profile is hidden from
	// every profile list, so including it made doctor report "2 profile(s)" for a config with one
	// visible profile. Readiness still keys off the raw map: a hidden profile is real and resolvable.
	visibleProfiles := 0
	for name := range in.Config.Profiles {
		if !config.IsHiddenProfile(name) {
			visibleProfiles++
		}
	}
	add("config: profiles present", len(in.Config.Profiles) > 0,
		fmt.Sprintf("%d profile(s)", visibleProfiles))
	add("config: model catalog present", len(in.Config.ModelCatalog) > 0,
		fmt.Sprintf("%d catalog entr(y/ies)", len(in.Config.ModelCatalog)))

	defProf := in.Config.DefaultProfile
	_, hasDef := in.Config.Profiles[defProf]
	add("config: default profile resolvable", defProf == "" || hasDef,
		fmt.Sprintf("defaultProfile=%q", defProf))

	// The target profile is the one the run actually uses: the explicitly-selected
	// profile if given, else the default. Only its adapters are "required"; adapters
	// used only by OTHER profiles are opt-in (reported, but a missing one does not
	// fail doctor). This keeps a review's preflight scoped to the lanes it will use —
	// an unrelated (possibly cloud) default profile never fails a `--profile X` run.
	target := in.Profile
	if target == "" {
		target = in.Config.DefaultProfile
	}
	profilesToCheck := dedupNonEmpty(target)
	required := map[string]bool{}
	for _, pn := range profilesToCheck {
		for a := range profileAdapters(in.Config, pn) {
			required[a] = true
		}
	}

	// Adapter inventory availability (generic mechanism over the runnable adapter registry). The
	// `fake` adapter is a HIDDEN internal test harness: doctor lists it only under the internal
	// gate (fake.Enabled), so it never advertises a name users cannot configure.
	names := sortedAdapterNames(in.Config.Adapters)
	if !fake.Enabled() {
		names = slices.DeleteFunc(names, func(n string) bool { return n == "fake" })
	}
	r.Checks = append(r.Checks, mdoctor.AdapterAvailability(in.Adapters, names, required)...)

	// Profile readiness: does each checked profile resolve, and are its lane adapters
	// available? Cloud model calls are NOT made here (binary presence only).
	//
	// The resolved lanes are also where the DEEP probe's seats come from, when it is asked for: a
	// deep probe passes the model argument a real run would pass, and this is the only place that
	// argument is authoritatively resolved.
	var deepSeats []mdoctor.DeepSeat
	for _, pn := range profilesToCheck {
		// THE CALLER'S OWN PLAN WINS for the profile it resolved. Re-resolving here would discard
		// its overrides and its composed panel, and then report on a plan nobody is going to run.
		var plan review.RunPlan
		lanes := []review.LaneResolution{}
		if in.Resolved != nil && pn == target {
			plan = *in.Resolved
			lanes = append(lanes, in.ResolvedSeats...)
		} else {
			p, err := in.Config.Resolve(config.ResolveRequest{Profile: pn, Surface: "cli"})
			if err != nil {
				add("profile: "+pn+" ready", false, err.Error())
				continue
			}
			plan = p
		}
		for _, lane := range plan.Lanes {
			lanes = append(lanes, lane)
		}
		missing := ""
		for _, lane := range lanes {
			if lane.Adapter == "" {
				continue
			}
			deepSeats = append(deepSeats, mdoctor.DeepSeat{Adapter: lane.Adapter, ModelArg: string(lane.ModelArg), Effort: lane.Effort})
			if a, ok := in.Adapters[lane.Adapter]; ok {
				if avail, _ := a.Available(); !avail {
					missing = lane.Adapter
				}
			}
		}
		if missing != "" {
			add("profile: "+pn+" ready", false, "requires unavailable adapter "+missing)
		} else {
			add("profile: "+pn+" ready", true, "lanes resolvable + adapters available")
		}
	}

	// Optional safe binary probe (no model call) for the selected profile's adapters. The deep probe
	// implies it, so the cheap ladder always reports first: a binary that cannot even resolve is named
	// before anything is spent finding that out the expensive way.
	if in.Probe || in.ProbeDeep {
		ctx := in.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		r.Checks = append(r.Checks, mdoctor.ProbeAdapters(ctx, in.Adapters, names, required)...)
		// The DEEP probe: one real, bounded invocation per required adapter in a throwaway directory.
		// It SPENDS, so it happens only on this explicit opt-in, and only for adapters a lane of the
		// checked profile actually uses.
		if in.ProbeDeep {
			r.Checks = append(r.Checks, mdoctor.ProbeAdaptersDeep(ctx, in.Adapters, deepSeats)...)
		}
	}

	add("artifact dir writable", mdoctor.WritableDir(in.ArtifactDir), in.ArtifactDir)

	if in.Workspace != "" {
		_, err := os.Stat(in.Workspace)
		add("workspace path exists", err == nil, in.Workspace)
	}

	r.Finalize()
	return r
}

func profileAdapters(cfg config.Config, prof string) map[string]bool {
	out := map[string]bool{}
	p, ok := cfg.Profiles[prof]
	if !ok {
		return out
	}
	for _, lane := range p.Lanes {
		if lane.Adapter != "" {
			out[lane.Adapter] = true
		}
	}
	return out
}

func dedupNonEmpty(names ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func sortedAdapterNames(m map[string]config.Adapter) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
