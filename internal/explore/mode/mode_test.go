package mode

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

func TestLookup_MapIsRegisteredAndFormulationFree(t *testing.T) {
	spec, ok := Lookup(Map)
	if !ok {
		t.Fatalf("map mode is not registered")
	}
	if spec.Name != Map {
		t.Errorf("spec.Name = %q, want %q", spec.Name, Map)
	}
	if !spec.FormulationFree {
		t.Error("map must be formulation-free (§3)")
	}
	if spec.Objective != ObjectiveCollateOnly {
		t.Errorf("map objective = %q, want collate_only", spec.Objective)
	}
	if spec.Prompt == nil || spec.ExplorerSchema == nil {
		t.Fatal("a formulation-free mode must supply an app-owned prompt builder + explorer schema")
	}
	// The app-owned explorer schema is the fixed minimum schema (NOT collator-authored).
	if err := schema.ExpandedSatisfiesMinimum(spec.ExplorerSchema()); err != nil {
		t.Errorf("map explorer schema is not the fixed app-owned schema: %v", err)
	}
	// The app-owned prompt preserves the task intent verbatim.
	raw := schema.RawTask{Purpose: "compare A and B", Criteria: []string{"cost"}}
	if p := spec.Prompt(raw); p == "" {
		t.Error("map prompt builder returned an empty prompt")
	}
}

func TestLookup_UnknownAndEmpty(t *testing.T) {
	if _, ok := Lookup("nope"); ok {
		t.Error("an unknown mode must not resolve")
	}
	if _, ok := Lookup(""); ok {
		t.Error("Lookup does not default an empty name (Resolve does)")
	}
}

func TestResolve_DefaultsEmptyToMap(t *testing.T) {
	spec, ok := Resolve("")
	if !ok || spec.Name != Map {
		t.Errorf("empty mode should resolve to the default (map), got %q ok=%v", spec.Name, ok)
	}
	if _, ok := Resolve("bogus"); ok {
		t.Error("a non-empty unknown mode must not resolve")
	}
}

func TestNames_SortedAndContainsMap(t *testing.T) {
	names := Names()
	if !slices.Contains(names, Map) {
		t.Errorf("Names() missing map: %v", names)
	}
	if !slices.Contains(names, Synthesize) {
		t.Errorf("Names() missing synthesize: %v", names)
	}
	if !slices.Contains(names, Catalog) {
		t.Errorf("Names() missing catalog: %v", names)
	}
	if !slices.IsSorted(names) {
		t.Errorf("Names() must be sorted: %v", names)
	}
}

func TestLookup_SynthesizeRegisteredAndFormulationFree(t *testing.T) {
	spec, ok := Lookup(Synthesize)
	if !ok {
		t.Fatalf("synthesize mode is not registered")
	}
	if !spec.FormulationFree {
		t.Error("synthesize must be formulation-free (§5: the app owns the round-1 contract)")
	}
	if spec.Objective != ObjectiveSelectCompose {
		t.Errorf("synthesize objective = %q, want select_compose", spec.Objective)
	}
	if spec.Prompt == nil || spec.ExplorerSchema == nil || spec.Collator == nil {
		t.Fatal("synthesize must supply an app-owned prompt + explorer schema + collator contract")
	}
	// The app-owned explorer schema commits the explorer to one best answer + rationale (NOT the Map
	// minimum schema — a formulation-free mode's explorer schema is its own contract).
	sc := spec.ExplorerSchema()
	if _, ok := fieldNamed(sc, "answer"); !ok {
		t.Errorf("synthesize explorer schema missing the required 'answer' field: %+v", sc.Fields)
	}
	if err := schema.ExpandedSatisfiesMinimum(sc); err == nil {
		t.Error("synthesize explorer schema must NOT be the Map minimum schema (it is a distinct contract)")
	}
	// The app-owned prompt renders the schema fields + preserves task intent.
	p := spec.Prompt(schema.RawTask{Purpose: "pick a datastore", Criteria: []string{"latency"}})
	if !strings.Contains(p, "pick a datastore") || !strings.Contains(p, "\"answer\"") {
		t.Errorf("synthesize prompt dropped intent or did not render the schema:\n%s", p)
	}
}

// TestModeCollatorContracts_DifferAndAreCorrectShape pins that each mode's collator contract yields ITS
// OWN terminal output type: map → CollatorOutput, synthesize → SynthesizeOutput.
func TestModeCollatorContracts_DifferAndAreCorrectShape(t *testing.T) {
	mapSpec, _ := Lookup(Map)
	synSpec, _ := Lookup(Synthesize)

	// The two contracts are distinct implementations.
	if _, ok := mapSpec.Collator.(mapCollator); !ok {
		t.Errorf("map contract is %T, want mapCollator", mapSpec.Collator)
	}
	if _, ok := synSpec.Collator.(synthesizeCollator); !ok {
		t.Errorf("synthesize contract is %T, want synthesizeCollator", synSpec.Collator)
	}

	// Map contract == old behavior: a valid CollatorOutput body parses to a CollatorOutput, and the
	// prompt renders the reserved synthesis fields + the no-fences instruction.
	mapPrompt, err := mapSpec.Collator.Prompt(nil)
	if err != nil {
		t.Fatalf("map prompt: %v", err)
	}
	for _, want := range []string{"synthesisSummary", "disagreementRegister", "no markdown code fences"} {
		if !strings.Contains(mapPrompt, want) {
			t.Errorf("map collator prompt missing %q", want)
		}
	}
	mapBody, _ := json.Marshal(schema.CollatorOutput{SynthesisSummary: "s"})
	mo, err := mapSpec.Collator.Parse(mapBody)
	if err != nil {
		t.Fatalf("map parse: %v", err)
	}
	if _, ok := mo.(schema.CollatorOutput); !ok {
		t.Errorf("map contract produced %T, want schema.CollatorOutput", mo)
	}
	// An empty summary is still rejected.
	if _, err := mapSpec.Collator.Parse([]byte(`{"synthesisSummary":""}`)); err == nil {
		t.Error("map contract must still reject an empty synthesis summary")
	}

	// Synthesize contract: the prompt renders the composed-artifact structure + no-tally instruction, and
	// a valid body parses to a SynthesizeOutput.
	synPrompt, err := synSpec.Collator.Prompt(nil)
	if err != nil {
		t.Fatalf("synthesize prompt: %v", err)
	}
	for _, want := range []string{"artifact", "componentProvenance", "minorityReport", "Do NOT tally", "no markdown code fences"} {
		if !strings.Contains(synPrompt, want) {
			t.Errorf("synthesize collator prompt missing %q", want)
		}
	}
	synBody, _ := json.Marshal(schema.SynthesizeOutput{Artifact: "the answer"})
	so, err := synSpec.Collator.Parse(synBody)
	if err != nil {
		t.Fatalf("synthesize parse: %v", err)
	}
	if _, ok := so.(schema.SynthesizeOutput); !ok {
		t.Errorf("synthesize contract produced %T, want schema.SynthesizeOutput", so)
	}
	// An empty artifact is rejected.
	if _, err := synSpec.Collator.Parse([]byte(`{"artifact":""}`)); err == nil {
		t.Error("synthesize contract must reject an empty artifact")
	}
}

// TestMapContract_PromptsEveryEnvelopeRegardlessOfIdentity pins that the collator is shown EVERY
// envelope, including one whose identity could not be verified. There is no second-class panel: a
// weak-identity response is cited the same way as any other, so its alias must appear in the prompt.
func TestMapContract_PromptsEveryEnvelopeRegardlessOfIdentity(t *testing.T) {
	mapSpec, _ := Lookup(Map)
	primary := []schema.Envelope{
		{Identity: schema.ExplorerIdentity{Adapter: "a", Model: "m", Effort: "high"}, IdentityStatus: schema.IdentityVerified, Order: 0},
		{Identity: schema.ExplorerIdentity{Adapter: "b", Model: "n", Effort: "high"}, IdentityStatus: schema.IdentitySelfReported, Order: 1},
		{Identity: schema.ExplorerIdentity{Adapter: "c", Model: "o", Effort: "high"}, IdentityStatus: schema.IdentityMismatch, Order: 2},
	}
	p, err := mapSpec.Collator.Prompt(primary)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for _, alias := range []string{"envelope#0", "envelope#1", "envelope#2"} {
		if !strings.Contains(p, alias) {
			t.Errorf("collator prompt omits %s — every seat is citable whatever its identity says", alias)
		}
	}
}

// TestLookup_CatalogRegisteredAndCanonicalizing pins the Catalog mode: formulation-free, the
// canonicalize+collate objective, an app-owned "enumerate broadly" prompt + candidates[] schema, and a
// CANONICALIZING contract (Canonicalizing set, Collator nil — the two are mutually exclusive).
func TestLookup_CatalogRegisteredAndCanonicalizing(t *testing.T) {
	spec, ok := Lookup(Catalog)
	if !ok {
		t.Fatalf("catalog mode is not registered")
	}
	if !spec.FormulationFree {
		t.Error("catalog must be formulation-free (§5: the app owns the round-1 contract)")
	}
	if spec.Objective != ObjectiveCanonicalizeCollate {
		t.Errorf("catalog objective = %q, want canonicalize_collate", spec.Objective)
	}
	if spec.Canonicalizing == nil {
		t.Fatal("catalog must supply a canonicalizing contract")
	}
	if spec.Collator != nil {
		t.Error("catalog is a canonicalizing mode — Collator must be nil (mutually exclusive with Canonicalizing)")
	}
	if _, ok := spec.Canonicalizing.(catalogCollator); !ok {
		t.Errorf("catalog contract is %T, want catalogCollator", spec.Canonicalizing)
	}
	// The app-owned explorer schema requires a candidates[] list (NOT the Map minimum schema).
	sc := spec.ExplorerSchema()
	f, ok := fieldNamed(sc, "candidates")
	if !ok || !f.Repeated || !f.Required {
		t.Errorf("catalog explorer schema missing a required candidates[] field: %+v", sc.Fields)
	}
	if err := schema.ExpandedSatisfiesMinimum(sc); err == nil {
		t.Error("catalog explorer schema must NOT be the Map minimum schema (it is a distinct contract)")
	}
	// The app-owned prompt asks for broad enumeration + preserves task intent.
	p := spec.Prompt(schema.RawTask{Purpose: "list caching strategies", Criteria: []string{"latency"}})
	if !strings.Contains(p, "list caching strategies") || !strings.Contains(p, "Enumerate as many DISTINCT") {
		t.Errorf("catalog prompt dropped intent or the enumerate-broadly instruction:\n%s", p)
	}
}

// TestCatalogContract_CanonicalizerFlow exercises the catalog contract's pieces in isolation: nomination
// extraction, the canonicalizer prompt (spells out the exact proposal structure + no-fences), proposal
// parsing (memberIndices → members, identity stamped), and Collate over a canon.Result → CatalogOutput.
func TestCatalogContract_CanonicalizerFlow(t *testing.T) {
	cc := catalogCollator{}
	// Nominations flatten each envelope's candidates[] with attribution.
	primary := []schema.Envelope{
		{Identity: schema.ExplorerIdentity{Adapter: "a", Model: "m-a", Effort: "high"}, Order: 0,
			Response: map[string]any{"candidates": []any{"Postgres", "SQLite"}}},
		{Identity: schema.ExplorerIdentity{Adapter: "b", Model: "m-b", Effort: "high"}, Order: 1,
			Response: map[string]any{"candidates": []any{"Postgres"}}},
	}
	noms := cc.Nominations(primary)
	if len(noms) != 3 {
		t.Fatalf("expected 3 nominations, got %d: %+v", len(noms), noms)
	}
	if noms[0].SourceExplorer.Adapter != "a" || noms[0].EnvelopeRef != "envelope#0" {
		t.Errorf("nomination attribution wrong: %+v", noms[0])
	}

	prompt, err := cc.CanonicalizerPrompt(noms)
	if err != nil {
		t.Fatalf("canonicalizer prompt: %v", err)
	}
	for _, want := range []string{"CANONICALIZER", "memberIndices", "proposedDimensions", "NEVER DROP", "no markdown code fences"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("canonicalizer prompt missing %q", want)
		}
	}

	raw := []byte(`{"clusters":[{"canonicalId":"c0","name":"Postgres","memberIndices":[0,2]},{"canonicalId":"c1","name":"SQLite","memberIndices":[1]}],"proposedDimensions":["maturity"],"coverageNotes":"ok"}`)
	id := schema.ExplorerIdentity{Adapter: "coll", Model: "canon-model", Effort: "high"}
	prop, err := cc.ParseProposal(raw, noms, "call-ref", id)
	if err != nil {
		t.Fatalf("parse proposal: %v", err)
	}
	if len(prop.Clusters) != 2 || prop.DecidedByCall != "call-ref" || prop.DecidedByIdentity != id {
		t.Errorf("parsed proposal wrong: %+v", prop)
	}

	// End-to-end through canon → Collate yields a CatalogOutput with a merged cluster + a carried singleton.
	res, err := canon.Canonicalize(context.Background(), noms, fixedProposal{prop})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	out, err := cc.Collate(res)
	if err != nil {
		t.Fatalf("collate: %v", err)
	}
	co, ok := out.(schema.CatalogOutput)
	if !ok {
		t.Fatalf("collate produced %T, want schema.CatalogOutput", out)
	}
	if err := co.Validate(); err != nil {
		t.Errorf("CatalogOutput failed its own validation: %v", err)
	}
	if len(co.Clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(co.Clusters))
	}
}

// TestRegisteredModes_ObserveModesUnchanged is the standing regression sentinel over the REGISTRY: the
// OBSERVE-posture modes (Map, Synthesize, Catalog) must stay exactly what they were — a SINGLE blind round
// with the ZERO canonicalization policy (one canonicalizer, no confirmation round) — no matter what governance
// the adjudicative modes opt into. Challenge/Shortlist/ai-collab DO opt in; this test is what proves that
// opting them in does not sweep the others along with it.
//
// Every EMERGENT-space mode is also asserted to be emergent-space, so no degraded terminal artifact of theirs
// can ever be a host-synthesized register over explorer-authored claims (which would be covert entity
// resolution). The two genuinely FIXED-space modes are held to the opposite requirement — a declared key
// universe and a declared space — and are excluded from the observe-posture assertions below, which are about
// the canonicalization policy they deliberately do not have.
func TestRegisteredModes_ObserveModesUnchanged(t *testing.T) {
	// The adjudicative modes are count/ballot-bearing and take the ranking-grade policy by design; every
	// OTHER emergent-space mode must still be the single-round, zero-policy observe shape.
	adjudicative := map[string]bool{Challenge: true, Shortlist: true, AICollab: true}
	fixedSpace := map[string]bool{Compare: true, Forecast: true}
	for _, name := range Names() {
		spec, ok := Lookup(name)
		if !ok {
			t.Fatalf("registered mode %q does not resolve", name)
		}
		if fixedSpace[name] {
			// A FIXED-SPACE mode: its value universe was DECLARED by the user, so a real host register is
			// honest for it — and it must run one blind round with NO canonicalization policy at all, which is
			// the whole reason it is a separate class rather than a variation on the others.
			if spec.ModeClass() != schema.FixedSpace || spec.FixedSpaceKeyField == "" {
				t.Errorf("%s: a fixed-space mode must declare the fixed-space class AND its degraded register key field", name)
			}
			if spec.FixedSpace == nil || spec.Canonicalizing != nil || spec.Collator != nil {
				t.Errorf("%s: a fixed-space mode declares EXACTLY the fixed-space terminal contract", name)
			}
			if spec.Canonicalization.Dual || spec.Canonicalization.Confirm {
				t.Errorf("%s: a fixed-space mode declares no canonicalization policy: %+v", name, spec.Canonicalization)
			}
			if n, err := spec.RoundCount(); err != nil || n != 1 {
				t.Errorf("%s: a fixed-space mode is a single blind round, got %d (%v)", name, n, err)
			}
			if spec.LaterRound != nil || spec.Ballot != nil {
				t.Errorf("%s: a fixed-space mode declares no later round and no ballot", name)
			}
			if spec.ValidateTask == nil {
				t.Errorf("%s: a fixed-space mode must validate its DECLARED space before any spend", name)
			}
			continue
		}
		if spec.ModeClass() != schema.EmergentSpace {
			t.Errorf("%s: expected emergent-space class, got %q", name, spec.ModeClass())
		}
		if spec.FixedSpaceKeyField != "" {
			t.Errorf("%s: an emergent-space mode declares no fixed-space key field", name)
		}
		if spec.FixedSpace != nil {
			t.Errorf("%s: an emergent-space mode declares no fixed-space contract", name)
		}
		if adjudicative[name] {
			// §3: confirmation covers EVERY count-bearing mode — giving one of them a guard the others lack is
			// exactly the asymmetry to avoid, so assert they all carry it.
			if !spec.Canonicalization.Dual || !spec.Canonicalization.Confirm {
				t.Errorf("%s: an adjudicative mode must take the ranking-grade policy (dual + confirm): %+v", name, spec.Canonicalization)
			}
			n, err := spec.RoundCount()
			if err != nil || n != 2 {
				t.Errorf("%s: an adjudicative mode runs 2 fixed rounds, got %d (%v)", name, n, err)
			}
			if spec.LaterRound == nil {
				t.Errorf("%s: a multi-round mode must supply its later-round contract", name)
			}
			continue
		}
		if spec.Canonicalization.Dual || spec.Canonicalization.Confirm {
			t.Errorf("%s: an observe-posture mode must NOT opt into the ranking-grade policy: %+v", name, spec.Canonicalization)
		}
		n, err := spec.RoundCount()
		if err != nil || n != 1 {
			t.Errorf("%s: an observe-posture mode is a single blind round, got %d (%v)", name, n, err)
		}
		if spec.Rounds > 1 || spec.LaterRound != nil {
			t.Errorf("%s: an observe-posture mode declares no later round", name)
		}
		if spec.Ballot != nil {
			t.Errorf("%s: only a ballot-bearing mode may declare a ballot contract", name)
		}
	}
	// The registry is exactly the eight registered modes; a new name appearing here without a decision is a regression.
	if got, want := len(Names()), 8; got != want {
		t.Errorf("registered modes = %d, want %d: %v", got, want, Names())
	}
	for _, want := range []string{Map, Synthesize, Catalog, Challenge, Shortlist, AICollab, Compare, Forecast} {
		if _, ok := Lookup(want); !ok {
			t.Errorf("mode %q is not registered", want)
		}
	}
}

// TestModeClass_DefaultsToEmergent pins the conservative default: an unset Class is emergent-space, because
// assuming a fixed value universe when there is none would license the host to group free-form claims.
func TestModeClass_DefaultsToEmergent(t *testing.T) {
	if (ModeSpec{}).ModeClass() != schema.EmergentSpace {
		t.Error("an unset mode class must default to emergent-space (the conservative direction)")
	}
	if (ModeSpec{Class: schema.FixedSpace}).ModeClass() != schema.FixedSpace {
		t.Error("an explicit fixed-space class must be honored")
	}
}

// TestRoundCount_BoundedByContract pins that the FIXED round count comes from the contract and fails (never
// clamps) above the hard maximum.
func TestRoundCount_BoundedByContract(t *testing.T) {
	if n, err := (ModeSpec{Rounds: 2}).RoundCount(); err != nil || n != 2 {
		t.Errorf("RoundCount(2) = %d, %v", n, err)
	}
	if _, err := (ModeSpec{Rounds: round.MaxRounds + 1}).RoundCount(); err == nil {
		t.Error("a contract above the hard maximum must ERROR, never clamp")
	}
}

// fixedProposal is a canon.CanonicalizerCall returning a pre-built Proposal (for the contract flow test).
type fixedProposal struct{ p canon.Proposal }

func (f fixedProposal) Propose(_ context.Context, _ []canon.Nomination) (canon.Proposal, error) {
	return f.p, nil
}

// fieldNamed reports whether schema s has a field named name.
func fieldNamed(s schema.Schema, name string) (schema.Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return schema.Field{}, false
}
