// Package capture writes an exploremesh run to disk for replay + gold fixtures (the graph/collab P2
// gate corpus). It runs CLI-side AFTER pipeline.Run returns from the pure Result, so the pipeline
// stays hermetic and side-effect-free. The manifest is written LAST and atomically: its absence means
// the run did not finish writing.
package capture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// ManifestSchemaVersion is the ManifestV1 schema version (bump on any incompatible change). A recording is
// only ever compared WITHIN a schemaVersion, so a manifest at any of the versions below can be on disk and
// a reader needs to know what each one carries.
//
// v2: the formulation-free shape. A formulation-free mode makes no collator formulate call, so
// formulation.json records source "formulation-free (map)" with collatorAttempted false and neither
// raw-formulation.txt nor formulate-prompt.txt is written. No registered mode takes the collator-formulated
// leg, so those two files are written by NO run.
//
// v3: a run FREEZES its panel + counting policy before any judgment is solicited (design §1), so
// `countingPolicyHash` is present on EVERY manifest, in every mode. The other governance fields (revision
// chain, confirmation tallies, governance-claims hash, degraded class, round count) are omitempty and appear
// only when the corresponding stage ran. The frozen policy hash is the evidence that the counting rules
// predate the counts.
//
// v4: the run directory carries the run's DECLARED TASK (`task.json`) and the manifest carries the `mode`
// the run executed. Both exist because §9's SQLite export is DERIVED FROM THE RUN DIRECTORY ALONE — and a
// run dir that does not record which mode produced it, or what the user actually asked for, is not a system
// of record. These are capture-INDEX facts, not prompt/output/governance behavior, which is precisely what a
// schemaVersion is for.
//
// v5: a canonicalizing run records its CANONICALIZERS and their PROVENANCE (`canonicalizers`,
// `canonicalizerProvenance`: explicit | derived). Every other governance rule in this manifest already says
// who applied it; this one said WHICH identities canonicalized (in canonicalizerCalls) but never WHO CHOSE
// THEM — so a run whose second canonicalizer had been picked arbitrarily was indistinguishable, from its own
// record, from one whose operator had chosen it deliberately. Two independent canonicalizers decide which
// merges hold versus contest, and therefore every corroboration count, so their provenance is a governance
// fact and belongs in the record rather than in the code that produced it. Both fields are omitempty, so a
// non-canonicalizing mode's manifest is unchanged except for the version number.
//
// v6: the manifest records `canonicalizerIndependence` (distinct_models | shared_model) — what the
// canonicalizer pair was WORTH. v5 recorded who chose them; a pair of two instances of one model makes every
// corroboration count in the run weaker evidence, and a manifest that named the identities without saying
// they shared a model left the reader to notice it. It is omitempty, so a non-canonicalizing run's manifest
// is unchanged except for the version number.
//
// v7: no `exploreMemory` key. A v6 manifest could carry one; a v7 manifest never can. The version
// moves because a reader comparing WITHIN a schemaVersion is entitled to know which keys are possible,
// and a version that stays still while the shape changes is what misleads later.
const ManifestSchemaVersion = 7

// ArtifactEntry indexes one written file: run-relative path, size, and content digest.
type ArtifactEntry struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// AliasEntry maps a prompt-facing `envelope#k` alias to the stable Envelope.ID.
type AliasEntry struct {
	Alias           string `json:"alias"`
	ID              string `json:"id"`
	Order           int    `json:"order"`
	PrimaryEligible bool   `json:"primaryEligible"`
}

// DropRecord is the manifest entry for one dropped explorer: identity + audited reason, the extraction
// repairs applied before the drop, and the bounded raw-body provenance (captured/full length, a
// truncation flag, and both digests) plus the run-relative path of the raw file. RawPath is empty when
// no body was captured (a drop before the model call). It makes a dropped/halted panel diagnosable from
// the dump alone — the gap a real 3-provider dogfood hit.
type DropRecord struct {
	Adapter     string   `json:"adapter"`
	Model       string   `json:"model"`
	Effort      string   `json:"effort,omitempty"`
	Reason      string   `json:"reason"`
	Repairs     []string `json:"repairs,omitempty"`
	RawPath     string   `json:"rawPath,omitempty"`
	CapturedLen int      `json:"capturedLen"`
	FullLen     int      `json:"fullLen"`
	Truncated   bool     `json:"truncated,omitempty"`
	SHA256      string   `json:"sha256,omitempty"`
	FullSHA256  string   `json:"fullSha256,omitempty"`
	// ExitCode + StderrExcerpt are the adapter process's failure signal — what makes an "empty response"
	// drop diagnosable from the dump alone (a refusing CLI writes its error to stderr and exits non-zero
	// while producing no stdout). Omitted when the process gave no failure signal.
	ExitCode      int    `json:"exitCode,omitempty"`
	StderrExcerpt string `json:"stderrExcerpt,omitempty"`
}

// ManifestV1 is the typed index of a recorded run.
type ManifestV1 struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         string `json:"runId"`
	Status        string `json:"status"` // "complete" | "halted"
	Fault         string `json:"fault,omitempty"`
	// Mode is the app-owned mode this run EXECUTED (design §3), stamped by the pipeline. It is what makes the
	// run directory self-describing for the derived export (§9): every other field means something different
	// depending on which contract produced it. omitempty so a caller that supplies no result mode (a
	// hand-built Input in a test) records exactly what it did before.
	Mode           string                    `json:"mode,omitempty"`
	EnvelopeIDAlgo string                    `json:"envelopeIdAlgo"`
	PayloadHash    string                    `json:"payloadHash"`
	Explorers      []schema.ExplorerIdentity `json:"explorers"`
	Collator       schema.ExplorerIdentity   `json:"collator"`
	Envelopes      []AliasEntry              `json:"envelopes"`
	Dropped        []DropRecord              `json:"dropped,omitempty"`
	Artifacts      []ArtifactEntry           `json:"artifacts"`
	// Canonicalization fields (design §4): populated ONLY for a CANONICALIZING mode (Catalog). A
	// non-zero PartitionRevisionHash means the surjectivity gate PASSED (canon.Canonicalize returns an
	// error otherwise, so a recorded partition is a held one — Surjectivity is "holds"). LedgerRows is the
	// append-only merge-ledger row count (one per raw nomination). omitempty ⇒ a plain-collate mode's
	// manifest carries none of them.
	PartitionRevisionHash string `json:"partitionRevisionHash,omitempty"`
	Surjectivity          string `json:"surjectivity,omitempty"`
	LedgerRows            int    `json:"ledgerRows,omitempty"`
	// Canonicalizers + CanonicalizerProvenance answer, from the manifest alone, "which identities decided
	// which merges hold, and who chose them" (design §4). Provenance is `explicit` (a request or a profile
	// named them) or `derived` (the host picked them — slot a the collator, slot b the first explorer by
	// PREFERENCE order that differs from it). Both are absent for a non-canonicalizing mode.
	Canonicalizers          []schema.ExplorerIdentity `json:"canonicalizers,omitempty"`
	CanonicalizerProvenance string                    `json:"canonicalizerProvenance,omitempty"`
	// CanonicalizerIndependence says what that choice BOUGHT: `distinct_models`, or `shared_model` when
	// both slots ran one model behind two adapters. Every corroboration count in the run rests on the
	// difference, so it belongs beside the identities rather than only on the live surfaces.
	CanonicalizerIndependence string `json:"canonicalizerIndependence,omitempty"`
	// Governance fields (design §1/§4/§9), all omitempty so a Map/Synthesize/Catalog manifest carries none of them:
	// the executed round count, the ledger revision + the revision it supersedes (a confirmation revision is a
	// NEW entry — the prior one is retained on disk), the confirmation rule version + challenge/contested
	// tallies, and the frozen counting-policy hash + emitted-claims hash. Together they make the governance
	// state of a run auditable from the manifest alone.
	Rounds                int    `json:"rounds,omitempty"`
	LedgerRevision        int    `json:"ledgerRevision,omitempty"`
	PriorRevisionHash     string `json:"priorRevisionHash,omitempty"`
	ConfirmationRule      string `json:"confirmationRule,omitempty"`
	Challenges            int    `json:"challenges,omitempty"`
	ContestedMappings     int    `json:"contestedMappings,omitempty"`
	AgreementRuleVersion  string `json:"agreementRuleVersion,omitempty"`
	CountingPolicyHash    string `json:"countingPolicyHash,omitempty"`
	GovernanceClaimsHash  string `json:"governanceClaimsHash,omitempty"`
	GovernanceClaims      int    `json:"governanceClaims,omitempty"`
	DegradedTerminalClass string `json:"degradedTerminalClass,omitempty"`
	// Ballot decision fields (design §4), all omitempty and present ONLY for a ballot-bearing mode — so a
	// manifest for any other mode carries none of them. DecisionInputsHash is the hash
	// of the inputs frozen BEFORE the ballot was solicited: it is what makes "the criteria predate the votes"
	// checkable from the manifest alone.
	DecisionRule       string `json:"decisionRule,omitempty"`
	DecisionInputsHash string `json:"decisionInputsHash,omitempty"`
	BallotsCast        int    `json:"ballotsCast,omitempty"`
	Ranked             int    `json:"ranked,omitempty"`
	// Trace is the caller's W3C trace context, when one arrived. ABSENT otherwise, so every run that
	// carries none is byte-identical to one written before the field existed.
	Trace *TraceContext `json:"trace,omitempty"`
}

// TraceContext is the caller's W3C trace context, CARRIED VERBATIM from the request that started the
// run so this run directory can be correlated with the caller's own trace.
//
// It is carried, never interpreted, and never validated. `basic/index` §`_meta` reserves the three
// unprefixed keys — "As an exception to the prefix requirement above, the keys `traceparent`,
// `tracestate`, and `baggage` are reserved for OpenTelemetry trace context propagation. When present,
// their values MUST follow W3C Trace Context and W3C Baggage formats respectively." — and, of every
// reserved key, "implementations MUST NOT make assumptions about values at these keys". The format
// MUST binds the SENDER; refusing a malformed `traceparent` would be exactly the assumption the page
// forbids, so a malformed value is recorded as it arrived and the caller's own tooling decides.
//
// WHAT THIS MEASURES, stated in the record itself via Measures: correlation, and nothing else. No run
// in this repo records token counts or cost, so any spend figure quoted about a run is an estimate
// and must say so. A trace id makes a run externally correlatable; it does not make it metered, and
// a reader of this file must not infer otherwise.
type TraceContext struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
	Baggage     string `json:"baggage,omitempty"`
	// Measures names what this block does and does not account for. It is a constant today
	// ("correlation_only"); the day a run records tokens or cost, this value changes and a consumer
	// can tell the two eras of the record apart without guessing from field presence.
	Measures string `json:"measures"`
}

// MeasuresCorrelationOnly is TraceContext.Measures' only value today: this run carries a correlation
// identity and no token or cost accounting.
const MeasuresCorrelationOnly = "correlation_only"

// NewTraceContext returns the record block for a caller's trace context, or nil when the caller sent
// none — nil is what keeps a trace-free run's manifest byte-identical to before.
func NewTraceContext(traceParent, traceState, baggage string) *TraceContext {
	if traceParent == "" && traceState == "" && baggage == "" {
		return nil
	}
	return &TraceContext{
		TraceParent: traceParent, TraceState: traceState, Baggage: baggage,
		Measures: MeasuresCorrelationOnly,
	}
}

// ArtifactSubdir is exploremesh's component name under the shared `.aimesh/` state directory, so the two
// aimesh apps never interleave their state. It is the DOMAIN word rather than the binary name: the
// directory holds what the app owns (its profiles and its run output), and that outlives whatever the
// binary happens to be called — which, after the CLI unification, is `aimesh`.
const ArtifactSubdir = "explore"

// DefaultArtifactDir is where a captured run lands when $EXPLOREMESH_ARTIFACT_DIR is unset:
// `<project .aimesh>/explore/runs`, else a subdirectory of the OS temp directory. The rule itself lives
// in meshcore/localstate because reviewmesh needs exactly the same one — see localstate.RunDir for why it
// is never a cwd-relative path.
func DefaultArtifactDir() string { return localstate.RunDir(ArtifactSubdir, "") }

// ArtifactDir resolves the base directory captured runs are written under. It is one function rather than
// three copies of the same env lookup because run capture is now selectable from EVERY surface (CLI
// `--dump-run`, ACP `_meta.exploremesh.dumpRun`, and by default for an MCP run) — and a surface that
// resolved the directory differently would leave its runs somewhere the others cannot find.
func ArtifactDir() string {
	return localstate.RunDir(ArtifactSubdir, strings.TrimSpace(os.Getenv("EXPLOREMESH_ARTIFACT_DIR")))
}

// Input is everything needed to dump a run.
type Input struct {
	Run    *audit.Run
	Plan   roster.Plan
	Result pipeline.Result
	Status string // "complete" | "halted"
	Fault  string // populated when Status == "halted"
	// Task is the user's DECLARED task (design §6.2), persisted as `task.json`. It is here for the derived
	// SQLite export (§9): a fixed-space run's whole governance rides on the option set / criteria /
	// estimation target the USER declared, and a run directory that recorded the answers but not the
	// question could not reproduce the result. The zero value writes nothing, so a caller that supplies no
	// task records exactly what it did before.
	Task schema.RawTask
	// Trace is the caller's W3C trace context, when the surface that accepted the request carried one.
	// nil (the zero value) writes nothing.
	Trace *TraceContext
}

// Dump writes all artifacts then the manifest (last, atomically). Called on BOTH the success and the
// halt path so halted runs — the interesting ones for fixtures — are still recorded.
func Dump(in Input) error {
	var arts []ArtifactEntry
	write := func(rel string, data []byte) error {
		p := filepath.Join(in.Run.Dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		arts = append(arts, ArtifactEntry{Path: rel, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])})
		return nil
	}
	writeJSON := func(rel string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return write(rel, b)
	}

	r := in.Result
	// Prompts + raw collator output (retained even on a parse failure — the case fixtures most need).
	// There is no formulate pair: formulation is app-owned, so the collator is never asked to formulate.
	if r.SynthesizePrompt != "" {
		if err := write("synthesize-prompt.txt", []byte(r.SynthesizePrompt)); err != nil {
			return err
		}
	}
	if len(r.RawSynthesis) > 0 {
		if err := write("raw-synthesis.txt", r.RawSynthesis); err != nil {
			return err
		}
	}
	// The DECLARED TASK (§6.2/§9). Written first among the JSON artifacts because it is what every other
	// artifact is an answer to — and, for a fixed-space mode, it carries the declared option set / criteria /
	// estimation target the whole result is computed over.
	if strings.TrimSpace(in.Task.Purpose) != "" {
		if err := writeJSON("task.json", in.Task); err != nil {
			return err
		}
	}
	if err := writeJSON("formulation.json", r.Formulation); err != nil {
		return err
	}
	if r.Output != nil {
		if err := writeJSON("synthesis.json", r.Output); err != nil {
			return err
		}
	}

	// Canonicalizing modes (Catalog, §4): the canonicalizer prompt + its RAW output (retained even on a
	// parse failure), and the APPEND-ONLY merge-ledger as merge-ledger.jsonl — one JSON row per raw
	// nomination → canonical ID decision, in the order the append-only ledger recorded them (§9's system
	// of record). Present only when a canonicalizing mode ran.
	if r.CanonicalizerPrompt != "" {
		if err := write("canonicalizer-prompt.txt", []byte(r.CanonicalizerPrompt)); err != nil {
			return err
		}
	}
	if len(r.RawCanonicalization) > 0 {
		if err := write("raw-canonicalization.txt", r.RawCanonicalization); err != nil {
			return err
		}
	}
	if r.Canonicalization != nil {
		if err := write("merge-ledger.jsonl", ledgerJSONL(r.Canonicalization)); err != nil {
			return err
		}
	}

	// Governance artifacts (design §1/§4/§9), each written only when the stage ran — so a Map/Synthesize/
	// Catalog run's dump contains none of them. The provisional merge-ledger is written ALONGSIDE the confirmed one
	// (never replaced): a confirmation revision is a new append-only entry and the superseded revision must stay
	// readable, which is what makes the revision chain checkable after the fact.
	if r.Provisional != nil {
		if err := write("merge-ledger-provisional.jsonl", ledgerJSONL(r.Provisional)); err != nil {
			return err
		}
	}
	if r.ConfirmationPrompt != "" {
		if err := write("confirmation-prompt.txt", []byte(r.ConfirmationPrompt)); err != nil {
			return err
		}
	}
	if r.Confirmation != nil {
		if err := writeJSON("confirmation.json", r.Confirmation); err != nil {
			return err
		}
	}
	if len(r.Preflight) > 0 {
		if err := writeJSON("preflight.json", r.Preflight); err != nil {
			return err
		}
	}
	if len(r.Rounds) > 0 {
		if err := writeJSON("rounds.json", r.Rounds); err != nil {
			return err
		}
	}
	if len(r.Mediations) > 0 {
		if err := writeJSON("mediations.json", r.Mediations); err != nil {
			return err
		}
	}
	for i, p := range r.LaterRoundPrompts {
		if err := write(fmt.Sprintf("round-%d-prompt.txt", i+2), []byte(p)); err != nil {
			return err
		}
	}
	if r.Governance != nil {
		if err := writeJSON("governance-claims.json", r.Governance); err != nil {
			return err
		}
	}
	// The BALLOT decision (§9's decisions.json + ballots.json): the frozen+hashed inputs, every recorded
	// ballot, and the host tally. Written only for a ballot-bearing mode, so no other mode's dump contains
	// it. It is written even when the run halted before the tally — the frozen inputs alone are the
	// evidence that the framing predated the vote.
	if r.Decision != nil {
		if err := writeJSON("decision.json", r.Decision); err != nil {
			return err
		}
	}
	if r.Degraded != nil {
		if err := writeJSON("degraded.json", r.Degraded); err != nil {
			return err
		}
	}

	// Per-explorer: raw body + the full envelope (order-keyed dir, hash ID inside).
	var aliases []AliasEntry
	for _, env := range r.Envelopes {
		dir := fmt.Sprintf("calls/envelope-%d", env.Order)
		if err := write(dir+"/raw.txt", env.RawResponse); err != nil {
			return err
		}
		if err := writeJSON(dir+"/envelope.json", env); err != nil {
			return err
		}
		aliases = append(aliases, AliasEntry{
			Alias: fmt.Sprintf("envelope#%d", env.Order), ID: env.ID, Order: env.Order,
			// An envelope that exists is citable. Only a deliberate abstention withholds a position, and an
			// abstaining response carries no content to cite in the first place.
			PrimaryEligible: !schema.IsAbstention(env.Response),
		})
	}

	// Per dropped explorer: the bounded raw body it sent (when captured) + a structured record carrying
	// the reason, applied repairs, lengths, truncation flag, and digests — so a halted panel is
	// reconstructable from the dump, not only its formulation.
	var dropped []DropRecord
	for i, d := range r.Dropped {
		rec := DropRecord{
			Adapter: d.Explorer.Adapter, Model: d.Explorer.Model, Effort: d.Explorer.Effort,
			Reason: d.Reason, Repairs: d.Repairs,
			CapturedLen: d.CapturedLen, FullLen: d.FullLen, Truncated: d.Truncated,
			SHA256: d.SHA256, FullSHA256: d.FullSHA256,
			ExitCode: d.ExitCode, StderrExcerpt: d.StderrExcerpt,
		}
		if len(d.Raw) > 0 {
			rel := fmt.Sprintf("dropped/explorer-%d/raw.txt", i)
			if err := write(rel, d.Raw); err != nil {
				return err
			}
			rec.RawPath = rel
		}
		dropped = append(dropped, rec)
	}

	payloadHash, _ := r.Formulation.Payload.Hash()
	// Manifest LAST + atomic (temp + rename): absence ⇒ the run did not finish writing.
	m := ManifestV1{
		SchemaVersion: ManifestSchemaVersion, RunID: in.Run.ID, Status: in.Status, Fault: in.Fault,
		Mode:           r.Mode,
		EnvelopeIDAlgo: pipeline.EnvelopeIDVersion(), PayloadHash: payloadHash,
		Explorers: explorerIdentities(in.Plan), Collator: in.Plan.Collator.Identity(),
		Envelopes: aliases, Dropped: dropped, Artifacts: arts,
		Trace: in.Trace,
	}
	// A held canonicalization (present ⇒ the surjectivity gate passed) records its partition hash + row
	// count in the manifest so the merge-ledger is auditable from the manifest alone.
	if r.Canonicalization != nil {
		m.PartitionRevisionHash = r.Canonicalization.PartitionRevisionHash
		m.Surjectivity = "holds"
		m.LedgerRows = r.Canonicalization.Ledger.Len()
		m.AgreementRuleVersion = r.Canonicalization.AgreementRuleVersion
		if rev := r.Canonicalization.Ledger.Revision(); rev > 1 {
			m.LedgerRevision = rev
			m.PriorRevisionHash = r.Canonicalization.Ledger.PriorRevisionHash()
		}
	}
	// The canonicalizer identities + how they were CHOSEN. Taken from CanonicalizerCalls (the identities that
	// actually ran) rather than from the plan, so the manifest records what happened, not what was intended.
	for _, cc := range r.CanonicalizerCalls {
		m.Canonicalizers = append(m.Canonicalizers, cc.Identity)
	}
	m.CanonicalizerProvenance = r.CanonicalizerProvenance
	m.CanonicalizerIndependence = r.CanonicalizerIndependence
	if len(r.Rounds) > 1 {
		m.Rounds = len(r.Rounds)
	}
	if r.Confirmation != nil {
		m.ConfirmationRule = r.Confirmation.RuleVersion
		m.Challenges = len(r.Confirmation.Challenges)
		m.ContestedMappings = len(r.Confirmation.Contested)
	}
	if r.Panel.PolicyHash != "" {
		m.CountingPolicyHash = r.Panel.PolicyHash
	}
	if r.Governance != nil {
		m.GovernanceClaimsHash = r.Governance.ClaimsHash
		m.GovernanceClaims = len(r.Governance.Claims)
	}
	if r.Degraded != nil {
		m.DegradedTerminalClass = string(r.Degraded.Class)
	}
	if r.Decision != nil {
		m.DecisionRule = r.Decision.RuleVersion
		m.DecisionInputsHash = r.Decision.Frozen.InputsHash
		m.BallotsCast = r.Decision.Cast
		m.Ranked = len(r.Decision.Shortlisted())
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(in.Run.Dir, ".manifest.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(in.Run.Dir, "manifest.json"))
}

// ledgerJSONL renders a canonicalization revision's append-only merge-ledger as JSON Lines — one row per raw
// nomination → canonical ID decision, in the exact order the ledger recorded them (§9's system of record).
func ledgerJSONL(res *canon.Result) []byte {
	var buf bytes.Buffer
	for _, row := range res.Ledger.Rows() {
		b, err := json.Marshal(row)
		if err != nil {
			continue // a LedgerRow is plain data; an unmarshalable row cannot occur in practice
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func explorerIdentities(p roster.Plan) []schema.ExplorerIdentity {
	out := make([]schema.ExplorerIdentity, 0, len(p.Explorers))
	for _, e := range p.Explorers {
		out = append(out, e.Identity())
	}
	return out
}
