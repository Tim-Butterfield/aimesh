package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/app"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/setup"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// list reports the configured adapters, model catalog and profiles from the setup manager's
// projections. identityEvidenceCapability is an adapter's declared evidence tier, not a live
// verification.

// listAdapter is the adapter projection list emits.
type listAdapter struct {
	Name                       string `json:"name"`
	DisplayName                string `json:"displayName"`
	Configured                 bool   `json:"configured"`
	Path                       string `json:"path,omitempty"`
	IsACP                      bool   `json:"isAcp"`
	IdentityEvidenceCapability string `json:"identityEvidenceCapability,omitempty"`
	SpecOnly                   bool   `json:"specOnly,omitempty"`
}

// listLane is one profile seat as a role, adapter and model.
type listLane struct {
	Role    string `json:"role"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
}

// listProfile is one profile: its name, whether it is the default, and its seats.
type listProfile struct {
	Name      string     `json:"name"`
	IsDefault bool       `json:"isDefault"`
	Lanes     []listLane `json:"lanes"`
}

// listCatalogEntry is one modelCatalog key, the vocabulary --reviewer model=… accepts, with each
// adapter binding and its resolved effort.
type listCatalogEntry struct {
	Key            string            `json:"key"`
	Provider       string            `json:"provider,omitempty"`
	CanonicalModel string            `json:"canonicalModel,omitempty"`
	Adapters       []listCatalogBind `json:"adapters"`
	AdapterDefault bool              `json:"adapterDefault,omitempty"`
}

// listCatalogBind is one adapter a catalog key is reachable through, with the model argument passed to
// it, which often differs from the key.
type listCatalogBind struct {
	Adapter  string `json:"adapter"`
	ModelArg string `json:"modelArg,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

// listView is the list --json projection: adapters, model catalog and profiles.
type listView struct {
	Adapters []listAdapter      `json:"adapters"`
	Catalog  []listCatalogEntry `json:"modelCatalog"`
	Profiles []listProfile      `json:"profiles"`
}

// runList reports the configured adapters and profiles; --json emits the projection.
func runList(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh review list")
	asJSON := fs.Bool("json", false, "emit a machine-readable projection (adapters + profiles) instead of a human summary")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	a, err := app.New(app.Options{})
	if err != nil {
		fmt.Fprintln(errw, "aimesh review list:", err)
		return int(fault.CodeOf(err))
	}
	// The projections are pure reads, so the progress writer is unused.
	view := buildListView(a.SetupManager(io.Discard))
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(view); err != nil {
			fmt.Fprintln(errw, "aimesh review list:", err)
			return int(fault.Internal)
		}
		return int(fault.OK)
	}
	printList(out, view)
	return int(fault.OK)
}

// buildListView maps the setup manager's read projections into the list view.
func buildListView(mgr *setup.Manager) listView {
	view := listView{Adapters: []listAdapter{}, Catalog: []listCatalogEntry{}, Profiles: []listProfile{}}
	for _, av := range mgr.AdapterViews() {
		view.Adapters = append(view.Adapters, listAdapter{
			Name: av.Name, DisplayName: av.DisplayName,
			Configured: av.Configured, Path: av.Path, IsACP: av.IsACP,
			IdentityEvidenceCapability: av.ModelIdentity, SpecOnly: av.SpecOnly,
		})
	}
	// The fake adapter is an internal test harness, listed only when fake.Enabled, at its alphabetical
	// position. exploremesh's list gates it the same way.
	if fake.Enabled() {
		fakeRow := listAdapter{Name: "fake", DisplayName: setup.AdapterDisplayName("fake"), Configured: true,
			IdentityEvidenceCapability: string(review.EvidenceInvocationTag)}
		at := len(view.Adapters)
		for i, a := range view.Adapters {
			if a.Name > "fake" {
				at = i
				break
			}
		}
		view.Adapters = append(view.Adapters[:at], append([]listAdapter{fakeRow}, view.Adapters[at:]...)...)
	}
	for _, cv := range mgr.CatalogViews() {
		binds := make([]listCatalogBind, 0, len(cv.Adapters))
		for _, b := range cv.Adapters {
			binds = append(binds, listCatalogBind{Adapter: b.Adapter, ModelArg: b.ModelArg, Effort: b.Effort})
		}
		view.Catalog = append(view.Catalog, listCatalogEntry{
			Key: cv.Key, Provider: cv.Provider, CanonicalModel: cv.CanonicalModel,
			Adapters: binds, AdapterDefault: cv.AdapterDefault,
		})
	}
	for _, pv := range mgr.ProfileViews() {
		lanes := make([]listLane, 0, len(pv.Lanes))
		for _, l := range pv.Lanes {
			lanes = append(lanes, listLane{Role: l.Role, Adapter: l.Adapter, Model: l.Model})
		}
		view.Profiles = append(view.Profiles, listProfile{Name: pv.Name, IsDefault: pv.IsDefault, Lanes: lanes})
	}
	return view
}

// printList renders the human summary: adapters, the model catalog, then profiles with each seat as
// role → adapter:model.
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
	// The catalog sits between adapters and profiles, beside the flag that consumes it.
	if len(view.Catalog) > 0 {
		fmt.Fprintln(w, "\nModel catalog (the keys `--reviewer model=…` and profile lanes accept):")
		for _, c := range view.Catalog {
			def := ""
			if c.AdapterDefault {
				def = " [adapter default]"
			}
			label := c.Key
			if c.CanonicalModel != "" && c.CanonicalModel != c.Key {
				label += " → " + c.CanonicalModel
			}
			fmt.Fprintf(w, "  %s%s\n", label, def)
			for _, b := range c.Adapters {
				line := "    via " + b.Adapter
				if b.ModelArg != "" {
					line += " as " + b.ModelArg
				}
				if b.Effort != "" {
					line += " (effort " + b.Effort + ")"
				}
				fmt.Fprintln(w, line)
			}
		}
	}
	fmt.Fprintln(w, "\nProfiles:")
	for _, p := range view.Profiles {
		def := ""
		if p.IsDefault {
			def = " [default]"
		}
		fmt.Fprintf(w, "  %s%s\n", p.Name, def)
		for _, l := range p.Lanes {
			fmt.Fprintf(w, "    %s → %s:%s\n", l.Role, l.Adapter, l.Model)
		}
	}
}
