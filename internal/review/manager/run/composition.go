package run

// Panel composition reports how independent the seats behind each agreement were, as a count of
// distinct model strings. Seats running the same model have correlated errors, so their agreement is
// worth less than the count suggests.
//
// agreementCount is never adjusted. Model families, vendors and weight lineage are not observable from
// a model string, so an adjusted figure would rest on an unverifiable table; composition asserts only
// string equality and a distinct-model count, and leaves interpretation to the reader. It uses the
// same distinct_models and shared_model vocabulary as the explore canonicalizer pair.

import (
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
)

// Independence verdicts, shared with the canonicalizer disclosure.
const (
	// IndependenceDistinctModels — every seat behind the count runs a different model.
	IndependenceDistinctModels = "distinct_models"
	// IndependenceSharedModel — at least two of them run the same model, through the same adapter or
	// different ones.
	IndependenceSharedModel = "shared_model"
)

// Where a seat's model string came from.
const (
	// ModelSourceCatalog — a configured modelCatalog key, resolved to that entry's per-adapter
	// model argument.
	ModelSourceCatalog = "catalog"
	// ModelSourcePassThrough — the caller composed this seat with a model the catalog does not
	// define, passed to the adapter verbatim. The adapter was still validated.
	ModelSourcePassThrough = "passthrough"
)

// PassThroughNote is the fixed sentence a surface shows when any seat passed a model through.
const PassThroughNote = "A pass-through seat names a model the configuration does not define, handed to the adapter verbatim. The ADAPTER was still validated against the configured set — that is what carries trust and identity-evidence capability — and nothing about the seat's findings, agreement or applyability differs. What differs is that `list` cannot describe this model, no catalog entry pins its effort or argument, and whether it exists at all was decided by the provider rather than here: a model string the provider does not recognise fails in the provider's own words, not ours."

// CompositionNote is the fixed sentence on the run-level record, stating the limits in both directions.
const CompositionNote = "agreementCount is NOT adjusted by any of this: it is how many seats reported the finding, and it means what it always meant. distinctModels says how many different models those seats ran — shared_model means at least two shared one, so their errors correlate and the extra agreement is worth less than the count suggests. It does not make the finding wrong. Vendor, base family and weight lineage are NOT asserted here: they are not observable from a model string, and a figure resting on a table we cannot verify would be worse than the raw count."

// composeAgreement labels each panel decision with its distinct-model count and independence verdict,
// and returns the run-level record. It writes only Decision.DistinctModels and
// Decision.AgreementIndependence.
func composeAgreement(resolved []review.LaneResolution, adj *adjudication.Result) *review.PanelComposition {
	seats := panelSeats(resolved)
	if len(seats) == 0 {
		// A run without a blind panel has no agreement to qualify.
		return nil
	}
	for i := range adj.Decisions {
		d := &adj.Decisions[i]
		if len(d.SupportingSeats) == 0 {
			// Not a panel finding, so there is no agreement to qualify.
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

// panelSeats projects the resolved panel in requested order. It describes the panel convened;
// per-finding independence covers the seats that actually agreed.
func panelSeats(resolved []review.LaneResolution) []review.SeatComposition {
	var out []review.SeatComposition
	for i, s := range resolved {
		// Record whether the model string came from the catalog or was passed through.
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

// SharedModels returns, sorted, the model strings that more than one seat runs.
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
