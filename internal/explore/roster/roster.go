// Package roster is exploremesh's configured explorer/collator set (design §6.1) plus its
// parse/validate/plan + persistence. It is the app-side "grammar" analogous to reviewmesh's
// profiles/lanes — exploremesh owns it; meshcore never learns it. Persistence goes through
// meshcore/config's domain-free store (strict decode + atomic write).
package roster

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	mcfg "github.com/Tim-Butterfield/aimesh/meshcore/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"gopkg.in/yaml.v3"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Explorer is one configured explorer — a distinct (adapter, model, effort) triple (design §6.1).
type Explorer struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// Identity returns the attribution key used throughout the pipeline + collator output.
func (e Explorer) Identity() schema.ExplorerIdentity {
	return schema.ExplorerIdentity{Adapter: e.Adapter, Model: e.Model, Effort: e.Effort}
}

// triple is the uniqueness key: the FULL (adapter, model, effort). Explorers differing only in
// effort are NOT duplicates (a different reasoning depth is a genuine vantage); an identical triple
// re-runs the same weights at the same depth and adds no independent signal, so it is rejected.
func (e Explorer) triple() string { return e.Adapter + "\x00" + e.Model + "\x00" + e.Effort }

// AttributionOrdered and PreferenceOrdered are the SAME selected explorer set carried in the two
// DIFFERENT orders a run needs, given distinct names so that reaching for the wrong one is a compile
// error rather than something a reviewer has to notice.
//
//   - AttributionOrdered is sorted by identity triple. It is the order envelope IDs, panel freezing and
//     every recorded artifact are built from, so a recording is reproducible and comparable across runs.
//     It is DELIBERATELY decoupled from what the author ranked: reordering the full explorer list must
//     not change the envelope IDs of an unchanged selected set.
//   - PreferenceOrdered is the author's own ranking (the profile's slice order). It is what SelectTopN
//     selects the top-N from, and it is the ONLY order a SELECTION with consequences may read.
//
// The distinction is load-bearing and was previously implicit: canonicalizer-b — half of the dual
// merge-agreement governance rule — was derived from the attribution order, which meant the second
// independent judgment was whichever explorer happened to sort first alphabetically. An attribution
// order must never be used as if it were a preference order.
type (
	AttributionOrdered []Explorer
	PreferenceOrdered  []Explorer
)

// ValidateCanonicalizers enforces the CANONICALIZER SPEC rule shared by every surface (design §4): a
// request supplies either NO canonicalizers (the host derives them) or EXACTLY TWO (the dual rule's two
// independent judgments). Anything else is refused with a teaching error, because:
//
//   - ONE entry is ambiguous about which slot it fills. Slot `a` defaults to the collator's identity and
//     slot `b` is the independent second opinion; a partial spec silently decides that for the user.
//   - MORE than two has no meaning: the merge-agreement rule (canon.DualRuleVersion) is defined over
//     exactly two proposals.
//   - TWO IDENTICAL identities halts for the same reason the derived path halts — a second proposal from
//     the same weights is not an independent judgment, and accepting it would manufacture exactly the
//     corroboration the dual rule exists to prevent.
//
// Effort is NOT part of the independence test: two efforts of one model are the same weights, and the
// derived path compares (adapter, model) for the same reason.
func ValidateCanonicalizers(cs []Explorer) error {
	switch len(cs) {
	case 0:
		return nil
	case 2: // the only accepted explicit shape
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

// Collator is exploremesh's single collating model (design §6.1). It MAY reuse an explorer's model
// (collation is a different function).
type Collator struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// Identity returns the collator's attribution key.
func (c Collator) Identity() schema.ExplorerIdentity {
	return schema.ExplorerIdentity{Adapter: c.Adapter, Model: c.Model, Effort: c.Effort}
}

// Roster is the full configured set: 2+ unique explorers, exactly one collator, and OPTIONALLY the two
// explicit canonicalizer identities.
type Roster struct {
	Explorers []Explorer `json:"explorers"`
	Collator  Collator   `json:"collator"`
	// Canonicalizers names the two identities that propose the canonicalization (design §4). It is
	// EITHER empty (the host derives them) or exactly two — see ValidateCanonicalizers. It is a separate
	// slot list rather than a flag on an explorer because canonicalization is a DISTINCT role whose
	// identity is decoupled from both the collator and the panel: a canonicalizer need not be an
	// explorer, and an explorer is not automatically fit to canonicalize.
	Canonicalizers []Explorer `json:"canonicalizers,omitempty"`
}

// IsZero reports a completely UNCONFIGURED roster (no explorers, zero collator) — the shape the
// shipped unconfigured `default` profile resolves to. Distinct from invalid: an unconfigured roster
// is a valid saved state that simply cannot run yet.
func (r Roster) IsZero() bool {
	return len(r.Explorers) == 0 && len(r.Canonicalizers) == 0 && r.Collator == (Collator{})
}

// Validate enforces the design §6.1 rules: minimum 2 explorers, no duplicate (adapter,model,effort)
// triple, non-empty adapter+model on every explorer and the collator.
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

// Plan is the validated, executable roster the pipeline consumes, after Validate has passed. It carries
// the SAME selected explorer set in BOTH orders — see AttributionOrdered / PreferenceOrdered — plus the
// collator and any explicit canonicalizer identities.
type Plan struct {
	// Explorers is the selected set in ATTRIBUTION order. Everything RECORDED about a run is built from
	// it (envelope IDs, the frozen panel, the dispatch order).
	Explorers AttributionOrdered
	// Preferred is the SAME set in the author's PREFERENCE order. Every SELECTION with consequences
	// reads it — today that is the derivation of canonicalizer-b (design §4).
	Preferred PreferenceOrdered
	Collator  Collator
	// Canonicalizers are the EXPLICIT canonicalizer identities carried from the roster/profile (empty
	// when the host is to derive them). Validated by ValidateCanonicalizers before it reaches here.
	Canonicalizers []Explorer
}

// SameSet reports whether the plan's two orders are permutations of one another — the invariant that
// makes them safely interchangeable as a SET while remaining distinct as ORDERS. A Plan whose orders
// diverged would mean a selection and its attribution disagreed about who is even on the panel.
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

// Plan validates the roster and returns the executable plan with ALL explorers in a STABLE attribution
// order (sorted by identity triple) so envelope ordering + attribution are reproducible across runs. It is
// SelectTopN over the whole roster.
func (r Roster) Plan() (Plan, error) {
	return r.SelectTopN(len(r.Explorers))
}

// SelectTopN validates the roster and returns the executable plan for the TOP-N explorers by PREFERENCE
// (the authored slice order — design §7), then canonicalizes the SELECTED subset into a STABLE ATTRIBUTION
// order (sorted by identity triple). Preference (selection) and attribution (envelope ordering) are thus
// DECOUPLED: reordering the full explorer list never changes the envelope IDs of an UNCHANGED selected set,
// because the plan's order depends only on WHICH explorers were selected, not on their preference order.
//
// BOTH orders are returned (Plan.Explorers / Plan.Preferred), because decoupling them is not the same as
// discarding one. A later stage that must SELECT (the dual path's canonicalizer-b) needs the preference
// order and previously had only the attribution order to reach for.
//
// n must be in [2, len(Explorers)]: n<2 is rejected (a panel needs 2+ explorers) and n greater than the
// roster size is a clear out-of-range error — the count is NEVER clamped (requested = executed, §7).
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
	copy(preferred, r.Explorers[:n]) // top-N by PREFERENCE (authored order) — RETAINED, not discarded
	selected := make(AttributionOrdered, n)
	copy(selected, preferred)
	// Canonicalize ATTRIBUTION order independently of preference so the selected set's envelope IDs are stable.
	// Both orders are carried: they are the same SET, and losing the preference order is what previously forced
	// a selection (canonicalizer-b) to read an alphabetical ordering it had no business reading.
	sort.Slice(selected, func(i, j int) bool { return selected[i].triple() < selected[j].triple() })
	return Plan{
		Explorers: selected, Preferred: preferred, Collator: r.Collator,
		Canonicalizers: append([]Explorer(nil), r.Canonicalizers...),
	}, nil
}

// Decode strict-decodes roster bytes (YAML or JSON by extension) into the typed Roster, using
// meshcore/config's domain-free decode plumbing. It does NOT run Validate (callers decide when).
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

// Load reads + strict-decodes + validates the roster at path.
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

// Save validates the roster and writes it atomically as YAML (camelCase keys via json tags) through
// meshcore/config's atomic writer. It refuses to persist an invalid roster.
func Save(path string, r Roster) error {
	if err := r.Validate(); err != nil {
		return err
	}
	// Route through the json tags so on-disk keys match the decode schema (YAML is a JSON superset).
	jb, err := json.Marshal(r)
	if err != nil {
		return fault.Wrap(fault.Internal, "marshal roster", err)
	}
	var v any
	if err := json.Unmarshal(jb, &v); err != nil {
		return fault.Wrap(fault.Internal, "marshal roster", err)
	}
	yb, err := yaml.Marshal(v)
	if err != nil {
		return fault.Wrap(fault.Internal, "marshal YAML roster", err)
	}
	return mcfg.WriteFileAtomic(path, yb)
}
