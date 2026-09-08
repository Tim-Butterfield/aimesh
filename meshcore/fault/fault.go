// Package fault carries an error together with the process exit code it maps to.
// This table is the source of the public CLI exit codes documented in docs/architecture.md,
// and BOTH binaries map through it — a script branching on exploremesh's exit codes branches
// on reviewmesh's the same way. Changing a value here is a breaking change to that contract.
package fault

import "errors"

// Code is a reviewmesh CLI process exit code.
type Code int

const (
	OK          Code = 0 // success
	Findings    Code = 1 // completed with blocking findings (gating on)
	Usage       Code = 2 // invalid CLI usage
	Config      Code = 3 // configuration error
	Adapter     Code = 4 // adapter / auth / binary unavailable
	Model       Code = 5 // model unavailable or a strong-evidence identity mismatch (unknown/weak self-report is a caveat, not a fault)
	Containment Code = 6 // containment breach / unexpected mutation
	Policy      Code = 7 // halted by policy or cap
	Internal    Code = 8 // internal error
)

// Fault is an error annotated with an exit Code and an optional halt class.
type Fault struct {
	Code Code
	Msg  string
	Halt string // optional halt-taxonomy class (A-G, M1-M6)
	// Rsn is the stable MACHINE reason code for this fault (lower_snake, e.g.
	// "adapter_exited_nonzero"). It is what a machine consumer branches on and what an
	// audit record persists; Msg stays the human sentence. Empty means "not explicitly
	// classified" — Reason() then derives a stable code from the exit Code, so a machine
	// reader NEVER receives a sentence where it expects a code.
	Rsn string
	// Signal is an optional classified actionability hint for the underlying tool
	// invocation (see meshcore/clihint: folder_trust / login_required / model_invalid /
	// update_prompt / timeout). It is orthogonal to Rsn: Rsn says what went wrong,
	// Signal says what the operator can do about it. Empty when nothing classified.
	Signal string
	Cause  error
}

// Error returns the message, appending the wrapped cause when present.
func (f *Fault) Error() string {
	if f.Cause != nil {
		return f.Msg + ": " + f.Cause.Error()
	}
	return f.Msg
}

// Unwrap exposes the wrapped cause for errors.Is / errors.As chains.
func (f *Fault) Unwrap() error { return f.Cause }

// New creates a Fault with a code and message.
func New(code Code, msg string) *Fault { return &Fault{Code: code, Msg: msg} }

// Wrap wraps an existing error with a code and message.
func Wrap(code Code, msg string, err error) *Fault {
	return &Fault{Code: code, Msg: msg, Cause: err}
}

// WithHalt sets the halt-taxonomy class and returns the fault.
func (f *Fault) WithHalt(class string) *Fault { f.Halt = class; return f }

// WithReason sets the stable machine reason code and returns the fault.
func (f *Fault) WithReason(code string) *Fault { f.Rsn = code; return f }

// WithSignal sets the classified actionability hint and returns the fault.
func (f *Fault) WithSignal(sig string) *Fault { f.Signal = sig; return f }

// codeReasons are the STABLE fallback reason codes, one per exit code. They exist so
// that an unclassified fault still yields a code-shaped reason (never a sentence): a
// machine consumer can always branch, and the specific classification can be tightened
// later without changing the shape of the contract.
var codeReasons = map[Code]string{
	OK:          "ok",
	Findings:    "gating_threshold_met",
	Usage:       "usage_error",
	Config:      "configuration_error",
	Adapter:     "adapter_unavailable",
	Model:       "model_identity_failure",
	Containment: "containment_breach",
	Policy:      "policy_halt",
	Internal:    "internal_error",
}

// Reason returns the machine reason code: the explicitly-set one, else the stable
// fallback for the exit code. The result is always code-shaped.
func (f *Fault) Reason() string {
	if f == nil {
		return ""
	}
	if f.Rsn != "" {
		return f.Rsn
	}
	if r, ok := codeReasons[f.Code]; ok {
		return r
	}
	return "internal_error"
}

// CodeOf returns the exit code for an error: OK for nil, the Fault's code for a
// Fault, otherwise Internal.
func CodeOf(err error) Code {
	if err == nil {
		return OK
	}
	if f, ok := errors.AsType[*Fault](err); ok {
		return f.Code
	}
	return Internal
}

// ReasonOf returns the machine reason code for an error: "" for nil, the Fault's Reason
// for a Fault (explicit or derived), otherwise the Internal fallback — so a persisted
// reasonCode is code-shaped whatever the error turns out to be.
func ReasonOf(err error) string {
	if err == nil {
		return ""
	}
	if f, ok := errors.AsType[*Fault](err); ok {
		return f.Reason()
	}
	return codeReasons[Internal]
}

// SignalOf returns the classified actionability hint for an error ("" when none).
func SignalOf(err error) string {
	if err == nil {
		return ""
	}
	if f, ok := errors.AsType[*Fault](err); ok {
		return f.Signal
	}
	return ""
}
