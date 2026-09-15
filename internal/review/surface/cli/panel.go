package cli

// This file parses ad-hoc reviewer panels on the CLI: `--reviewer adapter=…,model=…[,effort=…]`,
// repeatable and order-preserving, with the same fail-closed rules as panels on ACP and MCP.
//
// The grammar matches exploremesh's --explorer. An `adapter:model` shorthand would be ambiguous
// because model tags contain colons (llama3:8b), so each value is taken verbatim after the first `=`.

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// parseSeatSpec parses one --reviewer value into a seat. Every error carries fault.Usage.
func parseSeatSpec(spec string) (review.SeatSpec, error) {
	var seat review.SeatSpec
	seen := map[string]bool{}
	for field := range strings.SplitSeq(spec, ",") {
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

// parseReviewerPanel turns the repeated --reviewer values into an ordered panel and refuses an
// oversized panel at the flag boundary. The resolver enforces the same bound, plus uniqueness and
// resolvability, for every surface.
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
