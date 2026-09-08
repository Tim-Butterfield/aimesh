package schema

// This file holds the explorer outer envelope + model-identity policy and the fixed, exploremesh-
// owned collator-output schema (design §6.2–§6.4, §6.7).

import (
	"fmt"
)

// ExplorerIdentity is the full attribution key for an explorer (design §6.1): the (adapter, model,
// effort) triple. Attribution keys on the WHOLE triple, never the model alone — two explorers may
// share a model (Opus-high vs Opus-low), so a model-only key would collide.
type ExplorerIdentity struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// IdentityStatus mirrors meshcore's model-identity tiers (verify): a strong-evidence match, a
// self-reported claim, an unknown (no usable evidence), or a strong-evidence MISMATCH. exploremesh
// maps meshcore/verify's classification onto these; kept as a local type so the schema package does
// not depend on the identity engine (the pipeline in P3.4 does the mapping).
//
// The status is DESCRIPTIVE ONLY. It is recorded on every envelope and surfaced to the reader, and
// nothing in the pipeline reads it to decide whether a response is used: a response's content
// determines whether it is worth anything, never the label on who produced it. See
// ../../../docs/model-identity.md.
type IdentityStatus string

const (
	IdentityVerified     IdentityStatus = "verified"
	IdentitySelfReported IdentityStatus = "self_reported"
	IdentityUnknown      IdentityStatus = "unknown"
	IdentityMismatch     IdentityStatus = "mismatch"
)

// IsWeak reports whether the status falls short of a strong-evidence match. It drives the CAVEAT text
// on the envelope and nothing else — a weak seat participates exactly like any other.
func (s IdentityStatus) IsWeak() bool { return s != IdentityVerified }

// IdentityEvidence is the CAPPED evidence tier behind an envelope's IdentityStatus (envelope > trace >
// cli_status > invocation_tag > self_report > none). Kept as a local string type so the schema package
// stays free of the identity engine (mirroring IdentityStatus); the pipeline maps core.IdentityEvidence
// (after verify.CapEvidence) onto it.
type IdentityEvidence string

// Envelope is the exploremesh-owned outer wrapper around one explorer response (design §6.3): the
// verified identity, ordering, an optional identity caveat, and the decoded response object
// (already validated against the expanded schema). Untrusted response content stays inside Response.
type Envelope struct {
	ID               string           `json:"id"` // stable slot id (hash of payloadHash+triple+order); citation anchor
	Identity         ExplorerIdentity `json:"identity"`
	IdentityStatus   IdentityStatus   `json:"identityStatus"`
	IdentityEvidence IdentityEvidence `json:"identityEvidence,omitempty"` // the CAPPED tier behind the status
	IdentityCaveat   string           `json:"identityCaveat,omitempty"`
	Order            int              `json:"order"`       // stable ordering index across the panel
	PayloadHash      string           `json:"payloadHash"` // hash of the shared payload this explorer received
	Response         map[string]any   `json:"response"`
	// RawResponse is the SEMANTIC body the pipeline consumed (Payload-else-Stdout), retained for the
	// --dump-run capture. json:"-" so it is NEVER marshaled into the synthesis prompt (that would bloat
	// the prompt + duplicate Response). It is the body the pipeline consumed, NOT raw adapter
	// stdout/stderr; failed-explorer output is out of scope until a recorder exists.
	RawResponse []byte `json:"-"`
	// Repairs are the enumerated JSON-extraction repairs (see schema.ExtractJSONObject) applied to the
	// raw body before it decoded — e.g. a stripped code fence. omitempty so a clean response (the common
	// case, no repairs) marshals identically to before: zero change to the synthesize prompt/capture.
	Repairs []string `json:"repairs,omitempty"`
}

// EnvelopeRef renders the prompt-facing / ledger-facing reference for the envelope produced by the
// explorer at panel position `order` in explorer round `roundIndex` (design §6). Round 1 renders the
// unqualified `envelope#k`, so a single-round Map/Synthesize/Catalog artifact carries bare refs; a
// LATER round is qualified with its round (`r2:envelope#k`)
// so refs never collide across rounds — which is what lets a count filter itself to the immutable blind
// round-1 artifacts (§0 F-A, the anti-echo invariant).
func EnvelopeRef(roundIndex, order int) string {
	if roundIndex <= 1 {
		return fmt.Sprintf("envelope#%d", order)
	}
	return fmt.Sprintf("r%d:envelope#%d", roundIndex, order)
}

// AbstentionField is the reserved, OPTIONAL response field an explorer sets to decline to answer (design §1's
// DELIBERATE ABSTENTION, as distinct from a technical absence). It is a host-recognized channel rather than a
// prose convention precisely because the two must be tallied separately: an explorer that could not answer and
// an explorer that chose not to answer say different things about a count's denominator.
const AbstentionField = "abstain"

// IsAbstention reports whether a validated explorer response DELIBERATELY abstains — `"abstain": true`. An
// abstaining response is kept in the record (it is a position about the question) but contributes no content,
// so the host excludes it from the primary panel and from the RESPONDENTS denominator while leaving the PANEL
// denominator untouched.
func IsAbstention(response map[string]any) bool {
	v, ok := response[AbstentionField].(bool)
	return ok && v
}

// CollatorOutput is the FIXED, exploremesh-owned synthesis schema (design §6.4) — not collator-
// defined. It is the Map mode's terminal collator output; it satisfies the mode-package ModeOutput
// contract via Summary().
type CollatorOutput struct {
	SynthesisSummary     string              `json:"synthesisSummary"`
	Findings             []Finding           `json:"findings"`
	DisagreementRegister []DisagreementEntry `json:"disagreementRegister"`
}

// Summary returns the one-line human summary of a Map synthesis (its integrated narrative) — the
// implementation of the mode-package ModeOutput contract for Map's terminal output. A value receiver
// so both CollatorOutput and *CollatorOutput satisfy the interface.
func (o CollatorOutput) Summary() string { return o.SynthesisSummary }

// Finding is one synthesized conclusion. Synthesis is evidence-weighted (not a tally), so a finding
// records the supporting evidence + confidence, not a vote count.
type Finding struct {
	Statement  string  `json:"statement"`
	Evidence   string  `json:"evidence,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	// Sources are the `envelope#k` CITATIONS backing this finding (design §3 C1). After the host's
	// citation pass (ApplyCitations) every entry here is a ref that RESOLVES to a primary envelope of
	// this run: an unknown or malformed ref is dropped, never rewritten and never left in place.
	Sources []string `json:"sources,omitempty"`
	// Uncited is HOST-OWNED: it is set, unconditionally, by ApplyCitations when the finding is left with
	// no citation that resolves. It is decoded from the collator's output only so a model-supplied value
	// can be OVERWRITTEN — a model must never be able to self-certify that its own claim is sourced.
	// The finding itself is retained and reported: an uncited finding is labeled, not dropped.
	Uncited bool `json:"uncited,omitempty"`
	// UnverifiedReferences are the sources the collator supplied that name something OUTSIDE this run
	// entirely — a file path, a URL, a document title — rather than one of its `envelope#k` aliases.
	//
	// They are RETAINED AND LABELED rather than dropped, which is the same rule Uncited states one step
	// earlier. exploremesh has no workspace and reads no filesystem (that is a deliberate posture, not a
	// gap: its subject is the argument it was handed, and grounding a claim against a repository is the
	// CALLER's job), so it cannot check whether `src/foo.rs` exists. What it can do is refuse to let the
	// reference vanish: the honest report is "this finding points at src/foo.rs and NOTHING here verified
	// that", which is strictly more than either discarding the pointer or letting it sit in `Sources`
	// where it would read as provenance.
	//
	// They are NEVER citations. Sources holds refs that resolved; this holds refs that were not even
	// addressed to this run, and a finding carrying only these is still Uncited.
	UnverifiedReferences []string `json:"unverifiedReferences,omitempty"`
}

// DisagreementEntry records a substantive disagreement across explorers (design §6.4): the subject,
// each explorer's attributed position, the collator's resolution (or that it is left to the human),
// and the residual risk if the disagreement is unresolved.
type DisagreementEntry struct {
	Subject      string     `json:"subject"`
	Positions    []Position `json:"positions"`
	Resolution   string     `json:"resolution,omitempty"`
	ResidualRisk string     `json:"residualRisk,omitempty"`
}

// Position is one explorer's stance on a disagreement subject, keyed by full explorer identity.
type Position struct {
	Explorer ExplorerIdentity `json:"explorer"`
	Stance   string           `json:"stance"`
	Evidence string           `json:"evidence,omitempty"`
}
