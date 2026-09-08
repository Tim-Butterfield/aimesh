package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/manager"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	corefake "github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// list surfaces the CONFIGURED adapters + the resolved roster + the configured PROFILES + the available
// modes so a caller can verify (before spending) that the models it wants are configured — the read-only
// analogue of reviewmesh's `list` (design §8). It REUSES the manager's pure projections (AdapterViews +
// RosterView + Profiles) and the mode registry (mode.Names); it re-derives nothing.
// `identityEvidenceCapability` is the adapter's DECLARED evidence tier
// (envelope/cli_status/trace/self_report/…), NOT a live "verified" — proving a model's identity still
// needs a real call.

// listAdapter is the focused adapter projection `list` emits (a subset of the manager's AdapterViewDTO,
// with the declared evidence tier renamed to the honest `identityEvidenceCapability`).
type listAdapter struct {
	Name                       string `json:"name"`
	DisplayName                string `json:"displayName"`
	Configured                 bool   `json:"configured"`
	Path                       string `json:"path,omitempty"`
	IsACP                      bool   `json:"isAcp"`
	IdentityEvidenceCapability string `json:"identityEvidenceCapability,omitempty"`
	SpecOnly                   bool   `json:"specOnly,omitempty"`
}

// listSlot is one roster slot (explorer or collator) as an (adapter, model, effort) triple.
type listSlot struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// listRoster is the resolved roster (ordered explorers + the single collator + any explicit canonicalizers).
type listRoster struct {
	Explorers []listSlot `json:"explorers"`
	Collator  listSlot   `json:"collator"`
	// Canonicalizers is EITHER empty (the host derives them at run time) or exactly two. CanonicalizerSource
	// says which of those two it is IN WORDS, so a reader never has to infer a governance fact from an empty
	// array — which is exactly the inference that let a derived-and-arbitrary choice go unnoticed.
	Canonicalizers      []listSlot `json:"canonicalizers"`
	CanonicalizerSource string     `json:"canonicalizerSource"`
}

// listProfile is one configured profile (design §7): its name, whether a no-flag run binds to it, its
// per-profile default mode ("" = the app default map), its ORDERED explorers (the authored slice
// order IS the --count preference order — deliberately NOT the Plan's canonical attribution order), its
// collator and its explicit canonicalizers.
type listProfile struct {
	Name        string     `json:"name"`
	IsDefault   bool       `json:"isDefault"`
	DefaultMode string     `json:"defaultMode,omitempty"`
	Explorers   []listSlot `json:"explorers"`
	Collator    listSlot   `json:"collator"`
	// See listRoster: 0 or 2 entries, with the source stated rather than implied.
	Canonicalizers      []listSlot `json:"canonicalizers"`
	CanonicalizerSource string     `json:"canonicalizerSource"`
}

// canonicalizerSource labels HOW a roster's canonicalizers will be chosen, matching the provenance a run
// records: "explicit" when the config names them, "derived" when the host will pick them (slot a from the
// collator, slot b from the panel in preference order).
func canonicalizerSource(n int) string {
	if n > 0 {
		return "explicit"
	}
	return "derived"
}

// listProfiles is the profile-set section: the name of the default profile + every configured profile
// (sorted by name — the map order is not load-bearing; the default is NAMED, not positional).
type listProfiles struct {
	DefaultProfile string        `json:"defaultProfile"`
	Profiles       []listProfile `json:"profiles"`
}

// listView is exploremesh's machine-readable `list --json` projection: the configured adapters, the
// resolved roster, the configured profiles, and the available exploration modes. `roster` is the DEFAULT
// profile's roster — it predates profiles and is kept for existing consumers (additive-only projection).
type listView struct {
	Adapters []listAdapter `json:"adapters"`
	Roster   listRoster    `json:"roster"`
	Profiles listProfiles  `json:"profiles"`
	Modes    []string      `json:"modes"`
}

// runList reports the configured adapters + resolved roster + profiles + available modes. It resolves the
// STARTING roster through the SAME resolver a no-flag `explore`/`doctor`/`acp`/`ui` run uses (resolveSource
// → profile.Resolve: the discovered profiles.yaml's default profile, else a migrated legacy roster.yaml,
// else the built-in unconfigured default), so `list` reports the config a bare `explore` would actually bind to.
func runList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cliflags.Style(fs, "aimesh explore list")
	asJSON := fs.Bool("json", false, "emit a machine-readable projection (adapters + roster + profiles + modes) instead of a human summary")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "exploremesh: %v\n", err)
		return int(fault.Internal)
	}
	r, _, _, err := resolveSource("", "")
	if err != nil {
		fmt.Fprintf(stderr, "exploremesh: %v\n", err)
		return codeOf(err)
	}
	mgr, err := manager.New(cwd, r)
	if err != nil {
		fmt.Fprintf(stderr, "exploremesh: %v\n", err)
		return codeOf(err)
	}
	view := buildListView(mgr)

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(view); err != nil {
			fmt.Fprintf(stderr, "exploremesh: %v\n", err)
			return int(fault.Internal)
		}
		return int(fault.OK)
	}
	printList(stdout, view)
	return int(fault.OK)
}

// buildListView maps the manager's read projections into the focused `list` view (no re-derivation).
func buildListView(mgr *manager.Manager) listView {
	view := listView{Adapters: []listAdapter{}, Modes: mode.Names()}
	for _, av := range mgr.AdapterViews() {
		view.Adapters = append(view.Adapters, listAdapter{
			Name: av.Name, DisplayName: av.DisplayName,
			Configured: av.Configured, Path: av.Path, IsACP: av.IsACP,
			IdentityEvidenceCapability: av.ModelIdentity, SpecOnly: av.SpecOnly,
		})
	}
	// The built-in `fake` adapter is a HIDDEN internal test harness — absent from AdapterViews and
	// from this inventory unless the internal gate (corefake.Enabled — set by tests/golden runs,
	// never by users) is on, so `list` never advertises a name users cannot configure. When enabled
	// (tests), it is inserted in sorted position; reviewmesh's `list` gates it the same way, so the
	// two inventories stay symmetric.
	if corefake.Enabled() {
		fakeRow := listAdapter{Name: registry.FakeAdapter, DisplayName: "Fake", Configured: true}
		at := len(view.Adapters)
		for i, a := range view.Adapters {
			if a.Name > fakeRow.Name {
				at = i
				break
			}
		}
		view.Adapters = append(view.Adapters[:at], append([]listAdapter{fakeRow}, view.Adapters[at:]...)...)
	}
	rv := mgr.RosterView()
	view.Roster.Explorers = make([]listSlot, 0, len(rv.Explorers))
	for _, e := range rv.Explorers {
		view.Roster.Explorers = append(view.Roster.Explorers, listSlot{Adapter: e.Adapter, Model: e.Model, Effort: e.Effort})
	}
	view.Roster.Collator = listSlot{Adapter: rv.Collator.Adapter, Model: rv.Collator.Model, Effort: rv.Collator.Effort}
	view.Roster.Canonicalizers = make([]listSlot, 0, len(rv.Canonicalizers))
	for _, cz := range rv.Canonicalizers {
		view.Roster.Canonicalizers = append(view.Roster.Canonicalizers, listSlot{Adapter: cz.Adapter, Model: cz.Model, Effort: cz.Effort})
	}
	view.Roster.CanonicalizerSource = canonicalizerSource(len(rv.Canonicalizers))

	// The full profile set (design §7). The manager binds the SAME set a run resolves for this cwd (a
	// persisted profiles.yaml, else the caller's already-resolved starting roster as the `default`
	// profile), so `list` reports the profiles `explore --profile` can actually select.
	set := mgr.Profiles()
	view.Profiles = listProfiles{DefaultProfile: set.DefaultProfile, Profiles: []listProfile{}}
	for _, name := range set.Names() {
		p := set.Profiles[name]
		lp := listProfile{
			Name:           name,
			IsDefault:      name == set.DefaultProfile,
			DefaultMode:    p.DefaultMode,
			Explorers:      make([]listSlot, 0, len(p.Explorers)),
			Canonicalizers: make([]listSlot, 0, len(p.Canonicalizers)),
		}
		for _, e := range p.Explorers {
			lp.Explorers = append(lp.Explorers, listSlot{Adapter: e.Adapter, Model: e.Model, Effort: e.Effort})
		}
		lp.Collator = listSlot{Adapter: p.Collator.Adapter, Model: p.Collator.Model, Effort: p.Collator.Effort}
		for _, cz := range p.Canonicalizers {
			lp.Canonicalizers = append(lp.Canonicalizers, listSlot{Adapter: cz.Adapter, Model: cz.Model, Effort: cz.Effort})
		}
		lp.CanonicalizerSource = canonicalizerSource(len(p.Canonicalizers))
		view.Profiles.Profiles = append(view.Profiles.Profiles, lp)
	}
	return view
}

// printList renders the human summary: adapters (with configured?/kind/declared identity tier), then the
// resolved roster as `adapter:model:effort` triples, then the configured profiles, then the modes.
func printList(w io.Writer, view listView) {
	fmt.Fprintln(w, "Adapters:")
	for _, a := range view.Adapters {
		state := "not configured"
		if a.Configured {
			state = "configured"
			if a.Path != "" {
				state += " (" + a.Path + ")"
			}
		}
		kind := "shell"
		switch {
		case a.IsACP:
			kind = "ACP"
		case a.Name == "fake":
			kind = "fake"
		}
		line := fmt.Sprintf("  %s (%s) [%s] — %s", a.Name, a.DisplayName, kind, state)
		if a.IdentityEvidenceCapability != "" {
			line += "; identity: " + a.IdentityEvidenceCapability
		}
		if a.SpecOnly {
			line += "; spec-only"
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "\nRoster:")
	fmt.Fprintln(w, "  explorers:")
	for _, e := range view.Roster.Explorers {
		fmt.Fprintf(w, "    %s\n", slotDisplay(e))
	}
	fmt.Fprintf(w, "  collator:\n    %s\n", slotDisplay(view.Roster.Collator))
	printCanonicalizers(w, "  ", view.Roster.Canonicalizers)
	printProfiles(w, view.Profiles)
	fmt.Fprintf(w, "\nModes: %s\n", strings.Join(view.Modes, ", "))
}

// printProfiles renders the configured profiles: the default profile's name, then each profile with its
// default mode and its ORDERED explorers (preference order — what --count selects the top-N from) + its
// collator, as display-only `adapter:model[:effort]` triples.
func printProfiles(w io.Writer, p listProfiles) {
	fmt.Fprintf(w, "\nProfiles (default: %s):\n", p.DefaultProfile)
	for _, pr := range p.Profiles {
		line := "  " + pr.Name
		if pr.IsDefault {
			line += " (default)"
		}
		if pr.DefaultMode != "" {
			line += "; mode: " + pr.DefaultMode
		}
		fmt.Fprintln(w, line)
		// Preference order — reordering changes WHICH explorers a --count subset selects (design §7).
		fmt.Fprintln(w, "    explorers (preference order):")
		for _, e := range pr.Explorers {
			fmt.Fprintf(w, "      %s\n", slotDisplay(e))
		}
		fmt.Fprintf(w, "    collator:\n      %s\n", slotDisplay(pr.Collator))
		printCanonicalizers(w, "    ", pr.Canonicalizers)
	}
}

// printCanonicalizers renders the canonicalizer slots, or — when there are none — the DERIVATION RULE in
// words. Printing the rule rather than nothing is the point: "which two identities decide whether a merge
// holds" is a governance fact, and silence about it is what let an arbitrary alphabetical choice stand.
func printCanonicalizers(w io.Writer, indent string, slots []listSlot) {
	if len(slots) == 0 {
		fmt.Fprintf(w, "%scanonicalizers:\n%s  derived (a: the collator; b: the first explorer by preference order that differs from it)\n", indent, indent)
		return
	}
	fmt.Fprintf(w, "%scanonicalizers (explicit):\n", indent)
	for _, s := range slots {
		fmt.Fprintf(w, "%s  %s\n", indent, slotDisplay(s))
	}
}

// slotDisplay renders a roster slot as the DISPLAY-ONLY `adapter:model[:effort]` colon form (never
// parsed back — a model tag can itself contain a colon, so the colon form is one-way).
func slotDisplay(s listSlot) string {
	// An UNCONFIGURED slot (the shipped empty default) renders as an honest label, not a bare ":".
	if s.Adapter == "" && s.Model == "" {
		return "(not configured)"
	}
	out := s.Adapter + ":" + s.Model
	if s.Effort != "" {
		out += ":" + s.Effort
	}
	return out
}
