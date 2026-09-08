package pipeline

// End-to-end tests for the multi-round + ranking-grade governance machinery (design §0/§1/§4/§6).
// Everything here is hermetic — deterministic in-process fakes, no real CLI — and every test pins ONE
// invariant from the design rather than the shape of the implementation.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/model/fake"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// --- the example multi-round governed mode (an unregistered spec, so these tests exercise the machinery
// itself rather than any registered mode's configuration of it) ---

// exampleLaterRound is the app-owned contract for a mediated round 2+ (mode.LaterRoundContract). It accepts ONLY
// the pooled confirmed-canonical uniques at the current artifact schema version, and it embeds the host's
// already-framed untrusted-data block VERBATIM — a contract cannot re-frame or re-label it.
type exampleLaterRound struct {
	accepts round.Accepts
}

func (e exampleLaterRound) Accepts() round.Accepts {
	if len(e.accepts.Kinds) > 0 {
		return e.accepts
	}
	return round.Accepts{Kinds: []round.Kind{round.KindCanonicalUniques}, SchemaVersion: round.SchemaVersion}
}

func (exampleLaterRound) Prompt(raw schema.RawTask, untrustedDataBlock string) string {
	return "This is a LATER round of the same exploration. Deepen your analysis of the candidate set the panel " +
		"produced: keep the candidates you still endorse and add refinements. Respond with a SINGLE JSON object — " +
		"no prose, no markdown code fences.\n\n" +
		"Purpose:\n" + raw.Purpose + "\n\n" +
		untrustedDataBlock + "\n" +
		"The JSON object MUST contain EXACTLY these fields:\n" +
		schema.RenderSchema(exampleLaterRound{}.ExplorerSchema())
}

func (exampleLaterRound) ExplorerSchema() schema.Schema {
	return schema.Schema{Fields: []schema.Field{
		{Name: "candidates", Type: schema.TypeString, Required: true, Repeated: true},
		{Name: "refinements", Type: schema.TypeString, Required: true, Repeated: true},
		{Name: "confidence", Type: schema.TypeNumber, Required: true},
	}}
}

// governedSpec builds the example GOVERNED mode spec: Catalog's app-owned round-1 + canonicalizing contract (so
// the deterministic fakes drive it end-to-end) with a RANKING-GRADE canonicalization policy and an optional
// later round. It is intentionally NOT registered: it exercises the governance machinery without pinning
// any registered mode's policy.
func governedSpec(dual, confirm bool, rounds int, later mode.LaterRoundContract) mode.ModeSpec {
	cat, _ := mode.Lookup(mode.Catalog)
	return mode.ModeSpec{
		Name:             "example-governed",
		FormulationFree:  true,
		Prompt:           cat.Prompt,
		ExplorerSchema:   cat.ExplorerSchema,
		Objective:        cat.Objective,
		Canonicalizing:   cat.Canonicalizing,
		Canonicalization: mode.CanonicalizationPolicy{Dual: dual, Confirm: confirm},
		Rounds:           rounds,
		LaterRound:       later,
		Class:            schema.EmergentSpace,
	}
}

// runSpec runs an explicit spec over the given registry/plan with the default policy.
func runSpec(t *testing.T, reg Registry, plan roster.Plan, spec mode.ModeSpec, opts Options) (Result, error) {
	t.Helper()
	return RunSpec(context.Background(), reg, plan, schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}},
		spec, opts, nil)
}

// --- test adapters ---

// countingAdapter wraps an adapter and tallies invocations PER PHASE — how a test proves a halt fired BEFORE the
// explorer fan-out rather than after it.
type countingAdapter struct {
	inner model.Adapter
	mu    sync.Mutex
	calls map[string]int
}

func counted(inner model.Adapter) *countingAdapter {
	return &countingAdapter{inner: inner, calls: map[string]int{}}
}

func (c *countingAdapter) Name() string              { return c.inner.Name() }
func (c *countingAdapter) Available() (bool, string) { return c.inner.Available() }
func (c *countingAdapter) Evidence() core.IdentityEvidence {
	if ev, ok := c.inner.(interface{ Evidence() core.IdentityEvidence }); ok {
		return ev.Evidence()
	}
	return core.EvidenceNone
}
func (c *countingAdapter) Invoke(ctx context.Context, call model.Call) (model.Result, error) {
	c.mu.Lock()
	c.calls[call.Phase]++
	c.mu.Unlock()
	return c.inner.Invoke(ctx, call)
}
func (c *countingAdapter) count(phase string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[phase]
}

// total is every invocation that reached this adapter, whatever the phase — what a caller is billed for, and
// the figure the dry-run shape is measured against (see shape_test.go).
func (c *countingAdapter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

// swappedIdentityAdapter always reports a DIFFERENT model than requested — a provider that silently fell back.
type swappedIdentityAdapter struct{ inner model.Adapter }

func (s swappedIdentityAdapter) Name() string                    { return s.inner.Name() }
func (s swappedIdentityAdapter) Available() (bool, string)       { return s.inner.Available() }
func (s swappedIdentityAdapter) Evidence() core.IdentityEvidence { return core.EvidenceInvocationTag }
func (s swappedIdentityAdapter) Invoke(ctx context.Context, call model.Call) (model.Result, error) {
	r, err := s.inner.Invoke(ctx, call)
	r.ActualModel = "some-other-model-9"
	return r, err
}

// driftingAdapter is the REALISTIC mid-exploration model swap: the run requests the ALIAS `opus`, the provider
// resolves it to `claude-opus-4-1` on the pre-flight probe and to `claude-opus-4-5` afterwards. Both resolutions
// alias-match the requested name, so per-call identity classification passes BOTH times (no mismatch) — the swap
// is invisible to per-call verification and only the same-identity invariant catches it. That is exactly why the
// invariant exists in addition to per-call classification (§1).
type driftingAdapter struct {
	inner model.Adapter
	mu    sync.Mutex
	calls int
}

func (d *driftingAdapter) Name() string                    { return d.inner.Name() }
func (d *driftingAdapter) Available() (bool, string)       { return d.inner.Available() }
func (d *driftingAdapter) Evidence() core.IdentityEvidence { return core.EvidenceInvocationTag }
func (d *driftingAdapter) Invoke(ctx context.Context, call model.Call) (model.Result, error) {
	r, err := d.inner.Invoke(ctx, call)
	d.mu.Lock()
	d.calls++
	n := d.calls
	d.mu.Unlock()
	r.ActualModel = "claude-opus-4-1"
	if n > 1 {
		r.ActualModel = "claude-opus-4-5"
	}
	return r, err
}

// failAfterPreflight passes the identity pre-flight and then FAILS every later call — a collator that becomes
// unavailable AFTER the fan-out, the case the degraded terminal artifact exists for.
type failAfterPreflight struct{ inner model.Adapter }

func (f failAfterPreflight) Name() string                    { return f.inner.Name() }
func (f failAfterPreflight) Available() (bool, string)       { return f.inner.Available() }
func (f failAfterPreflight) Evidence() core.IdentityEvidence { return core.EvidenceInvocationTag }
func (f failAfterPreflight) Invoke(ctx context.Context, call model.Call) (model.Result, error) {
	if call.Phase == schema.PhasePreflight {
		return f.inner.Invoke(ctx, call)
	}
	return model.Result{ExitCode: 1, Stderr: []byte("collator gone: 503 service unavailable")}, context.DeadlineExceeded
}

// panelOf builds a registry + plan from per-explorer scenarios and a collator scenario, letting a test wrap any
// adapter afterwards.
func panelOf(t *testing.T, explorers []fake.Scenario, collator fake.Scenario) (Registry, roster.Plan) {
	t.Helper()
	reg := Registry{}
	tags := []string{"A", "B", "C", "D"}
	var exs []roster.Explorer
	for i, sc := range explorers {
		name := "explorer-" + tags[i]
		reg[name] = fake.New(name, tags[i], sc)
		exs = append(exs, roster.Explorer{Adapter: name, Model: "model-" + tags[i], Effort: "high"})
	}
	reg["collator"] = fake.New("collator", "C", collator)
	plan, err := roster.Roster{Explorers: exs, Collator: roster.Collator{Adapter: "collator", Model: "collator-model"}}.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return reg, plan
}

// --- 1. Typed round artifacts + the multi-round pipeline (design §6 + §1) ---

// TestRunSpec_MultiRound_CarriesPriorArtifactAsUntrustedData pins the multi-round machinery: round 1 is BLIND
// and immutable, round 2 is mediated, the round-2 prompt carries the prior artifact as explicitly-delimited
// UNTRUSTED DATA (never instructions), explorers never see raw peer output, and the exact digest shown is
// recorded per explorer.
func TestRunSpec_MultiRound_CarriesPriorArtifactAsUntrustedData(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := runSpec(t, reg, plan, governedSpec(false, true, 2, exampleLaterRound{}), Options{})
	if err != nil {
		t.Fatalf("2-round governed run: %v", err)
	}
	if len(res.Rounds) != 2 {
		t.Fatalf("expected 2 recorded rounds, got %d", len(res.Rounds))
	}
	if !res.Rounds[0].Blind() || res.Rounds[1].Blind() {
		t.Errorf("round 1 must be blind and round 2 must NOT be: %v / %v", res.Rounds[0].Blind(), res.Rounds[1].Blind())
	}
	// Round 1's envelopes are the epistemic baseline and are unchanged by the later round.
	if len(res.Rounds[0].Envelopes()) != 2 || len(res.Envelopes) != 2 {
		t.Errorf("the blind baseline must keep exactly its 2 envelopes: round1=%d result=%d", len(res.Rounds[0].Envelopes()), len(res.Envelopes))
	}
	// The round-2 prompt frames the carried artifact as DATA, with the host's delimiters + preamble.
	if len(res.LaterRoundPrompts) != 1 {
		t.Fatalf("expected 1 later-round prompt, got %d", len(res.LaterRoundPrompts))
	}
	p := res.LaterRoundPrompts[0]
	for _, want := range []string{"BEGIN UNTRUSTED DATA", "END UNTRUSTED DATA", "DATA ONLY", "NOT an instruction", "contentHash:"} {
		if !strings.Contains(p, want) {
			t.Errorf("round-2 prompt missing the untrusted-data framing %q", want)
		}
	}
	// It carries the CANONICAL items, not raw peer bodies.
	if !strings.Contains(p, "Postgres") {
		t.Errorf("round-2 prompt should carry the pooled canonical items:\n%s", p)
	}
	for _, env := range res.Rounds[0].Envelopes() {
		body := strings.TrimSpace(string(env.RawResponse))
		if len(body) >= 24 && strings.Contains(p, body[:24]) {
			t.Error("round-2 prompt contains a RAW peer response — explorers must never see raw peer output")
		}
	}
	// The mediation record names the artifact + the exact digest shown to each explorer.
	if len(res.Mediations) != 1 {
		t.Fatalf("expected 1 mediation record, got %d", len(res.Mediations))
	}
	med := res.Mediations[0]
	if med.RoundIndex != 2 || med.Artifact.ContentHash == "" || med.Artifact.Discriminant.Kind != round.KindCanonicalUniques {
		t.Errorf("mediation record incomplete: %+v", med)
	}
	if len(med.ShownTo) != len(plan.Explorers) {
		t.Fatalf("the digest shown must be recorded per explorer: %+v", med.ShownTo)
	}
	for _, s := range med.ShownTo {
		if s.ContentHash != med.Artifact.ContentHash || s.RoundIndex != 2 {
			t.Errorf("shown-digest record wrong: %+v", s)
		}
	}
	// The recorded round-2 envelopes carry the later-round schema's fields (the round really ran).
	r2 := res.Rounds[1].Envelopes()
	if len(r2) != 2 {
		t.Fatalf("expected 2 round-2 envelopes, got %d", len(r2))
	}
	if _, ok := r2[0].Response["refinements"]; !ok {
		t.Errorf("round-2 response should match the later-round schema: %+v", r2[0].Response)
	}
	if res.Output == nil {
		t.Error("a multi-round governed run must still produce its terminal output")
	}
}

// TestRunSpec_IncompatibleRoundEdge_RejectedBeforeSpend pins §6's edge check: a later round that accepts a
// DIFFERENT artifact kind is rejected BEFORE any round-2 model call — an incompatible edge must cost zero tokens.
func TestRunSpec_IncompatibleRoundEdge_RejectedBeforeSpend(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	// Count explorer calls so we can prove round 2 never dispatched.
	cA := counted(reg["explorer-A"])
	cB := counted(reg["explorer-B"])
	reg["explorer-A"], reg["explorer-B"] = cA, cB

	spec := governedSpec(false, true, 2, exampleLaterRound{accepts: round.Accepts{
		Kinds: []round.Kind{round.KindProvisionalPartition}, SchemaVersion: round.SchemaVersion,
	}})
	_, err := runSpec(t, reg, plan, spec, Options{})
	if err == nil {
		t.Fatal("an incompatible round→round edge must halt")
	}
	if !strings.Contains(err.Error(), "edge rejected before any model call") || !strings.Contains(err.Error(), "rejected before spend") {
		t.Errorf("the halt must name the rejected edge, got: %v", err)
	}
	// Exactly ONE explore call per explorer (round 1) — round 2 was never dispatched.
	for name, c := range map[string]*countingAdapter{"A": cA, "B": cB} {
		if got := c.count(schema.PhaseExplore); got != 1 {
			t.Errorf("explorer %s: expected 1 explore call (round 1 only), got %d — round 2 must not spend tokens on a rejected edge", name, got)
		}
	}
	// A schemaVersion mismatch is rejected the same way.
	spec2 := governedSpec(false, true, 2, exampleLaterRound{accepts: round.Accepts{
		Kinds: []round.Kind{round.KindCanonicalUniques}, SchemaVersion: round.SchemaVersion + 1,
	}})
	if _, err := runSpec(t, reg, plan, spec2, Options{}); err == nil || !strings.Contains(err.Error(), "schemaVersion") {
		t.Errorf("a schemaVersion mismatch must be rejected before spend, got: %v", err)
	}
}

// TestRunSpec_FixedRoundCount_TerminatesAndFailsOverCap pins §1's termination rule: the round count is FIXED by
// the mode contract (no data-dependent rule), it terminates exactly there, and a contract over the hard maximum
// is an ERROR — never clamped.
func TestRunSpec_FixedRoundCount_TerminatesAndFailsOverCap(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := runSpec(t, reg, plan, governedSpec(false, true, 3, exampleLaterRound{}), Options{})
	if err != nil {
		t.Fatalf("3-round governed run: %v", err)
	}
	if len(res.Rounds) != 3 {
		t.Errorf("a FIXED 3-round contract must run exactly 3 rounds, got %d", len(res.Rounds))
	}
	_, err = runSpec(t, reg, plan, governedSpec(false, true, round.MaxRounds+1, exampleLaterRound{}), Options{})
	if err == nil {
		t.Fatal("a contract over the hard round maximum must halt")
	}
	if !strings.Contains(err.Error(), "never clamped") {
		t.Errorf("the halt must say the count is never clamped, got: %v", err)
	}
	// A multi-round contract with no later-round contract, and one that is not canonicalizing, are both refused.
	if _, err := runSpec(t, reg, plan, governedSpec(false, true, 2, nil), Options{}); err == nil {
		t.Error("a multi-round mode with no LaterRound contract must halt")
	}
	mapSpec, _ := mode.Lookup(mode.Map)
	mapSpec.Rounds, mapSpec.LaterRound = 2, exampleLaterRound{}
	_, err = runSpec(t, reg, plan, mapSpec, Options{})
	if err == nil || !strings.Contains(err.Error(), "covert entity resolution") {
		t.Errorf("a non-canonicalizing multi-round mode must be refused (pooling un-canonicalized peer items is covert entity resolution), got: %v", err)
	}
}

// --- 2. Dual-canonicalizer merge-agreement (design §0 F-B) ---

// TestRunSpec_DualCanonicalizer_AgreedMergesHoldAndAgreedByRecorded: two independent canonicalizers that AGREE
// produce the agreed partition, and every ledger row records BOTH proposing calls while the DECIDING call is the
// versioned host rule.
func TestRunSpec_DualCanonicalizer_AgreedMergesHoldAndAgreedByRecorded(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := runSpec(t, reg, plan, governedSpec(true, false, 1, nil), Options{})
	if err != nil {
		t.Fatalf("dual governed run: %v", err)
	}
	if res.Canonicalization == nil {
		t.Fatal("a governed run must record its canonicalization")
	}
	if res.Canonicalization.AgreementRuleVersion != canon.DualRuleVersion {
		t.Errorf("the partition must record the dual agreement rule, got %q", res.Canonicalization.AgreementRuleVersion)
	}
	// TWO separately-verified canonicalizer calls with DIFFERENT identities (a second opinion from the same
	// weights would not be a second opinion).
	if len(res.CanonicalizerCalls) != 2 {
		t.Fatalf("expected 2 canonicalizer call records, got %d: %+v", len(res.CanonicalizerCalls), res.CanonicalizerCalls)
	}
	a, b := res.CanonicalizerCalls[0], res.CanonicalizerCalls[1]
	if a.Identity == b.Identity {
		t.Error("the two canonicalizers must be independent identities")
	}
	for _, rec := range res.CanonicalizerCalls {
		if rec.Status != schema.IdentityVerified {
			t.Errorf("each canonicalizer is verified SEPARATELY: %+v", rec)
		}
	}
	for _, row := range res.Canonicalization.Ledger.Rows() {
		if len(row.AgreedBy) != 2 {
			t.Fatalf("every dual row must record agreedBy for both canonicalizers: %+v", row)
		}
		if row.DecidedByCall != "host:"+canon.DualRuleVersion {
			t.Errorf("the deciding authority must be the host rule, got %q", row.DecidedByCall)
		}
	}
	// The agreed duplicate still merges and the singletons still carry (surjectivity + minority carry-through).
	if res.Canonicalization.Ledger.Len() != 4 {
		t.Errorf("expected 4 ledger rows (one per nomination), got %d", res.Canonicalization.Ledger.Len())
	}
	out, ok := res.Output.(schema.CatalogOutput)
	if !ok {
		t.Fatalf("expected the mode's terminal output, got %T", res.Output)
	}
	merged, carried := 0, 0
	for _, c := range out.Clusters {
		if len(c.Members) == 2 {
			merged++
		}
		if len(c.Members) == 1 && c.Members[0].SingleSource {
			carried++
		}
	}
	if merged != 1 || carried != 2 {
		t.Errorf("expected 1 agreed merge + 2 carried singletons, got merged=%d carried=%d", merged, carried)
	}
}

// TestRunSpec_DualCanonicalizer_ContestedSplitsAndWithholdsCorroborated is the headline governance invariant: a
// merge only ONE canonicalizer proposes is CONTESTED → SPLIT, the refused merge is recorded with its proposer,
// and every dependent count becomes CONDITIONAL with the definitive `corroborated` label WITHHELD.
func TestRunSpec_DualCanonicalizer_ContestedSplitsAndWithholdsCorroborated(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	// Canonicalizer B merges EVERYTHING (the aggressive merger the dual rule exists to neutralize).
	reg["merger"] = fake.New("merger", "M", fake.CanonMergeAll)
	opts := Options{Canonicalizers: []roster.Explorer{
		{Adapter: "collator", Model: "collator-model"},
		{Adapter: "merger", Model: "merger-model"},
	}}
	res, err := runSpec(t, reg, plan, governedSpec(true, true, 1, nil), opts)
	if err != nil {
		t.Fatalf("dual+confirm governed run: %v", err)
	}
	// B's unilateral merges were REFUSED: the partition stays at A's 3 entities, singletons single-source.
	if got := len(res.Canonicalization.Clusters); got != 3 {
		t.Fatalf("a merge only one canonicalizer proposed must be SPLIT (3 entities expected), got %d", got)
	}
	// The refused merge is recorded as a first-class decision, attributed to its proposer.
	contested := res.Provisional.Ledger.Contested()
	if len(contested) != 1 || contested[0].Resolution != canon.ResolutionSplit {
		t.Fatalf("the refused merge must be recorded with a split resolution: %+v", contested)
	}
	if !strings.Contains(contested[0].ProposedBy.Call, "canonicalizer-b") {
		t.Errorf("the contested record must name WHICH canonicalizer proposed it: %+v", contested[0].ProposedBy)
	}
	// A contested mapping makes every dependent count CONDITIONAL: a range, and the definitive label withheld.
	if res.Confirmation == nil || res.Confirmation.Settled() {
		t.Fatal("a refused dual merge must leave the run un-settled")
	}
	if res.Governance == nil || len(res.Governance.Claims) == 0 {
		t.Fatal("a ranking-grade run must emit governance claims")
	}
	for _, c := range res.Governance.Claims {
		if c.Label == govern.LabelCorroborated {
			t.Errorf("no claim over a CONTESTED partition may be labeled corroborated: %+v", c)
		}
		if c.Label != govern.LabelWithheldContested || c.Sensitivity == nil {
			t.Fatalf("a contested claim must be withheld WITH a sensitivity range: %+v", c)
		}
		if c.Sensitivity.Low > c.Sensitivity.High {
			t.Errorf("sensitivity range inverted: %+v", c.Sensitivity)
		}
		if !strings.Contains(c.Rendering(), "WITHHELD") {
			t.Errorf("a withheld claim's rendering must say so: %q", c.Rendering())
		}
	}
}

// TestRunSpec_DualCanonicalizer_UnreachableHalts: a canonicalizer that cannot be INVOKED halts — caught
// at the pre-flight probe, i.e. before the explorer fan-out is paid for. The dual path needs two working
// canonicalizers, so discovering the second one is dead after paying for a panel is pure waste.
func TestRunSpec_DualCanonicalizer_UnreachableHalts(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	cA := counted(reg["explorer-A"])
	reg["explorer-A"] = cA
	reg["bad-canon"] = fake.New("bad-canon", "M", fake.InvokeError)
	opts := Options{Canonicalizers: []roster.Explorer{
		{Adapter: "collator", Model: "collator-model"},
		{Adapter: "bad-canon", Model: "bad-canon-model"},
	}}
	_, err := runSpec(t, reg, plan, governedSpec(true, false, 1, nil), opts)
	if err == nil {
		t.Fatal("an uninvokable canonicalizer must halt")
	}
	if !strings.Contains(err.Error(), "canonicalizer-b") || !strings.Contains(err.Error(), "pre-flight") {
		t.Errorf("the halt must name the canonicalizer role + the pre-flight stage, got: %v", err)
	}
	if got := cA.count(schema.PhaseExplore); got != 0 {
		t.Errorf("a pre-flight halt must precede the fan-out: %d explorer call(s) were made", got)
	}
}

// TestRunSpec_DualWithoutIndependentIdentity_FailsClosed: with no distinct second identity available at all —
// every seat is the collator's own adapter AND model — the dual path REFUSES to run, because the alternative
// is inventing the second identity, which would manufacture the corroboration the rule exists to test.
//
// This is NOT the same case as a pair sharing a model behind two different adapters: that one is allowed and
// recorded as shared_model (see TestCanonicalizerSharedModelIsRecordedNotRefused). What is missing here is a
// distinct identity to name, not independence in the abstract.
func TestRunSpec_DualWithoutIndependentIdentity_FailsClosed(t *testing.T) {
	// Every panel member shares the collator's adapter+model (distinguished only by effort).
	reg := Registry{"shared": fake.New("shared", "A", fake.Valid)}
	exs := []roster.Explorer{
		{Adapter: "shared", Model: "one-model", Effort: "high"},
		{Adapter: "shared", Model: "one-model", Effort: "low"},
	}
	plan, err := roster.Roster{Explorers: exs, Collator: roster.Collator{Adapter: "shared", Model: "one-model"}}.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := runSpec(t, reg, plan, governedSpec(true, false, 1, nil), Options{}); err == nil ||
		!strings.Contains(err.Error(), "SECOND canonicalizer identity") {
		t.Errorf("the dual path must fail closed when there is no distinct second identity, got: %v", err)
	}
}

// --- 3. Binding host-adjudicated confirmation round (design §4) ---

// TestRunSpec_Confirmation_WrongMergeSplitsAsNewRevision pins the confirmation round end-to-end: the provisional
// partition is shown in a persisted randomized order, ONE explorer's typed wrong_merge splits the merge, and the
// split is a NEW append-only ledger revision while the provisional revision is retained.
func TestRunSpec_Confirmation_WrongMergeSplitsAsNewRevision(t *testing.T) {
	// The canonicalizer merges EVERYTHING (so there is a real conflation to challenge); explorer A challenges it.
	reg, plan := panelOf(t, []fake.Scenario{fake.ChallengeWrongMerge, fake.Valid}, fake.CanonMergeAll)
	res, err := runSpec(t, reg, plan, governedSpec(false, true, 1, nil), Options{})
	if err != nil {
		t.Fatalf("confirmed governed run: %v", err)
	}
	if res.Confirmation == nil || res.Provisional == nil {
		t.Fatal("a confirmed run must record BOTH the confirmation and the superseded provisional revision")
	}
	conf := res.Confirmation
	// Exactly one explorer challenged, and one flag was enough.
	if len(conf.Challenges) != 1 || conf.Challenges[0].Type != canon.ChallengeWrongMerge {
		t.Fatalf("expected exactly 1 typed wrong_merge challenge, got %+v", conf.Challenges)
	}
	if len(conf.Resolutions) != 1 || conf.Resolutions[0].Action != canon.ActionSplit {
		t.Fatalf("a single wrong_merge flag must SPLIT the merge: %+v", conf.Resolutions)
	}
	// The HOST rule resolved it (versioned), not the canonicalizer.
	if conf.RuleVersion != canon.HostConfirmationRuleVersion || conf.Resolutions[0].RuleVersion != canon.HostConfirmationRuleVersion {
		t.Errorf("the resolution must persist the versioned HOST rule: %+v", conf.Resolutions[0])
	}
	for _, row := range res.Canonicalization.Ledger.Rows() {
		if row.DecidedByCall != "host:"+canon.HostConfirmationRuleVersion {
			t.Errorf("a revision row's deciding call must be the host rule, got %q", row.DecidedByCall)
		}
	}
	// A NEW revision, chained to the retained provisional one.
	if res.Provisional.PartitionRevisionHash == res.Canonicalization.PartitionRevisionHash {
		t.Error("the confirmed revision must have a NEW partition revision hash")
	}
	if conf.PriorRevisionHash != res.Provisional.PartitionRevisionHash {
		t.Errorf("the confirmation must name the revision it supersedes: %q vs %q", conf.PriorRevisionHash, res.Provisional.PartitionRevisionHash)
	}
	if res.Canonicalization.Ledger.Revision() != 2 {
		t.Errorf("the confirmed ledger must be revision 2, got %d", res.Canonicalization.Ledger.Revision())
	}
	if got := len(res.Provisional.Clusters); got != 1 {
		t.Errorf("the provisional (merge-everything) revision must be retained intact: %d cluster(s)", got)
	}
	// The split produced one entity per distinct raw nomination.
	if got := len(res.Canonicalization.Clusters); got != 3 {
		t.Errorf("the split must yield one entity per distinct raw nomination, got %d: %+v", got, res.Canonicalization.Clusters)
	}
	// The presented order is PERSISTED and covers the provisional entities.
	if len(conf.Presentation.Order) != len(res.Provisional.Clusters) || conf.Presentation.Seed == "" {
		t.Errorf("the presentation order + seed must be persisted: %+v", conf.Presentation)
	}
	// The panel actually saw the ledger WITH attribution, in that order.
	for _, want := range []string{"CONFIRMATION ROUND", "sourceExplorer", "wrong_merge", "RANDOMIZED order"} {
		if !strings.Contains(res.ConfirmationPrompt, want) {
			t.Errorf("the confirmation prompt shown to the panel is missing %q", want)
		}
	}
	// Surjectivity still holds over the new revision.
	if res.Canonicalization.Ledger.Len() != 4 {
		t.Errorf("expected 4 rows in the confirmed revision, got %d", res.Canonicalization.Ledger.Len())
	}
}

// TestRunSpec_Confirmation_NoChallenges_SettledAndCorroborated: with no challenge and no dual disagreement the
// partition is SETTLED, so the definitive label is permitted — the control case that proves withholding is
// caused by contest, not by the machinery always withholding.
func TestRunSpec_Confirmation_NoChallenges_SettledAndCorroborated(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := runSpec(t, reg, plan, governedSpec(true, true, 1, nil), Options{})
	if err != nil {
		t.Fatalf("governed run: %v", err)
	}
	if res.Confirmation == nil || !res.Confirmation.Settled() {
		t.Fatalf("an unchallenged, fully-agreed partition must be SETTLED: %+v", res.Confirmation)
	}
	if res.Governance == nil {
		t.Fatal("a ranking-grade run must emit governance claims")
	}
	corroborated, single := 0, 0
	for _, c := range res.Governance.Claims {
		if c.Sensitivity != nil {
			t.Errorf("a settled partition must not produce a sensitivity range: %+v", c)
		}
		switch c.Label {
		case govern.LabelCorroborated:
			corroborated++
		case govern.LabelSingleSource:
			single++
		default:
			t.Errorf("unexpected label on a settled partition: %+v", c)
		}
	}
	if corroborated != 1 || single != 2 {
		t.Errorf("expected 1 corroborated (the agreed merge) + 2 single-source claims, got %d/%d", corroborated, single)
	}
}

// --- 4. Anti-echo: counts stay over the immutable blind round 1 (design §0 F-A) ---

// TestRunSpec_AntiEcho_LaterRoundNeverRaisesCounts is the anti-echo invariant end-to-end: round 2 is shown the
// pooled canonical set and ECHOES it back, and no count moves — because independence counts are computed only
// over the immutable blind round-1 artifacts, and a later round cannot become a counting baseline at all.
func TestRunSpec_AntiEcho_LaterRoundNeverRaisesCounts(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	single, err := runSpec(t, reg, plan, governedSpec(false, true, 1, nil), Options{})
	if err != nil {
		t.Fatalf("1-round run: %v", err)
	}
	multi, err := runSpec(t, reg, plan, governedSpec(false, true, 2, exampleLaterRound{}), Options{})
	if err != nil {
		t.Fatalf("2-round run: %v", err)
	}
	// The round-2 explorers really did echo the canonical set back (otherwise this test proves nothing).
	echoed := multi.Rounds[1].Envelopes()
	if len(echoed) == 0 {
		t.Fatal("no round-2 envelopes recorded")
	}
	cands, _ := echoed[0].Response["candidates"].([]any)
	if len(cands) == 0 {
		t.Fatal("the round-2 fake must echo the pooled candidates back for this test to be meaningful")
	}
	// Counts are IDENTICAL to the single-round run: the echo added nothing.
	bySubject := map[string]int{}
	for _, c := range single.Governance.Claims {
		bySubject[c.Subject] = c.Value
	}
	if len(multi.Governance.Claims) != len(single.Governance.Claims) {
		t.Fatalf("a later round must not change the claim SET: %d vs %d", len(multi.Governance.Claims), len(single.Governance.Claims))
	}
	for _, c := range multi.Governance.Claims {
		want, ok := bySubject[c.Subject]
		if !ok {
			t.Errorf("a later round must not introduce a new counted subject: %q", c.Subject)
			continue
		}
		if c.Value != want {
			t.Errorf("ANTI-ECHO VIOLATION: %q counted %d in the 2-round run vs %d in the 1-round run", c.Subject, c.Value, want)
		}
		if c.BaselineRoundID != "round-1" {
			t.Errorf("a count must be pinned to the blind round-1 baseline, got %q", c.BaselineRoundID)
		}
		for _, ref := range c.ContributingSourceIDs {
			if strings.HasPrefix(ref, "r2:") {
				t.Errorf("a later-round envelope must never be a contributing source: %v", c.ContributingSourceIDs)
			}
		}
	}
	// And the type system refuses to make a later round a baseline at all.
	if _, err := govern.NewBlindBaseline(multi.Rounds[1]); err == nil {
		t.Error("a later round must never be usable as an independence-count baseline")
	}
}

// --- 5. Robustness invariants (design §1) ---

// TestRun_Preflight_UnreachableRoleHaltsBeforeFanout: a collator that cannot be INVOKED at all is caught
// by the pre-flight probe, before any explorer token is spent. That is what pre-flight is for — spending
// a whole panel on a run whose collator was never going to answer wastes real money.
func TestRun_Preflight_UnreachableRoleHaltsBeforeFanout(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	cA, cB := counted(reg["explorer-A"]), counted(reg["explorer-B"])
	reg["explorer-A"], reg["explorer-B"] = cA, cB
	reg["collator"] = fake.New("collator", "C", fake.InvokeError)

	res, err := Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}}, Options{}, nil)
	if err == nil {
		t.Fatal("an uninvokable collator must halt")
	}
	if !strings.Contains(err.Error(), "pre-flight") || !strings.Contains(err.Error(), "no explorer tokens were spent") {
		t.Errorf("the halt must name the pre-flight stage, got: %v", err)
	}
	for name, c := range map[string]*countingAdapter{"A": cA, "B": cB} {
		if got := c.count(schema.PhaseExplore); got != 0 {
			t.Errorf("explorer %s was called %d time(s) — the pre-flight halt must precede the fan-out", name, got)
		}
	}
	if len(res.Preflight) == 0 || res.Preflight[0].Role != "collator" {
		t.Errorf("the pre-flight verdict must be recorded: %+v", res.Preflight)
	}
	if res.Envelopes != nil {
		t.Error("no envelopes should exist after a pre-flight halt")
	}
}

// TestRun_Preflight_SwappedIdentityIsRecordedNotHalted: a collator that silently resolved to a DIFFERENT
// model is detected at pre-flight and recorded there — and the run proceeds to a full synthesis. The
// probe's job is to prove the role is reachable and to capture whatever it says about itself; it is not
// a gate on what that answer turns out to be.
func TestRun_Preflight_SwappedIdentityIsRecordedNotHalted(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	reg["collator"] = swappedIdentityAdapter{fake.New("collator", "C", fake.Valid)}

	res, err := Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}}, Options{}, nil)
	if err != nil {
		t.Fatalf("a swapped collator identity must not halt: %v", err)
	}
	if res.Output == nil {
		t.Error("the run must still produce a synthesis")
	}
	if len(res.Preflight) == 0 || res.Preflight[0].Role != "collator" {
		t.Fatalf("the pre-flight verdict must be recorded: %+v", res.Preflight)
	}
	if got := res.Preflight[0].Status; got != schema.IdentityMismatch {
		t.Errorf("pre-flight status = %q, want mismatch RECORDED", got)
	}
	if res.Preflight[0].Caveat == "" {
		t.Error("the recorded pre-flight verdict must carry a caveat a reader will see")
	}
}

// TestRun_MidRunIdentityChange_Halts pins the same-identity invariant (§1): a role that passes pre-flight and then
// resolves to a DIFFERENT model mid-exploration halts — artifacts from two different models cannot be honestly
// combined into one result.
func TestRun_MidRunIdentityChange_Halts(t *testing.T) {
	reg, _ := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	// The collator is requested by ALIAS through the claude-code adapter, whose alias matching accepts any
	// concrete opus version — so the swap passes per-call verification and only the invariant catches it.
	reg["claude-code"] = &driftingAdapter{inner: fake.New("claude-code", "C", fake.Valid)}
	plan, perr := roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "explorer-A", Model: "model-A", Effort: "high"},
			{Adapter: "explorer-B", Model: "model-B", Effort: "high"},
		},
		Collator: roster.Collator{Adapter: "claude-code", Model: "opus"},
	}.Plan()
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	res, err := Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}}, Options{}, nil)
	if err == nil {
		t.Fatal("a mid-exploration model swap must halt")
	}
	if res.CollatorStatus == schema.IdentityMismatch {
		t.Fatal("this test must exercise the same-identity invariant, not per-call mismatch detection")
	}
	if !strings.Contains(err.Error(), "same-identity invariant") {
		t.Errorf("the halt must name the same-identity invariant, got: %v", err)
	}
	// The blind artifacts were already paid for, so the degraded terminal artifact is emitted on an identity halt.
	if res.Degraded == nil || res.Degraded.Reason != schema.DegradedIdentityHalt {
		t.Errorf("an identity halt must still emit the degraded terminal artifact: %+v", res.Degraded)
	}
}

// TestRun_DualDenominators_WithAbstainingExplorer pins §1's dual denominators end-to-end: an explorer that
// DELIBERATELY abstains leaves the PANEL denominator at 3 while the RESPONDENTS denominator drops to 2, with the
// absence categories tallied distinctly.
func TestRun_DualDenominators_WithAbstainingExplorer(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Abstain}, fake.Valid)
	res, err := runSpec(t, reg, plan, governedSpec(false, true, 1, nil), Options{})
	if err != nil {
		t.Fatalf("governed run with an abstention: %v", err)
	}
	if res.Panel.Selected != 3 {
		t.Errorf("the FROZEN panel denominator must stay 3, got %d", res.Panel.Selected)
	}
	if res.Panel.Outcome.DeliberateAbstention != 1 {
		t.Errorf("a deliberate abstention must be tallied as such: %+v", res.Panel.Outcome)
	}
	if res.Panel.Outcome.TechnicalAbsence != 0 {
		t.Errorf("an abstention is NOT a technical absence: %+v", res.Panel.Outcome)
	}
	if res.Panel.Respondents() != 2 {
		t.Errorf("the RESPONDENTS denominator must exclude the abstainer, got %d", res.Panel.Respondents())
	}
	if res.Panel.PolicyHash == "" {
		t.Error("the counting policy must be frozen + hashed before any judgment")
	}
	for _, c := range res.Governance.Claims {
		if c.KOfPanel.M != 3 || c.KOfRespondents.M != 2 {
			t.Errorf("every claim must carry BOTH denominators (3 panel / 2 respondents), got %+v / %+v", c.KOfPanel, c.KOfRespondents)
		}
		if c.PolicyHash != res.Panel.PolicyHash {
			t.Errorf("a claim must pin the frozen policy hash: %q vs %q", c.PolicyHash, res.Panel.PolicyHash)
		}
	}
	// The abstainer contributed no nominations (it declined), so no canonical entity is backed by it.
	for _, cl := range res.Canonicalization.Clusters {
		for _, m := range cl.Members {
			if m.SourceExplorer.Model == "model-C" {
				t.Errorf("an abstaining explorer must contribute no nominations: %+v", m)
			}
		}
	}
}

// TestRun_DegradedTerminalArtifact_EmergentSpaceIsRawAndLabeled pins §1's per-mode-class degraded artifact for an
// EMERGENT-space mode: the raw attributed blind round-1 envelopes plus a MECHANICAL typed-claim index, labeled
// `uncollated — no entity resolution performed`. It must NOT be a synthesized register (that would itself be
// covert entity resolution by the host).
func TestRun_DegradedTerminalArtifact_EmergentSpaceIsRawAndLabeled(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	reg["collator"] = failAfterPreflight{fake.New("collator", "C", fake.Valid)}
	res, err := Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}}, Options{}, nil)
	if err == nil {
		t.Fatal("a collator that dies after the fan-out must halt")
	}
	if res.Output != nil {
		t.Error("no collation exists on this path")
	}
	d := res.Degraded
	if d == nil {
		t.Fatal("the degraded terminal artifact must be emitted when the collator is lost after the fan-out")
	}
	if d.Class != schema.EmergentSpace || d.Reason != schema.DegradedCollatorUnavailable {
		t.Errorf("degraded class/reason wrong: %+v", d)
	}
	if d.Label != schema.UncollatedLabel {
		t.Errorf("an emergent-space degraded artifact must carry the exact uncollated label, got %q", d.Label)
	}
	if len(d.Envelopes) != 2 {
		t.Errorf("the RAW attributed blind envelopes must be carried: %d", len(d.Envelopes))
	}
	for _, e := range d.Envelopes {
		if e.Identity.Model == "" {
			t.Error("degraded envelopes must stay ATTRIBUTED")
		}
	}
	if len(d.ClaimIndex) == 0 {
		t.Fatal("an emergent-space degraded artifact must carry the mechanical typed-claim index")
	}
	if len(d.Register) != 0 {
		t.Error("an emergent-space degraded artifact must NOT contain a host register (that would be covert entity resolution)")
	}
	// The index is mechanical: every entry is attributed to one envelope + field, and NOTHING is grouped.
	for _, tc := range d.ClaimIndex {
		if tc.EnvelopeRef == "" || tc.Field == "" || tc.Explorer.Model == "" {
			t.Errorf("claim-index entry must be fully attributed: %+v", tc)
		}
	}
	if !strings.Contains(d.Summary(), schema.UncollatedLabel) {
		t.Errorf("the degraded summary must state the label so no log line can mistake it for a collation: %q", d.Summary())
	}
}

// TestRunSpec_DegradedTerminalArtifact_FixedSpaceIsRealRegister pins the OTHER half of the per-mode-class rule: a
// FIXED-space mode (its key universe is GIVEN to the explorers) gets a REAL host register — grouping by an exact
// value in a declared universe is arithmetic, not judgment.
func TestRunSpec_DegradedTerminalArtifact_FixedSpaceIsRealRegister(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	reg["collator"] = failAfterPreflight{fake.New("collator", "C", fake.Valid)}
	cat, _ := mode.Lookup(mode.Catalog)
	mapSpec, _ := mode.Lookup(mode.Map)
	// An example FIXED-space mode: the explorers answer over a declared key field ("candidates" here), and the
	// terminal collation is a plain collate — so losing the collator triggers the degraded path.
	fixed := mode.ModeSpec{
		Name: "example-fixed", FormulationFree: true,
		Prompt: cat.Prompt, ExplorerSchema: cat.ExplorerSchema,
		Objective: mapSpec.Objective, Collator: mapSpec.Collator,
		Class: schema.FixedSpace, FixedSpaceKeyField: "candidates",
	}
	res, err := runSpec(t, reg, plan, fixed, Options{})
	if err == nil {
		t.Fatal("a collator that dies after the fan-out must halt")
	}
	d := res.Degraded
	if d == nil || d.Class != schema.FixedSpace {
		t.Fatalf("a fixed-space mode must emit a fixed-space degraded artifact: %+v", d)
	}
	if len(d.Register) == 0 {
		t.Fatal("a fixed-space degraded artifact must be a REAL host register")
	}
	if len(d.ClaimIndex) != 0 || d.Label == schema.UncollatedLabel {
		t.Error("a fixed-space register is a genuine comparison — it is not the uncollated emergent artifact")
	}
	// The register is keyed on the DECLARED field's exact values, with attributed positions per key.
	found := false
	for _, e := range d.Register {
		if e.Key == "Postgres" && len(e.Positions) == 2 {
			found = true
		}
		for _, p := range e.Positions {
			if p.Explorer.Model == "" || p.EnvelopeRef == "" {
				t.Errorf("register position must be attributed: %+v", p)
			}
		}
	}
	if !found {
		t.Errorf("the register must group both explorers under the shared declared key: %+v", d.Register)
	}
}

// --- 6. Regression: the registered modes are unaffected by the governance machinery ---

// TestRun_Catalog_GovernanceOptOutUnchanged pins that Catalog does NOT silently acquire the ranking-grade layers:
// one canonicalizer, no confirmation round, no revision, no governance claims, one round.
func TestRun_Catalog_GovernanceOptOutUnchanged(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: mode.Catalog},
		Options{}, nil)
	if err != nil {
		t.Fatalf("catalog run: %v", err)
	}
	if _, ok := res.Output.(schema.CatalogOutput); !ok {
		t.Fatalf("catalog must still produce a CatalogOutput, got %T", res.Output)
	}
	if res.Confirmation != nil || res.Provisional != nil {
		t.Error("catalog (observe posture) must run NO confirmation round")
	}
	if res.Governance != nil {
		t.Error("catalog emits no counts, so it must emit no governance claims")
	}
	if len(res.CanonicalizerCalls) != 1 || res.CanonicalizerCalls[0].Role != "canonicalizer" {
		t.Errorf("catalog must use exactly ONE canonicalizer: %+v", res.CanonicalizerCalls)
	}
	if res.Canonicalization.AgreementRuleVersion != "" {
		t.Errorf("catalog's partition IS its single proposal — no agreement rule applies, got %q", res.Canonicalization.AgreementRuleVersion)
	}
	if res.Canonicalization.Ledger.Revision() != 1 || res.Canonicalization.Ledger.PriorRevisionHash() != "" {
		t.Error("catalog's ledger must stay the first revision")
	}
	for _, row := range res.Canonicalization.Ledger.Rows() {
		if len(row.AgreedBy) != 0 || row.Revision != 0 {
			t.Errorf("a single-canonicalizer row must carry NO agreedBy/revision: %+v", row)
		}
		if row.DecidedByCall != "canonicalizer:canonicalize" || row.DecidedByIdentity.Model == "" {
			t.Errorf("a single-canonicalizer row must still name its deciding call + identity: %+v", row)
		}
	}
	if len(res.Rounds) != 1 || !res.Rounds[0].Blind() {
		t.Errorf("catalog is a single blind round: %d round(s)", len(res.Rounds))
	}
	if len(res.Mediations) != 0 || len(res.LaterRoundPrompts) != 0 {
		t.Error("catalog has no mediated rounds")
	}
}

// TestRun_MapAndSynthesize_UnaffectedByGovernance pins that the plain-collate modes produce exactly their own
// terminal output and acquire none of the governance machinery.
func TestRun_MapAndSynthesize_UnaffectedByGovernance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  string
		check func(*testing.T, Result)
	}{
		{"map", mode.Map, func(t *testing.T, res Result) {
			out, ok := res.Output.(schema.CollatorOutput)
			if !ok {
				t.Fatalf("map must produce a CollatorOutput, got %T", res.Output)
			}
			if out.SynthesisSummary == "" || len(out.DisagreementRegister) == 0 {
				t.Errorf("map output regressed: %+v", out)
			}
		}},
		{"synthesize", mode.Synthesize, func(t *testing.T, res Result) {
			out, ok := res.Output.(schema.SynthesizeOutput)
			if !ok {
				t.Fatalf("synthesize must produce a SynthesizeOutput, got %T", res.Output)
			}
			if err := out.Validate(); err != nil {
				t.Errorf("synthesize output invalid: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
			res, err := Run(context.Background(), reg, plan,
				schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: tc.mode},
				Options{}, nil)
			if err != nil {
				t.Fatalf("%s run: %v", tc.name, err)
			}
			tc.check(t, res)
			if res.Canonicalization != nil || res.Confirmation != nil || res.Governance != nil {
				t.Error("a plain-collate mode acquires no canonicalization/confirmation/governance artifacts")
			}
			if len(res.CanonicalizerCalls) != 0 {
				t.Errorf("a plain-collate mode makes no canonicalizer call: %+v", res.CanonicalizerCalls)
			}
			if len(res.Rounds) != 1 || !res.Rounds[0].Blind() {
				t.Errorf("a plain-collate mode is a single blind round: %d", len(res.Rounds))
			}
			if res.Degraded != nil {
				t.Error("a successful run emits no degraded artifact")
			}
			// The frozen panel is recorded for every run (it is free and it is the counting substrate).
			if res.Panel.Selected != 2 || res.Panel.PolicyHash == "" {
				t.Errorf("the frozen panel must be recorded: %+v", res.Panel)
			}
		})
	}
}
