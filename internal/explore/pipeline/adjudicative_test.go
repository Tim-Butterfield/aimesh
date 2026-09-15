package pipeline

// End-to-end tests for the adjudicative modes (Challenge and Shortlist) and the ai-collab composition. They
// use deterministic in-process fakes, and each test checks one invariant.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/model/fake"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// theArtifact is the thing under review in every Challenge test — short, but long enough that the
// no-raw-peer-output guard has real bytes to compare against.
const theArtifact = "func Shutdown() { close(ch); drain(); free(state) } // no lock, two callers"

// adjudicativeTask builds the task an adjudicative run takes, with the supplied artifact.
func adjudicativeTask(modeName, artifact string) schema.RawTask {
	return schema.RawTask{
		Purpose: "review the shutdown path", Criteria: []string{"must be correct", "must be safe under concurrency"},
		Mode: modeName, Artifact: artifact,
	}
}

// mustSpec resolves a registered mode or fails the test.
func mustSpec(t *testing.T, name string) mode.ModeSpec {
	t.Helper()
	spec, ok := mode.Lookup(name)
	if !ok {
		t.Fatalf("mode %q is not registered", name)
	}
	return spec
}

// eventLog collects the pipeline's progress events in order, so a test can check that one stage preceded
// another. The pipeline writes it from several goroutines.
type eventLog struct {
	mu     sync.Mutex
	events []audit.EventLine
}

func (l *eventLog) hook() func(audit.EventLine) {
	return func(ev audit.EventLine) {
		l.mu.Lock()
		l.events = append(l.events, ev)
		l.mu.Unlock()
	}
}

// firstIndex returns the position of the first event of the given type (-1 when absent).
func (l *eventLog) firstIndex(eventType string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, ev := range l.events {
		if ev.EventType == eventType {
			return i
		}
	}
	return -1
}

// firstIndexWhere returns the position of the first event of the given type whose data satisfies pred.
func (l *eventLog) firstIndexWhere(eventType string, pred func(map[string]any) bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, ev := range l.events {
		if ev.EventType == eventType && pred(ev.Data) {
			return i
		}
	}
	return -1
}

// runAdjudicative runs a registered adjudicative mode over the given panel.
func runAdjudicative(t *testing.T, reg Registry, plan roster.Plan, name, artifact string, opts Options, onEvent func(audit.EventLine)) (Result, error) {
	t.Helper()
	return RunSpec(context.Background(), reg, plan, adjudicativeTask(name, artifact),
		mustSpec(t, name), opts, onEvent)
}

// entryFor returns the register entry whose statement matches, or fails.
func entryFor(t *testing.T, out mode.ChallengeOutput, statement string) mode.ChallengeEntry {
	t.Helper()
	for _, e := range out.Register {
		if e.Statement == statement {
			return e
		}
	}
	t.Fatalf("no register entry for %q; register = %+v", statement, out.Register)
	return mode.ChallengeEntry{}
}

// --- 1. Challenge ---

// A challenge run produces a severity-ordered register: a finding raised by 2 of 3 reviewers is corroborated
// with both denominators, a lone finding survives as single_source, severity is the maximum over the blind
// sources, and round 2 adds depth without affecting any count.
func TestChallenge_SeverityRegisterWithHostComputedCorroboration(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	res, err := runAdjudicative(t, reg, plan, mode.Challenge, theArtifact, Options{}, nil)
	if err != nil {
		t.Fatalf("challenge run: %v", err)
	}
	// Two fixed rounds: a blind attack, then the mediated cross-review.
	if len(res.Rounds) != 2 || !res.Rounds[0].Blind() || res.Rounds[1].Blind() {
		t.Fatalf("challenge is 2 rounds, blind then mediated: %d round(s)", len(res.Rounds))
	}
	out, ok := res.Output.(mode.ChallengeOutput)
	if !ok {
		t.Fatalf("challenge must produce a ChallengeOutput, got %T", res.Output)
	}
	if out.ArtifactDigest == "" || out.PartitionRevisionHash == "" {
		t.Errorf("a register must pin the artifact + the partition it was computed at: %+v", out)
	}

	// The finding two reviewers raised independently is corroborated, with both denominators over 3.
	shared := entryFor(t, out, "Race condition on shutdown")
	if shared.Corroboration.Label != govern.LabelCorroborated {
		t.Errorf("a finding raised by 2 distinct blind sources must be corroborated, got %q", shared.Corroboration.Label)
	}
	if shared.Corroboration.Value != 2 {
		t.Errorf("corroboration count = %d, want 2", shared.Corroboration.Value)
	}
	if shared.Corroboration.KOfPanel.K != 2 || shared.Corroboration.KOfPanel.M != 3 {
		t.Errorf("panel denominator = %s, want 2 of 3", shared.Corroboration.KOfPanel)
	}
	if shared.Corroboration.KOfRespondents.K != 2 || shared.Corroboration.KOfRespondents.M != 3 {
		t.Errorf("respondents denominator = %s, want 2 of 3", shared.Corroboration.KOfRespondents)
	}
	if shared.Corroboration.Query != govern.CorroborationQuery || shared.Corroboration.BaselineRoundID != "round-1" {
		t.Errorf("a corroboration count must be pinned to the blind round-1 baseline: %+v", shared.Corroboration)
	}
	for _, pin := range []string{shared.Corroboration.FormulationHash, shared.Corroboration.PartitionRevisionHash,
		shared.Corroboration.RulesVersion, shared.Corroboration.PolicyHash} {
		if pin == "" {
			t.Errorf("every count pins {formulationHash, partitionRevisionHash, rulesVersion, policyHash, baselineRoundID}: %+v", shared.Corroboration)
		}
	}
	// Host triage takes the highest severity any blind source assigned (A said high, B critical).
	if shared.Severity != schema.SeverityCritical {
		t.Errorf("host severity triage must take the MAX blind severity, got %q", shared.Severity)
	}
	if len(shared.Findings) != 2 {
		t.Fatalf("the entry must carry both reviewers' attributed findings: %+v", shared.Findings)
	}
	for _, f := range shared.Findings {
		if f.Explorer.Model == "" || f.EnvelopeRef == "" || f.FailureScenario == "" {
			t.Errorf("every carried finding stays attributed with its own words: %+v", f)
		}
	}

	// A finding only one reviewer raised is kept and labeled single_source.
	lone := entryFor(t, out, "No input validation on the config path")
	if lone.Corroboration.Label != govern.LabelSingleSource || !lone.SingleSource {
		t.Errorf("a lone finding must survive as single_source, got %q (singleSource=%v)", lone.Corroboration.Label, lone.SingleSource)
	}
	if lone.Corroboration.Value != 1 {
		t.Errorf("a lone finding counts 1, got %d", lone.Corroboration.Value)
	}
	if !strings.Contains(lone.Corroboration.Rendering(), "SINGLE SOURCE") {
		t.Errorf("a single-source rendering must say so: %q", lone.Corroboration.Rendering())
	}
	// Every blind finding survived into the register (6 nominations → 5 entities: one merge).
	if len(out.Register) != 5 {
		t.Errorf("expected 5 register entries (one merged + 4 singletons), got %d: %+v", len(out.Register), out.Register)
	}

	// Most severe first.
	for i := 1; i < len(out.Register); i++ {
		if out.Register[i-1].Severity.Rank() < out.Register[i].Severity.Rank() {
			t.Errorf("the register must be severity-triaged: %q(%s) precedes %q(%s)",
				out.Register[i-1].Statement, out.Register[i-1].Severity, out.Register[i].Statement, out.Register[i].Severity)
		}
	}

	// Round 2 ran and added depth, and its prompt carried the pooled canonical digest as untrusted data rather
	// than raw peer output.
	if len(shared.Deepening) == 0 {
		t.Error("the mediated cross-review must attach attributed depth to the register")
	}
	if len(res.LaterRoundPrompts) != 1 {
		t.Fatalf("expected 1 later-round prompt, got %d", len(res.LaterRoundPrompts))
	}
	p := res.LaterRoundPrompts[0]
	for _, want := range []string{"CROSS-REVIEW round", "BEGIN UNTRUSTED DATA", "DATA ONLY", "Do NOT count anything"} {
		if !strings.Contains(p, want) {
			t.Errorf("the cross-review prompt is missing %q", want)
		}
	}
	for _, env := range res.Rounds[0].Envelopes() {
		body := strings.TrimSpace(string(env.RawResponse))
		if len(body) >= 24 && strings.Contains(p, body[:24]) {
			t.Error("the cross-review prompt contains a RAW peer response — explorers must never see raw peer output")
		}
	}
	// The governance record is the register's substrate: claims pinned, panel frozen, partition confirmed.
	if res.Governance == nil || len(res.Governance.Claims) != len(out.Register) {
		t.Errorf("every register entry must correspond to an emitted claim: %+v", res.Governance)
	}
	if res.Confirmation == nil || res.Provisional == nil {
		t.Error("a count-bearing mode must run the binding confirmation round and retain the provisional revision")
	}
	if res.Canonicalization.AgreementRuleVersion == "" || len(res.CanonicalizerCalls) != 2 {
		t.Errorf("a count-bearing mode must use the DUAL canonicalizer: %d call(s), rule %q",
			len(res.CanonicalizerCalls), res.Canonicalization.AgreementRuleVersion)
	}
}

// Cross-reviewers who echo every pooled finding back move no count, because corroboration uses only the blind
// round-1 artifacts.
func TestChallenge_AntiEcho_CrossReviewNeverInflatesACount(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)

	// The control: the same contract without the cross-review round.
	single := mustSpec(t, mode.Challenge)
	single.Rounds, single.LaterRound = 1, nil
	one, err := RunSpec(context.Background(), reg, plan, adjudicativeTask(mode.Challenge, theArtifact),
		single, Options{}, nil)
	if err != nil {
		t.Fatalf("1-round challenge: %v", err)
	}
	two, err := runAdjudicative(t, reg, plan, mode.Challenge, theArtifact, Options{}, nil)
	if err != nil {
		t.Fatalf("2-round challenge: %v", err)
	}

	// The round-2 reviewers really did echo the pooled findings back (otherwise this test proves nothing).
	echoed := two.Rounds[1].Envelopes()
	if len(echoed) == 0 {
		t.Fatal("no round-2 envelopes recorded")
	}
	if len(schema.ParseAssessments(echoed[0].Response)) == 0 {
		t.Fatal("the round-2 fake must echo the pooled findings back for this test to be meaningful")
	}

	want := map[string]int{}
	for _, c := range one.Governance.Claims {
		want[c.Subject] = c.Value
	}
	if len(two.Governance.Claims) != len(one.Governance.Claims) {
		t.Fatalf("a cross-review round must not change the claim SET: %d vs %d", len(two.Governance.Claims), len(one.Governance.Claims))
	}
	for _, c := range two.Governance.Claims {
		got, ok := want[c.Subject]
		if !ok {
			t.Errorf("a cross-review round must not introduce a new counted subject: %q", c.Subject)
			continue
		}
		if c.Value != got {
			t.Errorf("ANTI-ECHO VIOLATION: %q counted %d with a cross-review round vs %d without", c.Subject, c.Value, got)
		}
		if c.BaselineRoundID != "round-1" {
			t.Errorf("a corroboration count must be pinned to the blind round-1 baseline, got %q", c.BaselineRoundID)
		}
		for _, ref := range c.ContributingSourceIDs {
			if strings.HasPrefix(ref, "r2:") {
				t.Errorf("a cross-review envelope must never be a contributing source: %v", c.ContributingSourceIDs)
			}
		}
	}
	// And the type system refuses to make the cross-review round a baseline at all.
	if _, err := govern.NewBlindBaseline(two.Rounds[1]); err == nil {
		t.Error("a cross-review round must never be usable as an independence-count baseline")
	}
}

// A merge only one canonicalizer proposed leaves the mapping contested, so every register entry carries a
// sensitivity range with `corroborated` withheld, and the rendering says so.
func TestChallenge_ContestedMapping_WithholdsCorroboratedAndEmitsRange(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	reg["merger"] = fake.New("merger", "M", fake.CanonMergeAll) // the aggressive merger the dual rule neutralizes
	opts := Options{Canonicalizers: []roster.Explorer{
		{Adapter: "collator", Model: "collator-model"},
		{Adapter: "merger", Model: "merger-model"},
	}}
	res, err := runAdjudicative(t, reg, plan, mode.Challenge, theArtifact, opts, nil)
	if err != nil {
		t.Fatalf("contested challenge run: %v", err)
	}
	if res.Confirmation == nil || res.Confirmation.Settled() {
		t.Fatal("a refused dual merge must leave the run un-settled")
	}
	out, ok := res.Output.(mode.ChallengeOutput)
	if !ok {
		t.Fatalf("expected a ChallengeOutput, got %T", res.Output)
	}
	for _, e := range out.Register {
		if e.Corroboration.Label == govern.LabelCorroborated {
			t.Errorf("no entry over a CONTESTED partition may be labeled corroborated: %+v", e.Corroboration)
		}
		if e.Corroboration.Label != govern.LabelWithheldContested || e.Corroboration.Sensitivity == nil {
			t.Fatalf("a contested entry must be WITHHELD with a sensitivity range: %+v", e.Corroboration)
		}
		s := e.Corroboration.Sensitivity
		if s.Low > s.High || len(s.ContestedCanonicalIDs) == 0 {
			t.Errorf("a sensitivity must be a real range naming the contested entities: %+v", s)
		}
		if !strings.Contains(e.Corroboration.Rendering(), "WITHHELD") {
			t.Errorf("a withheld entry's rendering must say so: %q", e.Corroboration.Rendering())
		}
	}
}

// A challenge task without an artifact is refused before the fan-out, and mode.ValidateTask applies the same
// rule for every surface.
func TestChallenge_MissingArtifact_RefusedBeforeAnySpend(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	cA := counted(reg["explorer-A"])
	reg["explorer-A"] = cA

	_, err := runAdjudicative(t, reg, plan, mode.Challenge, "", Options{}, nil)
	if err == nil {
		t.Fatal("a challenge run with no artifact must halt")
	}
	if !strings.Contains(err.Error(), "requires an ARTIFACT") || !strings.Contains(err.Error(), "--artifact") {
		t.Errorf("the halt must name the missing input + how to supply it, got: %v", err)
	}
	if got := cA.count(schema.PhaseExplore); got != 0 {
		t.Errorf("a task-contract halt must precede the fan-out: %d explorer call(s) were made", got)
	}
	// The same check is available to every surface through one call, so they cannot disagree about it.
	if err := mode.ValidateTask(mode.Challenge, adjudicativeTask(mode.Challenge, "")); err == nil {
		t.Error("mode.ValidateTask must reject a challenge task with no artifact")
	}
	if err := mode.ValidateTask(mode.Challenge, adjudicativeTask(mode.Challenge, theArtifact)); err != nil {
		t.Errorf("a challenge task WITH an artifact must pass: %v", err)
	}
}

// --- 2. Shortlist ---

// The host tallies the recorded ballots over the confirmed universe: ranked entries carry claims with both
// denominators, criteria carry origin and aggregation method, rejects carry a host reason, and the result is
// never rendered as consensus.
func TestShortlist_HostTalliedRankingOverConfirmedUniverse(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := runAdjudicative(t, reg, plan, mode.Shortlist, "", Options{}, nil)
	if err != nil {
		t.Fatalf("shortlist run: %v", err)
	}
	out, ok := res.Output.(mode.ShortlistOutput)
	if !ok {
		t.Fatalf("shortlist must produce a ShortlistOutput, got %T", res.Output)
	}
	// The ballot ran over the confirmed universe of 3 candidates, and the host tallied it.
	if res.Decision == nil || res.Decision.RuleVersion != govern.DecisionRuleVersion {
		t.Fatalf("a shortlist run must record the host tally: %+v", res.Decision)
	}
	if got := len(res.Decision.Entries); got != 3 {
		t.Fatalf("expected 3 confirmed candidates on the ballot, got %d", got)
	}
	if res.Decision.Cast != 2 {
		t.Errorf("both panel members cast a ballot, got %d", res.Decision.Cast)
	}
	if len(out.Ranked) != 2 || len(out.Rejects) != 1 {
		t.Fatalf("the frozen cut takes the top half (2 of 3): ranked=%d rejects=%d", len(out.Ranked), len(out.Rejects))
	}
	// A host ranking is strictly ordered by the recorded ballots.
	if out.Ranked[0].Rank != 1 || out.Ranked[0].Score <= out.Ranked[1].Score {
		t.Errorf("the ranking must be host-tallied and ordered: %+v", out.Ranked)
	}
	for _, e := range out.Ranked {
		if e.Claim.Label != govern.LabelRanked {
			t.Errorf("a settled, quorate ballot yields the `ranked` label, got %q", e.Claim.Label)
		}
		if e.Claim.Query != govern.BallotQuery {
			t.Errorf("a ranked entry must carry the BALLOT query, not the salience one: %q", e.Claim.Query)
		}
		if e.Claim.KOfPanel.M != 2 || e.Claim.KOfRespondents.M != 2 {
			t.Errorf("a ranked claim must carry BOTH denominators: %s / %s", e.Claim.KOfPanel, e.Claim.KOfRespondents)
		}
		if e.Claim.PolicyHash == "" || e.Claim.PartitionRevisionHash == "" || e.Claim.RulesVersion == "" {
			t.Errorf("a ranked claim must pin its inputs: %+v", e.Claim)
		}
		if len(e.Provenance) == 0 {
			t.Errorf("a ranked candidate must carry its blind round-1 provenance: %+v", e)
		}
		// Ballot support and blind salience are reported side by side as different measurements.
		if e.EmergentSalience == nil || e.EmergentSalience.Query != govern.CorroborationQuery {
			t.Errorf("a ranked entry must report its blind round-1 salience alongside its ballot support: %+v", e.EmergentSalience)
		}
	}
	// The criteria are frozen with their origin and aggregation method.
	if len(out.Criteria) != 2 {
		t.Fatalf("the user's criteria must be frozen with the decision: %+v", out.Criteria)
	}
	for _, c := range out.Criteria {
		if c.Origin != govern.OriginUser || c.AggregationMethod != govern.AggregationBallot {
			t.Errorf("every criterion carries origin + aggregationMethod: %+v", c)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("frozen criterion invalid: %v", err)
		}
	}
	// Rejects carry the host's reason, so no nominated candidate is dropped silently.
	if out.Rejects[0].Reason == "" || out.Rejects[0].Name == "" {
		t.Errorf("a reject must carry the host's arithmetic reason: %+v", out.Rejects[0])
	}

	// A ballot is an informed preference under shared framing: never consensus, and distinct from emergent
	// salience.
	for _, text := range []string{out.Decision, out.Summary(), res.Decision.Rendering} {
		// "consensus" may appear only as "not consensus".
		if strings.Contains(strings.ToLower(text), "consensus") && !strings.Contains(strings.ToLower(text), "not consensus") {
			t.Errorf("a ballot must never be rendered as consensus: %q", text)
		}
	}
	if !strings.Contains(out.Decision, "INFORMED PREFERENCE UNDER SHARED FRAMING") {
		t.Errorf("the decision rendering must state what a ballot is: %q", out.Decision)
	}
	if !strings.Contains(strings.ToLower(out.Decision), "not `emergent`") {
		t.Errorf("the decision rendering must distinguish voted from emergent: %q", out.Decision)
	}
	// The voters' prose is quarantined into the collatorNarrative namespace, never onto the machine record.
	if len(out.CollatorNarrative) == 0 {
		t.Error("the voters' rationales must be carried in the collatorNarrative namespace")
	}
	for _, b := range res.Decision.Ballots {
		if b.EnvelopeRef == "" || len(b.Ranking) == 0 {
			t.Errorf("every recorded ballot must be attributed to its envelope: %+v", b)
		}
	}
}

// The decision inputs (universe revision, criteria, method, cut, quorum, tie rule and missing-response policy)
// are frozen and hashed before the ballot, so no criterion can be chosen after seeing whom it favors. The event
// order shows the freeze before the ballot dispatch, and the ballot prompt carries the frozen hash.
func TestShortlist_DecisionInputsFrozenBeforeTheBallot(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	log := &eventLog{}
	res, err := runAdjudicative(t, reg, plan, mode.Shortlist, "", Options{}, log.hook())
	if err != nil {
		t.Fatalf("shortlist run: %v", err)
	}
	frozen := res.Decision.Frozen
	if frozen.InputsHash == "" || frozen.InputsHash != frozen.Inputs.Hash() {
		t.Fatalf("the decision inputs must be frozen + hashed: %+v", frozen)
	}

	// (a) Event order: the freeze precedes the ballot dispatch, which precedes the tally.
	froze := log.firstIndex("decision_frozen")
	ballotDispatch := log.firstIndexWhere("explorer_dispatch", func(d map[string]any) bool {
		phase, _ := d["phase"].(string)
		return phase == schema.PhaseBallot
	})
	tallied := log.firstIndex("ballot_tallied")
	if froze < 0 || ballotDispatch < 0 || tallied < 0 {
		t.Fatalf("expected freeze/ballot/tally events, got froze=%d dispatch=%d tallied=%d", froze, ballotDispatch, tallied)
	}
	if froze >= ballotDispatch {
		t.Errorf("the decision inputs must be frozen BEFORE the ballot is solicited (freeze@%d, ballot dispatch@%d)", froze, ballotDispatch)
	}
	if ballotDispatch >= tallied {
		t.Errorf("the tally must follow the ballot (dispatch@%d, tally@%d)", ballotDispatch, tallied)
	}

	// (b) Prompt content: the panel was shown the frozen hash and the framing it voted under.
	if len(res.LaterRoundPrompts) != 1 {
		t.Fatalf("expected 1 ballot prompt, got %d", len(res.LaterRoundPrompts))
	}
	p := res.LaterRoundPrompts[0]
	if !strings.Contains(p, frozen.InputsHash) {
		t.Error("the ballot prompt must carry the frozen inputs hash — that is what proves the framing predates the vote")
	}
	// The assembled prompt must not start with a dash: the shell adapter refuses an option-like prompt for
	// argv-passing recipes. govern tests Render() alone, so this checks the full concatenation.
	if strings.HasPrefix(p, "-") {
		t.Errorf("the assembled ballot prompt must not begin with '-': argv-passing adapters read it as an option. Got: %.80q", p)
	}
	for _, want := range []string{
		"FROZEN DECISION INPUTS", "fixed and hashed BEFORE this ballot was requested",
		"The HOST tallies the ballots", "This is the BALLOT round", "RANDOMIZED order",
		frozen.Inputs.PolicyHash[:12], string(frozen.Inputs.Method),
		"origin: " + string(govern.OriginUser), "aggregation: " + string(govern.AggregationBallot),
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the ballot prompt is missing %q", want)
		}
	}
	// The frozen record names every decision input.
	in := frozen.Inputs
	if in.UniverseRevisionHash != res.Canonicalization.PartitionRevisionHash {
		t.Errorf("the frozen universe must be the CONFIRMED partition revision: %q vs %q", in.UniverseRevisionHash, res.Canonicalization.PartitionRevisionHash)
	}
	if len(in.Presentation.Order) != 3 || in.Presentation.Seed == "" || in.Presentation.RuleVersion == "" {
		t.Errorf("the frozen presented order must be persisted + reproducible: %+v", in.Presentation)
	}
	if in.ShortlistSize != 2 || in.Quorum < 2 || in.TieRule == "" || in.MissingResponse == "" {
		t.Errorf("the frozen inputs must fix the cut, quorum, tie rule and missing-response policy: %+v", in)
	}
	// The frozen record is sealed: any change to the inputs invalidates the hash and the tally refuses it.
	tampered := frozen
	tampered.Inputs.ShortlistSize = 3
	if _, err := govern.Tally(govern.TallyInput{Frozen: tampered, Panel: res.Panel}); err == nil {
		t.Error("a tally must REFUSE inputs that no longer match their frozen hash")
	}
	if _, err := govern.Tally(govern.TallyInput{Panel: res.Panel}); err == nil {
		t.Error("a tally must REFUSE a decision whose inputs were never frozen")
	}
}

// A contested mapping makes the ballot's option set conditional, so ranked candidates carry a sensitivity
// range with `ranked` withheld.
func TestShortlist_ContestedCandidate_WithholdsRankedAndEmitsRange(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	reg["merger"] = fake.New("merger", "M", fake.CanonMergeAll)
	opts := Options{Canonicalizers: []roster.Explorer{
		{Adapter: "collator", Model: "collator-model"},
		{Adapter: "merger", Model: "merger-model"},
	}}
	res, err := runAdjudicative(t, reg, plan, mode.Shortlist, "", opts, nil)
	if err != nil {
		t.Fatalf("contested shortlist run: %v", err)
	}
	if res.Confirmation == nil || res.Confirmation.Settled() {
		t.Fatal("a refused dual merge must leave the run un-settled")
	}
	out, ok := res.Output.(mode.ShortlistOutput)
	if !ok {
		t.Fatalf("expected a ShortlistOutput, got %T", res.Output)
	}
	if len(out.Ranked) == 0 {
		t.Fatal("a contested partition withholds LABELS, it does not delete candidates")
	}
	for _, e := range out.Ranked {
		if e.Claim.Label == govern.LabelRanked {
			t.Errorf("no candidate over a CONTESTED partition may be labeled ranked: %+v", e.Claim)
		}
		if e.Claim.Label != govern.LabelWithheldContested || e.Claim.Sensitivity == nil {
			t.Fatalf("a contested candidate must be WITHHELD with a sensitivity range: %+v", e.Claim)
		}
		if e.Claim.Sensitivity.Low > e.Claim.Sensitivity.High || len(e.Claim.Sensitivity.ContestedCanonicalIDs) == 0 {
			t.Errorf("a sensitivity must be a real range naming the contested entities: %+v", e.Claim.Sensitivity)
		}
		if !strings.Contains(e.Claim.Rendering(), "WITHHELD") {
			t.Errorf("a withheld ranking's rendering must say so: %q", e.Claim.Rendering())
		}
	}
}

// A tie across the shortlist cut applies the frozen tie rule: no winner is picked, every tied candidate is
// carried, and their `ranked` label is withheld.
func TestShortlist_TieAtTheBoundary_WithholdsRankedUnderTheFrozenRule(t *testing.T) {
	// Voter B casts the reverse of voter A's ballot, so every candidate ties.
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.BallotReverse}, fake.Valid)
	res, err := runAdjudicative(t, reg, plan, mode.Shortlist, "", Options{}, nil)
	if err != nil {
		t.Fatalf("tied shortlist run: %v", err)
	}
	out, ok := res.Output.(mode.ShortlistOutput)
	if !ok {
		t.Fatalf("expected a ShortlistOutput, got %T", res.Output)
	}
	if out.TieOutcome == "" {
		t.Fatalf("a tie straddling the cut must be reported: %+v", res.Decision)
	}
	if !strings.Contains(out.TieOutcome, string(res.Decision.Frozen.Inputs.TieRule)) {
		t.Errorf("the tie outcome must name the FROZEN tie rule: %q", out.TieOutcome)
	}
	// Every tied candidate is carried...
	if len(out.Ranked) <= res.Decision.Frozen.Inputs.ShortlistSize {
		t.Errorf("a boundary tie carries every tied candidate: %d ranked for a cut of %d", len(out.Ranked), res.Decision.Frozen.Inputs.ShortlistSize)
	}
	// ...with a withheld label rather than a ranked one.
	withheld := 0
	for _, e := range out.Ranked {
		if e.Claim.Label == govern.LabelWithheldTie {
			withheld++
			if !strings.Contains(e.Claim.Rendering(), "WITHHELD") {
				t.Errorf("a tie-withheld rendering must say so: %q", e.Claim.Rendering())
			}
		}
	}
	if withheld == 0 {
		t.Errorf("the tied candidates must carry withheld_tie: %+v", out.Ranked)
	}
}

// An abstaining voter casts no ballot and the count continues with both denominators; a ballot naming a
// candidate outside the confirmed universe is rejected rather than repaired.
func TestShortlist_UnusableBallotIsRecordedNotInvented(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	// An abstaining explorer returns a schema-valid empty ballot: a recorded position, not a vote.
	reg["explorer-C"] = fake.New("explorer-C", "C", fake.Abstain)
	res, err := runAdjudicative(t, reg, plan, mode.Shortlist, "", Options{}, nil)
	if err != nil {
		t.Fatalf("shortlist run with an abstainer: %v", err)
	}
	if res.Decision.Cast != 2 {
		t.Errorf("an abstaining voter casts no ballot: %d ballot(s) recorded", res.Decision.Cast)
	}
	if res.Panel.Selected != 3 {
		t.Errorf("the FROZEN panel denominator stays 3, got %d", res.Panel.Selected)
	}
	if res.Panel.Outcome.DeliberateAbstention != 1 {
		t.Errorf("a deliberate abstention must be tallied as such: %+v", res.Panel.Outcome)
	}
	for _, e := range res.Decision.Entries {
		if e.Claim == nil {
			continue
		}
		if e.Claim.KOfPanel.M != 3 || e.Claim.KOfRespondents.M != 2 {
			t.Errorf("both denominators must reflect the abstention (3 panel / 2 respondents): %s / %s", e.Claim.KOfPanel, e.Claim.KOfRespondents)
		}
	}
	// A ballot naming a candidate outside the confirmed universe is an error, never a silent repair.
	bad := govern.Ballot{By: schema.ExplorerIdentity{Adapter: "x", Model: "y"}, Ranking: []string{"not-a-candidate"}}
	if err := bad.Validate(map[string]bool{"canon-0": true}); err == nil {
		t.Error("a ballot naming a candidate outside the confirmed universe must be rejected")
	}
}

// --- 3. ai-collab: a multi-round composition ---

// ai-collab runs end to end without a supplied artifact: each agent lists its own findings blind, the agents
// cross-review each other's findings, and one collation produces Challenge's register using Challenge's
// contracts.
func TestAICollab_ComposedMultiRoundExampleRunsEndToEnd(t *testing.T) {
	reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	res, err := RunSpec(context.Background(), reg, plan,
		schema.RawTask{Purpose: "plan the migration", Criteria: []string{"must be reversible"}, Mode: mode.AICollab},
		mustSpec(t, mode.AICollab), Options{}, nil)
	if err != nil {
		t.Fatalf("ai-collab run: %v", err)
	}

	// Stage 1: each agent produced its own findings blind, with no artifact supplied.
	if len(res.Rounds) != 2 || !res.Rounds[0].Blind() {
		t.Fatalf("ai-collab is a blind round then a cross-review round: %d round(s)", len(res.Rounds))
	}
	if len(res.Rounds[0].Envelopes()) != 3 {
		t.Fatalf("all 3 agents must contribute blind findings, got %d", len(res.Rounds[0].Envelopes()))
	}
	for _, env := range res.Rounds[0].Envelopes() {
		if len(schema.ParseFindings(env.Response)) == 0 {
			t.Errorf("agent %s produced no findings: %+v", env.Identity.Model, env.Response)
		}
	}

	// Stage 2: the agents cross-reviewed through the mediated pooled digest, and the panel confirmed the
	// partition.
	if len(res.Mediations) != 1 || res.Mediations[0].Artifact.ContentHash == "" {
		t.Fatalf("the cross-review round must be collator-mediated with a recorded digest: %+v", res.Mediations)
	}
	if len(res.Mediations[0].ShownTo) != len(plan.Explorers) {
		t.Errorf("the digest shown must be recorded per agent: %+v", res.Mediations[0].ShownTo)
	}
	if res.Confirmation == nil || res.Provisional == nil {
		t.Error("the composition is count-bearing, so it must run the binding confirmation round")
	}
	if len(res.CanonicalizerCalls) != 2 {
		t.Errorf("the composition is count-bearing, so it must use the DUAL canonicalizer: %+v", res.CanonicalizerCalls)
	}

	// Stage 3: one collation produces the severity-ordered register with host-computed counts.
	out, ok := res.Output.(mode.ChallengeOutput)
	if !ok {
		t.Fatalf("the composition must produce the same register Challenge does, got %T", res.Output)
	}
	if err := out.Validate(); err != nil {
		t.Errorf("ai-collab register invalid: %v", err)
	}
	if out.ArtifactDigest != "" {
		t.Errorf("ai-collab supplies no artifact, so the register pins none: %q", out.ArtifactDigest)
	}
	shared := entryFor(t, out, "Race condition on shutdown")
	if shared.Corroboration.Label != govern.LabelCorroborated || shared.Corroboration.Value != 2 {
		t.Errorf("a finding two agents independently produced must be corroborated 2-of-3: %+v", shared.Corroboration)
	}
	if len(shared.Deepening) == 0 {
		t.Error("the cross-review stage must attach attributed depth")
	}
	if res.Governance == nil || len(res.Governance.Claims) != len(out.Register) {
		t.Errorf("every register entry must be pinned to an emitted claim: %+v", res.Governance)
	}

	// The composition reuses Challenge's contracts.
	collab, chal := mustSpec(t, mode.AICollab), mustSpec(t, mode.Challenge)
	if collab.Objective != chal.Objective || collab.Canonicalization != chal.Canonicalization || collab.Rounds != chal.Rounds {
		t.Errorf("the composition must reuse Challenge's contract, not fork it: %+v vs %+v", collab, chal)
	}
	if collab.ValidateTask != nil {
		t.Error("ai-collab requires no supplied artifact — that is its one task-level difference from challenge")
	}
}

// --- 4. Regression: the observe-posture modes are unaffected by the adjudicative ones ---

// Map, Synthesize and Catalog keep their own output types with no ballot or mediated round; Catalog keeps its
// single-canonicalizer path with no confirmation or counts.
func TestObserveModes_UnaffectedByAdjudicativeModes(t *testing.T) {
	for _, name := range []string{mode.Map, mode.Synthesize, mode.Catalog} {
		t.Run(name, func(t *testing.T) {
			reg, plan := panelOf(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
			res, err := Run(context.Background(), reg, plan,
				schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: name},
				Options{}, nil)
			if err != nil {
				t.Fatalf("%s run: %v", name, err)
			}
			if res.Decision != nil {
				t.Error("an observe-posture mode acquires no ballot decision")
			}
			if len(res.Rounds) != 1 || !res.Rounds[0].Blind() {
				t.Errorf("an observe-posture mode is a single blind round: %d", len(res.Rounds))
			}
			if len(res.Mediations) != 0 || len(res.LaterRoundPrompts) != 0 {
				t.Error("an observe-posture mode has no mediated round")
			}
			switch name {
			case mode.Map:
				if _, ok := res.Output.(schema.CollatorOutput); !ok {
					t.Fatalf("map must still produce a CollatorOutput, got %T", res.Output)
				}
			case mode.Synthesize:
				if _, ok := res.Output.(schema.SynthesizeOutput); !ok {
					t.Fatalf("synthesize must still produce a SynthesizeOutput, got %T", res.Output)
				}
			case mode.Catalog:
				if _, ok := res.Output.(schema.CatalogOutput); !ok {
					t.Fatalf("catalog must still produce a CatalogOutput, got %T", res.Output)
				}
				// Catalog keeps its single-canonicalizer path without counts.
				if res.Confirmation != nil || res.Provisional != nil || res.Governance != nil {
					t.Error("catalog (observe posture) runs no confirmation round and emits no counts")
				}
				if len(res.CanonicalizerCalls) != 1 || res.Canonicalization.AgreementRuleVersion != "" {
					t.Errorf("catalog's partition IS its single proposal: %+v", res.CanonicalizerCalls)
				}
			}
		})
	}
}
