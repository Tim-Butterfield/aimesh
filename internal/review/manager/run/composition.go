package run

// PANEL COMPOSITION — what the seats behind an agreement count were WORTH.
//
// THE GOVERNING INVARIANT:
//
//	`agreementCount` is never adjusted. It means exactly what it has always meant — how many seats
//	reported this fingerprint — and nothing here reweights, discounts or replaces it.
//
// THE PROBLEM IT ADDRESSES. Three seats agreeing is weaker evidence when two of them run the same
// model behind two different vendor CLIs. Their errors correlate, because the priors are the same, so
// the third agreement adds much less than the second did. The count is arithmetic over seats and is
// correct as arithmetic; what it does not say is how independent those seats were.
//
// WHY IT LABELS RATHER THAN ADJUSTS, which is the decision this file exists to hold. An adjusted
// figure — "3 seats, effective agreement 2.1" — needs a model-FAMILY taxonomy: which model strings
// are the same weights under different names, which vendor rebadges whom, which version is a point
// release of which. None of that is host-observable. Publishing a number derived from a table we
// cannot verify would be the same failure that refuted the cost-ceiling proposal: an authoritative-
// looking figure resting on a guess. So this asserts only what is directly checkable and leaves the
// judgement to the reader, who is the one who knows their own panel.
//
// WHAT IS ASSERTED, and it is deliberately the narrowest thing that helps:
//
//   - THE MODEL STRING. Two seats naming the same model ARE the same model. That needs no taxonomy —
//     it is string equality over a value the operator configured.
//   - A COUNT OF DISTINCT MODELS behind the agreement. This is not an adjusted agreement figure; it
//     is a count of a different thing, computed the same way and equally verifiable. "3 seats, 2
//     distinct models" states the whole situation without anyone having to interpret a weighting.
//
// WHAT IS NOT ASSERTED: vendor, base family, weight lineage, or any equivalence between differently
// named models. The raw composition rides alongside so a reader who knows that `provider-a/x` and
// `provider-b/y` are the same weights can see it — this simply refuses to claim it on their behalf.
//
// It is the same vocabulary as the canonicalizer pair's `independence` (see the explore side's
// distinct_models / shared_model), one scale up: there it described two proposers, here it describes
// a whole panel.

import (
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
)

// Independence verdicts. STABLE MACHINE CODES, and the same two words the canonicalizer disclosure
// uses, so a reader who has met one has met both.
const (
	// IndependenceDistinctModels — every seat behind the count runs a different model.
	IndependenceDistinctModels = "distinct_models"
	// IndependenceSharedModel — at least two of them run the SAME model, through the same adapter or
	// through different ones. The agreement is real; it is just worth less than the count suggests.
	IndependenceSharedModel = "shared_model"
)

// Where a seat's model string came from. STABLE MACHINE CODES.
const (
	// ModelSourceCatalog — a configured modelCatalog key, resolved to that entry's per-adapter
	// model argument.
	ModelSourceCatalog = "catalog"
	// ModelSourcePassThrough — the caller COMPOSED this seat naming something the catalog does not
	// define, and the string went to the adapter verbatim. The adapter was still validated
	// fail-closed; only the model string is unvouched-for.
	ModelSourcePassThrough = "passthrough"
)

// PassThroughNote is the fixed sentence a surface carries when any seat passed a model through. It
// states the limit in the direction that actually bites: not "this may be wrong" but "your
// configuration says nothing about this one, so the usual inventory cannot answer questions about it".
const PassThroughNote = "A pass-through seat names a model the configuration does not define, handed to the adapter verbatim. The ADAPTER was still validated against the configured set — that is what carries trust and identity-evidence capability — and nothing about the seat's findings, agreement or applyability differs. What differs is that `list` cannot describe this model, no catalog entry pins its effort or argument, and whether it exists at all was decided by the provider rather than here: a model string the provider does not recognise fails in the provider's own words, not ours."

// CompositionNote is the fixed sentence carried on the run-level record. It states the limit in both
// directions, because the two misreadings are opposite: `shared_model` invites discarding a real
// agreement, and `distinct_models` invites treating one as proof.
const CompositionNote = "agreementCount is NOT adjusted by any of this: it is how many seats reported the finding, and it means what it always meant. distinctModels says how many different models those seats ran — shared_model means at least two shared one, so their errors correlate and the extra agreement is worth less than the count suggests. It does not make the finding wrong. Vendor, base family and weight lineage are NOT asserted here: they are not observable from a model string, and a figure resting on a table we cannot verify would be worse than the raw count."

// composeAgreement labels every decision with what the seats behind its agreement count were worth,
// and returns the run-level panel record.
//
// It writes `Decision.AgreementIndependence` and `Decision.DistinctModels` and nothing else. No
// count is changed, no finding is reordered, nothing is dropped.
func composeAgreement(resolved []review.LaneResolution, adj *adjudication.Result) *review.PanelComposition {
	seats := panelSeats(resolved)
	if len(seats) == 0 {
		// No blind panel to describe. A single-lane run has no agreement to qualify, and inventing a
		// record for it would suggest a panel that never existed.
		return nil
	}
	for i := range adj.Decisions {
		d := &adj.Decisions[i]
		if len(d.SupportingSeats) == 0 {
			// Not a panel finding (an evidence hook, the cross-check, the verifier). There is no
			// agreement to qualify, so there is nothing to say — and saying `distinct_models: 1`
			// would read as a weak agreement rather than as no agreement claim at all.
			continue
		}
		models := map[string]bool{}
		for _, s := range d.SupportingSeats {
			models[strings.TrimSpace(s.Model)] = true
		}
		d.DistinctModels = len(models)
		d.AgreementIndependence = independenceOf(len(d.SupportingSeats), len(models))
	}
	return &review.PanelComposition{
		Seats:          seats,
		DistinctModels: distinctModelCount(seats),
		Independence:   independenceOf(len(seats), distinctModelCount(seats)),
		Note:           CompositionNote,
	}
}

// independenceOf is the verdict for `seats` seats running `models` distinct models.
func independenceOf(seats, models int) string {
	if models < seats {
		return IndependenceSharedModel
	}
	return IndependenceDistinctModels
}

// panelSeats projects the RESOLVED blind panel as the composition record: what each seat is, in the
// order it was requested. It is the requested roster rather than the responding one on purpose — the
// run-level record describes the panel that was CONVENED, and per-finding independence (above)
// already describes the seats that actually agreed on each thing.
func panelSeats(resolved []review.LaneResolution) []review.SeatComposition {
	var out []review.SeatComposition
	for i, s := range resolved {
		// WHERE THE MODEL STRING CAME FROM. A pass-through seat names something the operator's
		// configuration does not define, so nothing here — and nothing in `list` — can tell a reader
		// what it is. Saying so is the disclosure; it gates nothing.
		source := ModelSourceCatalog
		if s.ModelPassThrough {
			source = ModelSourcePassThrough
		}
		out = append(out, review.SeatComposition{
			SeatID: s.SeatID, Index: i + 1,
			Adapter: s.Adapter, Model: s.Model, Effort: s.Effort,
			ModelSource: source, PassThroughHint: s.PassThroughHint,
		})
	}
	return out
}

// distinctModelCount counts distinct model strings across a seat list.
func distinctModelCount(seats []review.SeatComposition) int {
	models := map[string]bool{}
	for _, s := range seats {
		models[strings.TrimSpace(s.Model)] = true
	}
	return len(models)
}

// SharedModels reports which model strings more than one seat runs, in stable order. It is what a
// surface names when it wants to say WHICH model is doubled rather than merely that one is — an
// operator who has to go read the roster to find out has been told half a fact.
func SharedModels(seats []review.SeatComposition) []string {
	count := map[string]int{}
	for _, s := range seats {
		count[strings.TrimSpace(s.Model)]++
	}
	var out []string
	for model, n := range count {
		if n > 1 && model != "" {
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out
}
