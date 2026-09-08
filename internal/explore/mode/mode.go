// Package mode is exploremesh's mode registry (design §3): a mode is a VERSIONED, app-owned contract
// describing how one exploration is run. A mode's behavior is captured by a ModeSpec — its name,
// whether round-1 is formulation-free (the collator does NOT author the explorer schema), the
// app-owned explorer prompt builder + fixed explorer-response schema, and the collator's objective
// after the fan-out. The registry is deliberately small + extensible: adding a mode is registering one
// more ModeSpec at package init.
//
// It is exploremesh app grammar — meshcore never learns modes (design §10). It depends on the schema
// package only (never the pipeline), so the pipeline + surfaces resolve a ModeSpec from a task's mode.
package mode

import (
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Mode names (the app-owned vocabulary, design §3).
const (
	// Map (=today's behavior; the default) is FORMULATION-FREE: explorers answer with claims + reasons +
	// uncertainty against the fixed minimum schema, and the collator collates only. There is no
	// collator-authored explorer schema, so the collator cannot contradict itself between the field
	// guidance and the emitted schema (the real 3-provider dogfood halt this closes).
	Map = "map"
	// Synthesize (design §3) is FORMULATION-FREE like Map: each explorer gives its single best
	// COMPLETE answer + rationale + assumptions against the app-owned synthesize schema, and the collator
	// SELECTS/COMPOSES the strongest answer (grafting superior elements, recording provenance + a minority
	// report — never a tally). Formulation-free keeps the collator out of round-1 (§5): the explorer
	// prompt AND schema are app-owned, so the collator cannot dilute the task.
	Synthesize = "synthesize"
	// Catalog (design §3) is FORMULATION-FREE like Map/Synthesize: each explorer ENUMERATES BROADLY
	// against the app-owned catalog schema, and the terminal collation runs the raw nominations through
	// the CANONICALIZATION component (a decoupled canonicalizer proposes clusters; the host records an
	// append-only, revision-hashed merge-ledger + enforces the surjectivity gate) before organizing them
	// into clusters + PROPOSED dimensions. Catalog is OBSERVE posture — it enumerates + organizes, it does
	// NOT rank — so it uses the single-canonicalizer foundation without the binding confirmation round.
	Catalog = "catalog"
	// Challenge (design §3) is an ADJUDICATIVE mode: explorers blindly ATTACK a supplied typed
	// ARTIFACT, the findings are canonicalized by TWO independent canonicalizers and CONFIRMED by the panel,
	// a collator-mediated cross-review round deepens or refutes them over the pooled confirmed-canonical
	// digest, and the terminal collation is a severity-triaged register whose corroboration is computed BY THE
	// HOST over the immutable blind round-1 artifacts. It is count-bearing, so it uses the full ranking-grade
	// policy (Dual + Confirm) — §3: confirmation covers EVERY count-bearing mode, not Shortlist alone.
	Challenge = "challenge"
	// Shortlist (design §3) is the ADJUDICATIVE mode that ends in a ranking: explorers blindly ENUMERATE candidates, the
	// universe is canonicalized (dual) and CONFIRMED, and the confirmed canonical IDs are then put to an
	// explicit BALLOT under decision inputs the host froze + hashed beforehand. The ranking is TALLIED BY THE
	// HOST — never asserted by a model — and rendered as an informed preference under shared framing (§4).
	Shortlist = "shortlist"
	// AICollab (design §11) is the shipped COMPOSITION: each agent shortlists its
	// OWN findings blind, the agents then CHALLENGE each other's findings through the collator-mediated
	// cross-review, and one terminal collation produces the severity-triaged register. It is a real registered
	// mode rather than a documentation example precisely so the claim can be tested end-to-end: it reuses
	// Challenge's schema, severity enum, later-round contract and collator UNCHANGED, and differs only in its
	// blind round-1 prompt and in not requiring a supplied artifact.
	AICollab = "ai-collab"
	// Compare (design §3) is a FIXED-SPACE mode: the user DECLARES the option set and the
	// criteria (each with a direction and a filter/dimension role) before any explorer speaks, every explorer
	// evaluates the same grid blind, and the host computes the consolidated matrix, the per-cell agreement,
	// the filter gate and the Pareto frontier itself. It runs ONE round with NO canonicalizer and NO
	// confirmation round — not as a shortcut but because a declared universe has nothing to resolve, which is
	// exactly what makes it the counterexample to the emergent-space modes (§0 F-B).
	Compare = "compare"
	// Forecast (design §3) is the other FIXED-SPACE mode and the most deterministic thing exploremesh
	// does: the user declares the estimation target, unit and horizon, each explorer returns a MACHINE-READABLE
	// numeric estimate + interval blind, and the HOST pools them under a declared, versioned rule with
	// dispersion and identified outliers. Like Compare it is single-round and canonicalizer-free.
	Forecast = "forecast"
)

// DefaultName is the mode used when a task names none (design §3/§7: Map is the default). An empty
// task mode resolves to this.
const DefaultName = Map

// Objective labels what the collator does after the explorer fan-out (design §3): collate-only for the
// observe-posture modes, and the select/compose, canonicalize+confirm, mediate, ballot and
// host-aggregate objectives for the generative, adjudicative and fixed-space modes.
type Objective string

const (
	// ObjectiveCollateOnly = the terminal collation synthesizes the blind round-1 envelopes into the
	// fixed CollatorOutput; it does NOT author the explorer schema and does NOT tally.
	ObjectiveCollateOnly Objective = "collate_only"
	// ObjectiveSelectCompose (Synthesize) = the terminal collation SELECTS the strongest candidate
	// answer and COMPOSES it by grafting superior elements from the others; it records provenance + a
	// minority report and does NOT tally.
	ObjectiveSelectCompose Objective = "select_compose"
	// ObjectiveCanonicalizeCollate (Catalog) = the terminal collation CANONICALIZES the raw
	// nominations (decoupled canonicalizer proposes clusters; host records the append-only merge-ledger +
	// enforces the surjectivity gate) then organizes them into clusters + PROPOSED dimensions. It does NOT
	// rank, so it uses the single-canonicalizer foundation; the ranking-grade layers (dual canonicalizer +
	// binding confirmation round) are opted into per mode via CanonicalizationPolicy.
	ObjectiveCanonicalizeCollate Objective = "canonicalize_collate"
	// ObjectiveCanonicalizeMediateCollate (Challenge + the ai-collab composition) = canonicalize+confirm
	// the blind findings, MEDIATE a cross-review round over the pooled confirmed-canonical digest, then collate
	// into a severity-triaged register whose corroboration is HOST-computed over the blind round-1 baseline
	// (design §3 Challenge row).
	ObjectiveCanonicalizeMediateCollate Objective = "canonicalize_mediate_collate"
	// ObjectiveCanonicalizeBallotCollate (Shortlist) = canonicalize+confirm the candidate universe, put
	// the CONFIRMED canonical IDs to an explicit BALLOT under frozen+hashed decision inputs, then collate the
	// HOST tally into a ranked subset (design §3 Shortlist row / §4). The collator never ranks.
	ObjectiveCanonicalizeBallotCollate Objective = "canonicalize_ballot_collate"
	// ObjectiveHostAggregateCollate (Compare + Forecast) = the FIXED-SPACE objective: the HOST computes
	// the entire result (matrix + agreement + Pareto, or the pooled estimate + dispersion + outliers) from the
	// blind round-1 artifacts BEFORE the collator is called, and the terminal collation contributes NARRATIVE
	// only. It is a distinct objective precisely because the collator's role shrinks to prose here — nothing
	// it returns can reach a governance value (design §3 Compare/Forecast rows).
	ObjectiveHostAggregateCollate Objective = "host_aggregate_collate"
)

// ModeSpec is a mode's app-owned behavior contract (design §3). It is a value type registered once at
// package init; the pipeline resolves it from a task's mode and branches on FormulationFree.
type ModeSpec struct {
	// Name is the mode's stable identifier (e.g. "map").
	Name string
	// FormulationFree reports whether round-1 SKIPS the collator formulate call: explorers receive the
	// app-owned Prompt + are validated against ExplorerSchema (NOT a collator-authored schema). When
	// false, the pipeline runs the collator-formulated path instead.
	FormulationFree bool
	// Prompt builds the deterministic, app-owned explorer prompt from the raw task (formulation-free
	// modes only — it is the app's analogue of the collator's formulate output, preserving intent).
	Prompt func(schema.RawTask) string
	// ExplorerSchema returns the FIXED, app-owned explorer-response schema (a fresh copy per call).
	// For a formulation-free mode this is the schema explorers are validated against — the collator
	// never authors it (design §0 F-A: the common cause is removed for fixed-schema modes).
	ExplorerSchema func() schema.Schema
	// Objective is the collator's role after the fan-out (design §3).
	Objective Objective
	// Collator is the mode's app-owned PLAIN terminal-collation contract (design §3): it renders the
	// collator prompt (with the exact output structure spelled out) and parses/validates the collator's
	// raw output into the mode's own ModeOutput. The pipeline resolves it by mode to run the collate step.
	// It is nil for a CANONICALIZING mode (Catalog), which sets Canonicalizing instead.
	Collator CollatorContract
	// Canonicalizing is the mode's app-owned CANONICALIZING terminal-collation contract (design §3/§4):
	// a mode whose terminal step runs the raw explorer nominations through the canonicalization
	// component (append-only ledger + surjectivity gate) rather than a plain collate call. When set, the
	// pipeline runs the canonicalizing collate path and IGNORES Collator (they are mutually exclusive —
	// exactly one is set per mode). nil for plain-collate modes (Map, Synthesize).
	Canonicalizing CanonicalizingContract
	// FixedSpace is the mode's app-owned FIXED-SPACE terminal contract (design §3 Compare/Forecast rows):
	// a mode whose option set / estimation target is DECLARED by the user before the fan-out, so the host can
	// compute the whole result from the blind round-1 artifacts and the collator only narrates it. When set,
	// the pipeline runs the fixed-space path and IGNORES Collator + Canonicalizing — a mode sets EXACTLY ONE
	// of the three, and the pipeline refuses a mode that sets none. nil for every emergent-space mode.
	FixedSpace FixedSpaceContract
	// Canonicalization is the mode's RANKING-GRADE governance policy over the canonicalization step (design
	// §0 F-B / §4). Its ZERO VALUE is one decoupled canonicalizer and no confirmation round — what the
	// observe-posture Catalog mode uses. A count/ballot-bearing mode (Challenge/Shortlist) sets
	// Dual + Confirm.
	Canonicalization CanonicalizationPolicy
	// Rounds is the FIXED number of explorer rounds (design §1). 0/1 = the single blind round every mode
	// registered so far uses. >1 requires LaterRound and drives the pipeline's multi-round path; the count is
	// FIXED by this contract (there is deliberately no data-dependent termination rule) and bounded by
	// round.MaxRounds.
	Rounds int
	// LaterRound is the mode's app-owned contract for rounds 2..Rounds (design §6). Required when Rounds > 1;
	// nil otherwise.
	LaterRound LaterRoundContract
	// Ballot is the mode's app-owned BALLOT contract (design §3 Shortlist / §4), set ONLY by a mode whose
	// later round solicits an explicit ballot over the CONFIRMED canonical IDs. When it is set the pipeline
	// FREEZES + HASHES the decision inputs BEFORE dispatching that round and TALLIES the returned ballots
	// itself — the contract declares the criteria and parses a ballot, and never produces a ranking. nil for
	// every other mode, which is why no other mode can accidentally acquire a decision.
	Ballot BallotContract
	// ValidateTask is the mode's app-owned check on the USER TASK, run BEFORE any spend (design §3). A mode
	// whose explorer round needs an input the base RawTask does not require — Challenge's ARTIFACT under
	// review — states it here, so every surface rejects a missing input with the same message and a panel is
	// never paid for a task the mode cannot run. nil (the common case) means the base RawTask.Validate is
	// sufficient.
	ValidateTask func(schema.RawTask) error
	// Class is the mode's SPACE class (design §1) — it selects the honest DEGRADED terminal artifact when the
	// collator is lost after the fan-out. An unset Class is treated as EmergentSpace, the conservative
	// default: assuming a mode's space is fixed when it is not would license the host to group free-form
	// claims, which is covert entity resolution.
	Class schema.ModeClass
	// FixedSpaceKeyField is the response field a FIXED-space mode's degraded host register is keyed on — the
	// field whose value universe was GIVEN to the explorers (e.g. Compare's option name). Required when Class
	// is FixedSpace, ignored otherwise.
	FixedSpaceKeyField string
}

// CanonicalizationPolicy is a mode's governance policy over the canonicalization step (design §0 F-B / §4).
// Both flags default false, so a mode that says nothing gets the single-canonicalizer foundation.
type CanonicalizationPolicy struct {
	// Dual runs TWO INDEPENDENT canonicalizer calls and keeps only merges BOTH propose (contested → SPLIT).
	// It is the ranking-grade partition: costlier, and the only one honest enough to carry a count or a ballot.
	Dual bool
	// Confirm runs the BINDING confirmation round after the (provisional) partition: typed explorer challenges
	// resolved by a versioned HOST rule into a NEW append-only ledger revision. Per §3 it covers EVERY
	// count-bearing mode — giving one of them a guard the others lack is the asymmetry to avoid.
	Confirm bool
}

// ModeClass returns the mode's effective space class, defaulting an unset Class to EmergentSpace (the
// conservative direction, see the Class field).
func (s ModeSpec) ModeClass() schema.ModeClass {
	if s.Class == schema.FixedSpace {
		return schema.FixedSpace
	}
	return schema.EmergentSpace
}

// RoundCount returns the mode's FIXED explorer-round count, normalized and bounded by round.MaxRounds
// (design §1). An over-large contract is an error, never a clamp.
func (s ModeSpec) RoundCount() (int, error) { return round.Count(s.Rounds) }

// CheckTask runs the mode's app-owned task validation (ValidateTask), if it has one. Every surface and the
// pipeline call it BEFORE any model call, so a mode-specific missing input costs zero tokens and produces the
// same message everywhere.
func (s ModeSpec) CheckTask(raw schema.RawTask) error {
	if s.ValidateTask == nil {
		return nil
	}
	return s.ValidateTask(raw)
}

// ValidateTask resolves a mode by name and runs its app-owned task check — the one-call form the surfaces
// use, so none of them has to know which modes have extra requirements. An unknown mode is NOT an error here
// (the surfaces reject an unknown mode against the registry with their own message first).
func ValidateTask(name string, raw schema.RawTask) error {
	spec, ok := Resolve(name)
	if !ok {
		return nil
	}
	return spec.CheckTask(raw)
}

// LaterRoundContract is a mode's app-owned contract for an explorer round AFTER the blind round 1 (design
// §6). The host — never the mode — decides HOW the prior artifact is framed (delimited, "treat as data",
// size-capped): the contract only declares what it ACCEPTS and what its own prompt/schema are, so no mode can
// weaken the untrusted-data framing.
type LaterRoundContract interface {
	// Accepts declares the artifact kinds + schema version this round consumes. The host validates the
	// producing artifact against it BEFORE the fan-out, so an incompatible edge costs zero tokens.
	Accepts() round.Accepts
	// Prompt builds the round's explorer instruction from the raw task and the ALREADY-FRAMED untrusted-data
	// block (the host renders the block via Artifact.RenderAsUntrustedData and passes the text in). A contract
	// embeds it verbatim; it never re-frames or re-labels it.
	Prompt(raw schema.RawTask, untrustedDataBlock string) string
	// ExplorerSchema is the round's FIXED, app-owned response schema (never collator-authored, §5).
	ExplorerSchema() schema.Schema
}

// registry is the package-global mode registry, keyed by mode name. It is populated at init and read
// concurrently thereafter (never mutated at runtime), so it needs no lock.
var registry = map[string]ModeSpec{}

// register adds a spec to the registry (init-time only). It panics on a duplicate name — a programming
// error, caught at startup.
func register(s ModeSpec) {
	if _, dup := registry[s.Name]; dup {
		panic("mode: duplicate registration for " + s.Name)
	}
	registry[s.Name] = s
}

func init() {
	// Map (design §3): formulation-free; explorers get the deterministic app-owned prompt + the fixed
	// minimum schema; the collator collates only.
	// Class is EMERGENT-space: an explorer authors its own claims, so any cross-explorer grouping of them is
	// entity resolution (§0 F-B) — which is why Map's degraded artifact is the raw attributed envelopes plus a
	// mechanical claim index, never a host-synthesized disagreement register.
	register(ModeSpec{
		Name:            Map,
		FormulationFree: true,
		Prompt:          schema.DefaultPrompt,
		ExplorerSchema:  schema.MinimumSchema,
		Objective:       ObjectiveCollateOnly,
		Collator:        mapCollator{},
		Class:           schema.EmergentSpace,
	})
	// Synthesize (design §3): formulation-free like Map — the app owns BOTH the explorer prompt
	// ("your single best COMPLETE answer + rationale + assumptions") and the explorer schema, so the
	// collator authors no round-1 schema (§5). The collator then selects/composes the strongest answer.
	register(ModeSpec{
		Name:            Synthesize,
		FormulationFree: true,
		Prompt:          schema.SynthesizeExplorerPrompt,
		ExplorerSchema:  schema.SynthesizeExplorerSchema,
		Objective:       ObjectiveSelectCompose,
		Collator:        synthesizeCollator{},
		Class:           schema.EmergentSpace, // candidate answers are explorer-authored (see Map)
	})
	// Catalog (design §3/§4): formulation-free like Map/Synthesize — the app owns BOTH the explorer
	// prompt ("enumerate broadly") and schema (candidates[]), so the collator authors no round-1 schema
	// (§5). The terminal collation is CANONICALIZING (Canonicalizing, not Collator): the pipeline runs the
	// raw nominations through the decoupled canonicalizer + append-only ledger + surjectivity gate, then
	// organizes them into clusters + PROPOSED dimensions.
	register(ModeSpec{
		Name:            Catalog,
		FormulationFree: true,
		Prompt:          schema.CatalogExplorerPrompt,
		ExplorerSchema:  schema.CatalogExplorerSchema,
		Objective:       ObjectiveCanonicalizeCollate,
		Canonicalizing:  catalogCollator{},
		Class:           schema.EmergentSpace, // the candidate universe is explorer-authored (see Map)
		// Canonicalization stays the ZERO policy: single canonicalizer, NO confirmation round. Catalog is
		// observe posture — it enumerates + organizes and does NOT rank — so it needs the single-canonicalizer
		// foundation only, and deliberately does not take the ranking-grade layers.
	})
}

// Lookup returns the ModeSpec registered under name (exact match — an empty or unknown name yields
// ok=false; callers default an empty task mode to DefaultName first, see Resolve). ModeSpec is a value
// with function fields, so the returned copy shares the same (immutable) behavior.
func Lookup(name string) (ModeSpec, bool) {
	s, ok := registry[name]
	return s, ok
}

// Resolve returns the ModeSpec for a task's mode string, defaulting an EMPTY mode to DefaultName
// (design §3). A non-empty unknown mode yields ok=false — the surfaces reject it with a message
// listing Names().
func Resolve(name string) (ModeSpec, bool) {
	if name == "" {
		name = DefaultName
	}
	return Lookup(name)
}

// Names returns the registered mode names in sorted order — for a stable "known modes: …" error
// message and any listing.
func Names() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
