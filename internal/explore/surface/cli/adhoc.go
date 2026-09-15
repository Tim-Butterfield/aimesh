package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"
	corefake "github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"

	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// This file holds the ad-hoc run surface: repeatable `--explorer` and `--collator` specs that build a
// one-off roster. Specs use `adapter=<n>,model=<m>[,effort=<e>]` rather than `adapter:model`, because
// model tags can contain colons (for example llama3:8b).

// slotSpecs collects the repeatable --explorer specs in order.
type slotSpecs []string

func (s *slotSpecs) String() string { return strings.Join(*s, "; ") }
func (s *slotSpecs) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseSlot parses one `adapter=<n>,model=<m>[,effort=<e>]` spec. Each field splits on its first '=', so
// a value may contain ':' or '='. adapter and model are required. Every error carries fault.Usage.
func parseSlot(spec string) (roster.Explorer, error) {
	var out roster.Explorer
	seen := map[string]bool{}
	for field := range strings.SplitSeq(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			return roster.Explorer{}, fault.New(fault.Usage, fmt.Sprintf("invalid field %q (want key=value)", field))
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if seen[key] {
			return roster.Explorer{}, fault.New(fault.Usage, fmt.Sprintf("duplicate key %q", key))
		}
		seen[key] = true
		switch key {
		case "adapter":
			out.Adapter = val
		case "model":
			out.Model = val
		case "effort":
			out.Effort = val
		default:
			return roster.Explorer{}, fault.New(fault.Usage, fmt.Sprintf("unknown key %q (want adapter, model, or effort)", key))
		}
	}
	if out.Adapter == "" || out.Model == "" {
		return roster.Explorer{}, fault.New(fault.Usage, fmt.Sprintf("both adapter and model are required (got adapter=%q model=%q)", out.Adapter, out.Model))
	}
	return out, nil
}

// buildAdHocRoster assembles a one-off roster from the --explorer specs and the --collator spec. It
// requires at least two explorers and a collator; Roster.Plan enforces the remaining roster invariants.
func buildAdHocRoster(explorerSpecs []string, collatorSpec string) (roster.Roster, error) {
	if len(explorerSpecs) < 2 {
		return roster.Roster{}, fault.New(fault.Usage, fmt.Sprintf("ad-hoc explore needs at least 2 --explorer specs (got %d)", len(explorerSpecs)))
	}
	if strings.TrimSpace(collatorSpec) == "" {
		return roster.Roster{}, fault.New(fault.Usage, "ad-hoc explore needs a --collator spec")
	}
	var r roster.Roster
	for _, spec := range explorerSpecs {
		e, err := parseSlot(spec)
		if err != nil {
			return roster.Roster{}, fault.Wrap(fault.Usage, fmt.Sprintf("--explorer %q", spec), err)
		}
		r.Explorers = append(r.Explorers, e)
	}
	c, err := parseSlot(collatorSpec)
	if err != nil {
		return roster.Roster{}, fault.Wrap(fault.Usage, fmt.Sprintf("--collator %q", collatorSpec), err)
	}
	r.Collator = roster.Collator(c)
	return r, nil
}

// buildCanonicalizers parses the repeatable `--canonicalizer` specs, using the same grammar as
// --explorer. roster.ValidateCanonicalizers applies the zero-or-two rule shared by every surface.
func buildCanonicalizers(specs []string) ([]roster.Explorer, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]roster.Explorer, 0, len(specs))
	for _, spec := range specs {
		c, err := parseSlot(spec)
		if err != nil {
			return nil, fault.Wrap(fault.Usage, fmt.Sprintf("--canonicalizer %q", spec), err)
		}
		out = append(out, c)
	}
	if err := roster.ValidateCanonicalizers(out); err != nil {
		return nil, err
	}
	return out, nil
}

// configuredAdapterNames returns the sorted adapter names a roster may reference: every shell recipe and
// configured ACP instance, plus `fake` only when the test-harness gate is enabled.
func configuredAdapterNames(acpInsts map[string]acpagent.Instance) []string {
	set := map[string]bool{}
	if corefake.Enabled() {
		set[registry.FakeAdapter] = true
	}
	for n := range shell.Recipes() {
		set[n] = true
	}
	for n := range acpInsts {
		set[n] = true
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
