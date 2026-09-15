// Package roster defines an exploration panel (explorers, a collator and optional canonicalizers), its
// validation, the executable Plan built from it, and loading a roster file.
package roster

import (
	"fmt"
	"os"
	"sort"
	"strings"

	mcfg "github.com/Tim-Butterfield/aimesh/meshcore/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Explorer is one explorer identity: adapter, model and effort.
type Explorer struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// Identity returns the explorer's attribution key.
func (e Explorer) Identity() schema.ExplorerIdentity {
	return schema.ExplorerIdentity{Adapter: e.Adapter, Model: e.Model, Effort: e.Effort}
}

// triple returns the explorer's uniqueness key. Explorers that differ only in effort are distinct.
func (e Explorer) triple() string { return e.Adapter + "\x00" + e.Model + "\x00" + e.Effort }

// AttributionOrdered and PreferenceOrdered hold the same selected explorers in the two orders a run needs.
// Separate types make using the wrong order a compile error.
//
//   - AttributionOrdered is sorted by identity. Envelope ids and recorded artifacts use it, so reordering a
//     profile does not change the ids of an unchanged selection.
//   - PreferenceOrdered is the profile's own order. Selections with consequences, such as choosing
//     canonicalizer b, must use it; attribution order would choose alphabetically.
type (
	AttributionOrdered []Explorer
	PreferenceOrdered  []Explorer
)

// ValidateCanonicalizers accepts either no canonicalizers, so the host derives them, or exactly two with
// different adapter and model. One entry does not say which slot it fills, the merge-agreement rule is
// defined over exactly two proposals, and two proposals from the same model are not independent. Effort
// is ignored in the independence test.
func ValidateCanonicalizers(cs []Explorer) error {
	switch len(cs) {
	case 0:
		return nil
	case 2:
	case 1:
		return fault.New(fault.Config, fmt.Sprintf(
			"canonicalizers: got 1 entry — supply either 0 (the host derives both: slot a is the collator's identity, slot b the first explorer by PREFERENCE order that differs from it) or exactly 2 (the dual merge-agreement rule is defined over two independent proposals). One entry does not say which slot it fills (got adapter=%q model=%q)",
			cs[0].Adapter, cs[0].Model))
	default:
		return fault.New(fault.Config, fmt.Sprintf(
			"canonicalizers: got %d entries — supply either 0 (host-derived) or exactly 2; the merge-agreement rule holds a merge only when BOTH of two proposals contain it, so a third proposal has no role in it", len(cs)))
	}
	for i, c := range cs {
		if strings.TrimSpace(c.Adapter) == "" || strings.TrimSpace(c.Model) == "" {
			return fault.New(fault.Config, fmt.Sprintf("canonicalizers: entry %d needs a non-empty adapter and model", i))
		}
	}
	if cs[0].Adapter == cs[1].Adapter && cs[0].Model == cs[1].Model {
		return fault.New(fault.Config, fmt.Sprintf(
			"canonicalizers: both entries name (adapter=%q, model=%q) — two independent canonicalizer identities are required, and a second proposal from the same weights is not an independent judgment (a differing effort does not make it one)",
			cs[0].Adapter, cs[0].Model))
	}
	return nil
}

// Collator is the model that collates a run. It may use the same model as an explorer.
type Collator struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// Identity returns the collator's attribution key.
func (c Collator) Identity() schema.ExplorerIdentity {
	return schema.ExplorerIdentity{Adapter: c.Adapter, Model: c.Model, Effort: c.Effort}
}

// Roster is a configured panel: two or more unique explorers, one collator and optional canonicalizers.
type Roster struct {
	Explorers []Explorer `json:"explorers"`
	Collator  Collator   `json:"collator"`
	// Canonicalizers is empty, so the host derives them, or exactly two identities (see
	// ValidateCanonicalizers). A canonicalizer need not be an explorer.
	Canonicalizers []Explorer `json:"canonicalizers,omitempty"`
}

// IsZero reports whether the roster is entirely unconfigured, which is valid to save but cannot run.
func (r Roster) IsZero() bool {
	return len(r.Explorers) == 0 && len(r.Canonicalizers) == 0 && r.Collator == (Collator{})
}

// Validate checks for at least two explorers, no duplicate identities, a non-empty adapter and model on
// every seat, and valid canonicalizers.
func (r Roster) Validate() error {
	if len(r.Explorers) < 2 {
		return fault.New(fault.Config, fmt.Sprintf("roster: need at least 2 explorers, have %d", len(r.Explorers)))
	}
	seen := map[string]bool{}
	for i, e := range r.Explorers {
		if strings.TrimSpace(e.Adapter) == "" || strings.TrimSpace(e.Model) == "" {
			return fault.New(fault.Config, fmt.Sprintf("roster: explorer %d needs a non-empty adapter and model", i))
		}
		if seen[e.triple()] {
			return fault.New(fault.Config, fmt.Sprintf("roster: duplicate explorer (adapter=%q model=%q effort=%q) — an identical triple adds no independent vantage", e.Adapter, e.Model, e.Effort))
		}
		seen[e.triple()] = true
	}
	if strings.TrimSpace(r.Collator.Adapter) == "" || strings.TrimSpace(r.Collator.Model) == "" {
		return fault.New(fault.Config, "roster: collator needs a non-empty adapter and model")
	}
	if err := ValidateCanonicalizers(r.Canonicalizers); err != nil {
		return err
	}
	return nil
}

// Plan is the validated panel a run executes: the selected explorers in both orders, the collator and
// any explicit canonicalizers.
type Plan struct {
	// Explorers is the selection in attribution order, used for everything recorded about the run.
	Explorers AttributionOrdered
	// Preferred is the same selection in preference order, used to derive canonicalizer b.
	Preferred PreferenceOrdered
	Collator  Collator
	// Canonicalizers are the explicit canonicalizers, or empty when the host derives them.
	Canonicalizers []Explorer
}

// SameSet reports whether Explorers and Preferred contain the same explorers.
func (p Plan) SameSet() bool {
	if len(p.Explorers) != len(p.Preferred) {
		return false
	}
	counts := map[string]int{}
	for _, e := range p.Explorers {
		counts[e.triple()]++
	}
	for _, e := range p.Preferred {
		counts[e.triple()]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

// Plan validates the roster and returns a plan with every explorer. It is SelectTopN over the whole roster.
func (r Roster) Plan() (Plan, error) {
	return r.SelectTopN(len(r.Explorers))
}

// SelectTopN validates the roster and returns a plan for the first n explorers in preference order. The
// selection is also sorted into attribution order, so its envelope ids do not depend on preference order.
// n must be between 2 and the number of explorers; it is never clamped.
func (r Roster) SelectTopN(n int) (Plan, error) {
	if err := r.Validate(); err != nil {
		return Plan{}, err
	}
	if n < 2 {
		return Plan{}, fault.New(fault.Usage, fmt.Sprintf("count must be at least 2 — a panel needs 2+ explorers (got %d)", n))
	}
	if n > len(r.Explorers) {
		return Plan{}, fault.New(fault.Usage, fmt.Sprintf("count %d exceeds the %d explorer(s) available — the count is never clamped (requested = executed); lower --count or add explorers", n, len(r.Explorers)))
	}
	preferred := make(PreferenceOrdered, n)
	copy(preferred, r.Explorers[:n])
	selected := make(AttributionOrdered, n)
	copy(selected, preferred)
	sort.Slice(selected, func(i, j int) bool { return selected[i].triple() < selected[j].triple() })
	return Plan{
		Explorers: selected, Preferred: preferred, Collator: r.Collator,
		Canonicalizers: append([]Explorer(nil), r.Canonicalizers...),
	}, nil
}

// Decode strictly decodes roster data, as YAML or JSON according to path's extension. It does not call
// Validate.
func Decode(path string, b []byte) (Roster, error) {
	var r Roster
	if mcfg.IsYAML(path) {
		jb, err := mcfg.YAMLToJSON(b)
		if err != nil {
			return r, fault.Wrap(fault.Config, fmt.Sprintf("parse YAML roster %q", path), err)
		}
		b = jb
	}
	if err := mcfg.DecodeStrict(b, &r); err != nil {
		return r, fault.Wrap(fault.Config, fmt.Sprintf("parse roster %q", path), err)
	}
	return r, nil
}

// Load reads, decodes and validates the roster file at path.
func Load(path string) (Roster, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Roster{}, fault.Wrap(fault.Config, fmt.Sprintf("read roster %q", path), err)
	}
	r, err := Decode(path, b)
	if err != nil {
		return Roster{}, err
	}
	if verr := r.Validate(); verr != nil {
		return Roster{}, verr
	}
	return r, nil
}
