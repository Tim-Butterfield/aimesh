// Package govern is exploremesh's host-side governance arithmetic: the frozen panel and its two
// denominators, the blind round-1 baseline that independence counts use, and the claims that pin each
// count to its inputs. It calls no model.
//
//   - Independence counts use only blind round-1 envelopes. NewBlindBaseline refuses any other round, so a
//     later round cannot raise a count.
//   - Every claim reports both `k of M panel` and `k of respondents`.
//   - A claim over a contested mapping is computed over both plausible partitions and reported as a range
//     with the definitive label withheld.
//
// Claims carry no model prose; model narrative is kept separately in Narrative.
package govern

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// RulesVersion identifies the governance rules in this package. Every claim records it.
const RulesVersion = "exploremesh-governance-rules@v1"

// CorroborationQuery is the query id recorded on a corroboration claim.
const CorroborationQuery = "corroboration-over-blind-round-1@v1"

// TieRule is the frozen policy for a tie.
type TieRule string

const (
	// TieWithhold picks no winner in a tie and withholds the ranked label.
	TieWithhold TieRule = "withhold_no_winner"
)

// MissingResponsePolicy is the frozen policy for an explorer that produced no usable response.
type MissingResponsePolicy string

const (
	// MissingExcludeFromRespondents excludes a missing response from the respondents denominator only.
	MissingExcludeFromRespondents MissingResponsePolicy = "exclude_from_respondents"
)

// CountingPolicy is the quorum, tie and missing-response policy, frozen and hashed before any judgment is
// solicited.
type CountingPolicy struct {
	RulesVersion    string                `json:"rulesVersion"`
	Quorum          int                   `json:"quorum"`
	TieRule         TieRule               `json:"tieRule"`
	MissingResponse MissingResponsePolicy `json:"missingResponse"`
}

// DefaultCountingPolicy returns a policy with a quorum of 2 respondents, ties withheld, and missing
// responses excluded from the respondents denominator.
func DefaultCountingPolicy() CountingPolicy {
	return CountingPolicy{
		RulesVersion: RulesVersion, Quorum: 2,
		TieRule: TieWithhold, MissingResponse: MissingExcludeFromRespondents,
	}
}

// Validate checks for a rules version, a quorum of at least 2, and known rules.
func (p CountingPolicy) Validate() error {
	if strings.TrimSpace(p.RulesVersion) == "" {
		return fmt.Errorf("counting policy: no rulesVersion")
	}
	if p.Quorum < 2 {
		return fmt.Errorf("counting policy: quorum %d is below 2 — a single response is not a panel", p.Quorum)
	}
	if p.TieRule != TieWithhold {
		return fmt.Errorf("counting policy: unknown tie rule %q", p.TieRule)
	}
	if p.MissingResponse != MissingExcludeFromRespondents {
		return fmt.Errorf("counting policy: unknown missing-response policy %q", p.MissingResponse)
	}
	return nil
}

// Hash returns the hex SHA-256 of the policy's JSON encoding.
func (p CountingPolicy) Hash() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Basis names what a Denominator counts.
type Basis string

// Denominator bases.
const (
	BasisPanel       Basis = "panel"
	BasisRespondents Basis = "respondents"
)

// Denominator is a count k out of M, with the basis M is taken over.
type Denominator struct {
	K     int   `json:"k"`
	M     int   `json:"m"`
	Basis Basis `json:"basis"`
}

// String renders "2 of 3 panel" / "2 of 2 respondents".
func (d Denominator) String() string { return fmt.Sprintf("%d of %d %s", d.K, d.M, d.Basis) }

// Outcome is a run's participation tally, recorded after the calls. Technical absences (failures) and
// deliberate abstentions are counted separately. Identity evidence never excludes an explorer (see
// docs/model-identity.md).
type Outcome struct {
	Dispatched           int `json:"dispatched"`
	Eligible             int `json:"eligible"`
	TechnicalAbsence     int `json:"technicalAbsence"`
	DeliberateAbstention int `json:"deliberateAbstention"`
}

// Panel is the frozen panel: membership and counting policy fixed by Freeze before any call, plus the
// participation outcome added by WithOutcome.
type Panel struct {
	Members    []schema.ExplorerIdentity `json:"members"`
	Selected   int                       `json:"selected"`
	Policy     CountingPolicy            `json:"policy"`
	PolicyHash string                    `json:"policyHash"`
	Outcome    Outcome                   `json:"outcome"`
}

// Freeze fixes panel membership and hashes the counting policy. It rejects an empty panel and an invalid
// policy.
func Freeze(members []schema.ExplorerIdentity, p CountingPolicy) (Panel, error) {
	if len(members) == 0 {
		return Panel{}, fmt.Errorf("freeze panel: no members")
	}
	if err := p.Validate(); err != nil {
		return Panel{}, err
	}
	return Panel{
		Members:  append([]schema.ExplorerIdentity(nil), members...),
		Selected: len(members), Policy: p, PolicyHash: p.Hash(),
	}, nil
}

// WithOutcome returns a copy of p with its participation outcome set to o.
func (p Panel) WithOutcome(o Outcome) Panel {
	p.Outcome = o
	return p
}

// Respondents returns the panel size minus technical absences and deliberate abstentions, never below 0.
func (p Panel) Respondents() int {
	m := p.Selected - p.Outcome.TechnicalAbsence - p.Outcome.DeliberateAbstention
	if m < 0 {
		return 0
	}
	return m
}

// Denominators returns the panel and respondents denominators for a count of k.
func (p Panel) Denominators(k int) (panel, respondents Denominator) {
	return Denominator{K: k, M: p.Selected, Basis: BasisPanel},
		Denominator{K: k, M: p.Respondents(), Basis: BasisRespondents}
}

// QuorumMet reports whether the respondents meet the frozen quorum. A count below quorum is still
// reported but never labeled definitively.
func (p Panel) QuorumMet() bool { return p.Respondents() >= p.Policy.Quorum }

// BlindBaseline is the set of blind round-1 envelopes that independence counts are computed over. Its
// fields are unexported and NewBlindBaseline accepts only blind round 1, so later rounds cannot raise a
// count.
type BlindBaseline struct {
	roundID string
	refs    map[string]schema.ExplorerIdentity
}

// NewBlindBaseline builds the baseline from r, which must be blind round 1. Later rounds have seen their
// peers' output, so counting them would count echoes as independent support.
func NewBlindBaseline(r round.Round) (BlindBaseline, error) {
	if r.Index() != 1 {
		return BlindBaseline{}, fmt.Errorf("blind baseline: round %d is not round 1 — independence counts are computed ONLY over the immutable blind round-1 artifacts", r.Index())
	}
	if !r.Blind() {
		return BlindBaseline{}, fmt.Errorf("blind baseline: round 1 is recorded as NON-blind — independence counts require a blind round")
	}
	b := BlindBaseline{roundID: r.ID(), refs: map[string]schema.ExplorerIdentity{}}
	for _, env := range r.Envelopes() {
		b.refs[schema.EnvelopeRef(1, env.Order)] = env.Identity
	}
	if len(b.refs) == 0 {
		return BlindBaseline{}, fmt.Errorf("blind baseline: round 1 recorded no envelopes")
	}
	return b, nil
}

// RoundID returns the baseline round's id.
func (b BlindBaseline) RoundID() string { return b.roundID }

// Size returns the number of envelopes in the baseline.
func (b BlindBaseline) Size() int { return len(b.refs) }

// Contains reports whether envelopeRef is a blind round-1 envelope.
func (b BlindBaseline) Contains(envelopeRef string) bool { _, ok := b.refs[envelopeRef]; return ok }

// Label is a claim's verdict. There is no "consensus" label: a count measures salience under a shared
// framing, not independent agreement.
type Label string

// Claim labels.
const (
	// LabelCorroborated means at least two distinct blind sources, quorum met, and no contested partition.
	LabelCorroborated Label = "corroborated"
	// LabelSingleSource means exactly one distinct blind source.
	LabelSingleSource Label = "single_source"
	// LabelWithheldContested means the partition is contested; a sensitivity range is reported instead.
	LabelWithheldContested Label = "withheld_contested_partition"
	// LabelWithheldBelowQuorum means the respondents are below the frozen quorum.
	LabelWithheldBelowQuorum Label = "withheld_below_quorum"
	// LabelRanked marks a candidate's placement in the host-tallied ballot. It is separate from
	// LabelCorroborated because votes and blind salience measure different things.
	LabelRanked Label = "ranked"
	// LabelWithheldTie means candidates are tied across the shortlist boundary, so no winner is picked.
	LabelWithheldTie Label = "withheld_tie"
)

// Definitive reports whether l is a definitive verdict rather than a withheld one.
func (l Label) Definitive() bool {
	return l == LabelCorroborated || l == LabelSingleSource || l == LabelRanked
}

// Sensitivity is a claim's range over the held and alternative partitions of a contested mapping. Its
// presence withholds the definitive label.
type Sensitivity struct {
	Low                   int      `json:"low"`
	High                  int      `json:"high"`
	HeldValue             int      `json:"heldValue"`
	AlternativeValue      int      `json:"alternativeValue"`
	Direction             string   `json:"direction"`
	ContestedCanonicalIDs []string `json:"contestedCanonicalIds"`
	Note                  string   `json:"note"`
}

// Claim is a host-computed governance value pinned to its inputs: the formulation hash, partition
// revision, rules and policy versions, both denominators and the contributing source ids. Every field is
// host-produced.
type Claim struct {
	Query                 string      `json:"query"`
	Subject               string      `json:"subject"`
	SubjectLabel          string      `json:"subjectLabel"`
	Value                 int         `json:"value"`
	KOfPanel              Denominator `json:"kOfPanel"`
	KOfRespondents        Denominator `json:"kOfRespondents"`
	FormulationHash       string      `json:"formulationHash"`
	PartitionRevisionHash string      `json:"partitionRevisionHash"`
	RulesVersion          string      `json:"rulesVersion"`
	PolicyHash            string      `json:"policyHash"`
	// BaselineRoundID is the round the claim's evidence comes from: blind round 1 for counts, or the ballot
	// round for a ballot claim.
	BaselineRoundID       string       `json:"baselineRoundId"`
	ContributingSourceIDs []string     `json:"contributingSourceIds"`
	Label                 Label        `json:"label"`
	Sensitivity           *Sensitivity `json:"sensitivity,omitempty"`
}

// Rendering returns the human-readable sentence for c: the counts, inputs and label, or for a withheld
// claim the range and reason. It never describes a count as consensus.
func (c Claim) Rendering() string {
	verb := "independently produced"
	switch c.Query {
	case BallotQuery:
		verb = "placed on their ballot"
	case AgreementQuery:
		verb = "independently reported the same value for"
	case EstimateQuery:
		verb = "independently contributed an estimate for"
	}
	base := fmt.Sprintf("%s: %s (%s) %s %q under formulationHash %s at partition %s [rules %s]",
		c.Query, c.KOfPanel, c.KOfRespondents, verb, c.SubjectLabel, short(c.FormulationHash), partitionDisplay(c.PartitionRevisionHash), c.RulesVersion)
	switch {
	case c.Sensitivity != nil:
		return base + fmt.Sprintf(" — CONTESTED PARTITION: the count is conditional, range %d..%d (%s); the definitive label is WITHHELD",
			c.Sensitivity.Low, c.Sensitivity.High, c.Sensitivity.Note)
	case c.Label == LabelWithheldBelowQuorum:
		return base + fmt.Sprintf(" — below the frozen quorum of %d respondents; the definitive label is WITHHELD", c.KOfRespondents.M)
	case c.Label == LabelWithheldTie:
		return base + " — TIED at the shortlist boundary under the frozen tie rule; no winner is picked and the definitive `ranked` label is WITHHELD"
	case c.Label == LabelWithheldDisagreement:
		return base + " — the blind round-1 sources DISAGREE about this value; k is the largest agreeing group and the definitive label is WITHHELD (the disagreement is reported per cell, never averaged away)"
	case c.Label == LabelRanked:
		return base + " — RANKED by the host tally over the confirmed universe (informed preference under a shared framing; not consensus, and not emergent salience)"
	case c.Label == LabelSingleSource:
		return base + " — SINGLE SOURCE (carried minority: salience, not corroboration)"
	default:
		return base + " — corroborated by that many independent blind round-1 " + corroborationNoun(c.Query) + " (salience under a shared framing; not a consensus claim)"
	}
}

// corroborationNoun names what the blind sources produced for query: nominations, evaluations of a
// declared cell, or estimates of a declared target.
func corroborationNoun(query string) string {
	switch query {
	case AgreementQuery:
		return "evaluations of the same DECLARED cell"
	case EstimateQuery:
		return "estimates of the same DECLARED target"
	default:
		return "nominations"
	}
}

// partitionDisplay returns a short form of a claim's partition hash, or a fixed phrase for a fixed-space
// claim, which has no partition.
func partitionDisplay(h string) string {
	if h == FixedSpaceNoPartition {
		return "NONE (fixed space: no entity resolution was performed)"
	}
	return short(h)
}

// short returns the first 12 characters of a hash.
func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// CountInput is the input to Corroboration.
type CountInput struct {
	Baseline        BlindBaseline
	Partition       canon.Result
	Contested       []canon.ContestedMapping
	CanonicalID     string
	Panel           Panel
	FormulationHash string
}

// Corroboration returns a claim counting the distinct blind round-1 explorers behind one canonical entity.
// Nominations outside the baseline are ignored, each source counts once, and a contested subject gets a
// sensitivity range with the label withheld.
func Corroboration(in CountInput) (Claim, error) {
	if in.Baseline.Size() == 0 {
		return Claim{}, fmt.Errorf("corroboration: no blind round-1 baseline (build it with NewBlindBaseline)")
	}
	var cluster *canon.Cluster
	for i := range in.Partition.Clusters {
		if in.Partition.Clusters[i].CanonicalID == in.CanonicalID {
			cluster = &in.Partition.Clusters[i]
			break
		}
	}
	if cluster == nil {
		return Claim{}, fmt.Errorf("corroboration: canonical ID %q is not in the partition at revision %s", in.CanonicalID, in.Partition.PartitionRevisionHash)
	}
	sources := map[schema.ExplorerIdentity]bool{}
	refs := map[string]bool{}
	for _, m := range cluster.Members {
		if !in.Baseline.Contains(m.EnvelopeRef) {
			continue
		}
		sources[m.SourceExplorer] = true
		refs[m.EnvelopeRef] = true
	}
	k := len(sources)
	panelDen, respDen := in.Panel.Denominators(k)
	claim := Claim{
		Query: CorroborationQuery, Subject: in.CanonicalID, SubjectLabel: cluster.Name, Value: k,
		KOfPanel: panelDen, KOfRespondents: respDen,
		FormulationHash: in.FormulationHash, PartitionRevisionHash: in.Partition.PartitionRevisionHash,
		RulesVersion: RulesVersion, PolicyHash: in.Panel.PolicyHash,
		BaselineRoundID: in.Baseline.RoundID(), ContributingSourceIDs: sortedKeys(refs),
	}
	claim.Sensitivity = sensitivity(in, k)
	claim.Label = label(k, claim.Sensitivity, in.Panel)
	return claim, nil
}

// sensitivity returns the range between the held count and the alternative partition's count, or nil when
// no contested mapping affects the subject.
func sensitivity(in CountInput, held int) *Sensitivity {
	var ids []string
	alt := held
	direction := ""
	var notes []string
	for _, m := range in.Contested {
		if !m.Affects(in.CanonicalID) {
			continue
		}
		ids = append(ids, m.HeldCanonicalIDs...)
		direction = string(m.Direction)
		// A join can only raise the count and a separation can only lower it; take the extreme in that
		// direction across the alternative entities.
		for _, ae := range m.AlternativeEntities {
			c := countBaselineSources(in.Baseline, ae)
			switch m.Direction {
			case canon.DirectionAlternativeJoins:
				if c > alt {
					alt = c
				}
			case canon.DirectionAlternativeSeparates:
				if c < alt {
					alt = c
				}
			}
		}
		notes = append(notes, m.Reason)
	}
	if len(ids) == 0 {
		return nil
	}
	low, high := held, alt
	if alt < held {
		low, high = alt, held
	}
	return &Sensitivity{
		Low: low, High: high, HeldValue: held, AlternativeValue: alt, Direction: direction,
		ContestedCanonicalIDs: dedupe(ids),
		Note: "the entity resolution behind this count is contested; computed over both plausible partitions: " +
			strings.Join(notes, " | "),
	}
}

// countBaselineSources counts the distinct explorers behind ae whose contributions are in the blind
// baseline, the same filter the held count uses.
func countBaselineSources(b BlindBaseline, ae canon.AlternativeEntity) int {
	seen := map[schema.ExplorerIdentity]bool{}
	for _, c := range ae.Contributions {
		if !b.Contains(c.EnvelopeRef) {
			continue
		}
		seen[c.SourceExplorer] = true
	}
	return len(seen)
}

// label withholds the label for a contested partition or missed quorum; otherwise k >= 2 is corroborated
// and k < 2 is single source.
func label(k int, s *Sensitivity, p Panel) Label {
	switch {
	case s != nil:
		return LabelWithheldContested
	case !p.QuorumMet():
		return LabelWithheldBelowQuorum
	case k >= 2:
		return LabelCorroborated
	default:
		return LabelSingleSource
	}
}

// ClaimLedger is an append-only record of emitted claims whose head hash chains every emission.
type ClaimLedger struct {
	claims []Claim
	hash   string
}

// Emit appends c to the ledger, extends the hash chain, and returns c.
func (l *ClaimLedger) Emit(c Claim) Claim {
	l.claims = append(l.claims, c)
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(append([]byte(l.hash), b...))
	l.hash = hex.EncodeToString(sum[:])
	return c
}

// Claims returns a copy of the emitted claims in emission order.
func (l *ClaimLedger) Claims() []Claim {
	out := make([]Claim, len(l.claims))
	copy(out, l.claims)
	return out
}

// RevisionHash is the head of the claim ledger's hash chain.
func (l *ClaimLedger) RevisionHash() string { return l.hash }

// MarshalJSON encodes the ledger as {claims, revisionHash}.
func (l *ClaimLedger) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Claims       []Claim `json:"claims"`
		RevisionHash string  `json:"revisionHash"`
	}{l.claims, l.hash})
}

// Narrative is one piece of model-authored prose, kept in the `collatorNarrative` namespace so that
// governance fields hold only host-produced values.
type Narrative struct {
	Source schema.ExplorerIdentity `json:"source"`
	Phase  string                  `json:"phase"`
	Prose  string                  `json:"prose"`
}

// Report is the governance record of a run: the frozen panel, the emitted claims, and the model narrative
// kept separate from them.
type Report struct {
	Panel             Panel       `json:"panel"`
	RulesVersion      string      `json:"rulesVersion"`
	Claims            []Claim     `json:"claims"`
	ClaimsHash        string      `json:"claimsHash"`
	CollatorNarrative []Narrative `json:"collatorNarrative,omitempty"`
}

// NewReport builds a Report from a frozen panel, a claim ledger and narrative entries.
func NewReport(p Panel, l *ClaimLedger, narrative []Narrative) Report {
	return Report{
		Panel: p, RulesVersion: RulesVersion,
		Claims: l.Claims(), ClaimsHash: l.RevisionHash(),
		CollatorNarrative: append([]Narrative(nil), narrative...),
	}
}

// Summary returns a one-line summary of the claim counts by label.
func (r Report) Summary() string {
	by := map[Label]int{}
	for _, c := range r.Claims {
		by[c.Label]++
	}
	withheld := by[LabelWithheldContested] + by[LabelWithheldBelowQuorum] + by[LabelWithheldTie] + by[LabelWithheldDisagreement]
	return fmt.Sprintf("governance: %d claim(s) over %d panel member(s) (%d respondents) — %d corroborated, %d single-source, %d ranked, %d withheld [rules %s]",
		len(r.Claims), r.Panel.Selected, r.Panel.Respondents(),
		by[LabelCorroborated], by[LabelSingleSource], by[LabelRanked], withheld, r.RulesVersion)
}

// sortedKeys returns m's keys in sorted order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dedupe returns s without duplicates, preserving first-appearance order.
func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
