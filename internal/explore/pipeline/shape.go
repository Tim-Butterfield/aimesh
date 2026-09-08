package pipeline

// This file answers the question a DRY RUN exists to answer, before anything is spent: what would this
// exploration do, and what would those calls carry?
//
// WHERE THE STOP GOES, AND WHY IT IS NOT WHERE REVIEW'S IS. reviewmesh stops after its static preflight,
// because that preflight is free: it reads config and asks each adapter whether it is Available, so a
// review's shape can promise that every adapter answered. exploremesh's first stage is the identity
// PRE-FLIGHT (design §1), and that stage INVOKES — one real call per governed role. So the honest stop is
// BEFORE it, and the guarantee is correspondingly weaker: a shape says nothing left between here and the
// first call is a CONFIGURATION question, and it does NOT say the collator answered. Moving the stop after
// the pre-flight to strengthen the claim would mean a "dry" run that spent, which is the one thing the flag
// promises it will not do.
//
// Everything that is free still runs first, so a dry run is worth more than a printout: the task contract,
// the round contract, the terminal-contract count, the formulation-free check, the collator's registration
// — and canonicalizer DERIVATION, which is the config error most worth catching here, because "every panel
// member shares the collator's model" is invisible until something tries to pick two independent ones.

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Shape is WHAT AN EXPLORATION WOULD DO, disclosed before the first model call. It is produced by a dry
// run (Options.DryRun) and by nothing else.
//
// THE CALL COUNT IS ONE NUMBER, NOT A RANGE, AND THAT IS A PROPERTY OF THE DESIGN RATHER THAN A
// SIMPLIFICATION. A review's shape reports a min and a max because a review ITERATES: its outer cycles
// stop when adjudication converges, so the figure depends on model behaviour aimesh cannot predict. An
// exploration has no such rule — the round count is FIXED by the mode contract (round.Count refuses an
// over-large one rather than clamping it, and design §1 rules out a data-dependent termination rule
// precisely because an aggressively-merging canonicalizer could steer it). Every remaining multiplier —
// panel size, dual canonicalization, the confirmation round — is settled before the run starts. So the
// count here is exact for a run that completes, and a range would be false precision in the other
// direction.
//
// WHAT IT DOES NOT COUNT: an exploration that HALTS makes fewer calls than this (a halt is the only way to
// spend less), and a dropped explorer that never reached its adapter — an unregistered adapter, a scratch
// directory that could not be created — costs nothing. Neither is a discount anyone should plan around.
type Shape struct {
	// Mode is the app-owned mode contract this run would execute (design §3).
	Mode string `json:"mode"`
	// Explorers is the panel in ATTRIBUTION order — the order everything recorded about the run is built
	// from. Every one of them is called in every round; MaxParallel bounds only how many at once, so
	// shortening this list is the only thing that removes an explorer call.
	Explorers []schema.ExplorerIdentity `json:"explorers"`
	// Collator is the identity that preflights, and that makes the terminal call on the paths that have
	// one. On the canonicalizing path it is also the default source of canonicalizer slot a.
	Collator schema.ExplorerIdentity `json:"collator"`
	// Canonicalizers are the resolved canonicalizer roles (one, or two on the dual path), empty for a
	// non-canonicalizing mode. They are resolved HERE, at shape time, by the same function the run uses —
	// so a plan that cannot yield two independent identities fails in the dry run, for free.
	Canonicalizers []ShapeRole `json:"canonicalizers,omitempty"`
	// CanonicalizerProvenance records who DECIDED the canonicalizer identities: ProvenanceExplicit (a human
	// named them) or ProvenanceDerived (the host picked them). Empty for a non-canonicalizing mode.
	CanonicalizerProvenance string `json:"canonicalizerProvenance,omitempty"`
	// CanonicalizerIndependence says what that choice BOUGHT — `distinct_models`, or `shared_model` when
	// both slots run one model behind two adapters, which makes every merge-agreement on the run weaker
	// evidence. Disclosing it HERE is the most useful moment it can be said: before the panel is paid for,
	// while changing the pair still costs nothing.
	CanonicalizerIndependence string `json:"canonicalizerIndependence,omitempty"`
	// Rounds is the mode contract's FIXED explorer-round count.
	Rounds int `json:"rounds"`
	// Policy is the mode's governance grade — the flags that decide which of the middle stages run, and
	// therefore most of what this run costs.
	Policy ShapePolicy `json:"policy"`
	// MaxParallel is the requested parallelism bound; 0 runs the whole panel at once. It changes how long
	// the run takes and never how much it costs.
	MaxParallel int `json:"maxParallel"`
	// ModelCalls is the total of every group in Calls: what a completing run invokes, exactly.
	ModelCalls int `json:"modelCalls"`
	// Calls is the per-stage breakdown in EXECUTION order, including the stages that cost nothing —
	// a terminal collation the host composes in-process is a fact a reader pricing the run needs, and it
	// is invisible in a total.
	Calls []ShapeCall `json:"calls"`
	// Payload is what the blind round would carry: the exact prompt bytes, already assembled.
	Payload ShapePayload `json:"payload"`
}

// ShapePolicy is the mode contract's declared governance grade (design §4). Every field is fixed by the
// mode, never by the run, which is why the whole cost is knowable in advance.
type ShapePolicy struct {
	// Dual runs TWO independent canonicalizer proposals instead of one.
	Dual bool `json:"dual"`
	// Confirm adds the binding confirmation round — one call PER EXPLORER, so on a large panel it is the
	// single most expensive flag in this struct.
	Confirm bool `json:"confirm"`
	// Ballot marks a ballot-bearing mode: its later rounds are dispatched under the frozen decision inputs
	// and recorded in the ballot phase rather than the explore phase. It adds no calls of its own.
	Ballot bool `json:"ballot"`
	// Terminal names which of the three mutually-exclusive terminal contracts the mode declares:
	// collate (a collator model call), canonicalize (composed by the host, no model call) or fixed_space
	// (a narrative-only collator call over numbers the host already computed).
	Terminal string `json:"terminal"`
}

// Terminal contract labels for ShapePolicy.Terminal.
const (
	TerminalCollate       = "collate"
	TerminalCanonicalize  = "canonicalize"
	TerminalFixedSpace    = "fixed_space"
	terminalContractCount = 1 // a mode declares exactly one; RunSpec enforces it before a shape is built
)

// ShapeRole is one non-explorer role a run would call, with the identity it resolved to.
type ShapeRole struct {
	Role     string                  `json:"role"`
	Identity schema.ExplorerIdentity `json:"identity"`
}

// ShapeCall is one stage's model-call cost. Calls may be 0: a stage the host performs in-process is
// listed precisely so a reader can see that it is free rather than wonder whether it was forgotten.
type ShapeCall struct {
	// Phase is the adapter-facing phase label the calls would carry (preflight / explore / canonicalize /
	// confirm / ballot / synthesize) — the same string that lands in a run record, so a shape and a
	// recording of the run it described are readable side by side.
	Phase string `json:"phase"`
	// Round is the explorer round number for a fan-out stage, 0 for everything else.
	Round int `json:"round,omitempty"`
	// Role is who is called: explorer, collator, canonicalizer, or the governed roles collectively.
	Role string `json:"role"`
	// Calls is how many model invocations this stage makes.
	Calls int `json:"calls"`
	// Detail says what the stage is for, in the terms a reader deciding whether to pay for it needs.
	Detail string `json:"detail"`
}

// ShapePayload is WHAT THE CALLS WOULD CARRY.
//
// It is the exploration analogue of a review's file list, and it is a stronger disclosure than that one:
// exploremesh reviews nothing on disk, so the payload is not a sample of a tree that might change — it is
// the exact bytes, assembled by the same app-owned formulation the run would use, hashed with the same
// function. A dry run and the run it describes produce the identical PayloadHash for the identical task.
//
// It describes ROUND 1 ONLY, and that is not an omission that can be repaired. Rounds 2..N carry the
// pooled confirmed-canonical uniques, which are derived from what the panel says in round 1 — they do not
// exist yet, and any figure for them here would be invented.
type ShapePayload struct {
	// Prompt is the full round-1 explorer prompt, byte-identical for every explorer (§6.2). It is included
	// whole rather than summarized because the question it answers — "is this actually the exploration I
	// meant?" — cannot be answered from a byte count.
	Prompt string `json:"prompt"`
	// PromptBytes is its length, sent once per explorer per round.
	PromptBytes int `json:"promptBytes"`
	// PayloadHash is the audit fingerprint of the canonical payload (prompt + explorer schema) — the same
	// hash the run records as proof every explorer received identical bytes.
	PayloadHash string `json:"payloadHash"`
	// SchemaFields are the response fields the app-owned explorer schema requires, in schema order. The
	// collator never authors this schema (§5), which is why it can be shown before the run.
	SchemaFields []string `json:"schemaFields"`
}

// shapeOf builds the disclosure from the resolved plan, task and mode contract. It is pure — it resolves,
// derives and assembles, and calls nothing — so every error it returns is a configuration error a real run
// would have hit later at the same point, for money.
//
// rounds is the already-validated round count (RunSpec resolves it before the stop, so a bad round contract
// is reported as itself rather than as a strange call total).
func shapeOf(plan roster.Plan, raw schema.RawTask, spec mode.ModeSpec, opts Options, rounds int) (Shape, error) {
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

	// Canonicalizer derivation, run for real: the same call preflightRoles makes, and the reason a dry run
	// of a dual-canonicalizer mode is worth more than reading the config.
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

	// The payload, assembled by the SAME app-owned formulation the run uses. Duplicated work rather than
	// hoisted state: it is pure and cheap, and building it twice from one function is what guarantees the
	// dry run discloses the bytes the real run sends.
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
	for _, c := range sh.Calls {
		sh.ModelCalls += c.Calls
	}
	return sh, nil
}

// shapeCalls enumerates the run's stages in execution order. It mirrors the six invocation sites in this
// package one-for-one — preflight (robustness.go), the explorer fan-out (pipeline.go), the canonicalizer
// proposal (pipeline.go), the confirmation round (governed.go), the plain collate (pipeline.go) and the
// fixed-space narrative (fixedspace.go) — because a cost disclosure derived from a reading of the design
// rather than from the call sites is a disclosure that drifts.
func shapeCalls(spec mode.ModeSpec, explorers, canonicalizers, rounds int) []ShapeCall {
	var out []ShapeCall

	// PRE-FLIGHT: one probe per governed role. This is exploremesh's FIRST SPEND and the reason the dry-run
	// stop sits before it rather than after.
	roles := "the collator"
	if canonicalizers > 0 {
		roles = "the collator and each canonicalizer"
	}
	out = append(out, ShapeCall{
		Phase: schema.PhasePreflight, Role: "governed roles", Calls: 1 + canonicalizers,
		Detail: "one cheap probe per governed role (" + roles + ") before the fan-out — this is the FIRST call a real run makes, and the dry run stops in front of it",
	})

	// The BLIND round: every explorer, identical bytes.
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

	// Rounds 2..N. A ballot-bearing mode dispatches EVERY later round under the frozen decision inputs, so
	// they carry the ballot phase — not only the last one.
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

	// The terminal stage, including the case that costs nothing.
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

// terminalOf names the mode's declared terminal contract. RunSpec has already refused a mode that declares
// anything other than exactly one, so the fallthrough is unreachable in a shape and says so rather than
// inventing a label.
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
