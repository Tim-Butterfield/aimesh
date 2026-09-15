// Package mode is the registry of exploration modes. A ModeSpec is a mode's contract: its explorer prompt
// and response schema, its terminal step, its governance policy and its round count. Modes register
// themselves at package init.
package mode

import (
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Mode names.
const (
	// Map, the default, has explorers answer with claims, reasons and uncertainty; the collator collates.
	Map = "map"
	// Synthesize has each explorer give its best complete answer; the collator selects and composes the
	// strongest one, recording provenance and a minority report.
	Synthesize = "synthesize"
	// Catalog has explorers enumerate broadly; a single canonicalizer groups the nominations, which are
	// organized into clusters and proposed dimensions. It does not rank.
	Catalog = "catalog"
	// Challenge has explorers attack a supplied artifact. Findings are canonicalized by two canonicalizers,
	// confirmed by the panel, cross-reviewed in a mediated round, and collated into a severity-triaged
	// register with host-computed corroboration.
	Challenge = "challenge"
	// Shortlist has explorers enumerate candidates, which are canonicalized, confirmed and put to a ballot
	// under frozen decision inputs. The host tallies the ranking.
	Shortlist = "shortlist"
	// AICollab has each agent shortlist its own findings blind, then challenge the others' findings in a
	// mediated round. It reuses Challenge's schema, later round and collator, differing in its round-1
	// prompt and in needing no supplied artifact.
	AICollab = "ai-collab"
	// Compare is a fixed-space mode: the user declares options and criteria, explorers evaluate the grid
	// blind, and the host computes the matrix, agreement, filter gate and Pareto frontier. It runs one round
	// with no canonicalizer.
	Compare = "compare"
	// Forecast is a fixed-space mode: the user declares a target, unit and horizon, explorers return numeric
	// estimates, and the host pools them. It runs one round with no canonicalizer.
	Forecast = "forecast"
)

// DefaultName is the mode used when a task names none.
const DefaultName = Map

// Objective labels what the terminal step does after the explorer fan-out.
type Objective string

// Objectives.
const (
	// ObjectiveCollateOnly collates the blind envelopes into a CollatorOutput.
	ObjectiveCollateOnly Objective = "collate_only"
	// ObjectiveSelectCompose selects the strongest answer and composes it with elements of the others.
	ObjectiveSelectCompose Objective = "select_compose"
	// ObjectiveCanonicalizeCollate canonicalizes nominations and organizes them into clusters and proposed
	// dimensions.
	ObjectiveCanonicalizeCollate Objective = "canonicalize_collate"
	// ObjectiveCanonicalizeMediateCollate canonicalizes and confirms findings, runs a mediated cross-review
	// round, then collates a severity-triaged register.
	ObjectiveCanonicalizeMediateCollate Objective = "canonicalize_mediate_collate"
	// ObjectiveCanonicalizeBallotCollate canonicalizes and confirms candidates, runs a ballot, and collates
	// the host tally into a ranked subset.
	ObjectiveCanonicalizeBallotCollate Objective = "canonicalize_ballot_collate"
	// ObjectiveHostAggregateCollate computes the whole result on the host before the collator is called;
	// the collator contributes narrative only.
	ObjectiveHostAggregateCollate Objective = "host_aggregate_collate"
)

// ModeSpec is a mode's contract. Exactly one of Collator, Canonicalizing and FixedSpace is set.
type ModeSpec struct {
	// Name is the mode's identifier, such as "map".
	Name string
	// FormulationFree reports whether explorers receive the mode's own Prompt and ExplorerSchema. Every
	// registered mode sets it; the pipeline refuses a mode that does not.
	FormulationFree bool
	// Prompt builds the explorer prompt from the raw task.
	Prompt func(schema.RawTask) string
	// ExplorerSchema returns a fresh copy of the explorer response schema.
	ExplorerSchema func() schema.Schema
	// Objective is the terminal step's role.
	Objective Objective
	// Collator is the plain terminal-collation contract.
	Collator CollatorContract
	// Canonicalizing is the canonicalizing terminal contract.
	Canonicalizing CanonicalizingContract
	// FixedSpace is the fixed-space terminal contract, for modes whose option set or target is declared
	// before the fan-out.
	FixedSpace FixedSpaceContract
	// Canonicalization is the policy over canonicalization; the zero value is a single canonicalizer with
	// no confirmation round.
	Canonicalization CanonicalizationPolicy
	// Rounds is the fixed number of explorer rounds; 0 or 1 means one. More than one requires LaterRound.
	Rounds int
	// LaterRound is the contract for rounds after the first.
	LaterRound LaterRoundContract
	// Ballot is set by a mode whose later round is a ballot. The pipeline freezes the decision inputs before
	// that round and tallies the ballots itself.
	Ballot BallotContract
	// ValidateTask checks mode-specific task inputs, such as Challenge's artifact, before any spend. nil
	// means RawTask.Validate is sufficient.
	ValidateTask func(schema.RawTask) error
	// Class selects the degraded artifact produced if the collator is lost. Unset means EmergentSpace, the
	// safe default: treating an emergent space as fixed would let the host group free-form claims.
	Class schema.ModeClass
	// FixedSpaceKeyField is the response field a fixed-space degraded register is keyed on, such as
	// Compare's option name.
	FixedSpaceKeyField string
}

// CanonicalizationPolicy is a mode's policy over the canonicalization step.
type CanonicalizationPolicy struct {
	// Dual runs two independent canonicalizers and keeps only merges both propose.
	Dual bool
	// Confirm runs the confirmation round after the provisional partition.
	Confirm bool
}

// ModeClass returns the mode's space class, treating an unset Class as EmergentSpace.
func (s ModeSpec) ModeClass() schema.ModeClass {
	if s.Class == schema.FixedSpace {
		return schema.FixedSpace
	}
	return schema.EmergentSpace
}

// RoundCount returns the mode's round count, validated by round.Count.
func (s ModeSpec) RoundCount() (int, error) { return round.Count(s.Rounds) }

// CheckTask runs the mode's ValidateTask, if any.
func (s ModeSpec) CheckTask(raw schema.RawTask) error {
	if s.ValidateTask == nil {
		return nil
	}
	return s.ValidateTask(raw)
}

// ValidateTask runs the task check of the mode named name. It returns nil for an unknown mode, which
// callers reject separately.
func ValidateTask(name string, raw schema.RawTask) error {
	spec, ok := Resolve(name)
	if !ok {
		return nil
	}
	return spec.CheckTask(raw)
}

// LaterRoundContract is a mode's contract for rounds after the first. The host frames the carried artifact
// as untrusted data; the contract cannot change that framing.
type LaterRoundContract interface {
	// Accepts declares the artifacts the round consumes; the host validates the edge before the fan-out.
	Accepts() round.Accepts
	// Prompt builds the round's explorer prompt, embedding the host-rendered untrusted-data block as given.
	Prompt(raw schema.RawTask, untrustedDataBlock string) string
	// ExplorerSchema returns the round's response schema.
	ExplorerSchema() schema.Schema
}

// registry holds the registered modes. It is written only during init, so reads need no lock.
var registry = map[string]ModeSpec{}

// register adds s to the registry and panics on a duplicate name.
func register(s ModeSpec) {
	if _, dup := registry[s.Name]; dup {
		panic("mode: duplicate registration for " + s.Name)
	}
	registry[s.Name] = s
}

func init() {
	register(ModeSpec{
		Name:            Map,
		FormulationFree: true,
		Prompt:          schema.DefaultPrompt,
		ExplorerSchema:  schema.MinimumSchema,
		Objective:       ObjectiveCollateOnly,
		Collator:        mapCollator{},
		Class:           schema.EmergentSpace,
	})
	register(ModeSpec{
		Name:            Synthesize,
		FormulationFree: true,
		Prompt:          schema.SynthesizeExplorerPrompt,
		ExplorerSchema:  schema.SynthesizeExplorerSchema,
		Objective:       ObjectiveSelectCompose,
		Collator:        synthesizeCollator{},
		Class:           schema.EmergentSpace,
	})
	register(ModeSpec{
		Name:            Catalog,
		FormulationFree: true,
		Prompt:          schema.CatalogExplorerPrompt,
		ExplorerSchema:  schema.CatalogExplorerSchema,
		Objective:       ObjectiveCanonicalizeCollate,
		Canonicalizing:  catalogCollator{},
		Class:           schema.EmergentSpace,
	})
}

// Lookup returns the mode registered under exactly name.
func Lookup(name string) (ModeSpec, bool) {
	s, ok := registry[name]
	return s, ok
}

// Resolve returns the mode for a task's mode string, treating "" as DefaultName.
func Resolve(name string) (ModeSpec, bool) {
	if name == "" {
		name = DefaultName
	}
	return Lookup(name)
}

// Names returns the registered mode names in sorted order.
func Names() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
