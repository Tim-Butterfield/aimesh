package schema

// This file holds the C1 CITATION contract: the frozen
// prompt-facing citation grammar, the HOST's authoritative alias vocabulary for one collation, and
// the single validation pass that rewrites a collator's `Finding.Sources` into refs that actually
// resolve.
//
// The governing idea is that a citation is a HOST fact, not a model claim. The collator is asked to
// cite the `envelope#k` aliases it drew from, but nothing it emits is taken on trust: the host parses
// each ref against the frozen grammar, checks it against the aliases THIS run actually produced, and
// decides what survives. A model can therefore make a finding harder to source, but it can never
// manufacture provenance for one.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CitationRefPattern is the FROZEN citation grammar: the prompt-facing alias
// `envelope#k` (k = the envelope's panel Order), optionally narrowed to one claim as
// `envelope#k/claims/i` (i zero-based). Anchored, so a ref embedded in prose is NOT a citation.
//
// It is a package constant rather than an inline literal because it is a contract shared by the
// prompt (which teaches the vocabulary), the validator (which enforces it), and the capture manifest
// (whose `envelope#k → Envelope.ID` alias table is the same namespace). One of those drifting from
// the others is exactly the failure the constant exists to prevent.
const CitationRefPattern = `^envelope#([0-9]+)(?:/claims/([0-9]+))?$`

var citationRef = regexp.MustCompile(CitationRefPattern)

// claimsField is the explorer-response field a `envelope#k/claims/i` ref indexes into.
const claimsField = "claims"

// CitationIndex is the HOST's alias vocabulary for ONE collation: which `envelope#k` aliases exist
// and, per envelope, how many claims a `/claims/i` narrowing may address. It is built from the
// PRIMARY envelopes — the panel the collator was actually shown — so a syntactically well-formed ref
// to a dropped, weak or nonexistent explorer is correctly rejected as unknown.
//
// Note that k values are NOT contiguous in general: dropped and weak-identity explorers consume panel
// Order indices, so a three-explorer run whose middle explorer was weak has primary aliases {0, 2}.
// Membership is therefore a set lookup, never a range check.
type CitationIndex struct {
	claims map[int]int // envelope Order → len(Response["claims"]); 0 when the mode has no claims array
	order  []int       // primary Orders in panel order (the prompt-facing alias list)
}

// NewCitationIndex builds the vocabulary from the primary envelopes handed to a collator contract.
func NewCitationIndex(primary []Envelope) CitationIndex {
	ix := CitationIndex{claims: make(map[int]int, len(primary))}
	for _, env := range primary {
		n := 0
		if c, ok := env.Response[claimsField].([]any); ok {
			n = len(c)
		}
		ix.claims[env.Order] = n
		ix.order = append(ix.order, env.Order)
	}
	return ix
}

// Aliases returns the citable aliases in panel order — the exact vocabulary the collator prompt
// shows, so the prompt and the validator can never disagree about what is citable.
func (ix CitationIndex) Aliases() []string {
	out := make([]string, 0, len(ix.order))
	for _, k := range ix.order {
		out = append(out, EnvelopeRef(1, k))
	}
	return out
}

// Alias renders the prompt-facing alias for one primary envelope (round-1 `envelope#k`).
func (ix CitationIndex) Alias(order int) string { return EnvelopeRef(1, order) }

// Valid reports whether a raw `Finding.Sources` entry is a citation this run can honor: it matches
// the frozen grammar, has no leading-zero padding (so `envelope#0` and `envelope#00` cannot both name
// slot 0), names an envelope in the PRIMARY panel, and — when it narrows to a claim — indexes inside
// that envelope's actual `claims` array. Everything else is unknown, and unknown is dropped.
func (ix CitationIndex) Valid(ref string) bool {
	m := citationRef.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return false
	}
	k, ok := parseIndex(m[1])
	if !ok {
		return false
	}
	n, known := ix.claims[k]
	if !known {
		return false // syntactically fine, but not an envelope of THIS primary panel
	}
	if m[2] == "" {
		return true
	}
	i, ok := parseIndex(m[2])
	return ok && i < n
}

// parseIndex parses a citation index, rejecting leading-zero padding so exactly one spelling names
// each slot (`0`, `1`, `10` — never `00` or `01`).
func parseIndex(s string) (int, bool) {
	if len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// CitationReport is the host's record of what the citation pass did. It is reported, never gated:
// the citation axis is FAIL-SOFT by design (see ApplyCitations).
type CitationReport struct {
	Findings    int `json:"findings"`
	Cited       int `json:"cited"`
	Uncited     int `json:"uncited"`
	RefsKept    int `json:"refsKept"`
	RefsDropped int `json:"refsDropped"`
	// RefsUnverified counts the sources that named something OUTSIDE this run — a path, a URL, a
	// document — rather than one of its aliases. They are retained on the finding as
	// UnverifiedReferences instead of being dropped; see there for why.
	RefsUnverified int `json:"refsUnverified,omitempty"`
}

// String renders the report as one line for a progress event / log.
func (r CitationReport) String() string {
	s := fmt.Sprintf("%d/%d finding(s) cited, %d uncited; %d ref(s) kept, %d dropped",
		r.Cited, r.Findings, r.Uncited, r.RefsKept, r.RefsDropped)
	if r.RefsUnverified > 0 {
		s += fmt.Sprintf(", %d external reference(s) retained UNVERIFIED", r.RefsUnverified)
	}
	return s
}

// The bounds on retained external references. A reference is a pointer, not a payload: eight of them
// is already more than a reader will follow, and a model that puts a paragraph in `sources` must not
// be able to move that paragraph into the result under a label that says the host examined it.
const (
	maxUnverifiedRefs     = 8
	maxUnverifiedRefBytes = 200
)

// external reports whether a source names something outside this run ENTIRELY, as opposed to being a
// citation of this run that failed to resolve.
//
// The distinction is the whole reason unverified references are a separate field. `envelope#9` on a
// three-explorer panel is a ref the host CHECKED and rejected — it was addressed to this run, this run
// knows every alias it produced, and the answer is a definite no. `src/foo.rs` was never addressed to
// this run at all, and the honest thing to say about it is not "rejected" but "nothing here looked".
// Collapsing the two would either drop a live pointer or claim a check that never happened.
func external(ref string) bool { return !citationRef.MatchString(strings.TrimSpace(ref)) }

// clipRef normalizes one retained reference to a single bounded line. Newlines are collapsed because a
// reference occupying six lines of a result is prose that was put in the wrong field, and the clip is
// MARKED so a shortened pointer never reads like a complete one.
func clipRef(ref string) string {
	s := strings.Join(strings.Fields(ref), " ")
	if len(s) > maxUnverifiedRefBytes {
		s = strings.TrimSpace(s[:maxUnverifiedRefBytes]) + "…(clipped)"
	}
	return s
}

// ApplyCitations is the SINGLE, HOST-AUTHORITATIVE citation pass over a Map collation (design §3 C1).
// For every finding it:
//
//   - keeps each source that resolves to a primary `envelope#k` alias of this run;
//   - DROPS each source that is a ref to THIS run and does not resolve — `envelope#9` on a three-seat
//     panel is checked and rejected, never "corrected" to a nearby alias and never left in place.
//     Silently rewriting a ref would fabricate provenance; leaving it would let a dangling ref pass as
//     a citation;
//   - RETAINS each source that names something outside this run — a path, a URL, a document — as an
//     UnverifiedReference rather than dropping it. exploremesh reads no filesystem, so it cannot say
//     whether `src/foo.rs` exists; it can say that the finding points there and that nothing here
//     checked. That is a different statement from both "sourced" and "unsourced", and it needs its own
//     field to stay distinguishable from either;
//   - sets Uncited UNCONDITIONALLY from the surviving refs, overwriting whatever the model supplied.
//     The model is a witness about the world, never an authority about its own sourcing. An unverified
//     external reference does NOT count toward being cited — nothing was verified, so nothing was
//     sourced.
//
// The finding itself is always RETAINED. This is deliberately fail-soft on the citation axis: the
// collator contract is per-mode and a citation defect is not an epistemic failure of the run, so a
// bad ref must not turn a working exploration into a halt. An uncited finding is delivered and
// LABELED — the user sees a conclusion whose support the host could not confirm, which is strictly
// more information than either dropping it or pretending it was sourced.
func ApplyCitations(out *CollatorOutput, primary []Envelope) CitationReport {
	ix := NewCitationIndex(primary)
	rep := CitationReport{Findings: len(out.Findings)}
	for i := range out.Findings {
		f := &out.Findings[i]
		var kept, unverified []string
		for _, src := range f.Sources {
			if ix.Valid(src) {
				kept = append(kept, strings.TrimSpace(src))
				rep.RefsKept++
				continue
			}
			if external(src) {
				rep.RefsUnverified++
				if s := clipRef(src); s != "" && len(unverified) < maxUnverifiedRefs {
					unverified = append(unverified, s)
				}
				continue
			}
			rep.RefsDropped++
		}
		f.Sources = kept
		f.UnverifiedReferences = unverified
		f.Uncited = len(kept) == 0 // host-owned: overwrites any model-supplied claim
		if f.Uncited {
			rep.Uncited++
		} else {
			rep.Cited++
		}
	}
	return rep
}
