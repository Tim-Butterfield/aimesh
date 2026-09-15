package pipeline

// This file holds the checks that keep an exploration from discovering a governance problem only after
// the explorer fan-out has been paid for:
//
//   - Identity pre-flight: the collator and every canonicalizer are probed with a cheap call before any
//     explorer runs.
//   - Same identity: a role's resolved model is pinned at its first call, and a later call that resolves
//     to a different model halts the run, because artifacts from two models cannot be combined.
//   - Frozen panel: membership and the counting policy are fixed and hashed before any judgment is
//     solicited (package govern owns the arithmetic; the pipeline owns the freezing moment).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// preflightPrompt is the reachability probe body. Its reply is never parsed; the call only has to succeed.
const preflightPrompt = "Pre-flight check. Reply with ONLY this JSON object and nothing else: {\"ok\":true}"

// PreflightRecord is one role's pre-flight verdict, persisted with the run. Status and Caveat describe
// the identity evidence; neither can fail the pre-flight.
type PreflightRecord struct {
	Role          string                  `json:"role"`
	Requested     schema.ExplorerIdentity `json:"requested"`
	ResolvedModel string                  `json:"resolvedModel"`
	Status        schema.IdentityStatus   `json:"status"`
	Caveat        string                  `json:"caveat,omitempty"`
}

// preflight probes one role's adapter. It returns an error only when the adapter cannot be invoked; weak,
// unknown or mismatched identity evidence is recorded, never fatal, because identity does not decide
// whether a response is used.
func preflight(ctx context.Context, a model.Adapter, role string, id schema.ExplorerIdentity) (PreflightRecord, error) {
	rec := PreflightRecord{Role: role, Requested: id}
	work, cleanup, werr := isolatedWorkDir()
	if werr != nil {
		return rec, fault.Wrap(fault.Internal, "isolated work dir (preflight "+role+")", werr)
	}
	r, ierr := a.Invoke(ctx, model.Call{
		Role: role, Phase: schema.PhasePreflight,
		Model: id.Model, ModelArg: core.ModelArg(id.Model), Effort: id.Effort,
		WorkDir: work, Prompt: preflightPrompt,
	})
	cleanup()
	if ierr != nil {
		return rec, fault.Wrap(fault.Config, fmt.Sprintf("identity pre-flight for the %s failed before the fan-out (no explorer tokens were spent)", role), ierr)
	}
	status, _ := classify(a, r, id.Model, id.Adapter)
	rec.ResolvedModel, rec.Status = r.ActualModel, status
	rec.Caveat = collatorIdentityCaveat(status)
	return rec, nil
}

// identityLedger pins each role's resolved model across that role's calls in one exploration. Explorer
// calls run concurrently, so it is guarded by a mutex.
type identityLedger struct {
	mu    sync.Mutex
	first map[string]string
}

// newIdentityLedger returns an empty ledger.
func newIdentityLedger() *identityLedger { return &identityLedger{first: map[string]string{}} }

// observe records the model a role's call resolved to. The first observation pins the role; a later,
// different non-empty model returns a halt error. An empty resolved model is not evidence of a change, so
// it neither pins nor halts.
func (l *identityLedger) observe(role, resolved string) error {
	if resolved == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prev, seen := l.first[role]
	if !seen {
		l.first[role] = resolved
		return nil
	}
	if prev != resolved {
		return fault.New(fault.Config, fmt.Sprintf(
			"same-identity invariant: role %q resolved to model %q earlier in this exploration but to %q now — a mid-exploration fallback to a DIFFERENT model halts (artifacts from two different models cannot be honestly combined)",
			role, prev, resolved))
	}
	return nil
}

// canonicalizerIdentity names one canonicalizer's role label and identity. Labels are distinct per
// canonicalizer (`canonicalizer-a`, `canonicalizer-b`) so the same-identity check applies to each one
// separately; the pair is expected to differ.
type canonicalizerIdentity struct {
	role     string
	explorer roster.Explorer
}

// Canonicalizer provenance records who chose the identities that produced a partition. It is recorded on
// the Result and in the run manifest so a poor choice is visible from the run record.
const (
	// ProvenanceExplicit means the request or the profile named the identities.
	ProvenanceExplicit = "explicit"
	// ProvenanceDerived means the host derived them: slot a from the collator, slot b from the panel in
	// preference order.
	ProvenanceDerived = "derived"
)

// Canonicalizer independence records how different the two canonicalizers are, which bounds what their
// agreement is worth. It is separate from provenance: an explicit choice can also share a model.
const (
	// IndependenceDistinct means the two canonicalizers run different models.
	IndependenceDistinct = "distinct_models"
	// IndependenceSharedModel means both run the same model through different adapters or efforts. Their
	// errors are correlated, so agreement is weaker. It is recorded and warned about, never refused.
	IndependenceSharedModel = "shared_model"
)

// explicitCanonicalizers returns the canonicalizers named for a run: opts.Canonicalizers if set, else
// the plan's. The shape rule is re-checked here so a directly constructed Options cannot bypass it.
func explicitCanonicalizers(plan roster.Plan, opts Options) ([]roster.Explorer, error) {
	cs := opts.Canonicalizers
	if len(cs) == 0 {
		cs = plan.Canonicalizers
	}
	if err := roster.ValidateCanonicalizers(cs); err != nil {
		return nil, err
	}
	return cs, nil
}

// canonicalizerChoice is the resolved canonicalizer roles for one run, with their provenance and
// independence.
type canonicalizerChoice struct {
	ids          []canonicalizerIdentity
	provenance   string // ProvenanceExplicit | ProvenanceDerived
	independence string // IndependenceDistinct | IndependenceSharedModel; "" on the single-canonicalizer path
}

// canonicalizerIdentities resolves the canonicalizer identities for a run.
//
// Explicit identities are used as named: the dual path takes both, the single path takes the first.
// Otherwise the host derives them. The single path uses the collator's adapter and model in a separate
// call. The dual path takes slot a from the collator and slot b from the first panel member, in the
// author's preference order, whose adapter or model differs from the collator's.
//
// Slot b is read from plan.Preferred, not plan.Explorers: Explorers is sorted for attribution, and
// choosing from it would pick a canonicalizer alphabetically. If no panel member differs from the
// collator, the dual path halts rather than invent a second identity.
func canonicalizerIdentities(plan roster.Plan, dual bool, modeName string, opts Options) (canonicalizerChoice, error) {
	explicit, err := explicitCanonicalizers(plan, opts)
	if err != nil {
		return canonicalizerChoice{}, err
	}
	if len(explicit) == 2 {
		if !dual {
			return canonicalizerChoice{
				ids:        []canonicalizerIdentity{{role: "canonicalizer", explorer: explicit[0]}},
				provenance: ProvenanceExplicit,
			}, nil
		}
		return dualChoice(explicit[0], explicit[1], ProvenanceExplicit), nil
	}

	primary := canonicalizerIdentity{role: "canonicalizer", explorer: roster.Explorer(plan.Collator)}
	if !dual {
		return canonicalizerChoice{ids: []canonicalizerIdentity{primary}, provenance: ProvenanceDerived}, nil
	}
	// A plan without a preference order cannot be derived from; falling back to attribution order would
	// choose alphabetically. Every executable plan comes from Roster.SelectTopN, which records both.
	if !plan.SameSet() {
		return canonicalizerChoice{}, fault.New(fault.Internal, fmt.Sprintf(
			"canonicalizer derivation needs the panel in PREFERENCE order, but this plan carries %d explorer(s) in attribution order and %d in preference order — a plan not built by roster.SelectTopN cannot be derived from (using the attribution order would select canonicalizer-b alphabetically)",
			len(plan.Explorers), len(plan.Preferred)))
	}
	for _, ex := range plan.Preferred {
		if ex.Adapter == plan.Collator.Adapter && ex.Model == plan.Collator.Model {
			continue
		}
		return dualChoice(primary.explorer, ex, ProvenanceDerived), nil
	}
	return canonicalizerChoice{}, fault.New(fault.Config, fmt.Sprintf(
		"mode %q needs a SECOND canonicalizer identity for its dual merge-agreement rule, but every panel member is the collator's own adapter and model (%s / %s) — there is no distinct identity left to name, and inventing one would manufacture exactly the corroboration the dual rule exists to test. A same-model pair reached through DIFFERENT adapters is allowed and recorded as %s; this is not that case. THE ROSTER IS NOT WRONG ON ITS OWN — it is this roster WITH THIS MODE, and the same roster runs these modes unchanged: %s. Otherwise name a distinct canonicalizer (--canonicalizer adapter=…,model=… twice) or add an explorer whose adapter or model differs from the collator's",
		modeName, plan.Collator.Adapter, plan.Collator.Model, IndependenceSharedModel,
		strings.Join(singleCanonicalizerModes(), ", ")))
}

// singleCanonicalizerModes returns the sorted names of the modes that canonicalize with one identity. It
// reads the mode registry so the refusal above stays accurate when a mode is added.
func singleCanonicalizerModes() []string {
	var out []string
	for _, name := range mode.Names() {
		spec, ok := mode.Lookup(name)
		if !ok || spec.Canonicalization.Dual {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// dualChoice builds the two-slot choice and classifies its independence by model: two adapters reaching
// one model share that model's priors.
func dualChoice(a, b roster.Explorer, provenance string) canonicalizerChoice {
	independence := IndependenceDistinct
	if a.Model == b.Model {
		independence = IndependenceSharedModel
	}
	return canonicalizerChoice{
		ids: []canonicalizerIdentity{
			{role: "canonicalizer-a", explorer: a},
			{role: "canonicalizer-b", explorer: b},
		},
		provenance:   provenance,
		independence: independence,
	}
}
