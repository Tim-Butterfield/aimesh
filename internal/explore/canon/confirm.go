package canon

// This file is the BINDING, HOST-ADJUDICATED CONFIRMATION ROUND (design §4).
//
// The merge ledger records the partition; it does not VALIDATE it. Validation
// requires the panel that produced the nominations to be able to CONTEST the partition, and it requires the
// dispute to be resolved by something other than the party being complained about:
//
//   - The canonicalizer emits PROVISIONAL clusters. They are shown to every explorer WITH ATTRIBUTION
//     (blindness is already spent after round 1 — hiding attribution here would only reduce auditability)
//     in a PERSISTED, RANDOMIZED presentation order, so cluster position cannot smuggle in a preference.
//   - Explorers reply with TYPED challenges (wrong_merge | wrong_split | label_bias | missing_item |
//     injected_item) naming the exact target and a reason. Free-form objections are not accepted: an
//     untyped complaint cannot be mechanically adjudicated, which is how "governance" quietly becomes
//     "the collator decided".
//   - A VERSIONED HOST RULE resolves them — NOT the canonicalizer, which must never review complaints about
//     itself. Rule v1 is deliberately mechanical and conservative: ANY single wrong_merge flag splits the
//     merge, and no challenge can ever CREATE a merge or ADD an item (both would manufacture corroboration
//     out of one explorer's assertion). The resolution is a NEW append-only ledger revision, chained to the
//     revision it supersedes — never an in-place edit; the provisional revision is retained.
//   - A dispute the rule cannot settle in the safe direction (a refused merge, or a merge that cannot be
//     split because its members are the identical raw string) leaves a CONTESTED MAPPING. A count over a
//     contested mapping must then be computed over BOTH plausible partitions and emitted as a range with the
//     definitive `corroborated`/`ranked` label WITHHELD (internal/govern does this).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// HostConfirmationRuleVersion is the VERSIONED host adjudication rule (design §4). It is persisted with
// every resolution and on the resulting revision, so a partition revision can always be re-derived from the
// provisional partition + the recorded challenges by the exact rule version that produced it.
const HostConfirmationRuleVersion = "host-confirmation-rule@v1"

// ChallengeType is the CLOSED set of typed challenges an explorer may raise against the provisional
// partition (design §4). The set is closed because each type maps to a specific mechanical host action.
type ChallengeType string

const (
	// ChallengeWrongMerge: "you put these in ONE entity and they are DIFFERENT things." Rule v1: ANY single
	// flag splits the merge — one explorer noticing a conflation is enough, because splitting is safe.
	ChallengeWrongMerge ChallengeType = "wrong_merge"
	// ChallengeWrongSplit: "these two entities are the SAME thing." Rule v1 NEVER merges on it (a merge on
	// one explorer's assertion manufactures corroboration); it is recorded and leaves the mapping CONTESTED.
	ChallengeWrongSplit ChallengeType = "wrong_split"
	// ChallengeLabelBias: "the label given to this entity prejudices it." Recorded; labels carry no count, so
	// rule v1 changes no mapping — the biased label travels with the entity for the human to see.
	ChallengeLabelBias ChallengeType = "label_bias"
	// ChallengeMissingItem: "a candidate I nominated is not here." Recorded. Rule v1 never ADDS an item: the
	// surjectivity gate already guarantees every blind round-1 nomination is present in exactly one entity, so
	// a missing item is either a labeling complaint or a gate violation (which is an error, not a challenge).
	ChallengeMissingItem ChallengeType = "missing_item"
	// ChallengeInjectedItem: "this entity contains something nobody nominated." Recorded. The surjectivity
	// gate makes true injection impossible (every canonical ID is reachable from >=1 recorded nomination), so
	// the resolution states that and points at the attributed rows.
	ChallengeInjectedItem ChallengeType = "injected_item"
)

// Valid reports whether t is one of the five typed challenges.
func (t ChallengeType) Valid() bool {
	switch t {
	case ChallengeWrongMerge, ChallengeWrongSplit, ChallengeLabelBias, ChallengeMissingItem, ChallengeInjectedItem:
		return true
	}
	return false
}

// ChallengeTypes lists the typed challenges in the order they are rendered into the confirmation prompt.
func ChallengeTypes() []ChallengeType {
	return []ChallengeType{ChallengeWrongMerge, ChallengeWrongSplit, ChallengeLabelBias, ChallengeMissingItem, ChallengeInjectedItem}
}

// Challenge is one typed objection from one explorer against the provisional partition (design §4).
type Challenge struct {
	Type ChallengeType `json:"type"`
	// CanonicalID is the challenged entity. Required for wrong_merge / wrong_split / label_bias /
	// injected_item; optional for missing_item (the item is by definition not in an entity).
	CanonicalID string `json:"canonicalId,omitempty"`
	// OtherCanonicalID is the SECOND entity of a wrong_split ("this and that are the same thing").
	OtherCanonicalID string `json:"otherCanonicalId,omitempty"`
	// RawNomination names the specific raw text at issue (the conflated member, the missing item, …).
	RawNomination string `json:"rawNomination,omitempty"`
	// Reason is the explorer's stated reason — model prose, recorded verbatim and never parsed for meaning.
	Reason string `json:"reason"`
	// By is the challenging explorer's full identity (host-stamped from the verified call, never model-supplied).
	By schema.ExplorerIdentity `json:"by"`
}

// Validate checks a challenge is well-formed against the provisional partition it targets: a known type, a
// resolvable target entity where the type requires one, and a non-empty reason. An unresolvable target is
// rejected rather than silently dropped — a challenge naming an entity that does not exist is a protocol
// failure worth surfacing, not a no-op.
func (c Challenge) Validate(known map[string]bool) error {
	if !c.Type.Valid() {
		return fmt.Errorf("challenge: unknown type %q (known: %v)", c.Type, ChallengeTypes())
	}
	if strings.TrimSpace(c.Reason) == "" {
		return fmt.Errorf("challenge %s: empty reason", c.Type)
	}
	needsTarget := c.Type != ChallengeMissingItem
	if needsTarget {
		if strings.TrimSpace(c.CanonicalID) == "" {
			return fmt.Errorf("challenge %s: missing canonicalId", c.Type)
		}
		if !known[c.CanonicalID] {
			return fmt.Errorf("challenge %s: canonicalId %q is not in the presented partition", c.Type, c.CanonicalID)
		}
	}
	if c.Type == ChallengeWrongSplit {
		if strings.TrimSpace(c.OtherCanonicalID) == "" {
			return fmt.Errorf("challenge wrong_split: missing otherCanonicalId (a split names TWO entities)")
		}
		if !known[c.OtherCanonicalID] {
			return fmt.Errorf("challenge wrong_split: otherCanonicalId %q is not in the presented partition", c.OtherCanonicalID)
		}
		if c.OtherCanonicalID == c.CanonicalID {
			return fmt.Errorf("challenge wrong_split: canonicalId and otherCanonicalId are the same entity %q", c.CanonicalID)
		}
	}
	return nil
}

// Presentation is the PERSISTED, RANDOMIZED order in which the provisional entities were shown to the
// explorers (design §4: "candidate order persisted + randomized"). The permutation is derived
// DETERMINISTICALLY from a host seed rather than from wall-clock randomness: it must be uncorrelated with
// nomination/cluster order (so position cannot encode the canonicalizer's preference) while staying exactly
// reproducible from persisted artifacts (§0 F-C). It is an ordering, not a secret.
type Presentation struct {
	Order       []string `json:"order"`
	Seed        string   `json:"seed"`
	RuleVersion string   `json:"ruleVersion"`
}

// Present computes the randomized presentation order for a provisional partition. seedInput is the host's
// seed material (the pipeline uses the payload hash + the partition revision hash — both already persisted,
// so the order is re-derivable); entities are ordered by SHA-256(seed + canonicalID), which decorrelates the
// order from the canonicalizer's own cluster order.
func Present(res Result, seedInput string) Presentation {
	type keyed struct {
		id  string
		key string
	}
	ks := make([]keyed, 0, len(res.Clusters))
	for _, c := range res.Clusters {
		sum := sha256.Sum256([]byte(seedInput + "\x00" + c.CanonicalID))
		ks = append(ks, keyed{c.CanonicalID, hex.EncodeToString(sum[:])})
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].key == ks[j].key {
			return ks[i].id < ks[j].id
		}
		return ks[i].key < ks[j].key
	})
	order := make([]string, 0, len(ks))
	for _, k := range ks {
		order = append(order, k.id)
	}
	seedSum := sha256.Sum256([]byte(seedInput))
	return Presentation{Order: order, Seed: hex.EncodeToString(seedSum[:]), RuleVersion: HostConfirmationRuleVersion}
}

// ConfirmationPrompt renders the confirmation-round instruction for ONE explorer: the provisional entities in
// the persisted randomized order WITH attribution, the closed challenge vocabulary, the exact JSON field
// names, and "output ONLY JSON, no fences" (the hard-won lesson from the Map/Catalog contracts — a prompt
// that does not spell out the field names gets improvised ones). The bytes are identical for every explorer
// in the round, so one content hash describes what the whole panel was shown.
func ConfirmationPrompt(res Result, pres Presentation) (string, error) {
	byID := map[string]Cluster{}
	for _, c := range res.Clusters {
		byID[c.CanonicalID] = c
	}
	type memberWire struct {
		RawNomination  string                  `json:"rawNomination"`
		SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
		EnvelopeRef    string                  `json:"envelopeRef"`
	}
	type entityWire struct {
		Position     int          `json:"position"`
		CanonicalID  string       `json:"canonicalId"`
		Name         string       `json:"name"`
		SingleSource bool         `json:"singleSource"`
		Members      []memberWire `json:"members"`
	}
	wire := make([]entityWire, 0, len(pres.Order))
	for i, id := range pres.Order {
		c, ok := byID[id]
		if !ok {
			return "", fmt.Errorf("confirmation prompt: presented canonical ID %q is not in the partition", id)
		}
		e := entityWire{Position: i, CanonicalID: c.CanonicalID, Name: c.Name, SingleSource: c.SingleSource}
		for _, m := range c.Members {
			e.Members = append(e.Members, memberWire{m.RawNomination, m.SourceExplorer, m.EnvelopeRef})
		}
		wire = append(wire, e)
	}
	b, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("confirmation prompt: marshal provisional partition: %w", err)
	}
	return "This is the CONFIRMATION ROUND. A CANONICALIZER (a role distinct from you and from the collator) " +
		"grouped the raw candidate nominations this panel produced into PROVISIONAL canonical entities. Below is " +
		"that provisional grouping, with the raw nomination text and the explorer that nominated it. The entities " +
		"are presented in a RANDOMIZED order — position means nothing.\n\n" +
		"Your job is ONLY to challenge grouping errors you can see. Do NOT re-answer the original task, do NOT " +
		"rank the entities, and do NOT add new candidates. Raise a challenge ONLY where you have a concrete " +
		"reason; an empty challenge list is a valid and common answer.\n\n" +
		"Each challenge MUST use exactly one of these types:\n" +
		"- \"wrong_merge\": the named entity groups nominations that are DIFFERENT things.\n" +
		"- \"wrong_split\": the two named entities are the SAME thing and should be one.\n" +
		"- \"label_bias\": the entity's name prejudices or misrepresents what its members say.\n" +
		"- \"missing_item\": a candidate that was nominated does not appear in any entity.\n" +
		"- \"injected_item\": an entity contains something no explorer nominated.\n\n" +
		"Output ONLY a SINGLE JSON object — no prose before or after it, and no markdown code fences:\n" +
		"{\"challenges\": [ {\"type\": string (one of the five above), \"canonicalId\": string (the challenged " +
		"entity's canonicalId; omit only for missing_item), \"otherCanonicalId\": string (REQUIRED for " +
		"wrong_split — the second entity), \"rawNomination\": string (the specific raw text at issue, if any), " +
		"\"reason\": string (why — required)} ]}\n" +
		"Use an empty array if you have no challenge: {\"challenges\": []}\n\n" +
		"provisional entities:\n" + string(b) + "\n", nil
}

// challengeWire is the confirmation response shape parsed from an explorer's raw output.
type challengeWire struct {
	Challenges []struct {
		Type             string `json:"type"`
		CanonicalID      string `json:"canonicalId"`
		OtherCanonicalID string `json:"otherCanonicalId"`
		RawNomination    string `json:"rawNomination"`
		Reason           string `json:"reason"`
	} `json:"challenges"`
}

// ParseChallenges decodes one explorer's confirmation response into typed challenges, stamping the HOST's
// verified identity for the challenger (`by` is never taken from the model's own output — a model must not be
// able to attribute a challenge to a different explorer). Each challenge is validated against the presented
// entity set; the first invalid one is an error, so a malformed confirmation response is visible rather than
// silently thinned.
func ParseChallenges(raw []byte, by schema.ExplorerIdentity, known map[string]bool) ([]Challenge, error) {
	obj, _, xerr := schema.ExtractJSONObject(raw)
	if xerr != nil {
		return nil, xerr
	}
	var w challengeWire
	if err := json.Unmarshal(obj, &w); err != nil {
		return nil, err
	}
	out := make([]Challenge, 0, len(w.Challenges))
	for _, c := range w.Challenges {
		ch := Challenge{
			Type: ChallengeType(strings.TrimSpace(c.Type)), CanonicalID: strings.TrimSpace(c.CanonicalID),
			OtherCanonicalID: strings.TrimSpace(c.OtherCanonicalID), RawNomination: c.RawNomination,
			Reason: c.Reason, By: by,
		}
		if err := ch.Validate(known); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// Resolution actions applied by HostConfirmationRuleVersion. Each is a HOST decision, recorded with the rule
// version that produced it.
const (
	// ActionSplit: the challenged merge was split into one entity per distinct raw nomination text.
	ActionSplit = "split"
	// ActionRecordedNoSplitPossible: a wrong_merge on an entity whose members are the IDENTICAL raw string —
	// there is nothing mechanical left to split, so the dispute is recorded and the mapping stays CONTESTED.
	ActionRecordedNoSplitPossible = "recorded_no_split_possible"
	// ActionRecordedNoMerge: a wrong_split is NEVER applied (merging on one explorer's assertion would
	// manufacture corroboration); it is recorded and the mapping stays CONTESTED.
	ActionRecordedNoMerge = "recorded_no_merge"
	// ActionRecordedLabelOnly: a label_bias changes no mapping (labels carry no count); recorded on the entity.
	ActionRecordedLabelOnly = "recorded_label_only"
	// ActionRecordedNoInjection: a missing_item/injected_item cannot change the partition — the surjectivity
	// gate already guarantees every nomination is present exactly once and every entity is nomination-backed.
	ActionRecordedNoInjection = "recorded_no_injection"
)

// Resolution is the host's decision on ONE challenge, with the rule version that decided it.
type Resolution struct {
	Challenge   Challenge `json:"challenge"`
	Action      string    `json:"action"`
	Detail      string    `json:"detail"`
	RuleVersion string    `json:"ruleVersion"`
}

// ContestedDirection says which way the ALTERNATIVE partition differs from the held one.
type ContestedDirection string

const (
	// DirectionAlternativeJoins: the held partition keeps the entities SEPARATE; the alternative would treat
	// them as ONE (a refused wrong_split, or a merge only one canonicalizer proposed).
	DirectionAlternativeJoins ContestedDirection = "alternative_joins"
	// DirectionAlternativeSeparates: the held partition keeps them TOGETHER; the alternative would split them
	// (a wrong_merge the rule could not split mechanically).
	DirectionAlternativeSeparates ContestedDirection = "alternative_separates"
)

// ContestedSource records what made a mapping contested — a dual-canonicalizer disagreement or a specific
// unresolved challenge type.
type ContestedSource string

const (
	SourceDualDisagreement    ContestedSource = "dual_canonicalizer_disagreement"
	SourceWrongSplitChallenge ContestedSource = "wrong_split_challenge"
	SourceUnsplittableMerge   ContestedSource = "unsplittable_wrong_merge"
)

// Contribution is one nomination's attribution inside an alternative entity: which explorer nominated it and
// from which envelope. The pair is kept TOGETHER (rather than as two parallel lists) so a count can filter
// contributions to the blind round-1 baseline by envelope ref and still know whose support it is keeping.
type Contribution struct {
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
	EnvelopeRef    string                  `json:"envelopeRef"`
}

// AlternativeEntity describes ONE entity of the ALTERNATIVE partition over the affected nominations: its
// label, the raw nominations it contains, and — the part a count needs — the attributed contributions behind
// it. Carrying the contributions means a dependent count can be evaluated over the alternative WITHOUT
// re-running canonicalization, which is what makes emitting a sensitivity range cheap enough to always do.
type AlternativeEntity struct {
	Label          string         `json:"label"`
	RawNominations []string       `json:"rawNominations"`
	Contributions  []Contribution `json:"contributions"`
}

// ContestedMapping is a mapping the confirmation round could NOT settle (design §4). It names BOTH plausible
// partitions of the affected nominations so any dependent count can be computed over both and emitted as a
// range — with the definitive `corroborated`/`ranked` label WITHHELD.
type ContestedMapping struct {
	// HeldCanonicalIDs are the entity IDs the HELD (recorded, conservative) partition uses.
	HeldCanonicalIDs []string `json:"heldCanonicalIds"`
	// AlternativeEntities are the entities the ALTERNATIVE partition would form over the same nominations.
	AlternativeEntities []AlternativeEntity `json:"alternativeEntities"`
	Direction           ContestedDirection  `json:"direction"`
	Source              ContestedSource     `json:"source"`
	Reason              string              `json:"reason"`
	// RaisedBy attributes the dispute: the challenging explorer(s), or the canonicalizer whose merge was refused.
	RaisedBy    []schema.ExplorerIdentity `json:"raisedBy,omitempty"`
	RuleVersion string                    `json:"ruleVersion"`
}

// Affects reports whether a claim about canonicalID must be treated as CONDITIONAL under this contested
// mapping — i.e. whether the two plausible partitions can disagree about that entity's support.
func (m ContestedMapping) Affects(canonicalID string) bool {
	for _, id := range m.HeldCanonicalIDs {
		if id == canonicalID {
			return true
		}
	}
	return false
}

// Confirmation is the full record of one binding confirmation round (design §4): what was shown and in which
// order, every typed challenge received, the host's versioned resolution of each, the NEW ledger revision the
// resolutions produced, the hash of the revision it supersedes (retained, never edited), and every mapping
// that remains CONTESTED.
type Confirmation struct {
	Presentation      Presentation       `json:"presentation"`
	Challenges        []Challenge        `json:"challenges"`
	Resolutions       []Resolution       `json:"resolutions"`
	Revision          Result             `json:"revision"`
	PriorRevisionHash string             `json:"priorRevisionHash"`
	Contested         []ContestedMapping `json:"contested"`
	RuleVersion       string             `json:"ruleVersion"`
}

// Settled reports whether the confirmed partition carries NO contested mapping — the precondition for a
// dependent count to be labeled definitively (§4: otherwise the label is withheld and a range is emitted).
func (c Confirmation) Settled() bool { return len(c.Contested) == 0 }

// Confirm applies HostConfirmationRuleVersion to the typed challenges over a PROVISIONAL partition and
// returns the confirmed revision (design §4). The canonicalizer is NOT consulted — it must not adjudicate
// complaints about its own partition. prov must carry its Nominations (the pipeline sets them on the governed
// paths) because a revision re-partitions the SAME nominations rather than re-deriving them.
//
// Rule v1, in order:
//  1. every wrong_merge flag (>=1 is enough) SPLITS its entity into one entity per distinct raw nomination
//     text; an entity whose members are the identical raw string cannot be split mechanically and becomes a
//     CONTESTED mapping instead;
//  2. wrong_split is never applied — recorded, and the mapping becomes CONTESTED;
//  3. label_bias / missing_item / injected_item change no mapping and are recorded;
//  4. every merge the DUAL agreement already refused is carried forward as a CONTESTED mapping, because the
//     held partition may be undercounting exactly there.
func Confirm(prov Result, pres Presentation, challenges []Challenge) (Confirmation, error) {
	if len(prov.Nominations) == 0 {
		return Confirmation{}, fmt.Errorf("confirm: the provisional result carries no nominations — a confirmation revision must re-partition the exact nominations the provisional partition was computed over")
	}
	assign, err := assignmentOf(prov)
	if err != nil {
		return Confirmation{}, err
	}
	known := map[string]bool{}
	for _, c := range prov.Clusters {
		known[c.CanonicalID] = true
	}

	out := Confirmation{
		Presentation: pres, Challenges: append([]Challenge(nil), challenges...),
		PriorRevisionHash: prov.PartitionRevisionHash, RuleVersion: HostConfirmationRuleVersion,
	}
	// splitTargets collects the entities rule 1 must split (a set: N flags on one entity is still one split).
	splitTargets := map[string]bool{}
	for _, ch := range challenges {
		if err := ch.Validate(known); err != nil {
			return Confirmation{}, err
		}
		switch ch.Type {
		case ChallengeWrongMerge:
			splitTargets[ch.CanonicalID] = true
		case ChallengeWrongSplit:
			out.Resolutions = append(out.Resolutions, Resolution{
				Challenge: ch, Action: ActionRecordedNoMerge, RuleVersion: HostConfirmationRuleVersion,
				Detail: fmt.Sprintf("rule %s never MERGES on a challenge (a merge asserted by one explorer would manufacture corroboration); %q and %q stay separate and the mapping is CONTESTED — dependent counts are conditional",
					HostConfirmationRuleVersion, ch.CanonicalID, ch.OtherCanonicalID),
			})
			out.Contested = append(out.Contested, joinContested(prov, assign, []string{ch.CanonicalID, ch.OtherCanonicalID},
				SourceWrongSplitChallenge, ch.Reason, []schema.ExplorerIdentity{ch.By}))
		case ChallengeLabelBias:
			out.Resolutions = append(out.Resolutions, Resolution{
				Challenge: ch, Action: ActionRecordedLabelOnly, RuleVersion: HostConfirmationRuleVersion,
				Detail: "a label carries no count, so no mapping changed; the objection travels with the entity for the human",
			})
		default: // missing_item / injected_item
			out.Resolutions = append(out.Resolutions, Resolution{
				Challenge: ch, Action: ActionRecordedNoInjection, RuleVersion: HostConfirmationRuleVersion,
				Detail: "the surjectivity gate already holds (every blind round-1 nomination appears in exactly one entity and every entity is nomination-backed), so no item may be added or removed by a challenge; recorded for the human",
			})
		}
	}

	// Rule 1: apply the splits, building the NEW partition over the same nominations.
	next, splitDetail, unsplittable := applySplits(prov, assign, splitTargets)
	for id := range splitTargets {
		if unsplittable[id] {
			for _, ch := range challengesFor(challenges, ChallengeWrongMerge, id) {
				out.Resolutions = append(out.Resolutions, Resolution{
					Challenge: ch, Action: ActionRecordedNoSplitPossible, RuleVersion: HostConfirmationRuleVersion,
					Detail: fmt.Sprintf("entity %q groups only IDENTICAL raw nominations, so there is no mechanical split; the mapping is CONTESTED — dependent counts are conditional", id),
				})
			}
			out.Contested = append(out.Contested, separateContested(prov, assign, id, challengeReasons(challenges, ChallengeWrongMerge, id), challengers(challenges, ChallengeWrongMerge, id)))
			continue
		}
		for _, ch := range challengesFor(challenges, ChallengeWrongMerge, id) {
			out.Resolutions = append(out.Resolutions, Resolution{
				Challenge: ch, Action: ActionSplit, RuleVersion: HostConfirmationRuleVersion,
				Detail: splitDetail[id],
			})
		}
	}

	// Rule 4: a merge the dual agreement already refused keeps the held partition potentially UNDERCOUNTING
	// there, so it stays contested through the confirmation round.
	if prov.Ledger != nil {
		for _, cm := range prov.Ledger.Contested() {
			out.Contested = append(out.Contested, joinContested(prov, assign, cm.SplitInto, SourceDualDisagreement,
				fmt.Sprintf("canonicalizer %s proposed merging %v as %q; the other did not, so the host rule split them (%s)",
					cm.ProposedBy.Identity.Model, cm.RawNominations, cm.ProposedName, cm.RuleVersion), nil))
		}
	}

	// Build the NEW append-only revision. The provisional ledger object is untouched; this revision chains to
	// its head hash, so the successor relationship is verifiable and the superseded revision is retained.
	revision := prov.Ledger.Revision() + 1
	led, err := buildLedger(prov.Nominations, next, revision, prov.PartitionRevisionHash, func(r *LedgerRow) {
		r.DecidedByCall = "host:" + HostConfirmationRuleVersion
		if len(prov.Canonicalizers) > 0 {
			r.AgreedBy = append([]Attribution(nil), prov.Canonicalizers...)
		}
	})
	if err != nil {
		return Confirmation{}, err
	}
	out.Revision = Result{
		Clusters:              partitionView(prov.Nominations, next),
		ProposedDimensions:    prov.ProposedDimensions,
		CoverageNotes:         prov.CoverageNotes,
		PartitionRevisionHash: led.RevisionHash(),
		Ledger:                led,
		Canonicalizers:        prov.Canonicalizers,
		AgreementRuleVersion:  HostConfirmationRuleVersion,
		Nominations:           append([]Nomination(nil), prov.Nominations...),
	}
	return out, nil
}

// assignmentOf rebuilds "nomination index → held canonical ID" for a recorded partition by matching each
// nomination to the cluster member carrying the same raw text + envelope ref. It fails closed: a nomination
// that matches no member, or matches members in two different entities, means the recorded partition and the
// recorded nominations disagree — never something to guess around.
func assignmentOf(res Result) ([]string, error) {
	byKey := map[string]string{}
	for _, c := range res.Clusters {
		for _, m := range c.Members {
			k := m.RawNomination + "\x00" + m.EnvelopeRef
			if prev, dup := byKey[k]; dup && prev != c.CanonicalID {
				return nil, fmt.Errorf("confirm: nomination %q from %s is mapped to both %q and %q — the recorded partition is inconsistent", m.RawNomination, m.EnvelopeRef, prev, c.CanonicalID)
			}
			byKey[k] = c.CanonicalID
		}
	}
	out := make([]string, len(res.Nominations))
	for i, n := range res.Nominations {
		id, ok := byKey[n.Raw+"\x00"+n.EnvelopeRef]
		if !ok {
			return nil, fmt.Errorf("confirm: nomination %d (%q from %s) is not present in the recorded partition", i, n.Raw, n.EnvelopeRef)
		}
		out[i] = id
	}
	return out, nil
}

// applySplits builds the next partition: every entity in splitTargets is broken into one entity per DISTINCT
// raw nomination text (the only mechanical, judgment-free split available to the host — identical strings from
// different explorers stay together because that is string identity, not entity resolution), and every other
// entity is carried through unchanged. It returns the new clusters, a per-entity human detail for the
// resolution record, and the set of targets that could not be split at all.
func applySplits(prov Result, assign []string, splitTargets map[string]bool) (next []ProposedCluster, detail map[string]string, unsplittable map[string]bool) {
	detail, unsplittable = map[string]string{}, map[string]bool{}
	// Group nomination indices by held entity, preserving first-appearance order of both entities and members.
	var order []string
	members := map[string][]int{}
	for i, id := range assign {
		if _, seen := members[id]; !seen {
			order = append(order, id)
		}
		members[id] = append(members[id], i)
	}
	for _, id := range order {
		idxs := members[id]
		if !splitTargets[id] {
			next = append(next, ProposedCluster{CanonicalID: id, Name: nameOf(prov, id), Members: idxs})
			continue
		}
		// Split by exact raw text, first-appearance order.
		var rawOrder []string
		byRaw := map[string][]int{}
		for _, i := range idxs {
			raw := prov.Nominations[i].Raw
			if _, seen := byRaw[raw]; !seen {
				rawOrder = append(rawOrder, raw)
			}
			byRaw[raw] = append(byRaw[raw], i)
		}
		if len(rawOrder) < 2 {
			unsplittable[id] = true
			next = append(next, ProposedCluster{CanonicalID: id, Name: nameOf(prov, id), Members: idxs})
			continue
		}
		for k, raw := range rawOrder {
			next = append(next, ProposedCluster{
				CanonicalID: fmt.Sprintf("%s#s%d", id, k+1),
				Name:        raw,
				Members:     byRaw[raw],
			})
		}
		detail[id] = fmt.Sprintf("wrong_merge flagged: entity %q was SPLIT into %d entities, one per distinct raw nomination (%s)",
			id, len(rawOrder), strings.Join(rawOrder, ", "))
	}
	return next, detail, unsplittable
}

// nameOf returns a recorded entity's label ("" if absent).
func nameOf(res Result, canonicalID string) string {
	for _, c := range res.Clusters {
		if c.CanonicalID == canonicalID {
			return c.Name
		}
	}
	return canonicalID
}

// joinContested builds a contested mapping whose ALTERNATIVE partition JOINS the named held entities into one
// (a refused wrong_split, or a merge one canonicalizer proposed). The alternative's single entity carries the
// union of the affected nominations' sources — what a dependent count needs to evaluate the other branch.
func joinContested(prov Result, assign []string, heldIDs []string, src ContestedSource, reason string, raisedBy []schema.ExplorerIdentity) ContestedMapping {
	want := map[string]bool{}
	for _, id := range heldIDs {
		want[id] = true
	}
	alt := AlternativeEntity{Label: strings.Join(heldIDs, "+")}
	for i, id := range assign {
		if !want[id] {
			continue
		}
		n := prov.Nominations[i]
		alt.RawNominations = append(alt.RawNominations, n.Raw)
		alt.Contributions = append(alt.Contributions, Contribution{SourceExplorer: n.SourceExplorer, EnvelopeRef: n.EnvelopeRef})
	}
	return ContestedMapping{
		HeldCanonicalIDs: append([]string(nil), heldIDs...), AlternativeEntities: []AlternativeEntity{alt},
		Direction: DirectionAlternativeJoins, Source: src, Reason: reason,
		RaisedBy: raisedBy, RuleVersion: HostConfirmationRuleVersion,
	}
}

// separateContested builds a contested mapping whose ALTERNATIVE partition SEPARATES one held entity — the
// unsplittable wrong_merge case, where the members are the identical raw string so the only alternative the
// host can describe is one entity PER SOURCE (i.e. no corroboration at all). That is deliberately the
// pessimistic branch: it is the count the challenge implies, and emitting it as the range's low end is how a
// count stays honest about a dispute the rule could not settle.
func separateContested(prov Result, assign []string, heldID, reason string, raisedBy []schema.ExplorerIdentity) ContestedMapping {
	var order []schema.ExplorerIdentity
	bySource := map[schema.ExplorerIdentity]*AlternativeEntity{}
	for i, id := range assign {
		if id != heldID {
			continue
		}
		n := prov.Nominations[i]
		a, ok := bySource[n.SourceExplorer]
		if !ok {
			a = &AlternativeEntity{Label: heldID + " @ " + n.SourceExplorer.Model}
			bySource[n.SourceExplorer] = a
			order = append(order, n.SourceExplorer)
		}
		a.RawNominations = append(a.RawNominations, n.Raw)
		a.Contributions = append(a.Contributions, Contribution{SourceExplorer: n.SourceExplorer, EnvelopeRef: n.EnvelopeRef})
	}
	alts := make([]AlternativeEntity, 0, len(order))
	for _, s := range order {
		alts = append(alts, *bySource[s])
	}
	return ContestedMapping{
		HeldCanonicalIDs: []string{heldID}, AlternativeEntities: alts,
		Direction: DirectionAlternativeSeparates, Source: SourceUnsplittableMerge, Reason: reason,
		RaisedBy: raisedBy, RuleVersion: HostConfirmationRuleVersion,
	}
}

// challengesFor returns the challenges of type t against canonicalID (in arrival order).
func challengesFor(chs []Challenge, t ChallengeType, canonicalID string) []Challenge {
	var out []Challenge
	for _, c := range chs {
		if c.Type == t && c.CanonicalID == canonicalID {
			out = append(out, c)
		}
	}
	return out
}

// challengeReasons joins the stated reasons of the matching challenges (recorded prose, never interpreted).
func challengeReasons(chs []Challenge, t ChallengeType, canonicalID string) string {
	var rs []string
	for _, c := range challengesFor(chs, t, canonicalID) {
		rs = append(rs, c.Reason)
	}
	return strings.Join(rs, " | ")
}

// challengers returns the distinct identities that raised the matching challenges.
func challengers(chs []Challenge, t ChallengeType, canonicalID string) []schema.ExplorerIdentity {
	seen := map[schema.ExplorerIdentity]bool{}
	var out []schema.ExplorerIdentity
	for _, c := range challengesFor(chs, t, canonicalID) {
		if !seen[c.By] {
			seen[c.By] = true
			out = append(out, c.By)
		}
	}
	return out
}
