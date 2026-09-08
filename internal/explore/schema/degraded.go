package schema

// This file holds the DEGRADED terminal artifact (design §1) — what exploremesh emits when the
// collator becomes unavailable AFTER the fan-out, or when an identity halt fires: the blind round-1
// artifacts are equally valid in both cases, so the run still produces a terminal artifact rather than
// nothing. It is deliberately PER MODE-CLASS (the REV 3 re-check finding: a synthesized "disagreement
// register" over free-form claims would itself be a covert entity-resolution step performed by the host
// with no ledger, no attribution, and no contestability):
//
//   - EMERGENT-space modes (Map, Synthesize, Catalog — the candidate/claim space is authored by the
//     explorers) → the RAW, ATTRIBUTED round-1 envelopes plus a MECHANICAL typed-claim index (one entry
//     per response field value per envelope; NO grouping, NO clustering, NO similarity), labeled
//     `uncollated — no entity resolution performed`.
//   - FIXED-space modes (Compare/Forecast; the option/criterion universe is GIVEN) → a REAL host
//     register keyed on the mode's declared key field: grouping by an exact value in a DECLARED universe
//     is arithmetic, not judgment, so the comparison is genuine.

import (
	"fmt"
	"sort"
)

// ModeClass is a mode's SPACE class (design §1/§3) — it decides which degraded terminal artifact is
// honest for the mode, and nothing else. It is app-owned mode grammar carried on the ModeSpec.
type ModeClass string

const (
	// EmergentSpace: the candidate/claim space is AUTHORED by the explorers, so any grouping across
	// explorers is entity resolution (§0 F-B) and may only happen through the recorded canonicalization
	// ledger — never inside a degraded fallback.
	EmergentSpace ModeClass = "emergent_space"
	// FixedSpace: the option/criterion universe is GIVEN to the explorers, so grouping by an exact value
	// in that declared universe is mechanical and a real host register is honest.
	FixedSpace ModeClass = "fixed_space"
)

// UncollatedLabel is the exact, non-negotiable label carried by an EMERGENT-space degraded artifact
// (design §1). It states plainly that no entity resolution happened, so no reader (or downstream
// exploration) can mistake the raw envelope set for a collated result.
const UncollatedLabel = "uncollated — no entity resolution performed"

// DegradedReason records WHY the degraded path fired — collator unavailability after the fan-out, or an
// identity halt (both leave the blind round-1 artifacts fully valid).
type DegradedReason string

const (
	DegradedCollatorUnavailable DegradedReason = "collator_unavailable_after_fanout"
	DegradedIdentityHalt        DegradedReason = "identity_halt"
)

// TypedClaim is one MECHANICAL entry of the emergent-space claim index: a single field value exactly as
// one explorer wrote it, attributed to that explorer's envelope. Entries are NEVER merged, deduplicated,
// ranked or clustered — the index is a transposition of the envelopes, not an interpretation of them.
type TypedClaim struct {
	EnvelopeRef string           `json:"envelopeRef"`
	Explorer    ExplorerIdentity `json:"explorer"`
	Field       string           `json:"field"`
	Value       string           `json:"value"`
	Index       int              `json:"index"` // position within a repeated field (0 for a scalar)
}

// RegisterEntry is one row of a FIXED-space degraded register: an exact value of the mode's declared key
// field plus every explorer that reported it, with that explorer's full response for the row. Genuine
// because the key universe was GIVEN to the explorers (§1).
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

// DegradedOutput is the terminal artifact for a run that lost its collator after the fan-out (design §1).
// It satisfies the mode-package ModeOutput contract via Summary(), so the surfaces render it exactly like
// any other terminal output — a degraded run is a RESULT, not a hole. Exactly one of ClaimIndex (emergent)
// or Register (fixed) is populated, per Class.
type DegradedOutput struct {
	Class      ModeClass       `json:"class"`
	Mode       string          `json:"mode"`
	Reason     DegradedReason  `json:"reason"`
	Detail     string          `json:"detail"`
	Label      string          `json:"label"`
	Envelopes  []Envelope      `json:"envelopes"`            // the RAW, attributed blind round-1 envelopes
	ClaimIndex []TypedClaim    `json:"claimIndex,omitempty"` // EMERGENT-space only (mechanical, ungrouped)
	Register   []RegisterEntry `json:"register,omitempty"`   // FIXED-space only (declared key universe)
}

// Summary renders the one-line human summary — always stating the class + label so a degraded artifact
// can never be mistaken for a collation in a log line or manifest echo.
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

// Degrade builds the degraded terminal artifact for a mode class (design §1). keyField is the FIXED-space
// mode's declared key field and is ignored for the emergent path; an emergent-space artifact ALWAYS
// carries UncollatedLabel. envelopes must be the blind round-1 envelopes (raw + attributed).
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

// claimIndex transposes the envelopes into the mechanical typed-claim index: every string-valued response
// field (scalar or repeated) becomes one attributed entry. Field names are visited in SORTED order and
// repeated values in their authored order, so the index is deterministic. Non-string values are rendered
// with %v — a mechanical stringification, never an interpretation. NOTHING is grouped or deduplicated.
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

// hostRegister groups the envelopes by the EXACT value of the mode's declared key field — legitimate for a
// FIXED-space mode because that value space was handed to the explorers (§1). A repeated key field yields
// one register row per value. Keys are emitted in sorted order for determinism; an envelope missing the key
// field is recorded under the empty key so nothing is silently dropped.
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
