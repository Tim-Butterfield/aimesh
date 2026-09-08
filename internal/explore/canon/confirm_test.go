package canon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// provisional builds a provisional partition over overlappingNoms with the nominations retained (what the
// pipeline hands Confirm on a governed path).
func provisional(t *testing.T) Result {
	t.Helper()
	noms := overlappingNoms()
	res, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: clusterByRaw})
	if err != nil {
		t.Fatalf("provisional canonicalization: %v", err)
	}
	res.Nominations = noms
	return res
}

// mergedProvisional builds a provisional partition where the two DIFFERENT raw names are merged into one entity
// (the conflation a wrong_merge challenge is for).
func mergedProvisional(t *testing.T) Result {
	t.Helper()
	noms := overlappingNoms() // Postgres(a), SQLite(a), Postgres(b), MySQL(b)
	merge := func([]Nomination) (Proposal, error) {
		return Proposal{
			Clusters: []ProposedCluster{
				{CanonicalID: "sql", Name: "SQL databases", Members: []int{0, 1, 2, 3}},
			},
			DecidedByCall: "c:canonicalize", DecidedByIdentity: ex("c", "m-c"),
		}, nil
	}
	res, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: merge})
	if err != nil {
		t.Fatalf("provisional canonicalization: %v", err)
	}
	res.Nominations = noms
	return res
}

// TestPresent_RandomizedPersistedAndReproducible pins §4's presentation rule: the order covers every entity, is
// DECORRELATED from the canonicalizer's cluster order, and is exactly reproducible from the persisted seed
// material (so "what order was it shown in" is answerable later, §0 F-C).
func TestPresent_RandomizedPersistedAndReproducible(t *testing.T) {
	res := provisional(t)
	pres := Present(res, "payload-hash\x00"+res.PartitionRevisionHash)
	if len(pres.Order) != len(res.Clusters) {
		t.Fatalf("the presentation must cover every entity: %d vs %d", len(pres.Order), len(res.Clusters))
	}
	for _, c := range res.Clusters {
		if !slices.Contains(pres.Order, c.CanonicalID) {
			t.Errorf("entity %q missing from the presented order %v", c.CanonicalID, pres.Order)
		}
	}
	if pres.Seed == "" || pres.RuleVersion != HostConfirmationRuleVersion {
		t.Errorf("the presentation must persist its seed + rule version: %+v", pres)
	}
	same := Present(res, "payload-hash\x00"+res.PartitionRevisionHash)
	if !slices.Equal(pres.Order, same.Order) {
		t.Error("the same seed material must reproduce the same order (reconstruction rule)")
	}
	other := Present(res, "different-seed")
	if slices.Equal(pres.Order, other.Order) && len(pres.Order) > 2 {
		t.Error("a different seed should permute the order (the order must not be a function of cluster order alone)")
	}
}

// TestConfirmationPrompt_RendersAttributionTypesAndFieldNames pins the confirmation protocol's prompt: the
// entities are shown WITH attribution in the presented order, the closed challenge vocabulary is spelled out,
// the exact JSON field names are named, and fences are forbidden (the hard-won Map/Catalog lesson).
func TestConfirmationPrompt_RendersAttributionTypesAndFieldNames(t *testing.T) {
	res := provisional(t)
	pres := Present(res, "seed")
	prompt, err := ConfirmationPrompt(res, pres)
	if err != nil {
		t.Fatalf("confirmation prompt: %v", err)
	}
	for _, want := range []string{
		"CONFIRMATION ROUND", "RANDOMIZED order", "sourceExplorer",
		"wrong_merge", "wrong_split", "label_bias", "missing_item", "injected_item",
		"canonicalId", "otherCanonicalId", "challenges", "no markdown code fences",
		"{\"challenges\": []}",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("confirmation prompt missing %q", want)
		}
	}
	// The entities appear in the PRESENTED order, not the canonicalizer's order.
	prev := -1
	for _, id := range pres.Order {
		at := strings.Index(prompt, "\""+id+"\"")
		if at < prev {
			t.Errorf("entities must be rendered in the presented order (%v)", pres.Order)
		}
		prev = at
	}
	// An entity that is not in the partition cannot be presented (fail closed).
	if _, err := ConfirmationPrompt(res, Presentation{Order: []string{"ghost"}}); err == nil {
		t.Error("presenting an entity absent from the partition must fail")
	}
}

// TestParseChallenges_TypedAndHostAttributed pins the challenge parse: types are validated against the closed
// set, targets must exist in the presented partition, the reason is required, and the CHALLENGER identity is
// stamped by the HOST (a model must not be able to attribute a challenge to another explorer).
func TestParseChallenges_TypedAndHostAttributed(t *testing.T) {
	known := map[string]bool{"canon-Postgres": true, "canon-SQLite": true}
	by := ex("a", "m-a")
	chs, err := ParseChallenges([]byte(`{"challenges":[{"type":"wrong_merge","canonicalId":"canon-Postgres","reason":"different things"}]}`), by, known)
	if err != nil {
		t.Fatalf("parse challenges: %v", err)
	}
	if len(chs) != 1 || chs[0].Type != ChallengeWrongMerge || chs[0].By != by {
		t.Fatalf("parsed challenge wrong: %+v", chs)
	}
	// An empty list is valid and common.
	if got, err := ParseChallenges([]byte(`{"challenges":[]}`), by, known); err != nil || len(got) != 0 {
		t.Errorf("an empty challenge list must parse cleanly: %v %+v", err, got)
	}
	// A model cannot re-attribute a challenge: the wire has no `by` field, and the host stamps it.
	spoof, err := ParseChallenges([]byte(`{"challenges":[{"type":"label_bias","canonicalId":"canon-SQLite","reason":"r","by":{"adapter":"z","model":"z"}}]}`), by, known)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spoof[0].By != by {
		t.Errorf("the challenger identity must be HOST-stamped, got %+v", spoof[0].By)
	}
	for name, body := range map[string]string{
		"unknown type":        `{"challenges":[{"type":"burn_it_down","canonicalId":"canon-SQLite","reason":"r"}]}`,
		"unknown target":      `{"challenges":[{"type":"wrong_merge","canonicalId":"ghost","reason":"r"}]}`,
		"missing reason":      `{"challenges":[{"type":"wrong_merge","canonicalId":"canon-SQLite","reason":"  "}]}`,
		"wrong_split one arg": `{"challenges":[{"type":"wrong_split","canonicalId":"canon-SQLite","reason":"r"}]}`,
		"wrong_split self":    `{"challenges":[{"type":"wrong_split","canonicalId":"canon-SQLite","otherCanonicalId":"canon-SQLite","reason":"r"}]}`,
	} {
		if _, err := ParseChallenges([]byte(body), by, known); err == nil {
			t.Errorf("%s: expected a parse error, got nil", name)
		}
	}
	// missing_item needs no target (the item is by definition in no entity).
	if _, err := ParseChallenges([]byte(`{"challenges":[{"type":"missing_item","rawNomination":"DuckDB","reason":"I nominated it"}]}`), by, known); err != nil {
		t.Errorf("missing_item must not require a canonicalId: %v", err)
	}
}

// TestConfirm_WrongMerge_SplitsAsNewRevision is the core §4 invariant: ONE wrong_merge flag splits the merge,
// the split is a NEW append-only ledger revision chained to the one it supersedes, and the PRIOR revision is
// retained untouched (never an in-place edit). The resolution records the versioned host rule.
func TestConfirm_WrongMerge_SplitsAsNewRevision(t *testing.T) {
	prov := mergedProvisional(t)
	priorHash := prov.PartitionRevisionHash
	priorRows := prov.Ledger.Rows()
	pres := Present(prov, "seed")
	ch := Challenge{Type: ChallengeWrongMerge, CanonicalID: "sql", Reason: "Postgres and MySQL are different products", By: ex("a", "m-a")}

	conf, err := Confirm(prov, pres, []Challenge{ch})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// A single flag is enough (the conservative, mechanical rule).
	if len(conf.Resolutions) != 1 || conf.Resolutions[0].Action != ActionSplit {
		t.Fatalf("one wrong_merge flag must SPLIT: %+v", conf.Resolutions)
	}
	if conf.Resolutions[0].RuleVersion != HostConfirmationRuleVersion || conf.RuleVersion != HostConfirmationRuleVersion {
		t.Errorf("the resolution must persist the versioned host rule: %+v", conf.Resolutions[0])
	}
	// The merged entity is gone; one entity per DISTINCT raw nomination replaces it.
	if len(conf.Revision.Clusters) != 3 {
		t.Fatalf("the split must produce one entity per distinct raw nomination (Postgres/SQLite/MySQL), got %d: %+v", len(conf.Revision.Clusters), conf.Revision.Clusters)
	}
	names := map[string]Cluster{}
	for _, c := range conf.Revision.Clusters {
		names[c.Name] = c
		if !strings.HasPrefix(c.CanonicalID, "sql#s") {
			t.Errorf("a split entity's ID must derive from the entity it split out of: %q", c.CanonicalID)
		}
	}
	if pg := names["Postgres"]; len(pg.Members) != 2 || pg.SingleSource {
		t.Errorf("identical raw strings from two explorers stay together (string identity, not entity resolution): %+v", pg)
	}
	for _, n := range []string{"SQLite", "MySQL"} {
		if c := names[n]; len(c.Members) != 1 || !c.SingleSource {
			t.Errorf("%q must become a single-source entity: %+v", n, c)
		}
	}
	// A NEW revision, chained to the prior one, which is RETAINED unchanged.
	if conf.PriorRevisionHash != priorHash {
		t.Errorf("the confirmation must record the revision it supersedes: %q vs %q", conf.PriorRevisionHash, priorHash)
	}
	if conf.Revision.PartitionRevisionHash == priorHash {
		t.Error("a new revision must have a NEW partition revision hash")
	}
	if conf.Revision.Ledger.Revision() != 2 || conf.Revision.Ledger.PriorRevisionHash() != priorHash {
		t.Errorf("the new ledger must be revision 2 chained to the prior hash: rev=%d prior=%q", conf.Revision.Ledger.Revision(), conf.Revision.Ledger.PriorRevisionHash())
	}
	if got := prov.Ledger.Rows(); len(got) != len(priorRows) || got[0].CanonicalID != priorRows[0].CanonicalID {
		t.Error("the PROVISIONAL revision must be retained unchanged (a revision is a new entry, never an edit)")
	}
	for _, row := range conf.Revision.Ledger.Rows() {
		if row.Revision != 2 || row.DecidedByCall != "host:"+HostConfirmationRuleVersion {
			t.Errorf("a revision row must record its revision + the deciding HOST rule: %+v", row)
		}
	}
	// Surjectivity still holds over the new revision, and the split leaves the partition SETTLED (a split is a
	// resolution, not a standing dispute).
	if conf.Revision.Ledger.Len() != len(prov.Nominations) {
		t.Errorf("expected %d rows in the new revision, got %d", len(prov.Nominations), conf.Revision.Ledger.Len())
	}
	if !conf.Settled() {
		t.Errorf("a split wrong_merge is RESOLVED, so nothing should remain contested: %+v", conf.Contested)
	}
}

// TestConfirm_WrongSplit_NeverMerges_ContestedRange pins the asymmetry that keeps counts honest: the host rule
// never MERGES on a challenge (that would manufacture corroboration from one explorer's assertion), so the
// mapping stays CONTESTED — which is what makes a dependent count conditional with the definitive label
// withheld.
func TestConfirm_WrongSplit_NeverMerges_ContestedRange(t *testing.T) {
	prov := provisional(t)
	ids := []string{}
	for _, c := range prov.Clusters {
		ids = append(ids, c.CanonicalID)
	}
	ch := Challenge{
		Type: ChallengeWrongSplit, CanonicalID: ids[1], OtherCanonicalID: ids[2],
		Reason: "these are the same product", By: ex("a", "m-a"),
	}
	conf, err := Confirm(prov, Present(prov, "seed"), []Challenge{ch})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(conf.Resolutions) != 1 || conf.Resolutions[0].Action != ActionRecordedNoMerge {
		t.Fatalf("a wrong_split must be RECORDED, never applied: %+v", conf.Resolutions)
	}
	if len(conf.Revision.Clusters) != len(prov.Clusters) {
		t.Errorf("no entity may be merged by a challenge: %d → %d", len(prov.Clusters), len(conf.Revision.Clusters))
	}
	if conf.Settled() {
		t.Fatal("an unresolved dispute must leave the mapping CONTESTED")
	}
	cm := conf.Contested[0]
	if cm.Source != SourceWrongSplitChallenge || cm.Direction != DirectionAlternativeJoins {
		t.Errorf("contested mapping must record the source + direction: %+v", cm)
	}
	if !cm.Affects(ids[1]) || !cm.Affects(ids[2]) {
		t.Errorf("both challenged entities must be affected: %+v", cm)
	}
	// The ALTERNATIVE partition is described well enough to COUNT over (attributed contributions), which is what
	// makes a sensitivity range computable without re-running canonicalization.
	if len(cm.AlternativeEntities) != 1 || len(cm.AlternativeEntities[0].Contributions) != 2 {
		t.Fatalf("the alternative must pool both entities' attributed contributions: %+v", cm.AlternativeEntities)
	}
	if len(cm.RaisedBy) != 1 || cm.RaisedBy[0] != ex("a", "m-a") {
		t.Errorf("the dispute must be attributed to its challenger: %+v", cm.RaisedBy)
	}
}

// TestConfirm_UnsplittableWrongMerge_StaysContested: a wrong_merge on an entity whose members are the IDENTICAL
// raw string has no mechanical split available, so the rule records that and leaves the mapping contested with a
// per-source alternative — the pessimistic branch a range must show.
func TestConfirm_UnsplittableWrongMerge_StaysContested(t *testing.T) {
	noms := []Nomination{
		{Raw: "Postgres", SourceExplorer: ex("a", "m-a"), EnvelopeRef: "envelope#0"},
		{Raw: "Postgres", SourceExplorer: ex("b", "m-b"), EnvelopeRef: "envelope#1"},
	}
	res, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: clusterByRaw})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	res.Nominations = noms
	ch := Challenge{Type: ChallengeWrongMerge, CanonicalID: res.Clusters[0].CanonicalID, Reason: "different deployments", By: ex("a", "m-a")}
	conf, cerr := Confirm(res, Present(res, "seed"), []Challenge{ch})
	if cerr != nil {
		t.Fatalf("confirm: %v", cerr)
	}
	if len(conf.Resolutions) != 1 || conf.Resolutions[0].Action != ActionRecordedNoSplitPossible {
		t.Fatalf("an unsplittable wrong_merge must be recorded as such: %+v", conf.Resolutions)
	}
	if len(conf.Contested) != 1 || conf.Contested[0].Direction != DirectionAlternativeSeparates {
		t.Fatalf("the mapping must stay contested with a SEPARATING alternative: %+v", conf.Contested)
	}
	if len(conf.Contested[0].AlternativeEntities) != 2 {
		t.Errorf("the alternative must be one entity per source (i.e. no corroboration): %+v", conf.Contested[0].AlternativeEntities)
	}
}

// TestConfirm_NonPartitionChallengesRecordedOnly: label_bias / missing_item / injected_item change no mapping —
// the surjectivity gate already guarantees nothing is missing or injected, and a label carries no count.
func TestConfirm_NonPartitionChallengesRecordedOnly(t *testing.T) {
	prov := provisional(t)
	id := prov.Clusters[0].CanonicalID
	chs := []Challenge{
		{Type: ChallengeLabelBias, CanonicalID: id, Reason: "prejudicial name", By: ex("a", "m-a")},
		{Type: ChallengeMissingItem, RawNomination: "DuckDB", Reason: "I nominated it", By: ex("b", "m-b")},
		{Type: ChallengeInjectedItem, CanonicalID: id, Reason: "nobody said this", By: ex("b", "m-b")},
	}
	conf, err := Confirm(prov, Present(prov, "seed"), chs)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(conf.Resolutions) != 3 {
		t.Fatalf("every challenge must be resolved + recorded: %+v", conf.Resolutions)
	}
	actions := map[string]int{}
	for _, r := range conf.Resolutions {
		actions[r.Action]++
	}
	if actions[ActionRecordedLabelOnly] != 1 || actions[ActionRecordedNoInjection] != 2 {
		t.Errorf("unexpected actions: %+v", actions)
	}
	if len(conf.Revision.Clusters) != len(prov.Clusters) || !conf.Settled() {
		t.Errorf("these challenges change no mapping and leave nothing contested: %d clusters, %d contested", len(conf.Revision.Clusters), len(conf.Contested))
	}
}

// TestConfirm_CarriesDualContestedMergesForward: a merge the DUAL agreement already refused keeps the held
// partition potentially undercounting, so it stays contested through the confirmation round even with no
// explorer challenge — a count over it must be conditional.
func TestConfirm_CarriesDualContestedMergesForward(t *testing.T) {
	noms := overlappingNoms()
	prov, err := CanonicalizeDual(context.Background(), noms,
		fakeCanonicalizer{propose: stamp("a", clusterByRaw)}, fakeCanonicalizer{propose: stamp("b", mergeAll)})
	if err != nil {
		t.Fatalf("dual: %v", err)
	}
	conf, cerr := Confirm(prov, Present(prov, "seed"), nil)
	if cerr != nil {
		t.Fatalf("confirm: %v", cerr)
	}
	if len(conf.Challenges) != 0 || len(conf.Resolutions) != 0 {
		t.Errorf("no challenges were raised: %+v %+v", conf.Challenges, conf.Resolutions)
	}
	if len(conf.Contested) != 1 || conf.Contested[0].Source != SourceDualDisagreement {
		t.Fatalf("a refused dual merge must carry forward as contested: %+v", conf.Contested)
	}
	if conf.Settled() {
		t.Error("a run with a refused dual merge is NOT settled")
	}
}

// TestConfirm_RequiresNominations: a revision re-partitions the EXACT nominations the provisional partition was
// computed over, so a result without them is refused rather than guessed at.
func TestConfirm_RequiresNominations(t *testing.T) {
	noms := overlappingNoms()
	res, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: clusterByRaw})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if _, cerr := Confirm(res, Present(res, "seed"), nil); cerr == nil {
		t.Fatal("Confirm must refuse a provisional result with no recorded nominations")
	}
}

// TestConfirmation_MarshalsFully pins the persisted confirmation record (§9): the presentation order, the typed
// challenges, the versioned resolutions, the contested mappings and the revision chain all serialize.
func TestConfirmation_MarshalsFully(t *testing.T) {
	prov := mergedProvisional(t)
	conf, err := Confirm(prov, Present(prov, "seed"),
		[]Challenge{{Type: ChallengeWrongMerge, CanonicalID: "sql", Reason: "conflated", By: ex("a", "m-a")}})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	b, merr := json.Marshal(conf)
	if merr != nil {
		t.Fatalf("marshal confirmation: %v", merr)
	}
	for _, want := range []string{"presentation", "challenges", "resolutions", "priorRevisionHash", "ruleVersion", "revision"} {
		if !strings.Contains(string(b), "\""+want+"\"") {
			t.Errorf("serialized confirmation missing %q: %s", want, b)
		}
	}
	// The revision's ledger serializes its revision number + prior hash (the chain is auditable from the file).
	var probe struct {
		Revision struct {
			Ledger struct {
				Revision          int    `json:"revision"`
				PriorRevisionHash string `json:"priorRevisionHash"`
			} `json:"ledger"`
		} `json:"revision"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if probe.Revision.Ledger.Revision != 2 || probe.Revision.Ledger.PriorRevisionHash == "" {
		t.Errorf("the serialized ledger must carry the revision chain: %+v", probe.Revision.Ledger)
	}
}

// TestChallengeType_ClosedSet guards the closed challenge vocabulary: an invented type is never valid, and the
// rendered list is the five typed challenges.
func TestChallengeType_ClosedSet(t *testing.T) {
	if len(ChallengeTypes()) != 5 {
		t.Fatalf("expected 5 typed challenges, got %v", ChallengeTypes())
	}
	for _, ty := range ChallengeTypes() {
		if !ty.Valid() {
			t.Errorf("%q must be valid", ty)
		}
	}
	if ChallengeType("free_form_objection").Valid() {
		t.Error("an untyped/invented challenge must never be valid (it cannot be mechanically adjudicated)")
	}
	var zero schema.ExplorerIdentity
	if err := (Challenge{Type: "nope", Reason: "r", By: zero}).Validate(nil); err == nil {
		t.Error("Validate must reject an unknown type")
	}
}
