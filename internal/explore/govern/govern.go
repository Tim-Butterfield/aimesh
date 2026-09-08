// Package govern is exploremesh's HOST-SIDE GOVERNANCE ARITHMETIC (design §0 F-A/F-C, §1, §4): the
// frozen panel + dual denominators, the immutable blind round-1 baseline every independence count is
// computed over, and the structured governance CLAIM that pins a count to the exact inputs it was computed
// from. Nothing here calls a model; everything here is a deterministic host rule over persisted artifacts —
// which is precisely what §0 F-C requires of a machine governance field.
//
// Three invariants are enforced BY CONSTRUCTION rather than by convention:
//
//   - ANTI-ECHO (§0 F-A). Independence-based counts are computed ONLY over the immutable BLIND round-1
//     artifacts. The only way to obtain the evidence base a count accepts is NewBlindBaseline, which REFUSES
//     any round that is not blind round 1; its internals are unexported, so a later round's envelopes cannot
//     be laundered into a count even deliberately. Post-mediation rounds may add depth, severity and
//     refinement — they can never raise a count, because their envelope refs are not in the baseline.
//   - DUAL DENOMINATORS (§1). A count is never a bare k. Every claim carries BOTH `k of M panel` and
//     `k of (M − non-respondents) respondents`, plus the distinct selected / dispatched / eligible /
//     supporting / valid-non-supporting / technical-absence / deliberate-abstention
//     tallies — because "3 of 5" means something different when 2 explorers never answered.
//   - WITHHOLDING (§4). A claim over a CONTESTED mapping is computed over BOTH plausible partitions and
//     emitted as a range with the definitive `corroborated` label WITHHELD. There is no code path that
//     produces LabelCorroborated for a contested subject.
//
// Honest labeling (§0 F-B): a Claim has no field for model prose and Rendering() never says "consensus". All
// model narrative lives in the separate Narrative namespace carried alongside, never inside, the claims.
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

// RulesVersion is the version of the HOST governance rules implemented here (counting, denominators,
// labeling, withholding). Every emitted claim pins it, so a claim read later is interpretable under the
// rules that produced it rather than under whatever the code does today (§4/§9).
const RulesVersion = "exploremesh-governance-rules@v1"

// CorroborationQuery is the versioned query id recorded on a corroboration claim (§9: a governance claim
// persists its query id/version alongside its params and value).
const CorroborationQuery = "corroboration-over-blind-round-1@v1"

// --- Frozen panel + dual denominators (design §1) ---

// TieRule is the frozen policy for a tie. It must be frozen + hashed BEFORE any judgment is solicited,
// otherwise a rule can be chosen after seeing which candidate it favors (§4).
type TieRule string

const (
	// TieWithhold: a tie yields NO winner and the ranked label is withheld — the honest default.
	TieWithhold TieRule = "withhold_no_winner"
)

// MissingResponsePolicy is the frozen policy for an explorer that produced no usable response.
type MissingResponsePolicy string

const (
	// MissingExcludeFromRespondents: a missing response leaves the PANEL denominator untouched and is excluded
	// from the RESPONDENTS denominator — which is exactly why both denominators are always reported.
	MissingExcludeFromRespondents MissingResponsePolicy = "exclude_from_respondents"
)

// CountingPolicy is the quorum / tie / missing-response policy, frozen and hashed before any judgment is
// solicited (design §4). It is host-authored configuration, never model output.
type CountingPolicy struct {
	RulesVersion    string                `json:"rulesVersion"`
	Quorum          int                   `json:"quorum"`
	TieRule         TieRule               `json:"tieRule"`
	MissingResponse MissingResponsePolicy `json:"missingResponse"`
}

// DefaultCountingPolicy is the v1 policy: a 2-respondent quorum (a one-response panel is not a panel),
// ties withheld, missing responses excluded from the respondents denominator only.
func DefaultCountingPolicy() CountingPolicy {
	return CountingPolicy{
		RulesVersion: RulesVersion, Quorum: 2,
		TieRule: TieWithhold, MissingResponse: MissingExcludeFromRespondents,
	}
}

// Validate checks the policy is usable (a stated rules version, a quorum of at least 2, known rules).
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

// Hash returns the hex SHA-256 of the canonical policy JSON — the value pinned on every claim, proving the
// policy a count was computed under was fixed before the judgments were solicited.
func (p CountingPolicy) Hash() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Basis names which denominator a Denominator is (design §1: both are always reported).
type Basis string

const (
	BasisPanel       Basis = "panel"
	BasisRespondents Basis = "respondents"
)

// Denominator is one honest k-of-M: the count, the denominator, and what the denominator MEANS.
type Denominator struct {
	K     int   `json:"k"`
	M     int   `json:"m"`
	Basis Basis `json:"basis"`
}

// String renders "2 of 3 panel" / "2 of 2 respondents".
func (d Denominator) String() string { return fmt.Sprintf("%d of %d %s", d.K, d.M, d.Basis) }

// Outcome is the per-run participation tally recorded AFTER the calls, layered onto the frozen panel
// (design §1). The categories are kept DISTINCT because they mean different things: a technical absence is
// an infrastructure failure and a deliberate abstention is a position — collapsing them into "missing"
// hides both. There is no identity category: a seat is never excluded for what its identity evidence
// said (see ../../../docs/model-identity.md).
type Outcome struct {
	Dispatched           int `json:"dispatched"`
	Eligible             int `json:"eligible"`
	TechnicalAbsence     int `json:"technicalAbsence"`
	DeliberateAbstention int `json:"deliberateAbstention"`
}

// Panel is the FROZEN panel (design §1): membership fixed at the start of the exploration, the counting
// policy hashed before any judgment is solicited, and the participation outcome recorded afterwards. Freeze
// produces it; WithOutcome layers the tallies on without touching the frozen fields.
type Panel struct {
	Members    []schema.ExplorerIdentity `json:"members"`
	Selected   int                       `json:"selected"`
	Policy     CountingPolicy            `json:"policy"`
	PolicyHash string                    `json:"policyHash"`
	Outcome    Outcome                   `json:"outcome"`
}

// Freeze fixes panel membership and hashes the counting policy — called BEFORE any explorer call, so no
// judgment can influence either (design §1/§4). It rejects an empty panel and an invalid policy.
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

// WithOutcome returns a copy of the panel carrying the participation tallies. The frozen fields (members,
// selected, policy, policy hash) are carried through unchanged — an outcome can never re-open the freeze.
func (p Panel) WithOutcome(o Outcome) Panel {
	p.Outcome = o
	return p
}

// Respondents is the RESPONDENTS denominator: the frozen panel size minus every category of non-response
// (technical absence, deliberate abstention). Both tallies stay individually visible on the Outcome, so
// any other denominator a reader wants is recomputable.
func (p Panel) Respondents() int {
	m := p.Selected - p.Outcome.TechnicalAbsence - p.Outcome.DeliberateAbstention
	if m < 0 {
		return 0
	}
	return m
}

// Denominators returns BOTH denominators for a count of k (design §1) — never one without the other.
func (p Panel) Denominators(k int) (panel, respondents Denominator) {
	return Denominator{K: k, M: p.Selected, Basis: BasisPanel},
		Denominator{K: k, M: p.Respondents(), Basis: BasisRespondents}
}

// QuorumMet reports whether the respondents denominator satisfies the FROZEN quorum. A count below quorum is
// still reported (it is real arithmetic) but never labeled definitively — see label().
func (p Panel) QuorumMet() bool { return p.Respondents() >= p.Policy.Quorum }

// --- The immutable blind round-1 baseline (design §0 F-A: the anti-echo invariant) ---

// BlindBaseline is the ONLY evidence base an independence count may be computed over: the immutable BLIND
// round-1 artifacts. Its fields are unexported and its only constructor rejects any other round, so the
// anti-echo invariant is a property of the type — not a rule a later contributor has to remember. A
// post-mediation round that repeats an item cannot raise a count, because its envelope refs are not here.
type BlindBaseline struct {
	roundID string
	refs    map[string]schema.ExplorerIdentity
}

// NewBlindBaseline builds the baseline from a recorded round. It REFUSES anything that is not blind round 1:
// a later round is contaminated by the mediated view of its peers (§0 F-A), so counting over it would be
// counting an echo as independent support.
func NewBlindBaseline(r round.Round) (BlindBaseline, error) {
	if r.Index() != 1 {
		return BlindBaseline{}, fmt.Errorf("blind baseline: round %d is not round 1 — independence counts are computed ONLY over the immutable blind round-1 artifacts (design §0 F-A)", r.Index())
	}
	if !r.Blind() {
		return BlindBaseline{}, fmt.Errorf("blind baseline: round 1 is recorded as NON-blind — independence counts require a blind round (design §0 F-A)")
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

// RoundID is the baseline round's stable id (`round-1`), pinned on the claims computed from it.
func (b BlindBaseline) RoundID() string { return b.roundID }

// Size is the number of blind round-1 envelopes in the baseline.
func (b BlindBaseline) Size() int { return len(b.refs) }

// Contains reports whether an envelope ref belongs to the blind baseline — the single filter that keeps a
// later round's nomination out of a count.
func (b BlindBaseline) Contains(envelopeRef string) bool { _, ok := b.refs[envelopeRef]; return ok }

// --- Structured governance claims (design §0 F-C, §4, §9) ---

// Label is a claim's honest verdict. There is deliberately NO "consensus" label: an emergent-space count is
// salience under a shared framing at a specific partition, never independent consensus (§0 F-B).
type Label string

const (
	// LabelCorroborated: >= 2 DISTINCT blind round-1 sources, quorum met, and the partition NOT contested.
	LabelCorroborated Label = "corroborated"
	// LabelSingleSource: exactly one distinct blind round-1 source — carried minority, salience not support.
	LabelSingleSource Label = "single_source"
	// LabelWithheldContested: the partition the count rides on is CONTESTED, so the definitive label is
	// WITHHELD and a sensitivity range is emitted instead (§4).
	LabelWithheldContested Label = "withheld_contested_partition"
	// LabelWithheldBelowQuorum: the respondents denominator is below the FROZEN quorum.
	LabelWithheldBelowQuorum Label = "withheld_below_quorum"
	// LabelRanked: a candidate's placement in a HOST-TALLIED ballot over the confirmed universe, under
	// decision inputs frozen before the ballot was solicited. It is deliberately a SEPARATE label from
	// corroborated: `voted` and `emergent` measure different things (§4), and a reader must never have to
	// infer which one a claim carries.
	LabelRanked Label = "ranked"
	// LabelWithheldTie: the frozen tie rule (TieWithhold) applies — candidates are tied across the
	// shortlist boundary, so no winner is picked and the definitive `ranked` label is WITHHELD for them.
	LabelWithheldTie Label = "withheld_tie"
)

// Definitive reports whether the label is a definitive verdict (as opposed to a withheld one).
func (l Label) Definitive() bool {
	return l == LabelCorroborated || l == LabelSingleSource || l == LabelRanked
}

// Sensitivity is a claim's CONDITIONAL result over a contested partition (design §4): the count computed over
// BOTH plausible partitions, as a range, with the contested entities named. Its presence is what withholds
// the definitive label.
type Sensitivity struct {
	Low                   int      `json:"low"`
	High                  int      `json:"high"`
	HeldValue             int      `json:"heldValue"`
	AlternativeValue      int      `json:"alternativeValue"`
	Direction             string   `json:"direction"`
	ContestedCanonicalIDs []string `json:"contestedCanonicalIds"`
	Note                  string   `json:"note"`
}

// Claim is ONE structured governance claim (design §0 F-C, §4, §9): a host-computed value pinned to every
// input it depends on — the formulation bytes, the partition revision, the rules + policy versions, the panel
// and respondent denominators, and the EXACT contributing source IDs. It carries no model prose by design:
// every field is host-produced, so nothing in a claim can be asserted by a model.
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
	// BaselineRoundID is the recorded round the claim's evidence comes from. For an `emergent` corroboration
	// claim that is ALWAYS the immutable blind round 1 (the anti-echo invariant makes any other value
	// impossible to obtain). For a `ballot-over-confirmed-universe@v1` claim it is the BALLOT round: a ballot
	// is its own evidence, cast deliberately and non-blind, and pretending it was a blind-round measurement
	// would conflate `voted` with `emergent` — the exact conflation §4 forbids.
	BaselineRoundID       string       `json:"baselineRoundId"`
	ContributingSourceIDs []string     `json:"contributingSourceIds"`
	Label                 Label        `json:"label"`
	Sensitivity           *Sensitivity `json:"sensitivity,omitempty"`
}

// Rendering is the ONLY sanctioned human phrasing of a claim (design §0 F-B: "no emergent-space count is ever
// rendered as independent consensus"). It states what is actually reconstructable — k of M panel members
// independently produced this under formulation H at partition P — and, for a withheld claim, the range and
// the reason instead of a verdict.
func (c Claim) Rendering() string {
	// A ballot claim counts a different act over a different round, so it gets its own verb: k members PLACED
	// this candidate on a ballot. Everything else about the sentence — both denominators, the pinned
	// formulation + partition, the rules version — is identical, because the honesty requirements are.
	verb := "independently produced"
	switch c.Query {
	case BallotQuery:
		verb = "placed on their ballot"
	case AgreementQuery:
		// A fixed-space cell. What k measures is the largest group of blind sources that reported the
		// SAME value — so the verb names agreement on a value, never agreement on merit.
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
		// Fixed space: the panel disagreed about the VALUE, in a space nobody disputes. That is a finding,
		// so it is stated as one — and the definitive label is withheld so it can never read as agreement.
		return base + " — the blind round-1 sources DISAGREE about this value; k is the largest agreeing group and the definitive label is WITHHELD (the disagreement is reported per cell, never averaged away)"
	case c.Label == LabelRanked:
		// Deliberately never "consensus" and never "emergent": a ballot is an informed preference expressed
		// under one shared framing, and salience (who NAMED it blind) is a different measurement entirely.
		return base + " — RANKED by the host tally over the confirmed universe (informed preference under a shared framing; not consensus, and not emergent salience)"
	case c.Label == LabelSingleSource:
		return base + " — SINGLE SOURCE (carried minority: salience, not corroboration)"
	default:
		// Deliberately never the phrase "independent consensus" (§0 F-B): what the artifacts support is that k
		// blind round-1 responses named this entity under one framing at one partition — salience, not agreement
		// on merit, and not a consensus claim.
		return base + " — corroborated by that many independent blind round-1 " + corroborationNoun(c.Query) + " (salience under a shared framing; not a consensus claim)"
	}
}

// corroborationNoun names WHAT the k blind sources produced, per query. An emergent-space count is over
// NOMINATIONS the explorers authored; a fixed-space cell count is over EVALUATIONS of a declared cell, and a
// pooled forecast over ESTIMATES. The distinction is not cosmetic: "3 independent nominations" and "3
// independent evaluations of the same given option" are different claims about how much agreement there is.
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

// partitionDisplay renders the partition a claim was computed at. An emergent-space claim shows the short
// revision hash; a FIXED-SPACE claim carries a sentence instead of a hash —
// there is no partition, and truncating that sentence to 12 characters would make an honest statement look
// like a corrupt hash.
func partitionDisplay(h string) string {
	if h == FixedSpaceNoPartition {
		return "NONE (fixed space: no entity resolution was performed)"
	}
	return short(h)
}

// short renders the first 12 hex chars of a hash for a human line (the full value stays on the claim).
func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// CountInput is everything a corroboration count needs. Every field is either a host artifact or a frozen
// policy — there is no model input, and the Baseline can only be a blind round 1.
type CountInput struct {
	Baseline        BlindBaseline
	Partition       canon.Result
	Contested       []canon.ContestedMapping
	CanonicalID     string
	Panel           Panel
	FormulationHash string
}

// Corroboration counts the DISTINCT blind round-1 source explorers behind one canonical entity (design §4:
// `emergent` = host-counted references across independently-structured blind round-1 responses — salience,
// not merit) and returns it as a structured Claim.
//
// Three things it does NOT do, deliberately: it never counts a nomination whose envelope ref is outside the
// blind baseline (the anti-echo invariant); it never counts the same source twice (k is distinct SOURCES, not
// nominations); and it never labels a contested subject definitively — it computes the alternative partition's
// count too and emits a range with the label withheld.
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
	// k = distinct BLIND ROUND-1 source explorers. A member sourced from a later round is skipped here — that
	// is the anti-echo invariant in its operative form.
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

// sensitivity computes the CONDITIONAL result when the subject's mapping is contested (design §4): the count
// over the ALTERNATIVE partition alongside the held one, as a range. Returns nil when no contested mapping
// affects the subject — the only case in which a definitive label is permitted.
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
		// The alternative's count for the subject is the distinct BLIND source count of the alternative entity
		// covering it — a join can only RAISE it (the merged entity pools more sources), a separation can only
		// LOWER it. Taking the extreme in the contested direction keeps the emitted range honest when several
		// alternative entities cover the subject.
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

// countBaselineSources counts the distinct source explorers of an alternative entity whose contribution came
// from a BLIND round-1 envelope — the same anti-echo filter the held count uses, so both branches of a
// sensitivity range are computed over exactly the same evidence base.
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

// label applies the labeling rule: a contested partition or a below-quorum panel WITHHOLDS the definitive
// label (§4); otherwise >=2 distinct blind sources is corroborated and 1 is a carried single source.
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

// --- The append-only claim ledger + the collatorNarrative split (design §0 F-C, §4, §9) ---

// ClaimLedger is the APPEND-ONLY record of every emitted governance claim (§9: "every emitted governance
// claim persists its query id/version, params, partitionRevisionHash, universe/criterion hashes, rules
// version, value, and exact contributing source IDs"). Like the merge-ledger, its rows are unexported, the
// only write path is Emit, Claims() copies, and the head hash chains the emissions.
type ClaimLedger struct {
	claims []Claim
	hash   string
}

// Emit appends a claim to the ledger and returns it. It is the ONLY way a claim enters the record, so an
// emitted claim is always persisted with its inputs.
func (l *ClaimLedger) Emit(c Claim) Claim {
	l.claims = append(l.claims, c)
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(append([]byte(l.hash), b...))
	l.hash = hex.EncodeToString(sum[:])
	return c
}

// Claims returns a COPY of the emitted claims in emission order.
func (l *ClaimLedger) Claims() []Claim {
	out := make([]Claim, len(l.claims))
	copy(out, l.claims)
	return out
}

// RevisionHash is the head of the claim ledger's hash chain.
func (l *ClaimLedger) RevisionHash() string { return l.hash }

// Len reports how many claims were emitted.
func (l *ClaimLedger) Len() int { return len(l.claims) }

// MarshalJSON emits {claims, revisionHash} despite the unexported append-only rows.
func (l *ClaimLedger) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Claims       []Claim `json:"claims"`
		RevisionHash string  `json:"revisionHash"`
	}{l.claims, l.hash})
}

// Narrative is the `collatorNarrative` NAMESPACE (design §0 F-C): the one place model-authored prose is
// allowed to live. Keeping it in its own namespace is what makes "machine governance fields carry
// host-produced values only" enforceable — the rule cannot be enforced on free natural language, so the
// prose is quarantined instead of policed.
type Narrative struct {
	Source schema.ExplorerIdentity `json:"source"`
	Phase  string                  `json:"phase"`
	Prose  string                  `json:"prose"`
}

// Report is the terminal governance surface (design §4/§9): the frozen panel, every emitted claim with its
// inputs, and — strictly separated — the model narrative. A reader (or an export) can therefore tell, field
// by field, which values a host rule produced and which words a model wrote.
type Report struct {
	Panel             Panel       `json:"panel"`
	RulesVersion      string      `json:"rulesVersion"`
	Claims            []Claim     `json:"claims"`
	ClaimsHash        string      `json:"claimsHash"`
	CollatorNarrative []Narrative `json:"collatorNarrative,omitempty"`
}

// NewReport assembles the governance report from a frozen panel, an append-only claim ledger, and the
// quarantined narrative entries.
func NewReport(p Panel, l *ClaimLedger, narrative []Narrative) Report {
	return Report{
		Panel: p, RulesVersion: RulesVersion,
		Claims: l.Claims(), ClaimsHash: l.RevisionHash(),
		CollatorNarrative: append([]Narrative(nil), narrative...),
	}
}

// Summary renders the one-line human summary of the report (implements the mode-package ModeOutput contract
// shape so a surface can echo it): claim counts by label, never a verdict of its own.
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

// sortedKeys returns a set's keys in sorted order (deterministic contributing-source lists).
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
