// Package pipeline is exploremesh's fan-out→collate engine (design §6.3, §6.7): the collator
// bookends the run (formulate + synthesize) around a blind, parallel explorer fan-out. All model
// calls go through meshcore/model; model-identity is classified via meshcore/verify. Explorer and
// collator output is UNTRUSTED and only ever parsed as JSON data, never executed (§6.8).
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

// maxDroppedRaw bounds the raw dropped-explorer body retained for --dump-run: a diagnostic slice, not
// the full stream, so a runaway/garbage response can never blow up the dump. 64 KiB is ample for a JSON
// body; the record's FullLen/FullSHA256 still describe the whole stream when it is clipped.
const maxDroppedRaw = 64 << 10

// Dropped records an explorer removed from the panel (failed/timed-out/schema-invalid) with an
// audited reason — never a silent omission (§6.7). It also carries a BOUNDED copy of the raw body the
// pipeline tried to parse plus its provenance, so a dropped/halted panel is reconstructable from the
// dump alone (the exact gap a real 3-provider dogfood hit: --dump-run captured only the formulation).
type Dropped struct {
	Explorer schema.ExplorerIdentity
	Reason   string
	// Raw is a bounded (<= maxDroppedRaw) copy of responseBody(r) — the body the parse rejected. Empty
	// when the drop preceded the model call (e.g. adapter not registered, invoke failed). The lengths +
	// hashes below describe the FULL stream even when Raw was clipped.
	Raw         []byte
	Truncated   bool     // Raw was clipped to maxDroppedRaw
	CapturedLen int      // len(Raw)
	FullLen     int      // len of the full raw stream before clipping
	SHA256      string   // SHA-256 of the captured (bounded) bytes
	FullSHA256  string   // SHA-256 of the full stream (== SHA256 when not truncated)
	Repairs     []string // extraction repairs applied before the parse that dropped it (nil if pre-parse)
	// ExitCode + StderrExcerpt are the adapter PROCESS's failure signal (0 / "" when the call never ran, or
	// ran and exited cleanly). A CLI that REFUSES a request typically writes the decisive error to stderr and
	// exits non-zero while producing NO stdout — so without these an honest-but-useless "empty response"
	// parse reason hides the actual cause (a real dogfood reported exactly that while the CLI had printed a
	// 400 "model is not supported on this account"). They are what make a dropped panel self-diagnosing.
	ExitCode      int
	StderrExcerpt string
}

// maxStderrExcerpt bounds the adapter-stderr excerpt carried in a drop: enough to name the real cause (an
// auth / quota / model-unavailable refusal), never the whole stream.
const maxStderrExcerpt = 2 << 10 // 2 KiB

// stderrExcerpt renders adapter stderr as a bounded single-line excerpt: the LAST few non-blank lines,
// because a CLI prints its banner/status first and its ERROR last. Collapsed to one line so it reads inside
// a drop reason, and clipped to maxStderrExcerpt.
func stderrExcerpt(b []byte) string {
	var lines []string
	for _, ln := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:] // the tail carries the failure, not the banner
	}
	out := strings.Join(lines, " | ")
	if len(out) > maxStderrExcerpt {
		out = out[:maxStderrExcerpt] + "…(clipped)"
	}
	return out
}

// failureContext summarizes an adapter Result's process-level failure signal for appending to a drop
// REASON: the exit code when non-zero, plus the bounded stderr excerpt. Empty when the process gave no
// failure signal (exited 0 and wrote nothing to stderr) — then the parse-level reason already says it all.
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

// withResult attaches an adapter Result's process-level failure signal to a drop record AND appends the
// same context to its Reason, so the one-line reason alone names the cause. Returns d for chaining.
func (d *Dropped) withResult(r model.Result) *Dropped {
	d.ExitCode = r.ExitCode
	d.StderrExcerpt = stderrExcerpt(r.Stderr)
	d.Reason += failureContext(r)
	return d
}

// newDrop builds a Dropped, attaching the bounded raw-body diagnostics. raw is the body the pipeline
// tried to parse (nil/empty for a pre-invoke drop); repairs are the extraction repairs applied before
// the drop. Both the captured-slice and full-stream SHA-256 are recorded so a truncated capture is still
// verifiable against a re-run.
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

// Result is the outcome, populated as far as the run progressed (so a halt still yields the
// formulation state + any envelopes gathered, for audit).
type Result struct {
	// Mode is the app-owned mode this run EXECUTED (design §3), stamped by the pipeline rather than echoed
	// from the task, so a recorded run says which contract produced it even when the task defaulted the mode.
	// It exists because §9's derived export is built from the run directory alone: a run dir that does not
	// record which mode made it is not a system of record.
	Mode           string `json:"mode,omitempty"`
	Formulation    schema.Formulation
	CollatorStatus schema.IdentityStatus
	CollatorCaveat string
	Envelopes      []schema.Envelope // explorer envelopes (verified + weak), in stable order
	Dropped        []Dropped
	// Output is the mode's terminal collator output (design §3): Map → schema.CollatorOutput,
	// Synthesize → schema.SynthesizeOutput, carried behind the mode.ModeOutput marker interface. nil if
	// the run halted before the collate step. It JSON-marshals as the concrete type (so --json / --dump-run
	// persist whichever ran); the surfaces render Summary() with an optional type-switch for richer detail.
	Output mode.ModeOutput
	// Canonicalization is the append-only merge-ledger + surjectivity result for a CANONICALIZING mode
	// (Catalog, design §4); nil for plain-collate modes (Map, Synthesize). It is present ONLY when the
	// surjectivity gate PASSED (canon.Canonicalize returns an error otherwise), so a non-nil value is a
	// held partition. Persisted as an append-only merge-ledger.jsonl (§9). When a confirmation round ran it
	// is the CONFIRMED revision; the superseded provisional revision is retained in Provisional.
	Canonicalization *canon.Result `json:"canonicalization,omitempty"`
	// Provisional is the PRE-confirmation partition revision, retained unchanged when a confirmation round
	// produced a new revision (design §4: a revision is a new entry, never an edit). nil when no confirmation
	// round ran.
	Provisional *canon.Result `json:"provisional,omitempty"`
	// Confirmation is the full binding-confirmation record (presentation order, typed challenges, versioned
	// host resolutions, contested mappings). nil unless the mode's policy set Confirm.
	Confirmation *canon.Confirmation `json:"confirmation,omitempty"`
	// Preflight records the identity pre-flight verdicts taken BEFORE the fan-out (design §1), one per
	// governed role (collator + each canonicalizer).
	Preflight []PreflightRecord `json:"preflight,omitempty"`
	// Panel is the FROZEN panel + counting policy hash + participation outcome (design §1). Frozen before any
	// judgment is solicited; the dual denominators on every claim are derived from it.
	Panel govern.Panel `json:"panel"`
	// Rounds are the executed explorer rounds in order. Rounds[0] is the IMMUTABLE blind round 1 — the
	// epistemic baseline every independence count is computed over (§0 F-A). Its envelopes are unexported
	// inside round.Round, so no later stage can overwrite them.
	Rounds []round.Round `json:"rounds,omitempty"`
	// Mediations are the collator-mediated cross-review edges (design §1): the pooled confirmed-canonical
	// artifact and the exact digest shown to each explorer. Empty for a single-round run.
	Mediations []round.Mediation `json:"mediations,omitempty"`
	// Governance is the terminal governance surface: the frozen panel, every emitted claim pinned to its
	// inputs, and the quarantined collatorNarrative (design §0 F-C/§4). nil for a mode that emits no counts.
	Governance *govern.Report `json:"governance,omitempty"`
	// Decision is the HOST-TALLIED ballot decision (design §4), present ONLY for a ballot-bearing mode
	// (Shortlist). It is set with its FROZEN inputs before the ballot round is dispatched and replaced by the
	// full tally afterwards — so even a run that halts mid-ballot records the framing the panel was about to
	// vote under.
	Decision *govern.Decision `json:"decision,omitempty"`
	// BallotNarrative is the voters' stated reasoning, quarantined out of the machine ballot record into the
	// collatorNarrative namespace (§0 F-C) and folded into Governance.CollatorNarrative.
	BallotNarrative []govern.Narrative `json:"-"`
	// Degraded is the PER MODE-CLASS degraded terminal artifact (design §1), populated when the collator
	// became unavailable after the fan-out or an identity halt fired — so a halted run still carries a usable
	// terminal artifact built from the blind round-1 envelopes.
	Degraded *schema.DegradedOutput `json:"degraded,omitempty"`
	// CanonicalizerStatus/Caveat govern the DECOUPLED, separately-identity-verified canonicalizer call
	// (design §0 F-A): its own identity verdict, distinct from the collate call. Empty for non-
	// canonicalizing modes.
	CanonicalizerStatus schema.IdentityStatus `json:"canonicalizerStatus,omitempty"`
	CanonicalizerCaveat string                `json:"canonicalizerCaveat,omitempty"`
	// CanonicalizerCalls records EVERY canonicalizer call's role + verified identity + verdict (one entry on the
	// single path, two on the dual path) so the partition's proposing identities are auditable from the Result.
	CanonicalizerCalls []CanonicalizerRecord `json:"canonicalizerCalls,omitempty"`
	// CanonicalizerProvenance says HOW those identities were chosen: ProvenanceExplicit (named by the request
	// or the profile) or ProvenanceDerived (the host picked them — slot a from the collator, slot b from the
	// panel in preference order). Empty for a non-canonicalizing mode. WHO canonicalized was always recorded;
	// this records who DECIDED who canonicalizes, which is the fact that makes a bad choice visible from a run
	// record rather than only from reading the code.
	CanonicalizerProvenance string `json:"canonicalizerProvenance,omitempty"`
	// CanonicalizerIndependence says WHAT THAT CHOICE BOUGHT on the dual path: `distinct_models`, or
	// `shared_model` when both canonicalizers run the same model through different adapters. Empty for a
	// single-canonicalizer or non-canonicalizing mode.
	//
	// It rides the Result because every corroboration count in the artifact rests on it. A merge is HELD
	// when both canonicalizers propose it, so what that agreement is worth depends on how different the
	// two proposers were — and a run that recorded the identities without this left the question
	// unanswerable from the record. `shared_model` is permitted and warned, never refused: a panel is
	// configured deliberately, and the two proposals do differ (sampling sees to that); what is weaker is
	// that they are drawn from the same priors, so their errors correlate.
	CanonicalizerIndependence string `json:"canonicalizerIndependence,omitempty"`
	// Capture-only fields (for --dump-run): the collator's RAW synthesize/canonicalize output (the
	// semantic body, retained even on a parse failure — the case most valuable for fixtures) and the
	// prompt strings. Never serialized into a model prompt. There is no formulate pair: formulation is
	// app-owned, so no formulate call is ever made.
	SynthesizePrompt    string `json:"-"`
	CanonicalizerPrompt string `json:"-"`
	RawSynthesis        []byte `json:"-"`
	RawCanonicalization []byte `json:"-"`
	// ConfirmationPrompt is the exact confirmation-round prompt shown to the panel (identical bytes for every
	// explorer), and LaterRoundPrompts the per-round explorer prompts of rounds 2..N — capture-only, so the
	// "what was actually shown" record is complete without bloating the machine surface.
	ConfirmationPrompt string   `json:"-"`
	LaterRoundPrompts  []string `json:"-"`
	// Shape is what the run WOULD have done, populated ONLY on a dry run (Options.DryRun) and nil on every
	// real run, whose shape is no longer a prediction. See Shape.
	Shape *Shape `json:"shape,omitempty"`
}

// Run executes the app-owned payload build → blind fan-out → synthesize. It returns a fault error on halt (collator
// identity, explorer mismatch, <2 verified, synthesize failure); Result carries partial state.
// isolatedWorkDir returns a fresh empty scratch dir for ONE model call plus a cleanup func.
// CONTAINMENT: an explorer/collator CLI runs HERE (model.Call.WorkDir → shell cmd.Dir; ACP already
// uses a fresh temp cwd), never the launching project directory — exploremesh reviews nothing on disk,
// so a CLI must not be able to read the user's tree. A mkdir failure is fatal (never run uncontained).
func isolatedWorkDir() (string, func(), error) {
	d, err := os.MkdirTemp("", "exploremesh-call-")
	if err != nil {
		return "", func() {}, err
	}
	return d, func() { _ = os.RemoveAll(d) }, nil
}

// onEvent is the OPTIONAL in-process progress hook (nil = no-op — the CLI passes nil and its behavior
// is unchanged). A surface (e.g. the ACP server) sets it to stream progress: it is called on the
// pipeline's own goroutines (some explorer events fire concurrently), so a hook MUST be safe to call
// from multiple goroutines, and MUST NOT block or panic. Timestamps are real wall-clock (this is
// runtime, not a replayable workflow script).
func Run(ctx context.Context, reg Registry, plan roster.Plan, raw schema.RawTask, opts Options, onEvent func(audit.EventLine)) (Result, error) {
	var res Result
	if err := raw.Validate(); err != nil {
		return res, err
	}
	// Resolve the app-owned mode (design §3): an empty task mode defaults to Map; an unknown mode is a
	// config error listing the known modes. The mode governs whether round-1 is formulation-free.
	spec, ok := mode.Resolve(raw.Mode)
	if !ok {
		herr := fault.New(fault.Config, fmt.Sprintf("unknown mode %q (known modes: %s)", raw.Mode, strings.Join(mode.Names(), ", ")))
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
	return RunSpec(ctx, reg, plan, raw, spec, opts, onEvent)
}

// Options are the OPTIONAL host overrides for one run. The zero value is the default behavior, so Run passes
// it empty. It exists so a caller can supply explicit governance identities without widening Run's signature
// or persisting new roster state.
type Options struct {
	// Canonicalizers supplies the EXPLICIT canonicalizer identities as a HOST override, taking precedence over
	// whatever the plan carries. It is EITHER empty (the host derives them, or the plan's own spec applies) or
	// exactly two — roster.ValidateCanonicalizers states why one is refused. It exists for a caller driving
	// RunSpec directly; the CLI/ACP/MCP surfaces put a user's spec on the PLAN instead, so it travels with the
	// panel it governs.
	Canonicalizers []roster.Explorer
	// MaxParallel bounds how many explorers invoke their model CLI AT ONCE. 0 (the default) runs the
	// whole panel in parallel; the panel size is then the only bound, which is the caller's own
	// choice.
	//
	// It is stated PER INVOCATION rather than configured once, because the number it should be is a
	// fact about the machine the CLIs run on — resident memory and process count for a cloud CLI,
	// loaded weights for a local model — and about the caller's provider rate limits. A launch-time
	// or compiled-in constant would be exploremesh guessing on behalf of hardware it cannot see.
	//
	// It bounds PARALLELISM only, never membership: every explorer in the panel still answers the
	// byte-identical payload, so lowering it slows the round down without changing the blindness
	// property or who contributed.
	MaxParallel int
	// DryRun resolves everything and spends nothing: the run performs every FREE check a real run would
	// (the task contract, the round contract, the terminal-contract count, formulation, the collator's
	// registration, canonicalizer derivation), populates Result.Shape, and returns before the identity
	// pre-flight — which is exploremesh's first model call.
	//
	// The stop is EARLIER than reviewmesh's, and deliberately so. A review's static preflight is free, so
	// its dry run can run that too and promise every adapter answered. Explore's pre-flight INVOKES, so
	// stopping after it would spend; the shape therefore says nothing between here and the first call is a
	// configuration question, and does not claim the collator replied. See Shape.
	DryRun bool
}

// RunSpec is Run with the mode contract passed EXPLICITLY instead of resolved from the registry. It is the
// entry point for the multi-round + ranking-grade governance path (typed round artifacts, dual canonicalizer,
// binding confirmation, mediation, governance claims). A caller drives it with an UNREGISTERED contract — an
// example or a test spec — without adding a mode name the surfaces would advertise; Run resolves the
// registered modes through the registry and reaches the same code.
func RunSpec(ctx context.Context, reg Registry, plan roster.Plan, raw schema.RawTask, spec mode.ModeSpec, opts Options, onEvent func(audit.EventLine)) (Result, error) {
	var res Result
	res.Mode = spec.Name
	if err := raw.Validate(); err != nil {
		return res, err
	}
	// The round count is FIXED by the mode contract (design §1): no data-dependent termination rule (a
	// canonical-set-delta rule is manipulable by an aggressively-merging canonicalizer), and a hard maximum
	// that FAILS rather than clamps.
	// The mode's app-owned TASK check (design §3), before anything is spent: a mode that needs an input the
	// base RawTask does not require — Challenge's ARTIFACT under review — refuses here rather than after a
	// panel has been paid for. The surfaces run the same check earlier for a friendlier message; this one is
	// the backstop no caller can skip.
	if terr := spec.CheckTask(raw); terr != nil {
		herr := fault.Wrap(fault.Config, "mode task contract", terr)
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
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
			// A later round is fed the pooled CONFIRMED-CANONICAL uniques (§1). Without a canonicalization step
			// there is no confirmed partition, and the host pooling free-form peer items itself would BE the
			// entity-resolution judgment §0 F-B exists to keep visible and contestable. So it is refused.
			herr := fault.New(fault.Config, fmt.Sprintf("mode %q declares %d rounds but is not canonicalizing — a later round may only carry the pooled CONFIRMED-canonical uniques (design §1); pooling un-canonicalized peer items would be covert entity resolution", spec.Name, rounds))
			emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
			return res, herr
		}
	}
	// A mode declares EXACTLY ONE terminal contract (plain collate / canonicalizing / fixed-space). The check
	// is here rather than at registration so a spec supplied directly to RunSpec is held to the same rule, and
	// so the failure is a clear config halt instead of a nil dereference three phases later.
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
	// FORMULATION is APP-OWNED (design §3, §0 F-A): a mode supplies its own explorer prompt + explorer
	// schema, so the collator never authors the payload it will later be asked to collate. The
	// There is NO collator-FORMULATED round-1 leg: every registered mode declares
	// FormulationFree, so such a leg would be unreachable outside its own tests while `formulation:
	// collator | a deterministic fallback` advertised a live distinction no shipped mode could produce.
	// Refusing here — the same fail-closed posture as the terminal-contract check above — is honest about
	// the capability the binary actually has, instead of carrying an untested model-call path for it.
	if !spec.FormulationFree {
		herr := fault.New(fault.Config, fmt.Sprintf("mode %q is not formulation-free — collator-formulated round-1 is not implemented; a mode must own its explorer prompt and schema (design §3)", spec.Name))
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

	// -- THE DRY-RUN STOP (Options.DryRun) --
	// Every free check is behind us; the next statement invokes a model. So the disclosure is assembled here
	// and the run returns. shapeOf resolves the canonicalizer identities for real, which is why a dual-
	// canonicalizer plan that cannot yield two independent ones fails HERE, at no cost, instead of at the
	// pre-flight it would otherwise reach first.
	if opts.DryRun {
		shape, serr := shapeOf(plan, raw, spec, opts, rounds)
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

	// -- Phase 0: identity PRE-FLIGHT before the fan-out + the FROZEN panel (design §1) --
	// Both happen before a single explorer token is spent: a provider that silently fell back is caught while
	// it is still cheap, and the counting policy is hashed before any judgment could influence it.
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

	// -- Phase 1: build the shared explorer payload --
	// FORMULATION-FREE (guaranteed by the contract check above): there is NO collator formulate call —
	// explorers receive the mode's deterministic app-owned prompt + fixed schema, so the collator can
	// never author the explorer schema (§0 F-A). The collator identity is therefore governed at the
	// synthesize call, its only call, below.
	emit(onEvent, "info", "formulate_start", "formulation-free ("+spec.Name+"): building the app-owned explorer payload (no collator formulate call)", map[string]any{
		"mode": spec.Name, "formulationFree": true, "explorers": len(plan.Explorers),
	})
	res.Formulation = schema.FormulationFree(schema.FormulationFreeMap, spec.Prompt(raw), spec.ExplorerSchema())
	emit(onEvent, "info", "formulate_done", "formulation source: "+string(res.Formulation.Source), map[string]any{
		"source": string(res.Formulation.Source), "collatorStatus": string(res.CollatorStatus),
	})
	payload := res.Formulation.Payload
	payloadHash, _ := payload.Hash()

	// -- Phase 2: BLIND round-1 fan-out — every explorer gets the byte-identical payload (§6.2). These
	// envelopes are the EPISTEMIC BASELINE: they are recorded as an immutable round (§1) and are the only
	// evidence any independence count is ever computed over (§0 F-A).
	envs, drops, halt := run.fanout(1, schema.PhaseExplore, payload, payloadHash)
	res.Dropped = append(res.Dropped, drops...)
	res.Envelopes = append(res.Envelopes, envs...)
	if halt != nil {
		// A proven identity mismatch on ANY explorer halts the run (no silent fallback, §6.2/§6.7). The blind
		// artifacts gathered so far remain valid, so the degraded terminal artifact is emitted (§1).
		emit(onEvent, "error", "halt", "explorer identity halt: "+halt.Error(), map[string]any{"haltClass": haltClassOf(halt)})
		res.Degraded = run.degrade(schema.DegradedIdentityHalt, halt.Error())
		return res, halt
	}
	res.Rounds = append(res.Rounds, round.NewRound(1, true, payloadHash, res.Envelopes, nil))

	// -- Minimum-viable panel: ≥2 RESPONDING explorers for primary synthesis (§6.7) --
	// The bar is multiplicity, not provenance: a one-response panel is not a panel. Model identity is NOT
	// consulted — a response's content determines whether it is worth anything, and a recorded identity
	// (verified, self-reported, unknown, or a proven mismatch) never promotes or demotes it. See
	// ../../../docs/model-identity.md.
	//
	// Abstentions ARE separated: an explorer that explicitly declines (§1's deliberate abstention) is kept in
	// the record but contributes no position, so it is excluded from the primary panel AND from the
	// respondents denominator — which is exactly why both denominators are always reported. That is a
	// judgment the explorer itself made about the question, not a judgment the host made about the explorer.
	var primary, abstained []schema.Envelope
	for _, e := range res.Envelopes {
		if schema.IsAbstention(e.Response) {
			abstained = append(abstained, e)
			continue
		}
		primary = append(primary, e) // PANEL order preserved
	}
	if len(primary) < 2 {
		herr := fault.New(fault.Config, fmt.Sprintf(
			"primary synthesis requires >=2 responding explorers, have %d (abstained: %d, dropped: %d) — a one-response panel is not a panel",
			len(primary), len(abstained), len(res.Dropped)))
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		return res, herr
	}
	// The participation OUTCOME is layered onto the frozen panel (design §1): distinct tallies, because a
	// technical absence and a deliberate abstention mean different things.
	res.Panel = pan.WithOutcome(govern.Outcome{
		Dispatched:           len(plan.Explorers),
		Eligible:             len(primary),
		TechnicalAbsence:     len(res.Dropped),
		DeliberateAbstention: len(abstained),
	})

	// -- Phase 3+4 (CANONICALIZING modes, §4): canonicalize (single or DUAL) → optional binding CONFIRMATION
	// round → optional collator-mediated LATER ROUNDS → governance claims → collate into the mode output.
	// This path REPLACES the plain collate below for a mode that sets Canonicalizing (mutually exclusive with
	// Collator).
	if spec.Canonicalizing != nil {
		return run.canonicalizingPath(primary, payloadHash, rounds)
	}

	// -- Phase 3+4 (FIXED-SPACE modes, §3 Compare/Forecast rows): the HOST computes the entire result from
	// the blind round-1 artifacts and the terminal collate call contributes NARRATIVE only. It is checked
	// after the canonicalizing branch and before the plain collate for the same reason those two are mutually
	// exclusive — a mode sets exactly one terminal contract.
	if spec.FixedSpace != nil {
		return run.fixedSpacePath(primary, payloadHash)
	}

	// -- Phase 4: collator synthesize (bounded; on failure the raw envelopes are preserved) --
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
	res.RawSynthesis = append([]byte(nil), responseBody(sres)...) // captured even on serr / parse failure
	if serr != nil {
		herr := fault.Wrap(fault.Internal, "collator synthesize failed (raw explorer responses preserved for re-run)", serr)
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		// The collator became UNAVAILABLE after the fan-out: emit the per-mode-class degraded terminal
		// artifact (design §1) so the paid-for blind round-1 evidence is still delivered.
		res.Degraded = run.degrade(schema.DegradedCollatorUnavailable, serr.Error())
		return res, herr
	}
	// There was no formulate call to classify the collator identity at, so classify it HERE at its only call.
	// The result is RECORDED (status + caveat) and never gates the synthesis.
	cstatus, _ := classify(collAdapter, sres, plan.Collator.Model, plan.Collator.Adapter)
	res.CollatorStatus = cstatus
	res.CollatorCaveat = collatorIdentityCaveat(cstatus)
	// SAME-IDENTITY invariant (§1): the collate call must resolve to the model pinned at pre-flight/formulate.
	if herr := run.ids.observe("collator", sres.ActualModel); herr != nil {
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		res.Degraded = run.degrade(schema.DegradedIdentityHalt, herr.Error())
		return res, herr
	}
	// Parse + validate via the mode's app-owned collator contract (design §3). Map yields a
	// CollatorOutput; Synthesize yields a SynthesizeOutput. The pipeline persists whichever ran behind
	// mode.ModeOutput.
	out, oerr := spec.Collator.Parse(responseBody(sres))
	if oerr != nil {
		herr := fault.Wrap(fault.Internal, "collator synthesis output invalid (raw explorer responses preserved)", oerr)
		emit(onEvent, "error", "halt", herr.Error(), map[string]any{"haltClass": haltClassOf(herr)})
		// An unusable terminal collation leaves the run with no collation at all, so the same degraded artifact
		// applies — the blind evidence is intact and must not be thrown away with the bad collator output.
		res.Degraded = run.degrade(schema.DegradedCollatorUnavailable, "collator output invalid: "+oerr.Error())
		return res, herr
	}
	// The HOST-AUTHORITATIVE citation pass (design §3 C1): the collator was asked to cite the
	// `envelope#k` aliases behind each finding, and here the host checks what it actually cited against
	// the aliases THIS panel produced — keeping the refs that resolve, dropping the ones that do not, and
	// setting the `uncited` label itself. It runs INSIDE Run rather than in the CLI so every surface
	// (CLI, webui, ACP) gets validated citations from the one code path.
	//
	// FAIL-SOFT on purpose: a defective citation is not an epistemic failure of the exploration, and the
	// collator contract is per-mode, so this must not be able to turn a working run into a halt. An
	// uncited finding is reported and labeled, never dropped and never silently promoted.
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

// emit calls the optional progress hook with a freshly-timestamped EventLine (nil hook = no-op). It is
// the single seam the pipeline uses to surface progress to a surface without knowing anything about it.
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

// haltClassOf derives a short, honest halt-class label for a pipeline halt: the fault's explicit halt
// class when set, else the fault code's name (exploremesh's pipeline halts are Config/Internal). A
// non-fault error degrades to "internal".
func haltClassOf(err error) string {
	var f *fault.Fault
	if errors.As(err, &f) {
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

// CanonicalizerRecord is one canonicalizer call's governance record (design §4): its role label, the identity
// it was verified as, and the identity verdict. The DUAL path produces two records — that is how "which two
// independent judgments produced this partition" is answerable from the Result alone.
type CanonicalizerRecord struct {
	Role     string                  `json:"role"`
	Identity schema.ExplorerIdentity `json:"identity"`
	Status   schema.IdentityStatus   `json:"status"`
	Caveat   string                  `json:"caveat,omitempty"`
	// Raw is the canonicalizer's raw output, retained for capture even on a parse failure. json:"-" so it never
	// enters a machine surface or a prompt.
	Raw []byte `json:"-"`
}

// canonicalizerCall adapts a mode's CanonicalizingContract + ONE canonicalizer identity into a
// canon.CanonicalizerCall (design §4): it renders the canonicalizer prompt, makes the DECOUPLED canonicalizer
// model call (its own role label, Phase PhaseCanonicalize, isolated WorkDir), classifies its identity
// SEPARATELY (§0 F-A) and records it, enforces the same-identity invariant for that role, and parses the
// proposal. The single path builds ONE of these against the collator's adapter/model — a distinct call, never
// the collate call. The DUAL path builds TWO against two independent identities and hands both to
// canon.CanonicalizeDual.
type canonicalizerCall struct {
	contract mode.CanonicalizingContract
	adapter  model.Adapter
	role     string
	identity roster.Explorer
	ids      *identityLedger
	res      *Result
	// primary marks the FIRST canonicalizer, which also stamps the legacy single-canonicalizer fields on the
	// Result (CanonicalizerStatus/Caveat/RawCanonicalization) so a Catalog run's artifact is unchanged.
	primary bool
}

// Propose renders + issues the canonicalizer model call, governs its (decoupled) identity, and returns the
// parsed proposal. A mismatched/unverified canonicalizer HALTS exactly like the collator (same policy) —
// the canonicalization judgment must come from a verified model.
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
		c.res.RawCanonicalization = append([]byte(nil), responseBody(r)...) // captured even on error / parse failure
	}
	if ierr != nil {
		c.res.CanonicalizerCalls = append(c.res.CanonicalizerCalls, rec)
		return canon.Proposal{}, fault.Wrap(fault.Internal, "canonicalizer call failed (raw explorer nominations preserved)", ierr)
	}
	// SEPARATE identity classification (decoupled from the collate call, §0 F-A). It is RECORDED on the call
	// record and never gates the proposal.
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
	// SAME-IDENTITY invariant PER canonicalizer (§1): each canonicalizer's role was pinned at pre-flight, so a
	// swap between the probe and the proposal call halts.
	if ierr := c.ids.observe(c.role, r.ActualModel); ierr != nil {
		c.res.CanonicalizerCalls = append(c.res.CanonicalizerCalls, rec)
		return canon.Proposal{}, ierr
	}
	c.res.CanonicalizerCalls = append(c.res.CanonicalizerCalls, rec)
	// The deciding call ref + verified canonicalizer identity are stamped onto every ledger row. The ref is the
	// ROLE + phase, so the two dual proposals are distinguishable in `agreedBy` (and the single path's ref is
	// unchanged: role "canonicalizer" → "canonicalizer:canonicalize").
	return c.contract.ParseProposal(responseBody(r), noms, c.role+":"+schema.PhaseCanonicalize, c.identity.Identity())
}

// explorerOutcome is one explorer call's result: exactly one of env / drop / halt is meaningful, plus the
// model the call RESOLVED to (fed to the same-identity invariant, §1 — a role that resolves to a different
// model mid-exploration halts).
type explorerOutcome struct {
	env           schema.Envelope
	drop          *Dropped
	halt          error
	resolvedModel string
}

// runExplorer invokes one explorer with the shared payload, validates its response against the
// expanded schema, classifies identity, and returns an envelope (or a drop/halt outcome). `phase` is the
// adapter-facing phase label for this round (explore / ballot).
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
	// Untrusted model output: extract the single JSON object first (a real dogfood halt was a response
	// fenced in ```json …), recording any enumerated repairs, THEN unmarshal + schema-validate the
	// extracted bytes. On any failure drop with the reason AND the bounded raw body for --dump-run.
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
	// Identity is classified and RECORDED on the envelope; it never decides whether this response is used.
	// Even a proven mismatch is carried as a caveat: the response's content is what determines its worth.
	status, evidence := classify(a, r, ex.Model, ex.Adapter)
	env := schema.Envelope{
		ID:               envelopeID(payloadHash, ex.Identity(), order),
		Identity:         ex.Identity(),
		IdentityStatus:   status,
		IdentityEvidence: schema.IdentityEvidence(evidence),
		Order:            order,
		PayloadHash:      payloadHash,
		Response:         resp,
		RawResponse:      append([]byte(nil), body...), // copy: the semantic body, for --dump-run
		Repairs:          repairs,                      // enumerated extraction repairs (nil for a clean body)
	}
	env.IdentityCaveat = identityCaveat(status, ex.Model, r.ActualModel)
	o.env = env
	return
}

// identityCaveat renders the reader-facing note for a seat whose identity fell short of a strong-evidence
// match — empty for `verified`. It is a LABEL: it changes what a reader is told, never what the pipeline
// does with the response.
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

// classify caps the adapter's produced evidence to its declared ceiling (fail-closed EvidenceNone),
// then classifies via meshcore/verify (the single authority), returning BOTH the status and the capped
// tier (persisted on the envelope). Capping mirrors reviewmesh: a self_report/none adapter can never be
// classified on a stronger tier than its recipe declares, so untrusted model output can't inflate the
// identity. `adapter` is the adapter NAME (for verify's alias matching); `a` supplies the ceiling.
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

// envelopeID is the stable slot id for an explorer's envelope: a versioned SHA-256 over a MARSHALED
// typed struct (not string concatenation — delimiter-safe) of the payload hash + the (adapter, model,
// effort) identity + the panel order. Deterministic, and for identical inputs (same payload + roster)
// stable across runs — the citation anchor AND the cross-run comparison key. The response is
// deliberately NOT included (that would break cross-run comparability). Within a run, order makes it
// unique across the panel.
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
	return "env-" + hex.EncodeToString(sum[:8]) // 16 hex chars — ample for a per-run panel
}

// responseBody is the semantic content to parse: an adapter's unwrapped Payload when present (e.g.
// the ACP adapter surfaces the JSON answer extracted from an agentic agent's narration/fences),
// else the raw Stdout. Mirrors the reviewmesh Manager's Payload-else-Stdout rule.
func responseBody(r model.Result) []byte {
	if len(r.Payload) > 0 {
		return r.Payload
	}
	return r.Stdout
}

// collatorIdentityCaveat renders the reader-facing note for a collator whose identity fell short of a
// strong-evidence match — empty for `verified`. Like every other identity signal it is RECORDED and
// never enforced: the synthesis proceeds regardless, and its content is what a reader judges.
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

// explorerFanout returns how many explorer CLIs run concurrently: every explorer at once unless the
// CALLER asked for fewer, and never below 1 or above n.
//
// There is no built-in ceiling. Each explorer is a real model CLI under a long per-call timeout, so
// how many may run at once is a fact about the operator's machine — RAM and process count for a
// cloud CLI, loaded weights for a local model — and about their provider rate limits. exploremesh
// knows none of those, so any constant it picked would be a guess binding people it knows nothing
// about. maxParallel is therefore stated per invocation, and 0 means "all of them".
//
// It bounds PARALLELISM only: every explorer in the panel still runs, so the blind round is
// unchanged. (reviewmesh's seatFanout is the same rule for seats.)
func explorerFanout(n, maxParallel int) int {
	if n < 1 {
		return 1
	}
	if maxParallel > 0 && maxParallel < n {
		return maxParallel
	}
	return n
}
