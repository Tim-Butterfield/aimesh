package cli

// Ad-hoc BLIND REVIEWER PANEL composition on the CLI: `--reviewer adapter=…,model=…[,effort=…]`,
// repeatable, order-preserving. It is the CLI half of the surface-parity obligation that a panel
// be composable from every surface (ACP `_meta.reviewmesh.panel`, MCP `panel` later) with
// identical fail-closed semantics.
//
// The grammar is deliberately the same key=value shape exploremesh's `--explorer` uses, and for
// the same reason: the `adapter:model` colon shorthand is UNSAFE because model tags contain
// colons (`llama3:8b`), so a value is taken verbatim after the FIRST `=` only.

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// parseSeatSpec parses one `--reviewer` value into a seat. Every failure is a malformed FLAG, so
// each carries fault.Usage and the exit code follows from the error itself rather than from a
// literal the caller has to remember.
func parseSeatSpec(spec string) (review.SeatSpec, error) {
	var seat review.SeatSpec
	seen := map[string]bool{}
	for _, field := range strings.Split(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue // tolerate a trailing/doubled comma
		}
		kv := strings.SplitN(field, "=", 2) // FIRST '=' only: a value may contain '=' and ':'
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			return seat, fault.New(fault.Usage, fmt.Sprintf("invalid field %q (want key=value)", field))
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1]) // taken VERBATIM — never split on ':'
		if seen[key] {
			return seat, fault.New(fault.Usage, fmt.Sprintf("duplicate key %q", key))
		}
		seen[key] = true
		switch key {
		case "adapter":
			seat.Adapter = val
		case "model":
			seat.Model = val
		case "effort":
			seat.Effort = val
		default:
			return seat, fault.New(fault.Usage, fmt.Sprintf("unknown key %q (want adapter, model, or effort)", key))
		}
	}
	if seat.Adapter == "" || seat.Model == "" {
		return seat, fault.New(fault.Usage, fmt.Sprintf("both adapter and model are required (got adapter=%q model=%q)", seat.Adapter, seat.Model))
	}
	return seat, nil
}

// parseReviewerPanel turns the repeatable `--reviewer` values into an ordered panel. It enforces
// the panel CEILING here so an oversized panel is refused at the flag boundary with the flag's
// own vocabulary; the resolver enforces the same bound again (and the identity-uniqueness and
// resolvability rules) for every surface, so nothing depends on this check having run.
func parseReviewerPanel(specs []string) ([]review.SeatSpec, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	if len(specs) > review.MaxReviewerSeats {
		return nil, fault.New(fault.Usage, fmt.Sprintf(
			"%d --reviewer seats exceeds the panel cap of %d (each seat is a real model CLI; the count is never trimmed for you)",
			len(specs), review.MaxReviewerSeats))
	}
	out := make([]review.SeatSpec, 0, len(specs))
	for _, spec := range specs {
		seat, err := parseSeatSpec(spec)
		if err != nil {
			return nil, fault.Wrap(fault.Usage, fmt.Sprintf("--reviewer %q", spec), err)
		}
		out = append(out, seat)
	}
	return out, nil
}
