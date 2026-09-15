package schema

// This file holds the citation contract: the citation grammar the collator prompt teaches, the aliases
// one collation can cite, and the pass that reduces a collator's Finding.Sources to citations that
// resolve. The host decides which citations stand, so a model cannot invent provenance for a finding.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CitationRefPattern is the citation grammar: `envelope#k`, where k is the envelope's panel Order,
// optionally narrowed to one claim as `envelope#k/claims/i` (i zero-based). It is anchored, so a ref
// inside prose is not a citation. The prompt, the validator and the capture manifest's alias table all
// share it.
const CitationRefPattern = `^envelope#([0-9]+)(?:/claims/([0-9]+))?$`

var citationRef = regexp.MustCompile(CitationRefPattern)

// claimsField is the explorer-response field a `envelope#k/claims/i` ref indexes into.
const claimsField = "claims"

// CitationIndex is the set of aliases one collation can cite, with each envelope's claim count. It is built
// from the primary envelopes the collator was shown, so a ref to a dropped or weak explorer is unknown.
// Aliases need not be contiguous, because dropped explorers keep their panel Order.
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

// Aliases returns the citable aliases in panel order, as the collator prompt lists them.
func (ix CitationIndex) Aliases() []string {
	out := make([]string, 0, len(ix.order))
	for _, k := range ix.order {
		out = append(out, EnvelopeRef(1, k))
	}
	return out
}

// Valid reports whether ref is a citation this run can honor: it matches the grammar without leading
// zeros, names a primary envelope, and any claim index is within that envelope's claims.
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
		return false // well-formed, but not an envelope of this primary panel
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

// CitationReport records what the citation pass did. It is reported and never fails a run.
type CitationReport struct {
	Findings    int `json:"findings"`
	Cited       int `json:"cited"`
	Uncited     int `json:"uncited"`
	RefsKept    int `json:"refsKept"`
	RefsDropped int `json:"refsDropped"`
	// RefsUnverified counts sources that name something outside the run, such as a path or URL. They are
	// kept on the finding as UnverifiedReferences.
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

// Bounds on retained external references, so a model cannot move prose into the result through
// `sources`.
const (
	maxUnverifiedRefs     = 8
	maxUnverifiedRefBytes = 200
)

// external reports whether a source names something outside the run, rather than being a citation of
// the run. An unresolvable `envelope#9` is checked and dropped; `src/foo.rs` was never checked and is
// kept as unverified.
func external(ref string) bool { return !citationRef.MatchString(strings.TrimSpace(ref)) }

// clipRef collapses a retained reference to one line of at most maxUnverifiedRefBytes, marking a clipped
// reference.
func clipRef(ref string) string {
	s := strings.Join(strings.Fields(ref), " ")
	if len(s) > maxUnverifiedRefBytes {
		s = strings.TrimSpace(s[:maxUnverifiedRefBytes]) + "…(clipped)"
	}
	return s
}

// ApplyCitations is the host's citation pass over a map collation. For every finding it keeps sources
// that resolve to a primary alias, drops citations of the run that do not resolve (never correcting
// them), keeps sources naming something outside the run as unverified references, and sets Uncited from
// the kept citations alone, overriding the model. Findings are always kept: an uncited finding is
// delivered and labeled rather than failing the run.
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
		f.Uncited = len(kept) == 0 // overrides any model-supplied value
		if f.Uncited {
			rep.Uncited++
		} else {
			rep.Cited++
		}
	}
	return rep
}
