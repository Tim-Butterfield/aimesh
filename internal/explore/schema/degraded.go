package schema

// This file holds the degraded terminal artifact, emitted when the collator becomes unavailable after the
// fan-out or an identity halt fires. The blind round-1 responses are still valid, so the run still ends
// with a result. Its shape depends on the mode's space class:
//
//   - Emergent-space modes (map, synthesize, catalog), where explorers author the candidates, get the
//     raw attributed envelopes plus a mechanical claim index with no grouping, labeled uncollated.
//     Grouping free-form claims would be unrecorded entity resolution.
//   - Fixed-space modes (compare, forecast), where the options are declared, get a register grouped by
//     the exact value of the declared key field, which involves no judgment.

import (
	"fmt"
	"sort"
)

// ModeClass is a mode's space class, which selects its degraded terminal artifact. It is set on the
// ModeSpec.
type ModeClass string

const (
	// EmergentSpace means explorers author the candidates, so grouping across explorers is entity
	// resolution and happens only through the recorded canonicalization ledger.
	EmergentSpace ModeClass = "emergent_space"
	// FixedSpace means the options are declared up front, so grouping by an exact declared value is
	// mechanical.
	FixedSpace ModeClass = "fixed_space"
)

// UncollatedLabel is the label on an emergent-space degraded artifact, stating that no entity resolution
// was performed.
const UncollatedLabel = "uncollated — no entity resolution performed"

// DegradedReason records why the degraded path ran.
type DegradedReason string

// The reasons a run ends with a degraded artifact.
const (
	DegradedCollatorUnavailable DegradedReason = "collator_unavailable_after_fanout"
	DegradedIdentityHalt        DegradedReason = "identity_halt"
)

// TypedClaim is one entry of the emergent-space claim index: a single field value exactly as one explorer
// wrote it, attributed to its envelope. Entries are never merged, deduplicated, ranked or clustered.
type TypedClaim struct {
	EnvelopeRef string           `json:"envelopeRef"`
	Explorer    ExplorerIdentity `json:"explorer"`
	Field       string           `json:"field"`
	Value       string           `json:"value"`
	Index       int              `json:"index"` // position within a repeated field (0 for a scalar)
}

// RegisterEntry is one row of a fixed-space degraded register: an exact value of the mode's declared key
// field and every explorer that reported it, with that explorer's full response.
type RegisterEntry struct {
	Key       string             `json:"key"`
	Positions []RegisterPosition `json:"positions"`
}

// RegisterPosition is one explorer's attributed row within a fixed-space register entry.
type RegisterPosition struct {
	Explorer    ExplorerIdentity `json:"explorer"`
	EnvelopeRef string           `json:"envelopeRef"`
	Response    map[string]any   `json:"response"`
}

// DegradedOutput is the terminal artifact of a degraded run. It satisfies the mode package's ModeOutput
// contract, so surfaces render it like any other result. ClaimIndex is set for emergent-space modes and
// Register for fixed-space modes.
type DegradedOutput struct {
	Class      ModeClass       `json:"class"`
	Mode       string          `json:"mode"`
	Reason     DegradedReason  `json:"reason"`
	Detail     string          `json:"detail"`
	Label      string          `json:"label"`
	Envelopes  []Envelope      `json:"envelopes"`            // the raw, attributed round-1 envelopes
	ClaimIndex []TypedClaim    `json:"claimIndex,omitempty"` // emergent-space only
	Register   []RegisterEntry `json:"register,omitempty"`   // fixed-space only
}

// Summary returns a one-line summary that names the degraded state, so it cannot be mistaken for a
// collation.
func (o DegradedOutput) Summary() string {
	switch o.Class {
	case FixedSpace:
		return fmt.Sprintf("DEGRADED (%s, %s): host register over %d envelope(s), %d key(s) — %s",
			o.Mode, o.Reason, len(o.Envelopes), len(o.Register), o.Detail)
	default:
		return fmt.Sprintf("DEGRADED (%s, %s): %s — %d raw attributed envelope(s), %d mechanical claim(s); %s",
			o.Mode, o.Reason, o.Label, len(o.Envelopes), len(o.ClaimIndex), o.Detail)
	}
}

// Degrade builds the degraded terminal artifact for a mode class from the round-1 envelopes. keyField is
// the fixed-space mode's declared key field and is ignored for emergent-space modes, whose artifact
// always carries UncollatedLabel.
func Degrade(class ModeClass, modeName string, reason DegradedReason, detail string, envelopes []Envelope, keyField string) DegradedOutput {
	out := DegradedOutput{
		Class: class, Mode: modeName, Reason: reason, Detail: detail,
		Envelopes: append([]Envelope(nil), envelopes...),
	}
	if class == FixedSpace {
		out.Register = hostRegister(envelopes, keyField)
		return out
	}
	out.Label = UncollatedLabel
	out.ClaimIndex = claimIndex(envelopes)
	return out
}

// claimIndex lists every response field value of every envelope as one attributed entry. Fields are
// visited in sorted order and repeated values in their written order, so the index is deterministic;
// non-string values are formatted with %v.
func claimIndex(envelopes []Envelope) []TypedClaim {
	var out []TypedClaim
	for _, env := range envelopes {
		names := make([]string, 0, len(env.Response))
		for name := range env.Response {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			switch v := env.Response[name].(type) {
			case []any:
				for i, el := range v {
					out = append(out, TypedClaim{
						EnvelopeRef: EnvelopeRef(1, env.Order), Explorer: env.Identity,
						Field: name, Value: fmt.Sprintf("%v", el), Index: i,
					})
				}
			default:
				out = append(out, TypedClaim{
					EnvelopeRef: EnvelopeRef(1, env.Order), Explorer: env.Identity,
					Field: name, Value: fmt.Sprintf("%v", v),
				})
			}
		}
	}
	return out
}

// hostRegister groups the envelopes by the exact value of keyField, one row per value of a repeated field,
// with keys in sorted order. An envelope without the field is recorded under the empty key, so none is
// dropped.
func hostRegister(envelopes []Envelope, keyField string) []RegisterEntry {
	byKey := map[string][]RegisterPosition{}
	add := func(key string, env Envelope) {
		byKey[key] = append(byKey[key], RegisterPosition{
			Explorer: env.Identity, EnvelopeRef: EnvelopeRef(1, env.Order), Response: env.Response,
		})
	}
	for _, env := range envelopes {
		switch v := env.Response[keyField].(type) {
		case []any:
			for _, el := range v {
				add(fmt.Sprintf("%v", el), env)
			}
		case nil:
			add("", env)
		default:
			add(fmt.Sprintf("%v", v), env)
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]RegisterEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, RegisterEntry{Key: k, Positions: byKey[k]})
	}
	return out
}
