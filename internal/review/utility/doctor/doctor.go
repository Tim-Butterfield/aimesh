// Package doctor performs local readiness checks for review: config layers, adapter availability
// and profile resolvability. It makes no model call unless the deep probe is requested.
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

// Check and Report re-export meshcore/doctor's result types.
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
	// Resolved is the plan the caller already resolved for Profile, when it has one, so readiness is
	// judged against the lanes that will run, including `--set` overrides and a composed panel. A
	// standalone `doctor` leaves it nil and checks the profile.
	Resolved *review.RunPlan
	// ResolvedSeats is the resolved reviewer panel for Resolved. Lanes[reviewer] is only seat 1, so
	// the other seats' adapters are checked from here.
	ResolvedSeats []review.LaneResolution
	Probe         bool // run a no-model binary probe of the selected profile's adapters
	// ProbeDeep additionally makes one real, bounded model call per required adapter in a throwaway
	// directory. It spends tokens, so it is separate from the free Probe, which it implies.
	ProbeDeep bool
	// Ctx bounds the probes; nil means context.Background().
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

	// Config layers, lowest precedence first. These checks are informational and always OK.
	add("config: shipped defaults loaded", true, "built-in seed (defaultProfile=default)")
	add("config: user/global config", true, layerDetail(in.Layers.UserPath, in.Layers.UserLoaded, "~/.aimesh/review/config.yaml"))
	add("config: project config (optional)", true, layerDetail(in.Layers.ProjectPath, in.Layers.ProjectLoaded, "<repo-root>/.aimesh/review/config.yaml"))
	if in.Layers.ExplicitPath != "" {
		add("config: --config override", true, layerDetail(in.Layers.ExplicitPath, in.Layers.ExplicitLoaded, in.Layers.ExplicitPath))
	}

	// Shared adapter-location layers (`.aimesh/adapters.yaml`), also informational.
	add("adapters: user shared locations", true, layerDetail(in.Layers.SharedUserPath, in.Layers.SharedUserLoaded, "~/.aimesh/adapters.yaml"))
	if in.Layers.SharedProjectHasRoot {
		add("adapters: project shared locations", true, layerDetail(in.Layers.SharedProjectPath, in.Layers.SharedProjectLoaded, in.Layers.SharedProjectPath))
	}
	// A saved path overridden by a higher layer is reported, since the user may think it is active.
	if len(in.Layers.AdapterPathShadowed) > 0 {
		add("adapters: no shadowed saved paths", false,
			fmt.Sprintf("a higher layer overrides the saved path for: %s (see `reviewmesh config` for the effective source)", strings.Join(in.Layers.AdapterPathShadowed, ", ")))
	}

	// The count excludes hidden profiles; readiness still uses the full map.
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

	// Only the target profile's adapters are required, so an unrelated profile never fails a run's
	// preflight. Other adapters are reported without failing doctor.
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

	// The internal `fake` adapter is listed only under the test gate.
	names := sortedAdapterNames(in.Config.Adapters)
	if !fake.Enabled() {
		names = slices.DeleteFunc(names, func(n string) bool { return n == "fake" })
	}
	r.Checks = append(r.Checks, mdoctor.AdapterAvailability(in.Adapters, names, required)...)

	// Profile readiness: each profile resolves and its lane adapters are available. The resolved
	// lanes also supply the deep probe's model arguments.
	var deepSeats []mdoctor.DeepSeat
	for _, pn := range profilesToCheck {
		// The caller's resolved plan wins, keeping its overrides and composed panel.
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

	// The free binary probe runs before the deep probe, so a missing binary is reported before any
	// spend.
	if in.Probe || in.ProbeDeep {
		ctx := in.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		r.Checks = append(r.Checks, mdoctor.ProbeAdapters(ctx, in.Adapters, names, required)...)
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
