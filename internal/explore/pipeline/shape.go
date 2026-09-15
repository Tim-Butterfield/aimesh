package pipeline

// This file builds a dry run's Shape: what an exploration would do and what its calls would carry.
//
// The dry run stops before the identity pre-flight, which is the first model call. Every check that is
// free still runs first, including canonicalizer derivation, so configuration errors surface without
// spending. A shape does not claim that any adapter answered.

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Shape describes what an exploration would do, produced only by a dry run.
//
// ModelCalls is exact for a run that completes: the round count and every other multiplier are fixed by
// the mode contract before the run starts. A run that halts, or an explorer dropped before its call,
// costs less.
type Shape struct {
	// Mode is the mode the run would execute.
	Mode string `json:"mode"`
	// Explorers is the panel in attribution order. Every explorer is called in every round.
	Explorers []schema.ExplorerIdentity `json:"explorers"`
	// Collator is the identity that is pre-flighted and makes the terminal call where there is one. It is
	// also the default source of canonicalizer slot a.
	Collator schema.ExplorerIdentity `json:"collator"`
	// Canonicalizers are the resolved canonicalizer roles, empty for a non-canonicalizing mode.
	Canonicalizers []ShapeRole `json:"canonicalizers,omitempty"`
	// CanonicalizerProvenance is ProvenanceExplicit or ProvenanceDerived; empty for a non-canonicalizing mode.
	CanonicalizerProvenance string `json:"canonicalizerProvenance,omitempty"`
	// CanonicalizerIndependence is IndependenceDistinct or IndependenceSharedModel on the dual path.
	CanonicalizerIndependence string `json:"canonicalizerIndependence,omitempty"`
	// Rounds is the mode's fixed explorer-round count.
	Rounds int `json:"rounds"`
	// Policy is the mode's governance settings, which decide which middle stages run.
	Policy ShapePolicy `json:"policy"`
	// MaxParallel is the requested parallelism bound; 0 runs the whole panel at once. It affects duration,
	// not cost.
	MaxParallel int `json:"maxParallel"`
	// ModelCalls is the sum of Calls.
	ModelCalls int `json:"modelCalls"`
	// Calls is the per-stage breakdown in execution order, including stages that make no model call.
	Calls []ShapeCall `json:"calls"`
	// Payload is what the blind round would send.
	Payload ShapePayload `json:"payload"`
}

// ShapePolicy is the mode contract's governance settings.
type ShapePolicy struct {
	// Dual runs two independent canonicalizer proposals instead of one.
	Dual bool `json:"dual"`
	// Confirm adds a confirmation round with one call per explorer.
	Confirm bool `json:"confirm"`
	// Ballot marks a ballot-bearing mode, whose later rounds run in the ballot phase. It adds no calls.
	Ballot bool `json:"ballot"`
	// Terminal is the mode's terminal contract: TerminalCollate, TerminalCanonicalize or TerminalFixedSpace.
	Terminal string `json:"terminal"`
}

// Terminal contract labels for ShapePolicy.Terminal.
const (
	TerminalCollate       = "collate"
	TerminalCanonicalize  = "canonicalize"
	TerminalFixedSpace    = "fixed_space"
	terminalContractCount = 1
)

// ShapeRole is a non-explorer role a run would call, with its resolved identity.
type ShapeRole struct {
	Role     string                  `json:"role"`
	Identity schema.ExplorerIdentity `json:"identity"`
}

// ShapeCall is one stage's model-call count. Stages the host performs in-process are listed with 0 calls.
type ShapeCall struct {
	// Phase is the phase label the calls carry, matching the run record.
	Phase string `json:"phase"`
	// Round is the explorer round for a fan-out stage, 0 otherwise.
	Round int `json:"round,omitempty"`
	// Role is who is called.
	Role string `json:"role"`
	// Calls is the number of model invocations in this stage.
	Calls int `json:"calls"`
	// Detail describes the stage.
	Detail string `json:"detail"`
}

// ShapePayload is the round-1 payload. It is built and hashed by the same code the run uses, so a dry run
// and the run it describes have the same PayloadHash. Later rounds depend on round-1 answers and are not
// described.
type ShapePayload struct {
	// Prompt is the full round-1 explorer prompt, identical for every explorer.
	Prompt string `json:"prompt"`
	// PromptBytes is the prompt's length, sent once per explorer per round.
	PromptBytes int `json:"promptBytes"`
	// PayloadHash is the hash of the prompt and explorer schema that the run records.
	PayloadHash string `json:"payloadHash"`
	// SchemaFields are the response fields the explorer schema requires, in schema order.
	SchemaFields []string `json:"schemaFields"`
}

// shapeOf builds the Shape for a run without calling any model. rounds is the validated round count and
// probes is the number of readiness calls (0 when readiness checking is off).
func shapeOf(plan roster.Plan, raw schema.RawTask, spec mode.ModeSpec, opts Options, rounds, probes int) (Shape, error) {
	sh := Shape{
		Mode:        spec.Name,
		Collator:    plan.Collator.Identity(),
		Rounds:      rounds,
		MaxParallel: opts.MaxParallel,
		Policy: ShapePolicy{
			Dual:     spec.Canonicalization.Dual,
			Confirm:  spec.Canonicalization.Confirm,
			Ballot:   spec.Ballot != nil,
			Terminal: terminalOf(spec),
		},
	}
	sh.Explorers = panelIdentities(plan)

	if spec.Canonicalizing != nil {
		choice, err := canonicalizerIdentities(plan, spec.Canonicalization.Dual, spec.Name, opts)
		if err != nil {
			return Shape{}, err
		}
		sh.CanonicalizerProvenance = choice.provenance
		sh.CanonicalizerIndependence = choice.independence
		for _, c := range choice.ids {
			sh.Canonicalizers = append(sh.Canonicalizers, ShapeRole{Role: c.role, Identity: c.explorer.Identity()})
		}
	}

	payload := schema.FormulationFree(schema.FormulationFreeMap, spec.Prompt(raw), spec.ExplorerSchema()).Payload
	hash, herr := payload.Hash()
	if herr != nil {
		return Shape{}, fault.Wrap(fault.Internal, "shape: hash the explorer payload", herr)
	}
	sh.Payload = ShapePayload{
		Prompt:      payload.FinalPrompt,
		PromptBytes: len(payload.FinalPrompt),
		PayloadHash: hash,
	}
	for _, f := range payload.ExpandedSchema.Fields {
		sh.Payload.SchemaFields = append(sh.Payload.SchemaFields, f.Name)
	}

	sh.Calls = shapeCalls(spec, len(plan.Explorers), len(sh.Canonicalizers), rounds)
	if probes > 0 {
		readiness := ShapeCall{Phase: "readiness", Role: "adapters", Calls: probes,
			Detail: "one bounded deep-probe call per distinct agent the panel names, before anything else is invoked (verifyReadiness)"}
		sh.Calls = append([]ShapeCall{readiness}, sh.Calls...)
	}
	for _, c := range sh.Calls {
		sh.ModelCalls += c.Calls
	}
	return sh, nil
}

// shapeCalls lists the run's stages in execution order. Keep it in step with the model call sites:
// preflight (robustness.go), the explorer fan-out and canonicalizer proposal (pipeline.go), the
// confirmation round (governed.go), the plain collate (pipeline.go) and the fixed-space narrative
// (fixedspace.go).
func shapeCalls(spec mode.ModeSpec, explorers, canonicalizers, rounds int) []ShapeCall {
	var out []ShapeCall

	roles := "the collator"
	if canonicalizers > 0 {
		roles = "the collator and each canonicalizer"
	}
	out = append(out, ShapeCall{
		Phase: schema.PhasePreflight, Role: "governed roles", Calls: 1 + canonicalizers,
		Detail: "one cheap probe per governed role (" + roles + ") before the fan-out — this is the FIRST call a real run makes, and the dry run stops in front of it",
	})

	out = append(out, ShapeCall{
		Phase: schema.PhaseExplore, Round: 1, Role: "explorer", Calls: explorers,
		Detail: "the blind round — every explorer receives the byte-identical payload below",
	})

	if spec.Canonicalizing != nil {
		detail := "one canonicalizer proposes the partition over the panel's raw nominations"
		if spec.Canonicalization.Dual {
			detail = "TWO independent canonicalizers propose partitions; only merges BOTH propose survive"
		}
		out = append(out, ShapeCall{
			Phase: schema.PhaseCanonicalize, Role: "canonicalizer", Calls: canonicalizers, Detail: detail,
		})
	}
	if spec.Canonicalization.Confirm {
		out = append(out, ShapeCall{
			Phase: schema.PhaseConfirm, Role: "explorer", Calls: explorers,
			Detail: "the binding confirmation round — every explorer is shown the provisional partition and may challenge it; a silent explorer is recorded as raising no objection, never retried",
		})
	}

	// A ballot-bearing mode runs every later round in the ballot phase.
	phase := schema.PhaseExplore
	kind := "the mediated cross-review round — the pooled confirmed-canonical uniques as untrusted data"
	if spec.Ballot != nil {
		phase = schema.PhaseBallot
		kind = "a ballot round over the confirmed canonical IDs, under decision inputs the host froze and hashed first"
	}
	for k := 2; k <= rounds; k++ {
		out = append(out, ShapeCall{
			Phase: phase, Round: k, Role: "explorer", Calls: explorers, Detail: kind,
		})
	}

	switch {
	case spec.Canonicalizing != nil:
		out = append(out, ShapeCall{
			Phase: schema.PhaseSynthesize, Role: "host", Calls: 0,
			Detail: "the terminal collation is composed IN-PROCESS from the confirmed partition by the mode's own contract — no model call, and no model is asked to produce a count or a rank",
		})
	case spec.FixedSpace != nil:
		out = append(out, ShapeCall{
			Phase: schema.PhaseSynthesize, Role: "collator", Calls: 1,
			Detail: "narrative only — the host computes the whole result from the blind responses BEFORE this call, so nothing the collator returns can reach a governance value",
		})
	default:
		out = append(out, ShapeCall{
			Phase: schema.PhaseSynthesize, Role: "collator", Calls: 1,
			Detail: "the terminal collation over the blind explorer responses",
		})
	}
	return out
}

// terminalOf returns the label of spec's terminal contract. RunSpec has already rejected a spec without
// exactly one, so the final return is unreachable in practice.
func terminalOf(spec mode.ModeSpec) string {
	switch {
	case spec.Canonicalizing != nil:
		return TerminalCanonicalize
	case spec.FixedSpace != nil:
		return TerminalFixedSpace
	case spec.Collator != nil:
		return TerminalCollate
	}
	return fmt.Sprintf("unknown (a mode must declare exactly %d terminal contract)", terminalContractCount)
}
