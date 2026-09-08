// Package runview builds the machine-readable projection of a completed (or halted)
// review run.
//
// It is a PURE activity: one function over a RunOutcome (+ the error the run returned),
// no I/O, no re-reading of run artifacts, no re-derivation of dispositions. That matters
// because the projection must not become a second source of truth — it is assembled from
// the SAME adjudication result that `run-state.json` and `review-summary.md` are written
// from, so a machine consumer and a human reader can never be told different things about
// the same run.
//
// It is deliberately surface-agnostic. `aimesh review run --json` renders it today; a
// tool-calling surface is expected to return the same shape, so the contract a caller
// integrates against does not change with the transport it arrives over.
package runview

import (
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
)

// SchemaVersion is the projection's payload version (docs/schema/review-projection.schema.json).
const SchemaVersion = 1

// Finding is one adjudicated finding as a machine consumer sees it: what was found, where,
// and what the host DECIDED about it. `disposition` is the decision state — a consumer must
// never infer "this was fixed" from the presence of a finding.
type Finding struct {
	ID string `json:"id"`
	// Fingerprint is the HOST-COMPUTED identity of this finding — file and normalized
	// location, hashed. It is the selector for a SELECTIVE APPLY (`--select` / `select`), and it is
	// the only identifier that may be one.
	//
	// `id` is deliberately NOT that identifier and never can be: it is model-authored, and a model
	// that could relabel findings could otherwise steer which one a caller's "apply only this"
	// selects. It is additionally renumbered by the run after the write window, so it is not even
	// stable within one projection's own lifetime. This field is what a caller keeps.
	Fingerprint string   `json:"fingerprint"`
	Severity    string   `json:"severity"`
	Kind        string   `json:"kind"`
	File        string   `json:"file,omitempty"`
	Location    string   `json:"location,omitempty"`
	Summary     string   `json:"summary"`
	Disposition string   `json:"disposition"`
	Sources     []string `json:"sources,omitempty"`
	// Applyable is present (and false) ONLY when the WRITE-PATH RULE refused this finding:
	// an applied hunk must trace to evidence in the workspace copy, so a finding supported
	// only by authority/context text is reportable but never applyable. Its ABSENCE means
	// the write-path rule did not refuse it — not that anything was or will be written
	// (that is `disposition`).
	Applyable *bool `json:"applyable,omitempty"`
	// ApplyRefusalReason is the stable machine code for that refusal
	// ("authority_only" | "no_workspace_evidence" | "protected_path").
	ApplyRefusalReason string `json:"applyRefusalReason,omitempty"`
	// SupportingSeats / AgreementCount / DissentingSeats are the HOST-COMPUTED panel
	// provenance: which blind seats reported this finding, how many (never recount the array —
	// this is the number the host decided), and which seats that RAN did not report it. They are
	// absent for a finding that did not come from the blind panel (an evidence hook, the
	// cross-check, the verifier). Each SeatRef carries its identity tier for the reader; a weak
	// tier does not change the finding's disposition.
	SupportingSeats []review.SeatRef `json:"supportingSeats,omitempty"`
	AgreementCount  int              `json:"agreementCount,omitempty"`
	// DistinctModels and AgreementIndependence qualify that count WITHOUT changing it: how many
	// different MODELS the supporting seats ran, and `distinct_models` or `shared_model`. A shared
	// model means the agreeing seats share priors, so their errors correlate and the extra
	// agreement is worth less than the number suggests — it does NOT mean the finding is wrong.
	// Only the model string is asserted; vendor and base family are not observable and are not
	// claimed.
	DistinctModels        int      `json:"distinctModels,omitempty"`
	AgreementIndependence string   `json:"agreementIndependence,omitempty"`
	DissentingSeats       []string `json:"dissentingSeats,omitempty"`
	// Consensus is what the seats that RAN did with this finding: `unanimous`, `majority`, or
	// `contested` when a minority reported it or the seats split evenly. Absent when no panel stood
	// behind it, and absent when fewer than two seats completed.
	//
	// A SILENT SEAT IS NOT A SEAT THAT DISAGREED — a seat missing from SupportingSeats may have
	// disagreed, may never have reached that file, or may have stopped when its own set stabilized.
	// So `contested` is not evidence against the finding and nothing is gated on it: it marks the
	// finding worth reading rather than skimming.
	Consensus string `json:"consensus,omitempty"`
	// Grounding is what the host could verify about THIS finding's citation without executing
	// anything: whether the cited file exists, whether the cited lines are there, whether the named
	// symbol appears. Present on every finding of a run that had a tree to check against.
	//
	// IT IS NOT A DISPOSITION EITHER, and this is the field most likely to be misread as one. A
	// `file_missing` finding was raised, adjudicated and reported exactly as any other — the host can
	// see that a citation did not resolve, and it cannot see whether the defect is real one file
	// over. Read it as "how much of this pointer checks out", never as "how much of this finding is
	// true". `status: "grounded"` is a floor, not a corroboration.
	Grounding *review.CitationGrounding `json:"grounding,omitempty"`
}

// PanelSeat is one seat of the blind primary panel in the projection: what was REQUESTED and
// what actually ran. The array is the executed-roster echo — every requested seat appears, in
// requested order, including one that halted or never started, so a consumer can verify that the
// panel it asked for is the panel that executed.
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
	// Signal and Detail are THIS SEAT'S own actionable cause. On a halted panel the top-level
	// haltRecord carries only the first-by-index seat, so these are how a consumer learns that the
	// other dispatched (and paid-for) seats failed for different, separately fixable reasons.
	Signal string `json:"signal,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// IdentityCaveat is one lane that ran and returned valid output but whose model identity
// was not strongly verified. Governance rule: these are ALWAYS present in the projection
// when they exist — a consumer that ignores them is choosing to, rather than never being
// told.
type IdentityCaveat struct {
	Role           string `json:"role"`
	Adapter        string `json:"adapter"`
	RequestedModel string `json:"requestedModel"`
	ReportedModel  string `json:"reportedModel,omitempty"`
	Evidence       string `json:"evidence,omitempty"`
	Status         string `json:"status"`
}

// Withheld is one file a CONTAINMENT rule kept out of the reviewed set. Governance rule, the
// same one identityCaveats carries: these are ALWAYS present in the projection when they
// exist, because the alternative is that a file nobody reviewed is indistinguishable from a
// file that was never there — and a reviewer's silence about a file it was never shown reads
// as approval. A consumer that ignores them is choosing to.
type Withheld struct {
	Path string `json:"path"`
	// Reason is meshcore's stable machine code (e.g. "workspace_hardlink_denied").
	Reason string `json:"reason"`
	Rule   string `json:"rule,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Stage is "copy" (never placed in the isolated copy — so also never visible to an
	// agentic reviewer CLI working in it) or "snippets" (in the copy, never in a prompt).
	Stage string `json:"stage,omitempty"`
}

// HaltRecord is the machine-readable halt summary carried when a run halted: the taxonomy
// class, the STABLE reason code, the process exit code, an optional actionability signal,
// and the human message.
type HaltRecord struct {
	HaltClass  string `json:"haltClass,omitempty"`
	ReasonCode string `json:"reasonCode"`
	ExitCode   int    `json:"exitCode"`
	Signal     string `json:"signal,omitempty"`
	Message    string `json:"message"`
	// Failure describes the lane that caused the halt, when one did. Excerpts are
	// deliberately NOT included: this projection is printed to stdout and may be logged.
	Failure *LaneFailure `json:"failure,omitempty"`
}

// LaneFailure is the sanitized identification of the failing lane (no captured output).
type LaneFailure struct {
	Role       string `json:"role,omitempty"`
	Adapter    string `json:"adapter,omitempty"`
	Model      string `json:"model,omitempty"`
	ExitCode   int    `json:"exitCode"`
	HaltClass  string `json:"haltClass,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty"`
	Signal     string `json:"signal,omitempty"`
}

// Counts are host-computed totals. They exist so a consumer never has to (and never
// should) recount the arrays itself and reach a different number.
type Counts struct {
	Findings        int            `json:"findings"`
	BySeverity      map[string]int `json:"bySeverity"`
	ByDisposition   map[string]int `json:"byDisposition"`
	IdentityCaveats int            `json:"identityCaveats"`
	// Withheld is how many files a containment rule kept out of the reviewed set. It is a
	// first-class count rather than something a consumer derives, for the same reason the
	// array is always present: "0" is a meaningful, checkable statement.
	Withheld int `json:"withheld"`
	// PanelSeats is the number of seats the blind primary panel REQUESTED, PanelSeatsCompleted
	// how many produced a valid result. They are separate on purpose: a consumer must be able to
	// see "5 requested, 4 completed" rather than being handed one number that hides it. (A run
	// only reaches a successful projection with every seat completed — a seat failure halts —
	// so a gap appears exactly on a halted run.)
	PanelSeats          int `json:"panelSeats"`
	PanelSeatsCompleted int `json:"panelSeatsCompleted"`
	// Applied is how many findings this run wrote (to the live tree in apply mode, to the diff in
	// patch mode), and Refused how many it declined because their target is a PROTECTED PATH.
	// They are a PAIR and are always emitted together on a write run: the coarse "not clean"
	// signal a consumer keys on is `refused > 0`, and a refusal count with no applied count
	// beside it invites exactly the wrong reading ("nothing worked").
	Applied int `json:"applied"`
	Refused int `json:"refused"`
}

// View is the whole projection of one run.
type View struct {
	SchemaVersion int    `json:"schemaVersion"`
	Status        string `json:"status"` // running | stable | planned | single_pass | halted | cancelled
	Mode          string `json:"mode"`   // the EFFECTIVE output mode
	// RequestedMode is set only when the effective mode differs from what was asked for
	// (a surface policy ceiling degraded it) — its presence is the signal, so a consumer
	// cannot miss a silent downgrade.
	RequestedMode   string           `json:"requestedMode,omitempty"`
	RunID           string           `json:"runId,omitempty"`
	RunDir          string           `json:"runDir,omitempty"`
	Findings        []Finding        `json:"findings"`
	IdentityCaveats []IdentityCaveat `json:"identityCaveats"`
	// Withheld lists the files a containment rule kept out of the reviewed set — see Withheld.
	// ALWAYS present (empty when nothing was withheld), so "nothing was withheld" is a stated
	// fact rather than an absent key a consumer has to interpret.
	Withheld []Withheld `json:"withheld"`
	// Authority is the INCLUSION MANIFEST of the authority/context documents this run was
	// judged against: per document the source, the full-content and embedded-content hashes,
	// the byte counts and whether the inclusion was complete. It is ALWAYS present (empty
	// when none was declared) — "which intent was this judged against" is a governance fact,
	// so a consumer is never left to infer it from an absent key.
	Authority []review.AuthorityInclusion `json:"authority"`
	// Panel is the blind primary panel's executed roster: requested seats vs what ran. It is
	// ALWAYS present (empty only for a run that never resolved a panel), because the number of
	// independent vantages behind a finding set is a governance fact a consumer must not have to
	// infer — and because the count is variable, there is no default a reader could assume.
	Panel []PanelSeat `json:"panel"`
	// Outcome is the write-outcome discriminator ("applied" | "partial_refusal" |
	// "nothing_applied"), present only on a run that could write (patch/apply). Its ABSENCE means
	// "not a write run"; a consumer that ignores it is less informed but never wrong, because the
	// not-clean signal is `counts.refused > 0` and the surface's own coarse channel.
	Outcome string `json:"outcome,omitempty"`
	// Refusals lists the findings refused because their target is a protected path. It is ALWAYS
	// present on a write run (empty when nothing was refused) for the same reason `withheld` is:
	// "nothing was refused" is a checkable statement, and an absent key is not.
	Refusals []Refusal `json:"refusals,omitempty"`
	// Selection is what a narrowing `--select` / `select` did on this run's first write window:
	// which host-computed fingerprints were requested, which matched a finding in the run's
	// accepted set, and which matched nothing. Absent when no selection was supplied.
	//
	// The `unmatched` list is why this is projected at all rather than left implicit in the
	// applied set: a mistyped or stale selector writes nothing, and a caller that is not told
	// would read a smaller-than-expected apply as a smaller-than-expected finding set.
	Selection *review.ApplySelection `json:"selection,omitempty"` // Grounding is the run-level tally of the non-executing citation check: how many findings point
	// at a file, line or symbol that is actually there. Absent when there was no reviewed tree to
	// check against.
	//
	// It is projected at the top level so a consumer must not
	// have to scan findings to learn a property of the run. And it carries its own `note`, which
	// states what grounding does NOT establish — a grounded finding may still be wrong. A consumer
	// that surfaces the ratio without the note is reporting a stronger claim than the run made.
	Grounding *review.GroundingSummary `json:"grounding,omitempty"`
	// Composition is what the blind panel WAS: its seats and how many distinct models they ran
	// between them. It qualifies every agreementCount in this projection WITHOUT changing any of
	// them — a shared model means the agreeing seats share priors, not that the finding is wrong.
	Composition *review.PanelComposition `json:"composition,omitempty"`
	// Dissent is how much of this result the panel agreed on: the tally of the per-finding
	// `consensus` labels, with the fixed note stating what silence does and does not mean. A
	// consumer that surfaces the contested count without the note has turned a reading prompt into
	// a verdict, which is the one thing it is not.
	Dissent *review.DissentSummary `json:"dissent,omitempty"`
	// PartialPanel is present when a CAPACITY failure removed a seat and the run continued at a
	// smaller denominator. Its PRESENCE is the signal — a consumer never has to compare two counts
	// to notice that the panel configured is not the panel that answered. Read every agreement
	// count against `answered`, and pass on the `note`.
	PartialPanel any `json:"partialPanel,omitempty"`
	// Scope is present when this run was NARROWED to part of the tree. Its PRESENCE is the signal:
	// a narrowed review is silent about everything it was not shown, and that silence must not read
	// as approval. Absent on a full-tree run.
	Scope any `json:"scope,omitempty"`
	// Verification is what the operator's OWN build/test commands did on the containment copy, before
	// and after the accepted findings were applied to it. Absent when none were supplied.
	//
	// Read `delta` first, and pass on the `note`. Neither `unchanged_pass` nor `broken` is a verdict
	// on any finding: nothing here dropped, downgraded or blocked anything, and the second pass runs
	// after the commit precisely so that it cannot.
	Verification *review.VerificationReport `json:"verification,omitempty"`
	// Shape is what the run WOULD have done, present ONLY on a dry run and absent on every real
	// one. Its presence is the machine-readable form of `status: "planned"`: a consumer that
	// reads `findings: []` off a run must be able to tell "nothing was found" from "nothing was
	// looked at", and this key is the difference.
	Shape      *review.RunShape `json:"shape,omitempty"`
	HaltRecord *HaltRecord      `json:"haltRecord,omitempty"`
	Counts     Counts           `json:"counts"`
}

// Refusal is one finding the write path declined because its target is a protected path. Nothing
// was written to it; every other accepted finding was applied. It is keyed on the HOST-COMPUTED
// fingerprint rather than the model-authored finding id — the id is renumbered after the write
// window, and a model-controlled identifier is not a safe selection key.
type Refusal struct {
	Fingerprint string `json:"fingerprint"`
	File        string `json:"file"`
	Reason      string `json:"reason"`
}

// Input is what Build needs: the run outcome, the mode the caller asked for, and the
// error the run returned (nil on success).
type Input struct {
	Outcome review.RunOutcome
	// RequestedMode is the mode the caller asked for ("" when it did not ask for one and
	// took the surface default).
	RequestedMode review.Mode
	// Err is the run error. When it is a halt, the projection carries a HaltRecord and
	// status "halted" — the projection is emitted for halts too, so an automated caller
	// gets the taxonomy instead of only an exit code.
	Err error
	// ExitCode / ReasonCode / Signal are the taxonomy values the SURFACE resolved for Err
	// (via meshcore/fault). They are passed in rather than re-derived so this package stays
	// free of any dependency on the fault mapping.
	ExitCode   int
	ReasonCode string
	Signal     string
}

// Build assembles the projection. It is total: a zero outcome yields a well-formed view
// with empty (never null) arrays, so a consumer can index without null-checking.
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
	// Carried through verbatim — the host decided each of these once, during the run. A projection
	// that re-derived one could disagree with the record it is projecting.
	v.Grounding = o.Grounding
	v.Composition = o.Composition
	v.Dissent = o.Dissent
	v.PartialPanel = o.PartialPanel
	v.Scope = o.Scope
	v.Verification = o.Verification
	// Carried through verbatim; nil (and so absent) on every run that actually reviewed
	// something, whose shape stopped being a prediction the moment it spent.
	v.Shape = o.Shape
	for i, f := range o.Findings {
		disposition := ""
		if i < len(o.Decisions) {
			disposition = string(o.Decisions[i].State)
		}
		severity := string(f.Severity)
		// A host severity adjustment is the authoritative severity — reporting the
		// reviewer's original would misstate what the host actually decided.
		if i < len(o.Decisions) && o.Decisions[i].SeverityAdjusted != "" {
			severity = string(o.Decisions[i].SeverityAdjusted)
		}
		var sources []string
		if f.Source != "" {
			sources = []string{f.Source}
		}
		// The write-path rule's refusal is carried through verbatim — it is decided ONCE, by
		// the host, and must never be re-derived here into a different answer.
		var applyable *bool
		var refusal string
		if i < len(o.Decisions) && o.Decisions[i].Applyable != nil {
			val := *o.Decisions[i].Applyable
			applyable, refusal = &val, o.Decisions[i].ApplyRefusalReason
		}
		// The panel provenance is carried through VERBATIM — it is host arithmetic decided once,
		// during adjudication, and re-deriving it here would create a second answer.
		var support []review.SeatRef
		var agreement int
		var dissent []string // Same rule for the citation grounding: it was decided once, against the tree as it stood at
		// the end of the run, and re-checking it here (against a tree that may have moved since)
		// would let the projection contradict the run record it is projecting.
		var grounding *review.CitationGrounding
		// Carried through verbatim for the same reason as everything else here: the host decided
		// once, and a projection that re-derived independence from a roster read later could
		// disagree with the run record it is projecting.
		var distinctModels int
		var independence string
		// Carried through verbatim on the same rule: the host labelled this during the run, over the
		// seats that completed then. Re-deriving it here from a roster read later could disagree with
		// the run record it is projecting.
		var consensus string
		if i < len(o.Decisions) {
			support, agreement, dissent = o.Decisions[i].SupportingSeats, o.Decisions[i].AgreementCount, o.Decisions[i].DissentingSeats
			consensus = o.Decisions[i].Consensus
			grounding = o.Decisions[i].Grounding
			distinctModels, independence = o.Decisions[i].DistinctModels, o.Decisions[i].AgreementIndependence
		}
		v.Findings = append(v.Findings, Finding{
			// The fingerprint is COMPUTED FROM THE FINDING, here, by the host — the same function
			// the dedup, the provenance ledger and the write path's refusals use, so a selector a
			// caller reads here names exactly what the write path will match it against.
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
	// Carried through verbatim: which files were withheld, and by which rule, is decided ONCE
	// by the containment layer. Re-deriving anything here would create a second answer.
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

	// The write outcome. It is carried ONLY for a mode that can write: on a report run there is
	// nothing for `applied`/`refused` to mean, and emitting `outcome: "nothing_applied"` for a
	// review that was never asked to write anything would be a fact-shaped non-fact.
	if o.Mode == review.ModePatch || o.Mode == review.ModeApply {
		v.Refusals = make([]Refusal, 0, len(o.Refusals))
		for _, r := range o.Refusals {
			v.Refusals = append(v.Refusals, Refusal{Fingerprint: r.Fingerprint, File: r.File, Reason: r.Reason})
		}
		v.Counts.Applied, v.Counts.Refused = o.Applied, len(o.Refusals)
		v.Outcome = review.ApplyOutcome(o.Applied, len(o.Refusals))
		// The narrowing selection, carried through verbatim from the write window that reported it.
		// Like everything else here it is not re-derived: the write path decided what matched, and
		// a projection that recomputed it could disagree with the receipt.
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

// SortedKeys returns a map's keys in a stable order. Encoded JSON objects are already
// key-sorted by encoding/json; this exists for callers rendering counts as text.
func SortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
