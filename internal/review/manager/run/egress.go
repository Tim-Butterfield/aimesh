package run

// WHERE THIS RUN'S CONTENT GOES.
//
// A dry run already answers "how many calls will this cost". This answers the question underneath it,
// which is the one a person is actually weighing before pointing a panel at their source: WHO ENDS UP
// HOLDING IT. The mapping existed only as prose in docs/security.md — correct, but a table a reader
// had to cross-reference by hand against a roster, at the exact moment they were deciding whether to
// spend. Now the resolved run says it about itself.
//
// It is computed from the RESOLVED panel, which is what makes it true of THIS run: an ad-hoc panel
// with no profile gets an accurate answer, a seat added by `--reviewer` is included, and a profile
// changed since the docs were read cannot make the answer stale.
//
// TWO RULES, both about not overstating:
//
//  1. GROUPED BY DESTINATION. Four seats behind one provider is ONE disclosure. Listing it per seat
//     would make a diverse panel look like four leaks and a single-provider panel look like one.
//  2. UNKNOWN IS REPORTED, NEVER OMITTED. A user-defined ACP instance points at a binary the operator
//     chose, so this tool cannot say where it sends anything — and a missing row would read as
//     "nothing goes there", which is the one misreading that matters.

import (
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
)

// unknownDestination labels a destination this tool does not own. It is deliberately a phrase a
// reader can act on rather than a bare "unknown": the operator configured this adapter, so they are
// the one who knows, and the sentence says so.
const unknownDestination = "not known to aimesh — a user-defined adapter goes wherever you pointed it"

// shapeEgress groups a resolved run's seats and lanes by where their content goes.
//
// A lane whose execution is `host` is SKIPPED: it runs in process and sends nothing. Counting it
// would attribute an egress to a step that never leaves the machine, which is precisely backwards.
func shapeEgress(seats []review.ShapeSeat, lanes []review.ShapeLane) []review.ShapeEgress {
	type dest struct {
		egress shell.Egress
		known  bool
		via    []string
	}
	// Keyed on the DESTINATION rather than the adapter, so two adapters reaching one provider are
	// one row — which is the whole point of grouping, and is also the case a reader is most likely
	// to get wrong on their own.
	order := []string{}
	byDest := map[string]*dest{}
	add := func(adapter, via string) {
		if adapter == "" {
			return
		}
		eg, known := shell.EgressFor(adapter)
		// The internal `fake` harness is not a shell recipe, so the lookup does not own it — but
		// reporting it as an UNKNOWN destination would be wrong in the alarming direction: it is a
		// deterministic in-process responder that opens no connection at all. shape.go already
		// special-cases it for the same underlying reason (it costs no model call).
		if !known && adapter == "fake" {
			eg, known = shell.Egress{Local: true, Destination: "this machine"}, true
		}
		key := eg.Destination
		if !known {
			// Every unknown adapter shares one row: they are unknown for the same reason, and
			// splitting them by name would imply this tool knows something distinguishing.
			key = unknownDestination
		}
		d, seen := byDest[key]
		if !seen {
			d = &dest{egress: eg, known: known}
			byDest[key] = d
			order = append(order, key)
		}
		d.via = append(d.via, via)
	}
	for _, s := range seats {
		add(s.Adapter, s.SeatID)
	}
	for _, l := range lanes {
		if l.Execution == "host" {
			continue // in-process: nothing leaves
		}
		add(l.Adapter, string(l.Role))
	}
	if len(order) == 0 {
		return nil
	}
	// LOCAL LAST, then alphabetical. A reader scanning for what left the machine should meet those
	// rows first; ordering by first appearance would make the answer depend on seat order.
	sort.SliceStable(order, func(i, j int) bool {
		li, lj := byDest[order[i]].egress.Local, byDest[order[j]].egress.Local
		if li != lj {
			return !li
		}
		return order[i] < order[j]
	})
	out := make([]review.ShapeEgress, 0, len(order))
	for _, key := range order {
		d := byDest[key]
		row := review.ShapeEgress{Known: d.known, Via: d.via}
		if d.known {
			row.Destination, row.Local, row.Note = d.egress.Destination, d.egress.Local, d.egress.Note
		} else {
			row.Destination = unknownDestination
		}
		out = append(out, row)
	}
	return out
}

// EgressLeavesTheMachine reports whether any part of a run would send content off the machine. It is
// the single question a caller most often has, answered from the same rows rather than re-derived —
// and an UNKNOWN destination counts as leaving, because assuming otherwise is the unsafe direction.
func EgressLeavesTheMachine(rows []review.ShapeEgress) bool {
	for _, r := range rows {
		if !r.Known || !r.Local {
			return true
		}
	}
	return false
}
