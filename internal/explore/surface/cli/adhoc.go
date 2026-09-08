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

// This file holds the AD-HOC by-identifier run surface (design §8): repeatable `--explorer` /
// `--collator` structured specs on `explore`, which build a one-off roster bypassing --roster/profile.
// The specs use a STRUCTURED `adapter=<n>,model=<m>[,effort=<e>]` grammar — deliberately NOT the
// `adapter:model` colon shorthand, which is UNSAFE because model tags contain colons (e.g. llama3:8b).

// slotSpecs collects the repeatable --explorer specs (order preserved).
type slotSpecs []string

func (s *slotSpecs) String() string { return strings.Join(*s, "; ") }
func (s *slotSpecs) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseSlot parses one `adapter=<n>,model=<m>[,effort=<e>]` spec. Fields split on commas and each field
// splits on the FIRST '=' only — so a VALUE may safely contain ':' (a model tag like llama3:8b) or '='
// without being mangled (the colon form is display-only and is never parsed here). adapter+model are
// required; effort is optional.
//
// Every failure here is a malformed FLAG, so each one carries fault.Usage: the exit code then follows
// from the error itself at the print boundary rather than from a literal the caller has to remember.
func parseSlot(spec string) (roster.Explorer, error) {
	var out roster.Explorer
	seen := map[string]bool{}
	for _, field := range strings.Split(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			return roster.Explorer{}, fault.New(fault.Usage, fmt.Sprintf("invalid field %q (want key=value)", field))
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1]) // NOT split on ':' — the value is taken verbatim
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

// buildAdHocRoster assembles a one-off roster from the structured --explorer specs + the single
// --collator spec. It enforces the surface pre-conditions (>=2 explorers, a collator present); the
// deeper roster invariants (unique triples, non-empty fields) are enforced by Roster.Plan downstream.
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
	r.Collator = roster.Collator{Adapter: c.Adapter, Model: c.Model, Effort: c.Effort}
	return r, nil
}

// buildCanonicalizers parses the repeatable `--canonicalizer adapter=<n>,model=<m>[,effort=<e>]` specs into
// the explicit canonicalizer identities (design §4). It uses the SAME structured grammar as --explorer /
// --collator — deliberately not the colon shorthand, which cannot represent a model tag containing a colon.
//
// The 0-or-2 rule (and the two-identical-identities refusal) is roster.ValidateCanonicalizers', so the CLI,
// the ACP surface, the MCP server and the profile schema all state the same rule with the same words.
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

// configuredAdapterNames is the sorted set of adapter names a roster may reference: every shell recipe
// and every configured ACP instance (plus, ONLY under the internal test-harness gate, the hidden
// `fake` key — it must never be advertised to users). It names the choices in the ad-hoc fail-closed
// error.
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
