package canon

// This file implements the confirmation round, in which the panel can contest the provisional partition
// and a host rule, not the canonicalizer, resolves the dispute.
//
//   - Provisional clusters are shown to every explorer with attribution, in a seeded random order.
//   - Explorers reply with typed challenges (wrong_merge, wrong_split, label_bias, missing_item,
//     injected_item), each naming a target and a reason. Untyped objections are not accepted because they
//     cannot be resolved mechanically.
//   - Rule v1 is conservative: any wrong_merge splits the merge, and no challenge can create a merge or
//     add an item. The result is a new ledger revision chained to the provisional one, which is kept.
//   - A dispute the rule cannot settle safely becomes a contested mapping; package govern then reports
//     dependent counts as ranges with the definitive label withheld.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// HostConfirmationRuleVersion identifies the host rule that resolves challenges. It is recorded on every
// resolution and on the resulting revision.
const HostConfirmationRuleVersion = "host-confirmation-rule@v1"

// ChallengeType is a kind of challenge against the provisional partition. The set is closed because each
// type maps to a specific host action.
type ChallengeType string

// Challenge types.
const (
	// ChallengeWrongMerge says an entity groups different things. Any single one splits the entity.
	ChallengeWrongMerge ChallengeType = "wrong_merge"
	// ChallengeWrongSplit says two entities are the same. It never merges them; the mapping is contested.
	ChallengeWrongSplit ChallengeType = "wrong_split"
	// ChallengeLabelBias says an entity's label is prejudicial. It is recorded; no mapping changes.
	ChallengeLabelBias ChallengeType = "label_bias"
	// ChallengeMissingItem says a nomination is absent. It is recorded; the surjectivity gate already
	// guarantees every nomination appears exactly once.
	ChallengeMissingItem ChallengeType = "missing_item"
	// ChallengeInjectedItem says an entity contains something nobody nominated. It is recorded; the
	// surjectivity gate already guarantees every entity is backed by a nomination.
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

// Challenge is one typed objection from one explorer against the provisional partition.
type Challenge struct {
	Type ChallengeType `json:"type"`
	// CanonicalID is the challenged entity. Required for wrong_merge / wrong_split / label_bias /
	// injected_item; optional for missing_item (the item is by definition not in an entity).
	CanonicalID string `json:"canonicalId,omitempty"`
	// OtherCanonicalID is the second entity of a wrong_split.
	OtherCanonicalID string `json:"otherCanonicalId,omitempty"`
	// RawNomination is the raw text at issue, if any.
	RawNomination string `json:"rawNomination,omitempty"`
	// Reason is the explorer's stated reason, recorded verbatim.
	Reason string `json:"reason"`
	// By is the challenger's identity, set by the host from the call, never by the model.
	By schema.ExplorerIdentity `json:"by"`
}

// Validate checks that the challenge has a known type, a non-empty reason, and target entities in known
// where its type requires them.
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

// Presentation is the order in which entities were shown to explorers. The order is derived from a seed,
// so it is uncorrelated with the canonicalizer's cluster order yet reproducible from persisted data.
type Presentation struct {
	Order       []string `json:"order"`
	Seed        string   `json:"seed"`
	RuleVersion string   `json:"ruleVersion"`
}

// Present orders res's entities by SHA-256 of seedInput and the canonical ID. The pipeline seeds it with
// the payload hash and partition revision hash.
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

// ConfirmationPrompt renders the confirmation-round prompt: the provisional entities in presentation order
// with attribution, the challenge types, and the exact JSON response shape. It is identical for every
// explorer.
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

// ParseChallenges decodes an explorer's confirmation response into challenges attributed to by, which comes
// from the host rather than the model. It returns an error for the first invalid challenge.
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

// Resolution actions applied by HostConfirmationRuleVersion.
const (
	// ActionSplit means the entity was split into one entity per distinct raw nomination.
	ActionSplit = "split"
	// ActionRecordedNoSplitPossible means a wrong_merge targeted identical raw strings; the mapping is
	// contested.
	ActionRecordedNoSplitPossible = "recorded_no_split_possible"
	// ActionRecordedNoMerge means a wrong_split was recorded without merging; the mapping is contested.
	ActionRecordedNoMerge = "recorded_no_merge"
	// ActionRecordedLabelOnly means a label_bias was recorded; no mapping changed.
	ActionRecordedLabelOnly = "recorded_label_only"
	// ActionRecordedNoInjection means a missing_item or injected_item was recorded; the partition is unchanged.
	ActionRecordedNoInjection = "recorded_no_injection"
)

// Resolution is the host's decision on one challenge.
type Resolution struct {
	Challenge   Challenge `json:"challenge"`
	Action      string    `json:"action"`
	Detail      string    `json:"detail"`
	RuleVersion string    `json:"ruleVersion"`
}

// ContestedDirection says how the alternative partition differs from the held one.
type ContestedDirection string

const (
	// DirectionAlternativeJoins means the alternative would join entities the held partition keeps separate.
	DirectionAlternativeJoins ContestedDirection = "alternative_joins"
	// DirectionAlternativeSeparates means the alternative would split an entity the held partition keeps
	// together.
	DirectionAlternativeSeparates ContestedDirection = "alternative_separates"
)

// ContestedSource records what made a mapping contested.
type ContestedSource string

// Contested sources.
const (
	SourceDualDisagreement    ContestedSource = "dual_canonicalizer_disagreement"
	SourceWrongSplitChallenge ContestedSource = "wrong_split_challenge"
	SourceUnsplittableMerge   ContestedSource = "unsplittable_wrong_merge"
)

// Contribution attributes one nomination in an alternative entity to its explorer and envelope.
type Contribution struct {
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
	EnvelopeRef    string                  `json:"envelopeRef"`
}

// AlternativeEntity is one entity of the alternative partition, with the contributions needed to count it
// without re-running canonicalization.
type AlternativeEntity struct {
	Label          string         `json:"label"`
	RawNominations []string       `json:"rawNominations"`
	Contributions  []Contribution `json:"contributions"`
}

// ContestedMapping is a dispute the confirmation round could not settle. It describes both plausible
// partitions so dependent counts can be reported as ranges.
type ContestedMapping struct {
	// HeldCanonicalIDs are the entity ids in the held partition.
	HeldCanonicalIDs []string `json:"heldCanonicalIds"`
	// AlternativeEntities are the entities the alternative partition would form.
	AlternativeEntities []AlternativeEntity `json:"alternativeEntities"`
	Direction           ContestedDirection  `json:"direction"`
	Source              ContestedSource     `json:"source"`
	Reason              string              `json:"reason"`
	// RaisedBy lists the challenging explorers, if any.
	RaisedBy    []schema.ExplorerIdentity `json:"raisedBy,omitempty"`
	RuleVersion string                    `json:"ruleVersion"`
}

// Affects reports whether canonicalID is one of the mapping's held entities.
func (m ContestedMapping) Affects(canonicalID string) bool {
	return slices.Contains(m.HeldCanonicalIDs, canonicalID)
}

// Confirmation is the record of a confirmation round: presentation order, challenges, resolutions, the new
// ledger revision, the prior revision's hash and any remaining contested mappings.
type Confirmation struct {
	Presentation      Presentation       `json:"presentation"`
	Challenges        []Challenge        `json:"challenges"`
	Resolutions       []Resolution       `json:"resolutions"`
	Revision          Result             `json:"revision"`
	PriorRevisionHash string             `json:"priorRevisionHash"`
	Contested         []ContestedMapping `json:"contested"`
	RuleVersion       string             `json:"ruleVersion"`
}

// Settled reports whether no mapping remains contested.
func (c Confirmation) Settled() bool { return len(c.Contested) == 0 }

// Confirm applies HostConfirmationRuleVersion to the challenges against the provisional partition prov and
// returns the confirmed revision. prov must carry its Nominations, which the revision re-partitions.
//
// Rule v1:
//  1. each wrong_merge target is split into one entity per distinct raw nomination; an entity of identical
//     strings cannot be split and becomes contested;
//  2. wrong_split is recorded and the mapping becomes contested;
//  3. label_bias, missing_item and injected_item are recorded without changing any mapping;
//  4. merges the dual agreement refused are carried forward as contested mappings.
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

	// Rule 1.
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

	// Rule 4: the held partition may undercount where the dual agreement refused a merge.
	if prov.Ledger != nil {
		for _, cm := range prov.Ledger.Contested() {
			out.Contested = append(out.Contested, joinContested(prov, assign, cm.SplitInto, SourceDualDisagreement,
				fmt.Sprintf("canonicalizer %s proposed merging %v as %q; the other did not, so the host rule split them (%s)",
					cm.ProposedBy.Identity.Model, cm.RawNominations, cm.ProposedName, cm.RuleVersion), nil))
		}
	}

	// The new revision chains to the provisional ledger's head hash; the provisional ledger is unchanged.
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

// assignmentOf returns the held canonical ID of each nomination in res, matching on raw text and envelope
// ref. It returns an error if a nomination matches no member or members of two entities.
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

// applySplits builds the next partition. Each entity in splitTargets is split into one entity per distinct
// raw nomination; identical strings stay together. Other entities are unchanged. It returns the clusters, a
// description of each split, and the targets that could not be split.
func applySplits(prov Result, assign []string, splitTargets map[string]bool) (next []ProposedCluster, detail map[string]string, unsplittable map[string]bool) {
	detail, unsplittable = map[string]string{}, map[string]bool{}
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

// nameOf returns an entity's label, or canonicalID if the entity is not found.
func nameOf(res Result, canonicalID string) string {
	for _, c := range res.Clusters {
		if c.CanonicalID == canonicalID {
			return c.Name
		}
	}
	return canonicalID
}

// joinContested builds a contested mapping whose alternative joins heldIDs into one entity carrying all of
// their contributions.
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

// separateContested builds a contested mapping for an unsplittable wrong_merge. The alternative gives each
// source its own entity, which is the pessimistic low end of the resulting range.
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

// challengesFor returns the challenges of type t against canonicalID, in arrival order.
func challengesFor(chs []Challenge, t ChallengeType, canonicalID string) []Challenge {
	var out []Challenge
	for _, c := range chs {
		if c.Type == t && c.CanonicalID == canonicalID {
			out = append(out, c)
		}
	}
	return out
}

// challengeReasons joins the reasons of the matching challenges with " | ".
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
