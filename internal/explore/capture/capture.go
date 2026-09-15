// Package capture writes an exploration's run directory from a pipeline.Result, after the run returns,
// so the pipeline itself has no side effects. The manifest is written last and atomically; a directory
// without one did not finish writing.
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

// ManifestSchemaVersion is the manifest's schema version. Increment it whenever the set of possible keys
// changes, since recordings are compared only within one version.
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

// DropRecord is the manifest entry for a dropped explorer: its identity and reason, extraction repairs,
// raw-body lengths and digests, and the path of the captured raw body (empty if none was captured).
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
	// ExitCode and StderrExcerpt are the adapter process's failure signal, omitted when there was none.
	ExitCode      int    `json:"exitCode,omitempty"`
	StderrExcerpt string `json:"stderrExcerpt,omitempty"`
}

// ManifestV1 is the typed index of a recorded run.
type ManifestV1 struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         string `json:"runId"`
	Status        string `json:"status"` // "complete" | "halted"
	Fault         string `json:"fault,omitempty"`
	// Mode is the mode the run executed.
	Mode           string                    `json:"mode,omitempty"`
	EnvelopeIDAlgo string                    `json:"envelopeIdAlgo"`
	PayloadHash    string                    `json:"payloadHash"`
	Explorers      []schema.ExplorerIdentity `json:"explorers"`
	Collator       schema.ExplorerIdentity   `json:"collator"`
	Envelopes      []AliasEntry              `json:"envelopes"`
	Dropped        []DropRecord              `json:"dropped,omitempty"`
	Artifacts      []ArtifactEntry           `json:"artifacts"`
	// PartitionRevisionHash, Surjectivity and LedgerRows are set for a canonicalizing mode whose partition
	// passed the surjectivity gate.
	PartitionRevisionHash string `json:"partitionRevisionHash,omitempty"`
	Surjectivity          string `json:"surjectivity,omitempty"`
	LedgerRows            int    `json:"ledgerRows,omitempty"`
	// Canonicalizers are the identities that canonicalized; CanonicalizerProvenance is `explicit` or
	// `derived`.
	Canonicalizers          []schema.ExplorerIdentity `json:"canonicalizers,omitempty"`
	CanonicalizerProvenance string                    `json:"canonicalizerProvenance,omitempty"`
	// CanonicalizerIndependence is `distinct_models` or `shared_model`.
	CanonicalizerIndependence string `json:"canonicalizerIndependence,omitempty"`
	// The governance fields below are set only when the corresponding stage ran.
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
	// The ballot fields are set only for a ballot-bearing mode. DecisionInputsHash is the hash of the
	// inputs frozen before the ballot.
	DecisionRule       string `json:"decisionRule,omitempty"`
	DecisionInputsHash string `json:"decisionInputsHash,omitempty"`
	BallotsCast        int    `json:"ballotsCast,omitempty"`
	Ranked             int    `json:"ranked,omitempty"`
	// Trace is the caller's W3C trace context, if the request carried one.
	Trace *TraceContext `json:"trace,omitempty"`
}

// TraceContext is the caller's W3C trace context, copied as sent so the run can be correlated with the
// caller's trace. Values are not validated: the MCP specification says implementations must not make
// assumptions about reserved `_meta` values. It supports correlation only; runs record no token or cost
// data.
type TraceContext struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
	Baggage     string `json:"baggage,omitempty"`
	// Measures states what the block accounts for; currently always MeasuresCorrelationOnly.
	Measures string `json:"measures"`
}

// MeasuresCorrelationOnly is the TraceContext.Measures value for a block that carries correlation ids
// only.
const MeasuresCorrelationOnly = "correlation_only"

// NewTraceContext returns a TraceContext for the given values, or nil when all are empty.
func NewTraceContext(traceParent, traceState, baggage string) *TraceContext {
	if traceParent == "" && traceState == "" && baggage == "" {
		return nil
	}
	return &TraceContext{
		TraceParent: traceParent, TraceState: traceState, Baggage: baggage,
		Measures: MeasuresCorrelationOnly,
	}
}

// ArtifactSubdir is the explore domain's directory under the shared `.aimesh/` state directory.
const ArtifactSubdir = "explore"

// ArtifactDir returns the base directory for captured runs, honoring $EXPLOREMESH_ARTIFACT_DIR. Every
// surface uses it, so all runs land in the same place.
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
	// Task is the declared task, written as `task.json` when its purpose is non-empty.
	Task schema.RawTask
	// Trace is the caller's trace context, or nil.
	Trace *TraceContext
}

// Dump writes every artifact, then the manifest last and atomically. Callers use it for both completed and
// halted runs.
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

	// Canonicalizing modes: the canonicalizer prompt, its raw output and the merge ledger.
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

	// Governance artifacts, each written only when its stage ran. The provisional ledger is written beside
	// the confirmed one so the revision chain can be checked.
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
	// The ballot decision, written even if the run halted before the tally.
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
			PrimaryEligible: !schema.IsAbstention(env.Response),
		})
	}

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
	m := ManifestV1{
		SchemaVersion: ManifestSchemaVersion, RunID: in.Run.ID, Status: in.Status, Fault: in.Fault,
		Mode:           r.Mode,
		EnvelopeIDAlgo: pipeline.EnvelopeIDVersion(), PayloadHash: payloadHash,
		Explorers: explorerIdentities(in.Plan), Collator: in.Plan.Collator.Identity(),
		Envelopes: aliases, Dropped: dropped, Artifacts: arts,
		Trace: in.Trace,
	}
	// A recorded canonicalization has passed the surjectivity gate.
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
	// Canonicalizers come from the calls that ran, not from the plan.
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
	// Write to a temporary file and rename, so manifest.json appears only when complete.
	tmp := filepath.Join(in.Run.Dir, ".manifest.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(in.Run.Dir, "manifest.json"))
}

// ledgerJSONL renders a merge ledger as JSON Lines, one row per line in ledger order.
func ledgerJSONL(res *canon.Result) []byte {
	var buf bytes.Buffer
	for _, row := range res.Ledger.Rows() {
		b, err := json.Marshal(row)
		if err != nil {
			continue
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
