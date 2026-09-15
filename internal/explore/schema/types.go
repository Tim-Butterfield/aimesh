package schema

// This file holds the explorer envelope, identity types and the Map collator output.

import (
	"fmt"
)

// ExplorerIdentity is an explorer's attribution key. Explorers sharing a model with different efforts are
// distinct.
type ExplorerIdentity struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// IdentityStatus is how well a response's model identity was verified. It is recorded and reported but
// never used to include or exclude a response. See docs/model-identity.md.
type IdentityStatus string

// Identity statuses.
const (
	IdentityVerified     IdentityStatus = "verified"
	IdentitySelfReported IdentityStatus = "self_reported"
	IdentityUnknown      IdentityStatus = "unknown"
	IdentityMismatch     IdentityStatus = "mismatch"
)

// IsWeak reports whether s is anything other than IdentityVerified. It only controls caveat text.
func (s IdentityStatus) IsWeak() bool { return s != IdentityVerified }

// IdentityEvidence is the capped evidence tier behind an IdentityStatus, such as envelope, trace or
// self_report.
type IdentityEvidence string

// Envelope wraps one explorer response with its identity, order and payload hash. Response holds the
// decoded, schema-validated response.
type Envelope struct {
	ID               string           `json:"id"` // stable id derived from payload hash, identity and order
	Identity         ExplorerIdentity `json:"identity"`
	IdentityStatus   IdentityStatus   `json:"identityStatus"`
	IdentityEvidence IdentityEvidence `json:"identityEvidence,omitempty"`
	IdentityCaveat   string           `json:"identityCaveat,omitempty"`
	Order            int              `json:"order"`       // position in the panel
	PayloadHash      string           `json:"payloadHash"` // hash of the payload this explorer received
	Response         map[string]any   `json:"response"`
	// RawResponse is the response body the pipeline decoded, kept for run capture. It is never marshaled.
	RawResponse []byte `json:"-"`
	// Repairs lists the JSON-extraction repairs applied before decoding (see ExtractJSONObject).
	Repairs []string `json:"repairs,omitempty"`
}

// EnvelopeRef returns the reference for the envelope at panel position order in round roundIndex. Round 1
// uses envelope#k and later rounds use rN:envelope#k, so refs never collide across rounds.
func EnvelopeRef(roundIndex, order int) string {
	if roundIndex <= 1 {
		return fmt.Sprintf("envelope#%d", order)
	}
	return fmt.Sprintf("r%d:envelope#%d", roundIndex, order)
}

// AbstentionField is the optional response field an explorer sets to true to abstain deliberately, as
// opposed to failing to answer.
const AbstentionField = "abstain"

// IsAbstention reports whether response sets "abstain": true. Abstentions are recorded but excluded from
// the respondents denominator; the panel denominator is unchanged.
func IsAbstention(response map[string]any) bool {
	v, ok := response[AbstentionField].(bool)
	return ok && v
}

// CollatorOutput is the Map mode's collator output.
type CollatorOutput struct {
	SynthesisSummary     string              `json:"synthesisSummary"`
	Findings             []Finding           `json:"findings"`
	DisagreementRegister []DisagreementEntry `json:"disagreementRegister"`
}

// Summary returns the synthesis summary.
func (o CollatorOutput) Summary() string { return o.SynthesisSummary }

// Finding is one synthesized conclusion with its evidence, confidence and citations.
type Finding struct {
	Statement  string  `json:"statement"`
	Evidence   string  `json:"evidence,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	// Sources are envelope#k citations. After ApplyCitations every entry resolves to a primary envelope;
	// unresolvable refs are removed.
	Sources []string `json:"sources,omitempty"`
	// Uncited is set by ApplyCitations when no citation resolves, overwriting any model-supplied value. The
	// finding is kept.
	Uncited bool `json:"uncited,omitempty"`
	// UnverifiedReferences are sources that point outside the run, such as file paths or URLs. They are
	// kept but not verified and never count as citations.
	UnverifiedReferences []string `json:"unverifiedReferences,omitempty"`
}

// DisagreementEntry records a disagreement across explorers: the subject, each explorer's position, any
// resolution and the residual risk.
type DisagreementEntry struct {
	Subject      string     `json:"subject"`
	Positions    []Position `json:"positions"`
	Resolution   string     `json:"resolution,omitempty"`
	ResidualRisk string     `json:"residualRisk,omitempty"`
}

// Position is one explorer's stance on a disagreement subject.
type Position struct {
	Explorer ExplorerIdentity `json:"explorer"`
	Stance   string           `json:"stance"`
	Evidence string           `json:"evidence,omitempty"`
}
