package run

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
)

// TestConsensusOf_TheThreeVerdicts pins the arithmetic, including the even split. A 2-2 panel is
// contested rather than a majority: half the seats that ran did not report it, and calling that a
// majority would be the flattering reading.
func TestConsensusOf_TheThreeVerdicts(t *testing.T) {
	cases := []struct {
		name                   string
		supporting, dissenting int
		want                   string
	}{
		{"all four agreed", 4, 0, ConsensusUnanimous},
		{"three of four", 3, 1, ConsensusMajority},
		{"an even split is contested", 2, 2, ConsensusContested},
		{"one of five", 1, 4, ConsensusContested},
		{"two seats, one silent", 1, 1, ConsensusContested},
	}
	for _, c := range cases {
		if got := consensusOf(c.supporting, c.dissenting); got != c.want {
			t.Errorf("%s: consensusOf(%d,%d) = %q, want %q", c.name, c.supporting, c.dissenting, got, c.want)
		}
	}
}

// TestConsensusOf_RefusesToLabelWhatItCannotDescribe covers the two silences.
//
// The SINGLE-SEAT case is the one that matters. A lone seat that reported a finding has zero
// dissenters, so the naive arithmetic calls it `unanimous` — dressing the thinnest possible evidence
// in the strongest available word. There is no panel there to have agreed.
func TestConsensusOf_RefusesToLabelWhatItCannotDescribe(t *testing.T) {
	if got := consensusOf(1, 0); got != "" {
		t.Errorf("one seat, nobody to disagree = %q, want no label at all (it is not unanimity)", got)
	}
	if got := consensusOf(0, 3); got != "" {
		t.Errorf("no supporting seats = %q, want no label (not a panel finding)", got)
	}
	if got := consensusOf(0, 0); got != "" {
		t.Errorf("no panel = %q, want no label", got)
	}
}

// TestConsensus_NeverTouchesTheFinding is the invariant this whole file exists to hold, and it is
// asserted the same way the grounding and composition passes assert theirs: run the labeller over a
// set of decisions and prove every field that decides what a caller SEES is byte-identical after.
func TestConsensus_NeverTouchesTheFinding(t *testing.T) {
	adj := adjudication.Result{
		Findings: []review.Finding{
			{ID: "f1", Title: "the lone finding", File: "a.go", Severity: "high"},
			{ID: "f2", Title: "the agreed finding", File: "b.go", Severity: "low"},
		},
		Decisions: []review.Decision{
			{FindingID: "f1", Valid: true, State: review.StateReportedValid, AgreementCount: 1,
				SupportingSeats: []review.SeatRef{{SeatID: "reviewer-1", Model: "m1"}},
				DissentingSeats: []string{"reviewer-2", "reviewer-3"}},
			{FindingID: "f2", Valid: true, State: review.StateApplied, AgreementCount: 3,
				SupportingSeats: []review.SeatRef{{SeatID: "reviewer-1", Model: "m1"}, {SeatID: "reviewer-2", Model: "m2"}, {SeatID: "reviewer-3", Model: "m3"}}},
		},
	}
	before := append([]review.Finding(nil), adj.Findings...)
	states := []review.DecisionState{adj.Decisions[0].State, adj.Decisions[1].State}
	counts := []int{adj.Decisions[0].AgreementCount, adj.Decisions[1].AgreementCount}

	for i := range adj.Decisions {
		adj.Decisions[i].Consensus = consensusOf(len(adj.Decisions[i].SupportingSeats), len(adj.Decisions[i].DissentingSeats))
	}
	sum := DissentSummaryFor(&adj)

	// The labels are what we expected — otherwise the invariant below is vacuous.
	if adj.Decisions[0].Consensus != ConsensusContested {
		t.Fatalf("1 of 3 seats = %q, want contested", adj.Decisions[0].Consensus)
	}
	if adj.Decisions[1].Consensus != ConsensusUnanimous {
		t.Fatalf("3 of 3 seats = %q, want unanimous", adj.Decisions[1].Consensus)
	}
	// And NOTHING a caller reads has moved.
	if len(adj.Findings) != len(before) {
		t.Fatalf("findings = %d, want %d — the contested finding was DROPPED", len(adj.Findings), len(before))
	}
	for i := range before {
		if adj.Findings[i].ID != before[i].ID || adj.Findings[i].Title != before[i].Title ||
			adj.Findings[i].File != before[i].File || adj.Findings[i].Severity != before[i].Severity {
			t.Errorf("finding %d was rewritten:\n got %+v\nwant %+v", i, adj.Findings[i], before[i])
		}
	}
	for i := range states {
		if adj.Decisions[i].State != states[i] {
			t.Errorf("decision %d state = %q, want %q — a contested finding must not be downgraded", i, adj.Decisions[i].State, states[i])
		}
		if adj.Decisions[i].AgreementCount != counts[i] {
			t.Errorf("decision %d agreementCount = %d, want %d — no count is adjusted by this", i, adj.Decisions[i].AgreementCount, counts[i])
		}
	}
	if sum == nil {
		t.Fatal("summary = nil, want a tally over two panel findings")
	}
	if sum.Panelled != 2 || sum.Contested != 1 || sum.Unanimous != 1 || sum.Majority != 0 {
		t.Errorf("tally = %+v, want 2 panelled / 1 contested / 1 unanimous", *sum)
	}
	// The caveat travels WITH the number, in the artifact, so no surface can quote the count
	// without it.
	if !strings.Contains(sum.Note, "silent seat is not a seat that disagreed") {
		t.Errorf("note does not lead with what silence is NOT: %q", sum.Note)
	}
}

// TestDissentSummary_NoPanelMeansNoRecord: a run whose findings came from evidence hooks and the
// cross-check has no panel to describe. Three zeroes would read as "the panel agreed on nothing".
func TestDissentSummary_NoPanelMeansNoRecord(t *testing.T) {
	adj := adjudication.Result{
		Findings:  []review.Finding{{ID: "f1", Title: "from an evidence hook"}},
		Decisions: []review.Decision{{FindingID: "f1", Valid: true, State: review.StateReportedValid}},
	}
	if got := DissentSummaryFor(&adj); got != nil {
		t.Errorf("summary = %+v, want nil — there was no panel to agree or disagree", *got)
	}
}

// TestDissent_OnARealRun_TheContestedFindingIsReportedAnyway is the end-to-end claim.
//
// Three seats: two report the same finding, one reports something only it saw. The lone finding is
// exactly the case this feature is about — and the case a support-weighted design would bury. It must
// come back labelled `contested` AND fully reported, and the label must reach review-summary.md,
// which is the artifact a person actually opens.
func TestDissent_OnARealRun_TheContestedFindingIsReportedAnyway(t *testing.T) {
	m := panelManager(t,
		seatFake{name: "seat-a", title: "shared issue", file: "shared.go"},
		seatFake{name: "seat-b", title: "shared issue", file: "shared.go"}, // 2 of 3 → majority
		seatFake{name: "seat-c", title: "lone issue", file: "lone.go"},     // 1 of 3 → contested
	)
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	byTitle := map[string]review.Decision{}
	for i, f := range out.Findings {
		if i < len(out.Decisions) {
			byTitle[f.Title] = out.Decisions[i]
		}
	}
	lone, ok := byTitle["lone issue"]
	if !ok {
		t.Fatalf("THE CONTESTED FINDING IS MISSING — it must be reported like any other (findings: %+v)", out.Findings)
	}
	if lone.Consensus != ConsensusContested {
		t.Errorf("lone finding consensus = %q, want %q (1 of 3 seats)", lone.Consensus, ConsensusContested)
	}
	if lone.State != review.StateReportedValid {
		t.Errorf("lone finding state = %q — a contested finding must not be downgraded", lone.State)
	}
	if shared := byTitle["shared issue"]; shared.Consensus != ConsensusMajority {
		t.Errorf("shared finding consensus = %q, want %q (2 of 3 seats)", shared.Consensus, ConsensusMajority)
	}
	if out.Dissent == nil {
		t.Fatal("run-level dissent tally = nil, want a tally over two panel findings")
	}
	if out.Dissent.Contested != 1 || out.Dissent.Majority != 1 || out.Dissent.Panelled != 2 {
		t.Errorf("tally = %+v, want 2 panelled / 1 majority / 1 contested", *out.Dissent)
	}
	// AND IT REACHES THE HUMAN ARTIFACT. Until this landed, review-summary.md listed every finding
	// identically no matter how much of the panel stood behind it.
	summary := readFileT(t, filepath.Join(out.RunDir, "review-summary.md"))
	if !strings.Contains(summary, "1 of 3 seats (contested)") {
		t.Errorf("review-summary.md does not mark the contested finding:\n%s", summary)
	}
	if !strings.Contains(summary, "2 of 3 seats (majority)") {
		t.Errorf("review-summary.md does not mark the corroborated finding:\n%s", summary)
	}
	// The caveat travels with the label, in the same file — a reader who meets "contested" without
	// it will supply the one meaning it does not have.
	if !strings.Contains(summary, "silent seat is not a seat that disagreed") {
		t.Errorf("review-summary.md carries the labels without the caveat:\n%s", summary)
	}
}

// TestConsensusPhrase_ShowsTheDenominator. "1 of 3" is the whole point: the count alone cannot
// distinguish a lone voice on a big panel from a lone voice on a panel of one.
func TestConsensusPhrase_ShowsTheDenominator(t *testing.T) {
	if got := ConsensusPhrase(1, 2, ConsensusContested); got != "1 of 3 seats (contested)" {
		t.Errorf("phrase = %q", got)
	}
	if got := ConsensusPhrase(3, 0, ConsensusUnanimous); got != "3 of 3 seats (unanimous)" {
		t.Errorf("phrase = %q", got)
	}
	if got := ConsensusPhrase(1, 0, ""); got != "" {
		t.Errorf("unlabelled finding = %q, want an empty clause the caller can drop", got)
	}
}
