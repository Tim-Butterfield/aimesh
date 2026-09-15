// Package pipeline runs an exploration: a blind, parallel explorer fan-out followed by the mode's
// terminal step (collate, canonicalize, or a host-computed fixed-space result). Model calls go through
// meshcore/model and identity is classified by meshcore/verify. Model output is untrusted and is only
// parsed as JSON data.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/verify"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Registry maps an adapter name to its meshcore adapter.
type Registry map[string]model.Adapter

// maxDroppedRaw bounds the raw body a Dropped record keeps for --dump-run. FullLen and FullSHA256 still
// describe the whole stream when it is clipped.
const maxDroppedRaw = 64 << 10

// Dropped records an explorer removed from the panel (failed, timed out or schema-invalid) with its
// reason and a bounded copy of the body the pipeline tried to parse.
type Dropped struct {
	Explorer schema.ExplorerIdentity
	Reason   string
	// Raw is a copy of the rejected body, clipped to maxDroppedRaw. It is empty when the drop happened
	// before the model call.
	Raw         []byte
	Truncated   bool     // Raw was clipped to maxDroppedRaw
	CapturedLen int      // len(Raw)
	FullLen     int      // len of the full raw stream before clipping
	SHA256      string   // SHA-256 of the captured (bounded) bytes
	FullSHA256  string   // SHA-256 of the full stream (== SHA256 when not truncated)
	Repairs     []string // extraction repairs applied before the parse that dropped it (nil if pre-parse)
	// ExitCode and StderrExcerpt are the adapter process's failure signal. A CLI that refuses a request
	// usually explains why on stderr and writes nothing to stdout, so these carry the real cause.
	ExitCode      int
	StderrExcerpt string
}

// maxStderrExcerpt bounds the adapter stderr excerpt carried in a drop.
const maxStderrExcerpt = 2 << 10 // 2 KiB

// stderrExcerpt returns the last three non-blank lines of adapter stderr joined into one line and clipped
// to maxStderrExcerpt. A CLI prints its error last.
func stderrExcerpt(b []byte) string {
	var lines []string
	for ln := range strings.SplitSeq(string(b), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	out := strings.Join(lines, " | ")
	if len(out) > maxStderrExcerpt {
		out = out[:maxStderrExcerpt] + "…(clipped)"
	}
	return out
}

// failureContext formats an adapter Result's non-zero exit code and stderr excerpt for a drop reason. It
// returns "" when the process exited cleanly with empty stderr.
func failureContext(r model.Result) string {
	var parts []string
	if r.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit code %d", r.ExitCode))
	}
	if ex := stderrExcerpt(r.Stderr); ex != "" {
		parts = append(parts, "stderr: "+ex)
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, "; ") + "]"
}

// withResult records r's exit code and stderr excerpt on d, appends them to d.Reason, and returns d.
func (d *Dropped) withResult(r model.Result) *Dropped {
	d.ExitCode = r.ExitCode
	d.StderrExcerpt = stderrExcerpt(r.Stderr)
	d.Reason += failureContext(r)
	return d
}

// newDrop builds a Dropped for id. raw is the body the pipeline tried to parse (empty for a drop before
// the call) and repairs are the extraction repairs applied to it. Hashes of both the captured slice and
// the full stream are recorded.
func newDrop(id schema.ExplorerIdentity, reason string, raw []byte, repairs []string) *Dropped {
	full := sha256.Sum256(raw)
	captured := raw
	truncated := false
	if len(captured) > maxDroppedRaw {
		captured = captured[:maxDroppedRaw]
		truncated = true
	}
	capSum := sha256.Sum256(captured)
	d := &Dropped{
		Explorer: id, Reason: reason,
		Truncated: truncated, CapturedLen: len(captured), FullLen: len(raw),
		SHA256:     hex.EncodeToString(capSum[:]),
		FullSHA256: hex.EncodeToString(full[:]),
		Repairs:    repairs,
	}
	if len(captured) > 0 {
		d.Raw = append([]byte(nil), captured...)
	}
	return d
}

// Result is a run's outcome, filled in as far as the run progressed, so a halted run still carries what
// it gathered.
type Result struct {
	// Mode is the mode this run executed, stamped by the pipeline so the run directory records it even
	// when the task left the mode to its default.
	Mode           string `json:"mode,omitempty"`
	Formulation    schema.Formulation
	CollatorStatus schema.IdentityStatus
	CollatorCaveat string
	Envelopes      []schema.Envelope // explorer envelopes (verified + weak), in stable order
	Dropped        []Dropped
	// Output is the mode's terminal output (for example schema.CollatorOutput for Map), or nil if the run
	// halted before it. It marshals as its concrete type.
	Output mode.ModeOutput
	// Canonicalization is the merge ledger and partition for a canonicalizing mode. It is set only when the
	// surjectivity gate passed. After a confirmation round it is the confirmed revision.
	Canonicalization *canon.Result `json:"canonicalization,omitempty"`
	// Provisional is the partition before confirmation, retained when a confirmation round produced a new
	// revision.
	Provisional *canon.Result `json:"provisional,omitempty"`
	// Confirmation is the confirmation-round record, set when the mode's policy enables confirmation.
	Confirmation *canon.Confirmation `json:"confirmation,omitempty"`
	// Preflight holds the identity pre-flight verdicts for the collator and each canonicalizer.
	Preflight []PreflightRecord `json:"preflight,omitempty"`
	// Panel is the frozen panel, its counting-policy hash and the participation outcome.
	Panel govern.Panel `json:"panel"`
	// Rounds are the executed explorer rounds in order. Rounds[0] is the blind first round, the baseline
	// for every independence count.
	Rounds []round.Round `json:"rounds,omitempty"`
	// Mediations record, for each later round, the pooled artifact and the digest shown to each explorer.
	Mediations []round.Mediation `json:"mediations,omitempty"`
	// Governance is the frozen panel, the emitted claims and the quarantined collator narrative. It is nil
	// for a mode that emits no counts.
	Governance *govern.Report `json:"governance,omitempty"`
	// Decision is the host-tallied ballot decision for a ballot-bearing mode. Its frozen inputs are set
	// before the ballot round, so a run that halts mid-ballot still records them.
	Decision *govern.Decision `json:"decision,omitempty"`
	// BallotNarrative is the voters' stated reasoning, kept out of the ballot record and added to
	// Governance.CollatorNarrative.
	BallotNarrative []govern.Narrative `json:"-"`
	// Degraded is the fallback artifact built from the blind first-round envelopes when the collator became
	// unavailable or an identity halt fired.
	Degraded *schema.DegradedOutput `json:"degraded,omitempty"`
	// CanonicalizerStatus and CanonicalizerCaveat are the first canonicalizer call's identity verdict.
	CanonicalizerStatus schema.IdentityStatus `json:"canonicalizerStatus,omitempty"`
	CanonicalizerCaveat string                `json:"canonicalizerCaveat,omitempty"`
	// CanonicalizerCalls records each canonicalizer call's role, identity and verdict.
	CanonicalizerCalls []CanonicalizerRecord `json:"canonicalizerCalls,omitempty"`
	// CanonicalizerProvenance is ProvenanceExplicit or ProvenanceDerived; empty for a non-canonicalizing mode.
	CanonicalizerProvenance string `json:"canonicalizerProvenance,omitempty"`
	// CanonicalizerIndependence is IndependenceDistinct or IndependenceSharedModel on the dual path, and
	// empty otherwise. Every corroboration count depends on it.
	CanonicalizerIndependence string `json:"canonicalizerIndependence,omitempty"`
	// SynthesizePrompt, CanonicalizerPrompt, RawSynthesis and RawCanonicalization are captured for
	// --dump-run only. The raw bodies are kept even when parsing fails.
	SynthesizePrompt    string `json:"-"`
	CanonicalizerPrompt string `json:"-"`
	RawSynthesis        []byte `json:"-"`
	RawCanonicalization []byte `json:"-"`
	// ConfirmationPrompt and LaterRoundPrompts are the prompts shown to the panel after round 1, captured
	// for --dump-run only.
	ConfirmationPrompt string   `json:"-"`
	LaterRoundPrompts  []string `json:"-"`
	// Shape is the priced plan of a dry run; nil on a real run.
	Shape *Shape `json:"shape,omitempty"`
}

// isolatedWorkDir creates an empty temporary directory for one model call and returns it with a cleanup
// function. Model CLIs run there rather than in the user's project, so they cannot read the user's tree.
func isolatedWorkDir() (string, func(), error) {
	d, err := os.MkdirTemp("", "exploremesh-call-")
	if err != nil {
		return "", func() {}, err
	}
	return d, func() { _ = os.RemoveAll(d) }, nil
}

// Run resolves raw's mode from the registry and runs it with RunSpec. It returns a fault error on halt;
// the Result carries the partial state.
//
// onEvent is an optional progress hook. It is called from several goroutines, so it must be safe for
// concurrent use and must not block.
func Run(ctx context.Context, reg Registry, plan roster.Plan, raw schema.RawTask, opts Options, onEvent func(audit.EventLine)) (Result, error) {
	var res Result
	if err := raw.Validate(); err != nil {
		return res, err
	}
	spec, ok := mode.Resolve(raw.Mode)
	if !ok {
		herr := fault.New(fault.Config, fmt.Sprintf("unknown mode %q (known modes: %s)", raw.Mode, strings.Join(mode.Names(), ", ")))
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
	return RunSpec(ctx, reg, plan, raw, spec, opts, onEvent)
}

// Options are optional per-run overrides. The zero value is the default behavior.
type Options struct {
	// Canonicalizers overrides the plan's canonicalizer identities. It is empty or exactly two (see
	// roster.ValidateCanonicalizers). The surfaces set canonicalizers on the plan instead.
	Canonicalizers []roster.Explorer
	// MaxParallel bounds how many explorers run at once; 0 runs the whole panel in parallel. It limits
	// parallelism only: every explorer still receives the same payload.
	MaxParallel int
	// DryRun performs every check that makes no model call, fills in Result.Shape, and returns before the
	// identity pre-flight, which is the first model call.
	DryRun bool
	// VerifyReadiness makes a bounded one-token call to each distinct adapter, model and effort before the
	// identity pre-flight and halts if any fails. A dry run prices these calls without making them.
	VerifyReadiness bool
}

// RunSpec runs an exploration under an explicit mode contract instead of one resolved from the registry,
// so tests and examples can run unregistered contracts.
func RunSpec(ctx context.Context, reg Registry, plan roster.Plan, raw schema.RawTask, spec mode.ModeSpec, opts Options, onEvent func(audit.EventLine)) (Result, error) {
	var res Result
	res.Mode = spec.Name
	if err := raw.Validate(); err != nil {
		return res, err
	}
	// Mode-specific task inputs (such as Challenge's artifact) are checked before anything is spent.
	if terr := spec.CheckTask(raw); terr != nil {
		herr := fault.Wrap(fault.Config, "mode task contract", terr)
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
	// The round count is fixed by the contract. A data-dependent stopping rule could be steered by an
	// aggressively merging canonicalizer.
	rounds, cerr := spec.RoundCount()
	if cerr != nil {
		herr := fault.Wrap(fault.Config, "mode round contract", cerr)
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
	if rounds > 1 {
		if spec.LaterRound == nil {
			herr := fault.New(fault.Config, fmt.Sprintf("mode %q declares %d rounds but supplies no LaterRound contract", spec.Name, rounds))
			emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			return res, herr
		}
		if spec.Canonicalizing == nil {
			// Later rounds are fed the confirmed canonical entities. Pooling uncanonicalized peer output would
			// be hidden entity resolution by the host.
			herr := fault.New(fault.Config, fmt.Sprintf("mode %q declares %d rounds but is not canonicalizing — a later round may only carry the pooled CONFIRMED-canonical uniques; pooling un-canonicalized peer items would be covert entity resolution", spec.Name, rounds))
			emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			return res, herr
		}
	}
	// A mode sets exactly one terminal contract. Checking here also covers specs passed directly to RunSpec.
	terminals := 0
	for _, set := range []bool{spec.Collator != nil, spec.Canonicalizing != nil, spec.FixedSpace != nil} {
		if set {
			terminals++
		}
	}
	if terminals != 1 {
		herr := fault.New(fault.Config, fmt.Sprintf("mode %q declares %d terminal contracts — a mode must set EXACTLY ONE of Collator, Canonicalizing or FixedSpace", spec.Name, terminals))
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
	// Each mode supplies its own explorer prompt and schema, so the collator never authors the payload it
	// later collates. A collator-formulated first round is not implemented.
	if !spec.FormulationFree {
		herr := fault.New(fault.Config, fmt.Sprintf("mode %q is not formulation-free — collator-formulated round-1 is not implemented; a mode must own its explorer prompt and schema", spec.Name))
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
	collAdapter, ok := reg[plan.Collator.Adapter]
	if !ok {
		emit(onEvent, "error", "halt", fmt.Sprintf("collator adapter %q not registered", plan.Collator.Adapter), nil)
		return res, fault.New(fault.Config, fmt.Sprintf("collator adapter %q not registered", plan.Collator.Adapter))
	}
	run := &runner{
		ctx: ctx, reg: reg, plan: plan, raw: raw, spec: spec, opts: opts,
		onEvent: onEvent, ids: newIdentityLedger(), collAdapter: collAdapter, res: &res,
	}

	// Dry run: every free check has passed and the next step calls a model. shapeOf resolves the
	// canonicalizer identities, so an underivable dual pair fails here at no cost.
	if opts.DryRun {
		probes := 0
		if opts.VerifyReadiness {
			probes = len(readinessTargets(reg, plan))
		}
		shape, serr := shapeOf(plan, raw, spec, opts, rounds, probes)
		if serr != nil {
			emit(onEvent, "error", "halt", serr.Error(), map[string]any{"haltClass": haltClassOf(serr)})
			return res, serr
		}
		res.Shape = &shape
		emit(onEvent, "info", "dry_run", fmt.Sprintf("dry run: %d model call(s) priced, none made — stopped before the identity pre-flight, which is this run's first spend", shape.ModelCalls), map[string]any{
			"mode": shape.Mode, "modelCalls": shape.ModelCalls, "explorers": len(shape.Explorers),
			"rounds": shape.Rounds, "payloadHash": shape.Payload.PayloadHash,
		})
		return res, nil
	}

	if opts.VerifyReadiness {
		if herr := verifyReadiness(ctx, reg, plan, onEvent); herr != nil {
			emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			return res, herr
		}
	}

	// Identity pre-flight and panel freeze, both before any explorer runs.
	if herr := run.preflightRoles(); herr != nil {
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
	pan, perr := govern.Freeze(panelIdentities(plan), govern.DefaultCountingPolicy())
	if perr != nil {
		return res, fault.Wrap(fault.Config, "freeze panel", perr)
	}
	res.Panel = pan
	emit(onEvent, "info", "panel_frozen", fmt.Sprintf("panel frozen: %d member(s); counting policy hashed (%s)", pan.Selected, pan.PolicyHash[:12]), map[string]any{
		"selected": pan.Selected, "policyHash": pan.PolicyHash, "rulesVersion": govern.RulesVersion,
	})

	// Build the shared explorer payload from the mode's own prompt and schema.
	emit(onEvent, "info", "formulate_start", "formulation-free ("+spec.Name+"): building the app-owned explorer payload (no collator formulate call)", map[string]any{
		"mode": spec.Name, "formulationFree": true, "explorers": len(plan.Explorers),
	})
	res.Formulation = schema.FormulationFree(schema.FormulationFreeMap, spec.Prompt(raw), spec.ExplorerSchema())
	emit(onEvent, "info", "formulate_done", "formulation source: "+string(res.Formulation.Source), map[string]any{
		"source": string(res.Formulation.Source), "collatorStatus": string(res.CollatorStatus),
	})
	payload := res.Formulation.Payload
	payloadHash, _ := payload.Hash()

	// Blind first round: every explorer gets the same payload. These envelopes are the baseline for every
	// independence count.
	envs, drops, halt := run.fanout(1, schema.PhaseExplore, payload, payloadHash)
	res.Dropped = append(res.Dropped, drops...)
	res.Envelopes = append(res.Envelopes, envs...)
	if halt != nil {
		emit(onEvent, "error", "halt", "explorer identity halt: "+halt.Error(), map[string]any{"haltClass": haltClassOf(halt)})
		res.Degraded = run.degrade(schema.DegradedIdentityHalt, halt.Error())
		return res, halt
	}
	res.Rounds = append(res.Rounds, round.NewRound(1, true, payloadHash, res.Envelopes, nil))

	// At least two explorers must respond with a position. Identity is not consulted (see
	// docs/model-identity.md); abstentions stay in the record but are excluded from the primary panel.
	var primary, abstained []schema.Envelope
	for _, e := range res.Envelopes {
		if schema.IsAbstention(e.Response) {
			abstained = append(abstained, e)
			continue
		}
		primary = append(primary, e)
	}
	if len(primary) < 2 {
		herr := fault.New(fault.Config, fmt.Sprintf(
			"primary synthesis requires >=2 responding explorers, have %d (abstained: %d, dropped: %d) — a one-response panel is not a panel",
			len(primary), len(abstained), len(res.Dropped)))
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
	// Technical absences and deliberate abstentions are counted separately.
	res.Panel = pan.WithOutcome(govern.Outcome{
		Dispatched:           len(plan.Explorers),
		Eligible:             len(primary),
		TechnicalAbsence:     len(res.Dropped),
		DeliberateAbstention: len(abstained),
	})

	if spec.Canonicalizing != nil {
		return run.canonicalizingPath(primary, payloadHash, rounds)
	}
	if spec.FixedSpace != nil {
		return run.fixedSpacePath(primary, payloadHash)
	}

	// Plain collate.
	emit(onEvent, "info", "synthesize_start", fmt.Sprintf("collator synthesizing %d explorer response(s)", len(primary)), map[string]any{
		"primary": len(primary), "abstained": len(abstained),
	})
	sprompt, err := spec.Collator.Prompt(primary)
	if err != nil {
		return res, fault.Wrap(fault.Internal, "collator synthesize prompt", err)
	}
	res.SynthesizePrompt = sprompt
	swork, scleanup, swerr := isolatedWorkDir()
	if swerr != nil {
		return res, fault.Wrap(fault.Internal, "isolated work dir (synthesize)", swerr)
	}
	sres, serr := collAdapter.Invoke(ctx, model.Call{
		Role: "collator", Phase: schema.PhaseSynthesize,
		Model: plan.Collator.Model, ModelArg: core.ModelArg(plan.Collator.Model), Effort: plan.Collator.Effort,
		WorkDir: swork, Prompt: sprompt,
	})
	scleanup()
	res.RawSynthesis = append([]byte(nil), responseBody(sres)...) // captured even when the call or parse fails
	if serr != nil {
		herr := fault.Wrap(fault.Internal, "collator synthesize failed (raw explorer responses preserved for re-run)", serr)
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		res.Degraded = run.degrade(schema.DegradedCollatorUnavailable, serr.Error())
		return res, herr
	}
	// The collator's identity is recorded at this call and never gates the synthesis.
	cstatus, _ := classify(collAdapter, sres, plan.Collator.Model, plan.Collator.Adapter)
	res.CollatorStatus = cstatus
	res.CollatorCaveat = collatorIdentityCaveat(cstatus)
	if herr := run.ids.observe("collator", sres.ActualModel); herr != nil {
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		res.Degraded = run.degrade(schema.DegradedIdentityHalt, herr.Error())
		return res, herr
	}
	out, oerr := spec.Collator.Parse(responseBody(sres))
	if oerr != nil {
		herr := fault.Wrap(fault.Internal, "collator synthesis output invalid (raw explorer responses preserved)", oerr)
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		res.Degraded = run.degrade(schema.DegradedCollatorUnavailable, "collator output invalid: "+oerr.Error())
		return res, herr
	}
	// The host checks each finding's `envelope#k` citations against this panel's envelopes, keeps the ones
	// that resolve and labels uncited findings. A bad citation never halts the run.
	if co, ok := out.(schema.CollatorOutput); ok {
		cites := schema.ApplyCitations(&co, primary)
		out = co
		emit(onEvent, "info", "citations_validated", "citations: "+cites.String(), map[string]any{
			"findings": cites.Findings, "cited": cites.Cited, "uncited": cites.Uncited,
			"refsKept": cites.RefsKept, "refsDropped": cites.RefsDropped,
		})
	}
	res.Output = out
	emit(onEvent, "info", "synthesize_done", "synthesis complete", map[string]any{
		"mode": spec.Name, "summary": out.Summary(),
	})
	return res, nil
}

// emit sends a timestamped EventLine to onEvent, if it is set.
func emit(onEvent func(audit.EventLine), level, eventType, message string, data map[string]any) {
	if onEvent == nil {
		return
	}
	onEvent(audit.EventLine{
		SchemaVersion: 1,
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		Level:         level,
		EventType:     eventType,
		Message:       message,
		Data:          data,
	})
}

// haltClassOf returns a short halt-class label for err: the fault's halt class if set, else its code's
// name, else "internal".
func haltClassOf(err error) string {
	if f, ok := errors.AsType[*fault.Fault](err); ok {
		if f.Halt != "" {
			return f.Halt
		}
		switch f.Code {
		case fault.Config:
			return "config"
		case fault.Usage:
			return "usage"
		case fault.Adapter:
			return "adapter"
		case fault.Model:
			return "model"
		case fault.Policy:
			return "policy"
		default:
			return "internal"
		}
	}
	return "internal"
}

// CanonicalizerRecord is one canonicalizer call's role, identity and identity verdict.
type CanonicalizerRecord struct {
	Role     string                  `json:"role"`
	Identity schema.ExplorerIdentity `json:"identity"`
	Status   schema.IdentityStatus   `json:"status"`
	Caveat   string                  `json:"caveat,omitempty"`
	// Raw is the canonicalizer's raw output, kept for capture only.
	Raw []byte `json:"-"`
}

// canonicalizerCall implements canon.CanonicalizerCall for one canonicalizer identity under a mode's
// CanonicalizingContract. The single path uses one; the dual path uses two.
type canonicalizerCall struct {
	contract mode.CanonicalizingContract
	adapter  model.Adapter
	role     string
	identity roster.Explorer
	ids      *identityLedger
	res      *Result
	// primary marks the first canonicalizer, which also sets the Result's single-canonicalizer fields
	// (CanonicalizerStatus, CanonicalizerCaveat, RawCanonicalization).
	primary bool
}

// Propose renders the canonicalizer prompt, calls the model in an isolated directory, records its
// identity, enforces the same-identity check for its role, and returns the parsed proposal.
func (c *canonicalizerCall) Propose(ctx context.Context, noms []canon.Nomination) (canon.Proposal, error) {
	prompt, perr := c.contract.CanonicalizerPrompt(noms)
	if perr != nil {
		return canon.Proposal{}, fault.Wrap(fault.Internal, "canonicalizer prompt", perr)
	}
	c.res.CanonicalizerPrompt = prompt // identical bytes for both canonicalizers (same nominations, same contract)
	work, cleanup, werr := isolatedWorkDir()
	if werr != nil {
		return canon.Proposal{}, fault.Wrap(fault.Internal, "isolated work dir (canonicalize)", werr)
	}
	r, ierr := c.adapter.Invoke(ctx, model.Call{
		Role: c.role, Phase: schema.PhaseCanonicalize,
		Model: c.identity.Model, ModelArg: core.ModelArg(c.identity.Model), Effort: c.identity.Effort,
		WorkDir: work, Prompt: prompt,
	})
	cleanup()
	rec := CanonicalizerRecord{Role: c.role, Identity: c.identity.Identity(), Raw: append([]byte(nil), responseBody(r)...)}
	if c.primary {
		c.res.RawCanonicalization = append([]byte(nil), responseBody(r)...) // captured even when the call or parse fails
	}
	if ierr != nil {
		c.res.CanonicalizerCalls = append(c.res.CanonicalizerCalls, rec)
		return canon.Proposal{}, fault.Wrap(fault.Internal, "canonicalizer call failed (raw explorer nominations preserved)", ierr)
	}
	status, _ := classify(c.adapter, r, c.identity.Model, c.identity.Adapter)
	rec.Status = status
	if c.primary {
		c.res.CanonicalizerStatus = status
	}
	caveat := collatorIdentityCaveat(status)
	rec.Caveat = caveat
	if c.primary {
		c.res.CanonicalizerCaveat = caveat
	}
	if ierr := c.ids.observe(c.role, r.ActualModel); ierr != nil {
		c.res.CanonicalizerCalls = append(c.res.CanonicalizerCalls, rec)
		return canon.Proposal{}, ierr
	}
	c.res.CanonicalizerCalls = append(c.res.CanonicalizerCalls, rec)
	// The call ref is "<role>:canonicalize", which distinguishes the two dual proposals in `agreedBy`.
	return c.contract.ParseProposal(responseBody(r), noms, c.role+":"+schema.PhaseCanonicalize, c.identity.Identity())
}

// explorerOutcome is one explorer call's result. Exactly one of env, drop and halt is meaningful;
// resolvedModel feeds the same-identity check.
type explorerOutcome struct {
	env           schema.Envelope
	drop          *Dropped
	halt          error
	resolvedModel string
}

// runExplorer invokes one explorer with the shared payload, validates the response against the expanded
// schema, classifies its identity, and returns the outcome. phase is the round's phase label.
func runExplorer(ctx context.Context, reg Registry, ex roster.Explorer, phase string, payload schema.ExplorerTaskPayload, payloadHash string, order int) (o explorerOutcome) {
	a, ok := reg[ex.Adapter]
	if !ok {
		o.drop = newDrop(ex.Identity(), "adapter not registered", nil, nil)
		return
	}
	work, cleanup, werr := isolatedWorkDir()
	if werr != nil {
		o.drop = newDrop(ex.Identity(), "isolated work dir: "+werr.Error(), nil, nil)
		return
	}
	defer cleanup()
	r, err := a.Invoke(ctx, model.Call{
		Role: "explorer", Phase: phase,
		Model: ex.Model, ModelArg: core.ModelArg(ex.Model), Effort: ex.Effort,
		WorkDir: work, Prompt: payload.FinalPrompt,
	})
	o.resolvedModel = r.ActualModel
	if err != nil {
		o.drop = newDrop(ex.Identity(), "invoke failed: "+err.Error(), nil, nil).withResult(r)
		return
	}
	// Extract the JSON object (a response may be fenced), then unmarshal and validate it.
	body := responseBody(r)
	extracted, repairs, xerr := schema.ExtractJSONObject(body)
	if xerr != nil {
		o.drop = newDrop(ex.Identity(), "response is not a JSON object: "+xerr.Error(), body, repairs).withResult(r)
		return
	}
	var resp map[string]any
	if jerr := json.Unmarshal(extracted, &resp); jerr != nil {
		o.drop = newDrop(ex.Identity(), "response is not JSON: "+jerr.Error(), body, repairs).withResult(r)
		return
	}
	if verr := schema.ValidateResponse(payload.ExpandedSchema, resp); verr != nil {
		o.drop = newDrop(ex.Identity(), "schema-invalid response: "+verr.Error(), body, repairs).withResult(r)
		return
	}
	// Identity is recorded on the envelope and never decides whether the response is used.
	status, evidence := classify(a, r, ex.Model, ex.Adapter)
	env := schema.Envelope{
		ID:               envelopeID(payloadHash, ex.Identity(), order),
		Identity:         ex.Identity(),
		IdentityStatus:   status,
		IdentityEvidence: schema.IdentityEvidence(evidence),
		Order:            order,
		PayloadHash:      payloadHash,
		Response:         resp,
		RawResponse:      append([]byte(nil), body...),
		Repairs:          repairs,
	}
	env.IdentityCaveat = identityCaveat(status, ex.Model, r.ActualModel)
	o.env = env
	return
}

// identityCaveat returns the reader-facing note for an explorer whose identity is not verified, or ""
// when it is.
func identityCaveat(status schema.IdentityStatus, requested, actual string) string {
	switch status {
	case schema.IdentityVerified:
		return ""
	case schema.IdentityMismatch:
		return fmt.Sprintf("identity mismatch — requested %q, the adapter reported %q; this response is used and attributed as reported", requested, actual)
	case schema.IdentitySelfReported:
		return fmt.Sprintf("identity self-reported only — the model claimed %q and nothing corroborates it", actual)
	default:
		return "identity unknown — the adapter reported no usable model evidence"
	}
}

// classify caps r's identity evidence at the adapter's declared ceiling (EvidenceNone if it declares
// none) and classifies it with meshcore/verify. The cap keeps model output from claiming stronger
// evidence than the adapter can provide. adapter is the adapter name, used for alias matching.
func classify(a model.Adapter, r model.Result, requestedModel, adapter string) (schema.IdentityStatus, core.IdentityEvidence) {
	ceiling := core.EvidenceNone
	if ev, ok := a.(interface{ Evidence() core.IdentityEvidence }); ok {
		ceiling = ev.Evidence()
	}
	capped := verify.CapEvidence(ceiling, r.Evidence)
	status, _ := verify.ClassifyIdentity(capped, requestedModel, r.ActualModel, adapter)
	return schema.IdentityStatus(status), capped
}

// envelopeIDVersion tags the ID algorithm so a change to it is detectable in recordings.
const envelopeIDVersion = "v1"

// EnvelopeIDVersion exposes the envelope-ID algorithm tag for the capture manifest.
func EnvelopeIDVersion() string { return envelopeIDVersion }

// envelopeID returns a stable id for an explorer's envelope: a versioned SHA-256 of the payload hash,
// the explorer identity and its panel order. The response is excluded so the id is comparable across
// runs with the same payload and roster.
func envelopeID(payloadHash string, id schema.ExplorerIdentity, order int) string {
	b, _ := json.Marshal(struct {
		V           string `json:"v"`
		PayloadHash string `json:"payloadHash"`
		Adapter     string `json:"adapter"`
		Model       string `json:"model"`
		Effort      string `json:"effort"`
		Order       int    `json:"order"`
	}{envelopeIDVersion, payloadHash, id.Adapter, id.Model, id.Effort, order})
	sum := sha256.Sum256(b)
	return "env-" + hex.EncodeToString(sum[:8])
}

// responseBody returns the content to parse: the adapter's extracted Payload when present, else Stdout.
func responseBody(r model.Result) []byte {
	if len(r.Payload) > 0 {
		return r.Payload
	}
	return r.Stdout
}

// collatorIdentityCaveat returns the reader-facing note for a collator or canonicalizer whose identity is
// not verified, or "" when it is.
func collatorIdentityCaveat(status schema.IdentityStatus) string {
	switch status {
	case schema.IdentityVerified:
		return ""
	case schema.IdentityMismatch:
		return "COLLATOR REPORTED A MISMATCHED MODEL — the synthesis below was produced by a model other than the one configured"
	case schema.IdentitySelfReported:
		return "COLLATOR IDENTITY IS SELF-REPORTED ONLY — nothing corroborates which model produced this synthesis"
	default: // unknown
		return "COLLATOR IDENTITY IS UNKNOWN — the adapter reported no usable model evidence for this synthesis"
	}
}

func identityString(id schema.ExplorerIdentity) string {
	return fmt.Sprintf("(%s, %s, %s)", id.Adapter, id.Model, id.Effort)
}

// explorerFanout returns how many of n explorers run concurrently: maxParallel when it is positive and
// below n, otherwise n, and never below 1. There is no built-in ceiling because the right limit depends
// on the caller's machine and provider rate limits.
func explorerFanout(n, maxParallel int) int {
	if n < 1 {
		return 1
	}
	if maxParallel > 0 && maxParallel < n {
		return maxParallel
	}
	return n
}
