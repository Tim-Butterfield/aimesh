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

// This file implements `aimesh explore list`, which reports the configured adapters, the resolved roster,
// the profiles and the modes, so a caller can check its configuration before a run. It projects the
// manager's views and the mode registry. `identityEvidenceCapability` is an adapter's declared evidence
// tier, not proof: confirming a model's identity takes a real call.

// listAdapter is one adapter as `list` reports it, a subset of manager.AdapterViewDTO.
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

// listRoster is the resolved roster: explorers in order, the collator and any explicit canonicalizers.
type listRoster struct {
	Explorers []listSlot `json:"explorers"`
	Collator  listSlot   `json:"collator"`
	// Canonicalizers is empty, meaning the host derives them, or exactly two. CanonicalizerSource states
	// which, so a reader need not infer it from an empty list.
	Canonicalizers      []listSlot `json:"canonicalizers"`
	CanonicalizerSource string     `json:"canonicalizerSource"`
}

// listProfile is one profile: its name, whether it is the default, its default mode ("" means map), its
// explorers in preference order (the order --count selects from), its collator and its canonicalizers.
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

// canonicalizerSource returns "explicit" when a roster names n > 0 canonicalizers, and "derived" when the
// host will choose them (slot a from the collator, slot b from the panel in preference order).
func canonicalizerSource(n int) string {
	if n > 0 {
		return "explicit"
	}
	return "derived"
}

// listProfiles is the default profile's name and every profile, sorted by name.
type listProfiles struct {
	DefaultProfile string        `json:"defaultProfile"`
	Profiles       []listProfile `json:"profiles"`
}

// listView is the `list --json` output: the adapters, the default profile's roster, the profiles and the
// modes.
type listView struct {
	Adapters []listAdapter `json:"adapters"`
	Roster   listRoster    `json:"roster"`
	Profiles listProfiles  `json:"profiles"`
	Modes    []string      `json:"modes"`
}

// runList reports the adapters, roster, profiles and modes. It resolves the roster through resolveSource,
// as a run without flags does, so it reports the configuration such a run would use.
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

// buildListView maps the manager's views into the `list` view.
func buildListView(mgr *manager.Manager) listView {
	view := listView{Adapters: []listAdapter{}, Modes: mode.Names()}
	for _, av := range mgr.AdapterViews() {
		view.Adapters = append(view.Adapters, listAdapter{
			Name: av.Name, DisplayName: av.DisplayName,
			Configured: av.Configured, Path: av.Path, IsACP: av.IsACP,
			IdentityEvidenceCapability: av.ModelIdentity, SpecOnly: av.SpecOnly,
		})
	}
	// The `fake` test adapter is listed, in sorted position, only when the internal test gate is on, so
	// `list` never shows users a name they cannot configure.
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

	// The manager holds the profile set a run resolves in this directory, so these are the profiles
	// `explore --profile` can select.
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

// printList writes the text report: adapters, the roster, the profiles and the modes.
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

// printProfiles writes each profile with its default mode, its explorers in preference order, its collator
// and its canonicalizers.
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
		// Preference order decides which explorers a --count subset selects.
		fmt.Fprintln(w, "    explorers (preference order):")
		for _, e := range pr.Explorers {
			fmt.Fprintf(w, "      %s\n", slotDisplay(e))
		}
		fmt.Fprintf(w, "    collator:\n      %s\n", slotDisplay(pr.Collator))
		printCanonicalizers(w, "    ", pr.Canonicalizers)
	}
}

// printCanonicalizers writes the canonicalizer slots, or the rule that derives them when there are none.
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

// slotDisplay renders a slot as `adapter:model[:effort]` for display only; a model name may contain a
// colon, so the form is never parsed.
func slotDisplay(s listSlot) string {
	if s.Adapter == "" && s.Model == "" {
		return "(not configured)"
	}
	out := s.Adapter + ":" + s.Model
	if s.Effort != "" {
		out += ":" + s.Effort
	}
	return out
}
