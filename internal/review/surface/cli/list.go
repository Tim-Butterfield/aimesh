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

// `list` surfaces the CONFIGURED adapters + the configured profiles so a caller can verify (before
// spending) that the models it wants are configured. It REUSES the SetupManager's pure projections
// (AdapterViews + ProfileViews); it re-derives nothing. `identityEvidenceCapability` is the adapter's
// DECLARED evidence tier (envelope/cli_status/trace/self_report/…), NOT a live "verified" — proving a
// model's identity still requires a real call.

// listAdapter is the focused adapter projection `list` emits (a subset of AdapterView, with the declared
// evidence tier renamed to the honest `identityEvidenceCapability`).
type listAdapter struct {
	Name                       string `json:"name"`
	DisplayName                string `json:"displayName"`
	Configured                 bool   `json:"configured"`
	Path                       string `json:"path,omitempty"`
	IsACP                      bool   `json:"isAcp"`
	IdentityEvidenceCapability string `json:"identityEvidenceCapability,omitempty"`
	SpecOnly                   bool   `json:"specOnly,omitempty"`
}

// listLane is one profile lane as a role → adapter:model assignment.
type listLane struct {
	Role    string `json:"role"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
}

// listProfile is one profile (name, default flag, lanes).
type listProfile struct {
	Name      string     `json:"name"`
	IsDefault bool       `json:"isDefault"`
	Lanes     []listLane `json:"lanes"`
}

// listCatalogEntry is one `modelCatalog` key: what a caller may write, and what it resolves to.
//
// It is here because the catalog is the EXACT vocabulary `--reviewer model=…` requires, and it was
// the one part of that vocabulary `list` never showed — leaving reading the config file by hand as
// the only way to find a valid value. Effort is embedded in the key by convention, so the resolved
// effort is reported beside each binding rather than left to be inferred from the string.
type listCatalogEntry struct {
	Key            string            `json:"key"`
	Provider       string            `json:"provider,omitempty"`
	CanonicalModel string            `json:"canonicalModel,omitempty"`
	Adapters       []listCatalogBind `json:"adapters"`
	AdapterDefault bool              `json:"adapterDefault,omitempty"`
}

// listCatalogBind is one adapter a catalog key is reachable through, with the model argument actually
// passed to it — frequently NOT the key itself.
type listCatalogBind struct {
	Adapter  string `json:"adapter"`
	ModelArg string `json:"modelArg,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

// listView is reviewmesh's machine-readable `list --json` projection: the configured adapters, the
// model catalog, and the configured profiles.
type listView struct {
	Adapters []listAdapter      `json:"adapters"`
	Catalog  []listCatalogEntry `json:"modelCatalog"`
	Profiles []listProfile      `json:"profiles"`
}

// runList reports the configured adapters + profiles (--json for a machine-readable projection).
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
	// io.Discard: the projections are pure reads — the SetupManager's progress writer is unused here.
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

// buildListView maps the SetupManager's read projections into the focused `list` view (no re-derivation).
func buildListView(mgr *setup.Manager) listView {
	view := listView{Adapters: []listAdapter{}, Catalog: []listCatalogEntry{}, Profiles: []listProfile{}}
	for _, av := range mgr.AdapterViews() {
		view.Adapters = append(view.Adapters, listAdapter{
			Name: av.Name, DisplayName: av.DisplayName,
			Configured: av.Configured, Path: av.Path, IsACP: av.IsACP,
			IdentityEvidenceCapability: av.ModelIdentity, SpecOnly: av.SpecOnly,
		})
	}
	// The built-in `fake` adapter is a HIDDEN internal test harness — it is absent from AdapterViews
	// and from this inventory unless the internal gate (fake.Enabled — set by tests/golden runs, never
	// by users) is on, so `list` never advertises a name users cannot configure. When enabled (tests),
	// it is inserted in the alphabetical position AdapterViews' sorted output would have given it;
	// exploremesh's `list` gates it the same way, so the two inventories stay symmetric.
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

// printList renders the human summary: adapters (configured?/kind/declared identity tier), then the
// profiles with each lane as `role → adapter:model`.
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
	// The catalog sits between the adapters and the profiles because that is what it joins: a key
	// names a model, and each binding says which adapter carries it and with what argument. It is
	// printed with the flag that consumes it, so a reader meets the vocabulary and its use together.
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
