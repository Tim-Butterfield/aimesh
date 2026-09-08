package pipeline

// This file holds the ROBUSTNESS INVARIANTS (design §1). All three exist because the expensive part of
// an exploration is the explorer fan-out, and every one of them is about not discovering a governance
// problem after paying for it:
//
//   - IDENTITY PRE-FLIGHT BEFORE THE FAN-OUT. The collator and every canonicalizer are probed with a cheap
//     call first. A provider that silently fell back to a different model is then caught before a single
//     explorer token is spent, instead of at the collate call with a full panel already paid for.
//   - THE SAME-IDENTITY INVARIANT. A role's resolved model is pinned at its first call and every later call
//     of that role must match it. A mid-exploration fallback to a DIFFERENT model HALTS: half the run
//     governed by one model and half by another is not a run with a caveat, it is two partial runs whose
//     artifacts cannot be honestly combined.
//   - THE FROZEN PANEL. Membership and the counting policy are fixed + hashed before any judgment is
//     solicited (internal/govern owns the arithmetic; the pipeline owns the freezing moment).

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

// preflightPrompt is the cheap REACHABILITY probe body. It asks for a trivial fixed JSON object: the
// CONTENT is irrelevant (nothing parses it) — the probe exists so a role that cannot be invoked at all
// (missing binary, expired auth, an unusable model slug) fails before a panel is paid for. Whatever
// identity evidence the call happens to expose is classified and recorded on the way past.
const preflightPrompt = "Pre-flight check. Reply with ONLY this JSON object and nothing else: {\"ok\":true}"

// PreflightRecord is one role's pre-flight verdict (design §1), persisted so "the role was reachable
// before the fan-out" is a recorded fact rather than a claim about the code. Status/Caveat DESCRIBE the
// identity evidence; neither can fail the pre-flight.
type PreflightRecord struct {
	Role          string                  `json:"role"`
	Requested     schema.ExplorerIdentity `json:"requested"`
	ResolvedModel string                  `json:"resolvedModel"`
	Status        schema.IdentityStatus   `json:"status"`
	Caveat        string                  `json:"caveat,omitempty"`
}

// preflight probes ONE role's adapter. It fails ONLY when the adapter cannot be invoked — a role whose
// identity is weak, unknown, or a proven mismatch still passes, because identity never decides whether a
// response is used. It returns the record plus a halt error; on halt the caller returns before the fan-out.
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

// identityLedger pins each ROLE's resolved model across all of that role's calls in ONE exploration (design
// §1: the same-identity invariant). It is written from the pipeline's own goroutines (explorer calls run
// concurrently), so it carries a mutex.
type identityLedger struct {
	mu    sync.Mutex
	first map[string]string
}

// newIdentityLedger builds an empty ledger.
func newIdentityLedger() *identityLedger { return &identityLedger{first: map[string]string{}} }

// observe records the model a role's call RESOLVED to. The first observation pins the role; a later
// observation of a DIFFERENT non-empty model is a mid-exploration fallback and returns a HALT error. An empty
// resolved model (an adapter that reports nothing) is not evidence of a change, so it never pins and never
// halts — that case is already governed by the identity classification (unknown → halt at the policy).
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

// pinned returns the model pinned for a role ("" if the role has not been observed).
func (l *identityLedger) pinned(role string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.first[role]
}

// canonicalizerIdentity names one canonicalizer's role label + identity. Role labels are distinct per
// canonicalizer (`canonicalizer-a` / `canonicalizer-b`) so the same-identity invariant is enforced PER
// canonicalizer rather than across the pair — two independent canonicalizers are supposed to differ.
type canonicalizerIdentity struct {
	role     string
	explorer roster.Explorer
}

// Canonicalizer PROVENANCE (design §4): how the identities that produced this partition were chosen. It is
// a governance fact, not a diagnostic — it is recorded on the Result and in the run manifest because it is
// precisely what makes a bad canonicalizer choice visible FROM A RUN RECORD instead of requiring a code
// read. (The defect this exists to prevent recurring was invisible in every artifact a run produced.)
const (
	// ProvenanceExplicit: the identities were NAMED by the request or the profile.
	ProvenanceExplicit = "explicit"
	// ProvenanceDerived: the host derived them — slot a from the collator's identity, slot b from the panel
	// in PREFERENCE order.
	ProvenanceDerived = "derived"
)

// Canonicalizer INDEPENDENCE (design §4): what the merge-agreement rule is actually worth on this run.
//
// A held merge means "both canonicalizers proposed it". How much that is worth depends entirely on how
// different the two proposers are, and until now nothing recorded it — a run whose two canonicalizers were
// the same model behind two adapters produced a record indistinguishable from one with two genuinely
// different models, while every corroboration count downstream rested on the difference.
//
// It is a SEPARATE axis from provenance, not another provenance value: provenance says who CHOSE the
// identities, this says what the choice bought. Folding them together would make "explicit" and
// "shared_model" mutually exclusive when they are routinely both true.
const (
	// IndependenceDistinct: the two canonicalizers run DIFFERENT models. Agreement between them is the
	// evidence the dual rule was designed around.
	IndependenceDistinct = "distinct_models"
	// IndependenceSharedModel: the two canonicalizers run the SAME model, reached through different
	// adapters (or at different efforts). Agreement is weaker here — not because the two proposals are
	// identical (they are not: sampling makes two calls to one model differ, sometimes materially) but
	// because they are drawn from the same priors, so the errors they make are correlated in a way two
	// different models' are not.
	//
	// It is RECORDED AND WARNED, never refused. A panel is configured deliberately, and a reader who
	// wanted two different models would have named two different models; refusing this would override a
	// choice on the strength of an assumption about what the configurer meant.
	IndependenceSharedModel = "shared_model"
)

// explicitCanonicalizers resolves the EXPLICIT canonicalizer spec for a run, from the host override
// (opts.Canonicalizers) if present, else from the plan the surfaces resolved (profile / CLI flag / ACP
// `_meta` / MCP argument all land there). Either is "explicit": the user named them.
//
// The shape rule is re-checked here even though every surface checks it first. The surfaces produce the
// good error message; this is the backstop no caller can skip, and it is what keeps a directly-constructed
// Options honest.
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

// canonicalizerChoice is the resolved canonicalizer roles for one run plus the two governance facts about
// the choice itself: who made it, and what it bought. Both are recorded on the Result and disclosed by a
// dry run, because a corroboration count cannot be read honestly without them.
type canonicalizerChoice struct {
	ids          []canonicalizerIdentity
	provenance   string // ProvenanceExplicit | ProvenanceDerived
	independence string // IndependenceDistinct | IndependenceSharedModel; "" on the single-canonicalizer path
}

// canonicalizerIdentities resolves the canonicalizer identities for a run (design §4: canonicalization is a
// distinct role whose identity is DECOUPLED from the collator). It returns them with their PROVENANCE
// (explicit / derived) and, on the dual path, their INDEPENDENCE — which the caller records and warns on.
//
//   - EXPLICIT identities (named by the request or the profile) are used verbatim. The dual path takes both;
//     the SINGLE path takes the first — a mode with a single-canonicalizer policy makes one call, and
//     honoring the user's first named identity is the only reading that neither ignores the spec nor
//     invents a second call the mode's contract does not have.
//   - Otherwise the host DERIVES them. The single path issues one canonicalizer call through the collator's
//     adapter/model — a distinct, separately-verified CALL, never the collate call itself. The dual path
//     takes slot a from the collator and derives slot b from the panel: the first explorer by the author's
//     PREFERENCE order whose (adapter, model) differs from the collator's.
//
// Slot b is read from plan.Preferred and NOT from plan.Explorers. Both hold the same set, but
// plan.Explorers is sorted by identity triple for stable attribution — so deriving from it selected
// whichever explorer sorted first ALPHABETICALLY, an ordering with no relationship to capability, for half
// of a governance rule. Agreement between a and b decides which merges hold versus contest, which decides
// every corroboration count; a weak model in slot b systematically fails to agree and quietly undercounts.
//
// If NO panel member differs from the collator at all — every seat is the same adapter AND model — the dual
// path HALTS. That is not the same case as slot b merely sharing the collator's model: there, a second
// identity exists and the run proceeds with its independence recorded (see IndependenceSharedModel). Here
// there is no second identity to name, so the alternative to refusing is inventing one.
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

	primary := canonicalizerIdentity{role: "canonicalizer", explorer: roster.Explorer{
		Adapter: plan.Collator.Adapter, Model: plan.Collator.Model, Effort: plan.Collator.Effort,
	}}
	if !dual {
		return canonicalizerChoice{ids: []canonicalizerIdentity{primary}, provenance: ProvenanceDerived}, nil
	}
	// A plan that recorded no preference order cannot be derived from: the only order left is the
	// attribution order, and using it is the exact defect this function was rewritten to remove. Refuse
	// rather than silently fall back — every executable plan comes from Roster.SelectTopN, which records both.
	if !plan.SameSet() {
		return canonicalizerChoice{}, fault.New(fault.Internal, fmt.Sprintf(
			"canonicalizer derivation needs the panel in PREFERENCE order, but this plan carries %d explorer(s) in attribution order and %d in preference order — a plan not built by roster.SelectTopN cannot be derived from (using the attribution order would select canonicalizer-b alphabetically)",
			len(plan.Explorers), len(plan.Preferred)))
	}
	for _, ex := range plan.Preferred { // PREFERENCE order — the author's ranking, never the attribution sort
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

// singleCanonicalizerModes names the modes that canonicalize with ONE identity, sorted.
//
// It exists because of the specific confusion this refusal is designed around: the halt is reported
// against the roster, but the roster is fine — the SAME roster runs these modes unchanged, and the
// mode is the variable. Reading that off the registry rather than writing a list into a string keeps
// it true when a mode is added, which is exactly the kind of list that otherwise rots into a lie.
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

// dualChoice builds the two-slot result and classifies its independence. The test is the MODEL: two
// adapters reaching one model are two routes to the same priors, and it is the priors — not the process —
// that decide whether agreement between the pair means anything.
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
