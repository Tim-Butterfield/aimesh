// Package doctor holds domain-free readiness primitives: the Check and Report types, a writable
// directory check, and adapter availability and probe scans over a model.Adapter registry.
// Applications add their own checks to build the final Report.
package doctor

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// Check is one readiness check result.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Report is the doctor outcome (an ordered list of checks + an overall OK).
type Report struct {
	Checks []Check
	OK     bool
}

// Finalize sets OK to the AND of every check's OK (call once after all checks are appended).
func (r *Report) Finalize() {
	r.OK = true
	for _, c := range r.Checks {
		if !c.OK {
			r.OK = false
		}
	}
}

// String renders the report for a CLI.
func (r Report) String() string {
	var sb strings.Builder
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&sb, "[%s] %s — %s\n", mark, c.Name, c.Detail)
	}
	if r.OK {
		sb.WriteString("\ndoctor: all static checks passed.\n")
	} else {
		sb.WriteString("\ndoctor: one or more checks FAILED.\n")
	}
	return sb.String()
}

// --- machine-readable projection ---

// ReportSchemaVersion is the payload version of ReportView (docs/schema/doctor-report.schema.json).
const ReportSchemaVersion = 1

// probePrefix marks the checks produced by ProbeAdapters. A projection flags them so a
// machine consumer can tell an actually-executed binary probe apart from a static check
// without pattern-matching the human check name itself.
const probePrefix = "probe: "

// deepProbePrefix marks the checks produced by ProbeAdaptersDeep. It differs from probePrefix because a
// deep probe spends tokens and a consumer must not confuse the two.
const deepProbePrefix = "probe-deep: "

// CheckView is one readiness check in the machine projection.
type CheckView struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Probe marks a check produced by an executed adapter probe (as opposed to a static
	// check). Absent means static.
	Probe bool `json:"probe,omitempty"`
	// Deep marks a check produced by a deep probe, a real bounded invocation that spent tokens. Deep
	// implies Probe.
	Deep bool `json:"deep,omitempty"`
}

// ReportView is the machine-readable projection of a Report. It is domain-free, so both
// applications can emit the identical `doctor --json` shape.
type ReportView struct {
	SchemaVersion int         `json:"schemaVersion"`
	OK            bool        `json:"ok"`
	Checks        []CheckView `json:"checks"`
}

// Project turns a Report into its machine projection. Checks is always a non-nil slice,
// so a consumer can iterate without a null check.
func Project(r Report) ReportView {
	v := ReportView{SchemaVersion: ReportSchemaVersion, OK: r.OK, Checks: make([]CheckView, 0, len(r.Checks))}
	for _, c := range r.Checks {
		deep := strings.HasPrefix(c.Name, deepProbePrefix)
		v.Checks = append(v.Checks, CheckView{
			Name:   c.Name,
			OK:     c.OK,
			Detail: c.Detail,
			Probe:  deep || strings.HasPrefix(c.Name, probePrefix),
			Deep:   deep,
		})
	}
	return v
}

// AdapterAvailability reports each adapter in names (in the given order) as available or not,
// labeled "required" or "opt-in" per the required set. A missing/unavailable adapter fails the
// check only when it is required. `adapters` is the runnable registry (name → model.Adapter).
func AdapterAvailability(adapters map[string]model.Adapter, names []string, required map[string]bool) []Check {
	var out []Check
	for _, name := range names {
		label := "opt-in"
		if required[name] {
			label = "required"
		}
		if a, ok := adapters[name]; ok {
			avail, detail := a.Available()
			out = append(out, Check{Name: "adapter: " + name, OK: avail || !required[name], Detail: fmt.Sprintf("%s — %s", label, detail)})
		} else {
			out = append(out, Check{Name: "adapter: " + name, OK: !required[name], Detail: label + " — not registered"})
		}
	}
	return out
}

// ProbeAdapters runs a safe no-model readiness probe (via model.Prober) for the required, available
// adapters in names, under the caller's ctx (so a cancel/timeout propagates). An adapter that is not
// required, not registered, or unavailable is skipped (availability is reported separately by
// AdapterAvailability). Adapters without a Prober report "no probe". A classified blocker signal
// (folder_trust / login_required / …), when present, is appended to the detail.
func ProbeAdapters(ctx context.Context, adapters map[string]model.Adapter, names []string, required map[string]bool) []Check {
	var out []Check
	for _, name := range names {
		if !required[name] {
			continue
		}
		a, ok := adapters[name]
		if !ok {
			continue
		}
		if avail, _ := a.Available(); !avail {
			continue // already reported as a failing required-adapter check
		}
		p, ok := a.(model.Prober)
		if !ok {
			out = append(out, Check{Name: probePrefix + name, OK: true, Detail: "no probe (built-in adapter)"})
			continue
		}
		r := p.Probe(ctx)
		detail := r.Detail
		if r.Signal != "" {
			detail = fmt.Sprintf("%s [%s]", detail, r.Signal)
		}
		out = append(out, Check{Name: probePrefix + name, OK: r.OK, Detail: detail})
	}
	return out
}

// DeepSeat names one adapter to deep-probe with the model argument and effort a real run would pass,
// since a recipe builds the model argument into the argv.
type DeepSeat struct {
	Adapter  string
	ModelArg string
	Effort   string
}

// ProbeAdaptersDeep runs the deep readiness probe (model.DeepProber) for the named seats, in order, at
// most once per adapter (the first seat naming it wins), because the result depends on the CLI's trust
// and authentication rather than on the seat. It spends tokens, so callers must gate it behind an
// explicit opt-in. Unavailable or unregistered adapters are skipped; one without the capability
// reports "no deep probe".
func ProbeAdaptersDeep(ctx context.Context, adapters map[string]model.Adapter, seats []DeepSeat) []Check {
	var out []Check
	seen := map[string]bool{}
	for _, seat := range seats {
		if seat.Adapter == "" || seen[seat.Adapter] {
			continue
		}
		seen[seat.Adapter] = true
		a, ok := adapters[seat.Adapter]
		if !ok {
			continue
		}
		if avail, _ := a.Available(); !avail {
			continue // already reported as a failing required-adapter check
		}
		dp, ok := a.(model.DeepProber)
		if !ok {
			out = append(out, Check{Name: deepProbePrefix + seat.Adapter, OK: true, Detail: "no deep probe for this adapter kind"})
			continue
		}
		r := dp.ProbeDeep(ctx, model.DeepProbeSpec{ModelArg: core.ModelArg(seat.ModelArg), Effort: seat.Effort})
		detail := r.Detail
		if r.Signal != "" {
			detail = fmt.Sprintf("%s [%s]", detail, r.Signal)
		}
		out = append(out, Check{Name: deepProbePrefix + seat.Adapter, OK: r.OK, Detail: detail})
	}
	return out
}

// WritableDir reports whether dir can be created and written (a temp file is created + removed).
func WritableDir(dir string) bool {
	if dir == "" {
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".mesh-write-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}
