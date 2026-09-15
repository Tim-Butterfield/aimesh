package run

// Egress reporting tells a dry run where the run's content would be sent, computed from the resolved
// panel so composed panels and added seats are included. Rows are grouped by destination, so several
// seats behind one provider form one row, and an adapter whose destination aimesh cannot know (a
// user-defined ACP instance) is reported as unknown rather than omitted.

import (
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
)

// unknownDestination labels a destination aimesh does not know; the operator who configured the
// adapter does.
const unknownDestination = "not known to aimesh — a user-defined adapter goes wherever you pointed it"

// shapeEgress groups a resolved run's seats and lanes by where their content goes. Host-execution lanes
// run in process and are skipped.
func shapeEgress(seats []review.ShapeSeat, lanes []review.ShapeLane) []review.ShapeEgress {
	type dest struct {
		egress shell.Egress
		known  bool
		via    []string
	}
	// Keyed by destination, so two adapters reaching one provider share a row.
	order := []string{}
	byDest := map[string]*dest{}
	add := func(adapter, via string) {
		if adapter == "" {
			return
		}
		eg, known := shell.EgressFor(adapter)
		// The `fake` harness is not a shell recipe, but it is an in-process responder that opens no
		// connection, so it is local rather than unknown.
		if !known && adapter == "fake" {
			eg, known = shell.Egress{Local: true, Destination: "this machine"}, true
		}
		key := eg.Destination
		if !known {
			// All unknown adapters share one row.
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
	// Off-machine destinations first, then alphabetical, so the order does not depend on seat order.
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

// EgressLeavesTheMachine reports whether any part of a run would send content off the machine. An
// unknown destination counts as leaving.
func EgressLeavesTheMachine(rows []review.ShapeEgress) bool {
	for _, r := range rows {
		if !r.Known || !r.Local {
			return true
		}
	}
	return false
}
