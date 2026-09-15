// Package runview builds the machine-readable projection of a completed or halted review run.
//
// Build is a pure function over a RunOutcome and the run's error. It copies what the run decided
// rather than re-deriving it, so the projection agrees with run-state.json and review-summary.md.
// The CLI's `--json` output and the agent surfaces return the same shape.
package runview

import (
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
)

// SchemaVersion is the projection's payload version (docs/schema/review-projection.schema.json).
const SchemaVersion = 1

// Finding is one adjudicated finding: what was found, where, and what the host decided. Disposition
// is the decision state; the presence of a finding does not mean it was fixed.
type Finding struct {
	ID string `json:"id"`
	// Fingerprint is the host-computed identity (file and normalized location, hashed) and the only
	// valid selector for a selective apply. ID is model-authored and renumbered after the write
	// window, so it cannot serve.
	Fingerprint string   `json:"fingerprint"`
	Severity    string   `json:"severity"`
	Kind        string   `json:"kind"`
	File        string   `json:"file,omitempty"`
	Location    string   `json:"location,omitempty"`
	Summary     string   `json:"summary"`
	Disposition string   `json:"disposition"`
	Sources     []string `json:"sources,omitempty"`
	// Applyable is present and false only when the write-path rule refused the finding (see
	// review.Decision.Applyable). Absence does not mean anything was written; see Disposition.
	Applyable *bool `json:"applyable,omitempty"`
	// ApplyRefusalReason is the refusal's machine code ("authority_only" |
	// "no_workspace_evidence" | "protected_path").
	ApplyRefusalReason string `json:"applyRefusalReason,omitempty"`
	// SupportingSeats, AgreementCount and DissentingSeats are the host-computed panel provenance.
	// They are absent for a finding not from the blind panel.
	SupportingSeats []review.SeatRef `json:"supportingSeats,omitempty"`
	AgreementCount  int              `json:"agreementCount,omitempty"`
	// DistinctModels and AgreementIndependence qualify AgreementCount without changing it (see
	// review.Decision).
	DistinctModels        int      `json:"distinctModels,omitempty"`
	AgreementIndependence string   `json:"agreementIndependence,omitempty"`
	DissentingSeats       []string `json:"dissentingSeats,omitempty"`
	// Consensus is `unanimous`, `majority` or `contested` over the seats that completed. A seat
	// that did not report a finding may not have looked there, so nothing is gated on it.
	Consensus string `json:"consensus,omitempty"`
	// Grounding is the non-executing citation check for this finding. It says whether the pointer
	// resolves, not whether the finding is true, and is not a disposition.
	Grounding *review.CitationGrounding `json:"grounding,omitempty"`
}

// PanelSeat is one seat of the executed roster: every requested seat appears in requested order,
// including one that halted or never started.
type PanelSeat struct {
	SeatID       string `json:"seatId"`
	Index        int    `json:"index"`
	Adapter      string `json:"adapter"`
	Model        string `json:"model"`
	Effort       string `json:"effort,omitempty"`
	Status       string `json:"status"`
	Rounds       int    `json:"rounds"`
	Findings     int    `json:"findings"`
	IdentityTier string `json:"identityTier,omitempty"`
	ReasonCode   string `json:"reasonCode,omitempty"`
	// Signal and Detail are this seat's own cause; haltRecord names only the first halting seat.
	Signal string `json:"signal,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// IdentityCaveat is one lane that ran and returned valid output but whose model identity was not
// strongly verified. Every caveat is projected.
type IdentityCaveat struct {
	Role           string `json:"role"`
	Adapter        string `json:"adapter"`
	RequestedModel string `json:"requestedModel"`
	ReportedModel  string `json:"reportedModel,omitempty"`
	Evidence       string `json:"evidence,omitempty"`
	Status         string `json:"status"`
}

// Withheld is one file a containment rule kept out of the reviewed set. Every withheld file is
// projected, so an unreviewed file is distinguishable from one that does not exist.
type Withheld struct {
	Path string `json:"path"`
	// Reason is meshcore's machine code (e.g. "workspace_hardlink_denied").
	Reason string `json:"reason"`
	Rule   string `json:"rule,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Stage is "copy" (never placed in the isolated copy) or "snippets" (in the copy, never in a
	// prompt).
	Stage string `json:"stage,omitempty"`
}

// HaltRecord summarizes a halted run: the halt class, reason code, exit code, optional signal and
// the human message.
type HaltRecord struct {
	HaltClass  string `json:"haltClass,omitempty"`
	ReasonCode string `json:"reasonCode"`
	ExitCode   int    `json:"exitCode"`
	Signal     string `json:"signal,omitempty"`
	Message    string `json:"message"`
	// Failure identifies the lane that caused the halt, when one did. Captured output is omitted
	// because this projection is printed and may be logged.
	Failure *LaneFailure `json:"failure,omitempty"`
}

// LaneFailure identifies the failing lane without its captured output.
type LaneFailure struct {
	Role       string `json:"role,omitempty"`
	Adapter    string `json:"adapter,omitempty"`
	Model      string `json:"model,omitempty"`
	ExitCode   int    `json:"exitCode"`
	HaltClass  string `json:"haltClass,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty"`
	Signal     string `json:"signal,omitempty"`
}

// Counts are host-computed totals, so a consumer need not recount the arrays.
type Counts struct {
	Findings        int            `json:"findings"`
	BySeverity      map[string]int `json:"bySeverity"`
	ByDisposition   map[string]int `json:"byDisposition"`
	IdentityCaveats int            `json:"identityCaveats"`
	// Withheld is how many files a containment rule kept out of the reviewed set.
	Withheld int `json:"withheld"`
	// PanelSeats is how many seats the panel requested and PanelSeatsCompleted how many produced a
	// valid result; they differ only on a halted run.
	PanelSeats          int `json:"panelSeats"`
	PanelSeatsCompleted int `json:"panelSeatsCompleted"`
	// Applied is how many findings the run wrote (the live tree in apply mode, the diff in patch
	// mode) and Refused how many it declined for a protected path. They are emitted together on a
	// write run; `refused > 0` is the not-clean signal.
	Applied int `json:"applied"`
	Refused int `json:"refused"`
}

// View is the whole projection of one run.
type View struct {
	SchemaVersion int    `json:"schemaVersion"`
	Status        string `json:"status"` // running | stable | planned | single_pass | halted | cancelled
	Mode          string `json:"mode"`   // the effective output mode
	// RequestedMode is set only when a surface ceiling lowered the requested mode.
	RequestedMode   string           `json:"requestedMode,omitempty"`
	RunID           string           `json:"runId,omitempty"`
	RunDir          string           `json:"runDir,omitempty"`
	Findings        []Finding        `json:"findings"`
	IdentityCaveats []IdentityCaveat `json:"identityCaveats"`
	// Withheld lists the files a containment rule kept out of the reviewed set; always present.
	Withheld []Withheld `json:"withheld"`
	// Authority is the inclusion manifest of the authority documents the run was judged against;
	// always present.
	Authority []review.AuthorityInclusion `json:"authority"`
	// Panel is the executed roster; always present, and empty only when no panel resolved.
	Panel []PanelSeat `json:"panel"`
	// Outcome is the write-outcome discriminator ("applied" | "partial_refusal" |
	// "nothing_applied"), present only on a patch or apply run.
	Outcome string `json:"outcome,omitempty"`
	// Refusals lists findings refused for a protected path; present (possibly empty) on a write run.
	Refusals []Refusal `json:"refusals,omitempty"`
	// Selection is what a narrowing `select` did on the first write window, including the
	// fingerprints that matched nothing. Absent when no selection was supplied.
	Selection *review.ApplySelection `json:"selection,omitempty"`
	// Grounding is the run-level citation-check tally, with a note stating its limits. Absent when
	// there was no reviewed tree to check.
	Grounding *review.GroundingSummary `json:"grounding,omitempty"`
	// Composition is what the blind panel was. It qualifies every agreementCount without changing
	// it.
	Composition *review.PanelComposition `json:"composition,omitempty"`
	// Dissent tallies the per-finding consensus labels, with a note stating what silence means.
	Dissent *review.DissentSummary `json:"dissent,omitempty"`
	// PartialPanel is present when a capacity failure removed a seat and the run continued. Read
	// agreement counts against its `answered` count.
	PartialPanel any `json:"partialPanel,omitempty"`
	// Scope is present when the run was narrowed to part of the tree.
	Scope any `json:"scope,omitempty"`
	// Verification is what the operator's build and test commands did on the containment copy.
	// Absent when none were supplied; it is a record, not a verdict on any finding.
	Verification *review.VerificationReport `json:"verification,omitempty"`
	// Shape is present only on a dry run (status "planned"), so an empty findings list can be told
	// apart from a run that looked at nothing.
	Shape      *review.RunShape `json:"shape,omitempty"`
	HaltRecord *HaltRecord      `json:"haltRecord,omitempty"`
	Counts     Counts           `json:"counts"`
}

// Refusal is one finding the write path declined because its target is a protected path. It is
// keyed on the host-computed fingerprint, not the model-authored finding id.
type Refusal struct {
	Fingerprint string `json:"fingerprint"`
	File        string `json:"file"`
	Reason      string `json:"reason"`
}

// Input is what Build needs: the run outcome, the requested mode, and the run's error.
type Input struct {
	Outcome review.RunOutcome
	// RequestedMode is the mode the caller asked for, or "" when it took the default.
	RequestedMode review.Mode
	// Err is the run error, nil on success. A non-nil error adds a HaltRecord.
	Err error
	// ExitCode, ReasonCode and Signal are the values the surface resolved for Err, so this package
	// does not depend on the fault mapping.
	ExitCode   int
	ReasonCode string
	Signal     string
}

// Build assembles the projection. A zero outcome yields a well-formed view whose arrays are empty,
// never null.
func Build(in Input) View {
	o := in.Outcome
	v := View{
		SchemaVersion:   SchemaVersion,
		Status:          o.Status,
		Mode:            string(o.Mode),
		RunID:           o.RunID,
		RunDir:          o.RunDir,
		Findings:        make([]Finding, 0, len(o.Findings)),
		IdentityCaveats: make([]IdentityCaveat, 0, len(o.IdentityCaveats)),
		Withheld:        make([]Withheld, 0, len(o.Withheld)),
		Panel:           make([]PanelSeat, 0, len(o.Panel)),
		Authority:       append(make([]review.AuthorityInclusion, 0, len(o.Authority)), o.Authority...),
		Counts: Counts{
			BySeverity:    map[string]int{},
			ByDisposition: map[string]int{},
		},
	}
	if in.RequestedMode != "" && in.RequestedMode != o.Mode {
		v.RequestedMode = string(in.RequestedMode)
	}
	// Everything below is copied from the outcome, never re-derived, so the projection cannot
	// disagree with the run record.
	v.Grounding = o.Grounding
	v.Composition = o.Composition
	v.Dissent = o.Dissent
	v.PartialPanel = o.PartialPanel
	v.Scope = o.Scope
	v.Verification = o.Verification
	v.Shape = o.Shape
	for i, f := range o.Findings {
		disposition := ""
		if i < len(o.Decisions) {
			disposition = string(o.Decisions[i].State)
		}
		severity := string(f.Severity)
		// A host severity adjustment overrides the reviewer's severity.
		if i < len(o.Decisions) && o.Decisions[i].SeverityAdjusted != "" {
			severity = string(o.Decisions[i].SeverityAdjusted)
		}
		var sources []string
		if f.Source != "" {
			sources = []string{f.Source}
		}
		var applyable *bool
		var refusal string
		if i < len(o.Decisions) && o.Decisions[i].Applyable != nil {
			val := *o.Decisions[i].Applyable
			applyable, refusal = &val, o.Decisions[i].ApplyRefusalReason
		}
		var support []review.SeatRef
		var agreement int
		var dissent []string
		var grounding *review.CitationGrounding
		var distinctModels int
		var independence string
		var consensus string
		if i < len(o.Decisions) {
			support, agreement, dissent = o.Decisions[i].SupportingSeats, o.Decisions[i].AgreementCount, o.Decisions[i].DissentingSeats
			consensus = o.Decisions[i].Consensus
			grounding = o.Decisions[i].Grounding
			distinctModels, independence = o.Decisions[i].DistinctModels, o.Decisions[i].AgreementIndependence
		}
		v.Findings = append(v.Findings, Finding{
			// The same fingerprint function the write path matches selectors against.
			ID: f.ID, Fingerprint: schema.Fingerprint(f), Severity: severity, Kind: string(f.Kind),
			File: f.File, Location: f.Location, Summary: f.Title,
			Disposition: disposition, Sources: sources,
			Applyable: applyable, ApplyRefusalReason: refusal,
			SupportingSeats: support, AgreementCount: agreement, DissentingSeats: dissent,
			DistinctModels: distinctModels, AgreementIndependence: independence, Consensus: consensus,
			Grounding: grounding,
		})
		v.Counts.BySeverity[severity]++
		v.Counts.ByDisposition[disposition]++
	}
	v.Counts.Findings = len(v.Findings)
	for _, c := range o.IdentityCaveats {
		v.IdentityCaveats = append(v.IdentityCaveats, IdentityCaveat{
			Role: c.Role, Adapter: c.Adapter, RequestedModel: c.RequestedModel,
			ReportedModel: c.ReportedModel, Evidence: string(c.Evidence), Status: c.Status,
		})
	}
	v.Counts.IdentityCaveats = len(v.IdentityCaveats)
	for _, w := range o.Withheld {
		v.Withheld = append(v.Withheld, Withheld{
			Path: w.Path, Reason: w.Reason, Rule: w.Rule, Detail: w.Detail, Stage: w.Stage,
		})
	}
	v.Counts.Withheld = len(v.Withheld)
	for _, s := range o.Panel {
		v.Panel = append(v.Panel, PanelSeat{
			SeatID: s.SeatID, Index: s.Index, Adapter: s.Adapter, Model: s.Model, Effort: s.Effort,
			Status: s.Status, Rounds: s.Rounds, Findings: s.Findings,
			IdentityTier: s.IdentityTier, ReasonCode: s.ReasonCode,
			Signal: s.Signal, Detail: s.Detail,
		})
		if s.Status == "completed" || s.Status == "budget_capped" {
			v.Counts.PanelSeatsCompleted++
		}
	}
	v.Counts.PanelSeats = len(v.Panel)

	// The write outcome is projected only for a mode that can write.
	if o.Mode == review.ModePatch || o.Mode == review.ModeApply {
		v.Refusals = make([]Refusal, 0, len(o.Refusals))
		for _, r := range o.Refusals {
			v.Refusals = append(v.Refusals, Refusal{Fingerprint: r.Fingerprint, File: r.File, Reason: r.Reason})
		}
		v.Counts.Applied, v.Counts.Refused = o.Applied, len(o.Refusals)
		v.Outcome = review.ApplyOutcome(o.Applied, len(o.Refusals))
		v.Selection = o.Selection
	}

	if in.Err != nil {
		if v.Status == "" || v.Status == "running" {
			v.Status = "halted"
		}
		h := &HaltRecord{
			ReasonCode: in.ReasonCode, ExitCode: in.ExitCode,
			Signal: in.Signal, Message: in.Err.Error(),
		}
		if o.Halt != nil {
			h.HaltClass = string(*o.Halt)
		}
		if f := o.Failure; f != nil {
			h.Failure = &LaneFailure{
				Role: f.Role, Adapter: f.Adapter, Model: f.Model, ExitCode: f.ExitCode,
				HaltClass: f.HaltClass, ReasonCode: f.ReasonCode, Signal: f.Signal,
			}
		}
		v.HaltRecord = h
	}
	return v
}
