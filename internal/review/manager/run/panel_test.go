package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// seatFake is one panel seat's adapter: it reports ONE finding whose title/file are its own, so a
// union across seats is distinguishable from a single seat's output. `evidence` lets a test give a
// seat a WEAK identity (self-report) to exercise the quarantine.
type seatFake struct {
	name     string
	title    string
	file     string
	evidence review.IdentityEvidence
	// actual overrides the echoed model (an identity MISMATCH when it differs from the request).
	actual string
}

func (f seatFake) Name() string              { return f.name }
func (f seatFake) Available() (bool, string) { return true, "seat fake (test)" }
func (f seatFake) Evidence() review.IdentityEvidence {
	if f.evidence == "" {
		return review.EvidenceInvocationTag
	}
	return f.evidence
}
func (f seatFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	actual := string(c.ModelArg)
	if f.actual != "" {
		actual = f.actual
	}
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"request_changes","findings":[{"id":"S1","kind":"fail","severity":"high","title":%q,"file":%q,"location":"1","source":"reviewer"}]}`,
		string(c.Role), string(c.Phase), f.title, f.file)
	return model.Result{ExitCode: 0, ActualModel: actual, Evidence: f.Evidence(), Stdout: []byte(js)}, nil
}

// panelManager builds a manager whose `panel` profile has an ordered `reviewers` list of the given
// seat adapters (host stays the deterministic fake, so adjudication is deterministic).
func panelManager(t *testing.T, seats ...seatFake) *Manager {
	t.Helper()
	cfg := config.Default()
	adapters := map[string]model.Adapter{"fake": fake.New(fake.Valid)}
	var reviewers []config.Lane
	for _, s := range seats {
		cfg.Adapters[s.name] = config.Adapter{ModelIdentity: "invocation_tag"}
		cfg.ModelCatalog[s.name+"-model"] = config.CatalogEntry{
			Provider: "x", CanonicalModel: "mm",
			Adapters: map[string]config.AdapterModel{s.name: {ModelArg: s.name + "-arg"}},
		}
		adapters[s.name] = s
		reviewers = append(reviewers, config.Lane{Execution: "adapter", Adapter: s.name, Model: s.name + "-model"})
	}
	cfg.Profiles["panel"] = config.Profile{
		Description:       "panel test",
		AdapterPreference: []string{"fake"},
		Lanes:             map[string]config.Lane{"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"}},
		Reviewers:         reviewers,
	}
	return &Manager{Cfg: cfg, Adapters: adapters, ArtifactDir: t.TempDir(), TempBase: t.TempDir()}
}

// TestPanel_EverySeatExecutesAndFindingsUnion: an N-seat panel runs EVERY seat (no clamping, no
// dropping), the host sees the union of their findings, and the executed roster echoes one entry
// per requested seat. On pre-panel code there is only one blind reviewer, so a 3-seat profile
// would run once and report one finding.
func TestPanel_EverySeatExecutesAndFindingsUnion(t *testing.T) {
	// Distinct files ⇒ distinct fingerprints, so the union is observable (findings are deduped by
	// kind|file|location, not by title).
	m := panelManager(t,
		seatFake{name: "seat-a", title: "issue A", file: "a.go"},
		seatFake{name: "seat-b", title: "issue B", file: "b.go"},
		seatFake{name: "seat-c", title: "issue C", file: "c.go"},
	)
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(out.Panel) != 3 {
		t.Fatalf("executed roster = %d seats, want 3 (one per REQUESTED seat)", len(out.Panel))
	}
	wantIDs := []string{"reviewer", "reviewer-2", "reviewer-3"}
	for i, s := range out.Panel {
		if s.SeatID != wantIDs[i] || s.Index != i+1 {
			t.Errorf("roster[%d] = %q/#%d, want %q/#%d", i, s.SeatID, s.Index, wantIDs[i], i+1)
		}
		if s.Status != "completed" {
			t.Errorf("seat %q status = %q, want completed", s.SeatID, s.Status)
		}
		if s.Rounds < 1 {
			t.Errorf("seat %q ran %d rounds, want >= 1 (every requested seat executes)", s.SeatID, s.Rounds)
		}
	}
	titles := map[string]bool{}
	for _, f := range out.Findings {
		titles[f.Title] = true
	}
	for _, want := range []string{"issue A", "issue B", "issue C"} {
		if !titles[want] {
			t.Errorf("the union of seat findings is missing %q (have %v)", want, titles)
		}
	}
	// The roster is also a run artifact, so the audit record answers "what panel ran".
	b, rerr := os.ReadFile(filepath.Join(out.RunDir, "panel", "roster.json"))
	if rerr != nil {
		t.Fatalf("panel/roster.json: %v", rerr)
	}
	for _, id := range wantIDs {
		if !strings.Contains(string(b), id) {
			t.Errorf("panel/roster.json does not mention seat %q", id)
		}
	}
}

// TestPanel_SeatsAreBlindToEachOther: every seat's FIRST-round prompt is byte-identical (a seat can
// only have seen the workspace), and no seat's prompt ever contains another seat's finding. This is
// the blindness spec's observable half — the structural half is that runSeat passes nil decisions
// and a seat-local `reported` slice.
func TestPanel_SeatsAreBlindToEachOther(t *testing.T) {
	m := panelManager(t,
		seatFake{name: "seat-a", title: "issue A", file: "a.go"},
		seatFake{name: "seat-b", title: "issue B", file: "b.go"},
	)
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	read := func(callID string) string {
		b, rerr := os.ReadFile(filepath.Join(out.RunDir, "calls", callID, "prompt.md"))
		if rerr != nil {
			t.Fatalf("prompt for %s: %v", callID, rerr)
		}
		return string(b)
	}
	first, second := read("c-0001"), read("c-0001-s2")
	if first != second {
		t.Errorf("round-1 seat prompts differ; a blind seat may see only the workspace, so every seat's opening prompt must be identical")
	}
	if strings.Contains(first, "issue B") {
		t.Error("seat 1's prompt contains seat 2's finding — blindness violated")
	}
	if strings.Contains(second, "issue A") {
		t.Error("seat 2's prompt contains seat 1's finding — blindness violated")
	}
	// Later rounds carry ONLY the seat's own prior finding.
	if b, rerr := os.ReadFile(filepath.Join(out.RunDir, "calls", "c-0001-s2-i2", "prompt.md")); rerr == nil {
		if strings.Contains(string(b), "issue A") {
			t.Error("seat 2's round-2 prompt contains seat 1's finding — blindness violated between rounds")
		}
	}
}

// TestPanel_OneSeatKeepsHistoricalCallIDs pins the compatibility that the golden run depends on: a
// panel of one writes exactly the call directories the pre-panel single reviewer wrote.
func TestPanel_OneSeatKeepsHistoricalCallIDs(t *testing.T) {
	for _, tc := range []struct {
		seat, round int
		want        string
	}{
		{0, 1, "c-0001"},
		{0, 2, "c-0001-i2"},
		{1, 1, "c-0001-s2"},
		{1, 3, "c-0001-s2-i3"},
	} {
		if got := seatCallID("c-0001", tc.seat, tc.round); got != tc.want {
			t.Errorf("seatCallID(seat %d, round %d) = %q, want %q", tc.seat, tc.round, got, tc.want)
		}
	}
}

// TestPanel_IdentityMismatchKeepsTheSeatAndItsFindings: a mismatch on one seat of a blind panel does
// not halt the run and does not drop that seat. Its findings enter adjudication alongside every other
// seat's, and the mismatch is recorded as a caveat. Discarding the seat would silently shrink the panel
// — the two-seat agreement counts the rest of the run reports would then be computed over a panel the
// reader was told had two members.
func TestPanel_IdentityMismatchKeepsTheSeatAndItsFindings(t *testing.T) {
	m := panelManager(t,
		seatFake{name: "seat-a", title: "issue A", file: "a.go"},
		seatFake{name: "seat-b", title: "issue B", file: "b.go", actual: "some-other-model"},
	)
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel"})
	if err != nil {
		t.Fatalf("a seat identity mismatch must not halt the panel: %v", err)
	}
	if out.Halt != nil {
		t.Errorf("halt class = %v, want none", out.Halt)
	}
	if len(out.Panel) != 2 {
		t.Fatalf("roster = %d seats, want 2", len(out.Panel))
	}
	for _, s := range out.Panel {
		if s.Status != "completed" {
			t.Errorf("seat %s = %q, want completed; a mismatched identity must not fail its seat", s.SeatID, s.Status)
		}
	}
	// Both seats' findings survived into the run's findings.
	titles := map[string]bool{}
	for _, f := range out.Findings {
		titles[f.Title] = true
	}
	if !titles["issue A"] || !titles["issue B"] {
		t.Errorf("both seats' findings must survive, got %v", titles)
	}
	found := false
	for _, c := range out.IdentityCaveats {
		if c.Status == review.VerifMismatch {
			found = true
		}
	}
	if !found {
		t.Errorf("the mismatching seat must be recorded as a caveat, got %+v", out.IdentityCaveats)
	}
}

// TestPanel_ProvenanceIsHostComputed: agreement counts and supporting seats are recorded on the
// DECISION (never parsed from a model), a finding two seats reported carries both of them, and the
// seats that ran but did not report it are recorded as dissent.
func TestPanel_ProvenanceIsHostComputed(t *testing.T) {
	m := panelManager(t,
		seatFake{name: "seat-a", title: "shared issue", file: "shared.go"},
		seatFake{name: "seat-b", title: "shared issue", file: "shared.go"}, // same fingerprint → agreement 2
		seatFake{name: "seat-c", title: "lone issue", file: "lone.go"},
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
	shared, ok := byTitle["shared issue"]
	if !ok {
		t.Fatalf("no decision for the corroborated finding (findings: %+v)", out.Findings)
	}
	if shared.AgreementCount != 2 || len(shared.SupportingSeats) != 2 {
		t.Errorf("corroborated finding: agreement %d / %d supporting seats, want 2/2 — the count is host arithmetic over which seats reported it",
			shared.AgreementCount, len(shared.SupportingSeats))
	}
	if len(shared.DissentingSeats) != 1 || shared.DissentingSeats[0] != "reviewer-3" {
		t.Errorf("dissent = %v, want [reviewer-3] (the seat that ran and did not report it)", shared.DissentingSeats)
	}
	lone := byTitle["lone issue"]
	if lone.AgreementCount != 1 {
		t.Errorf("single-source finding: agreement %d, want 1", lone.AgreementCount)
	}
}

// TestPanel_WeakIdentitySupportIsAppliableAndLabeled: a finding supported ONLY by weak-identity seats
// is applyable like any other, and each supporting seat's tier is recorded so a reader can see what
// the support consists of.
//
// This replaces a "weak-identity quarantine" that refused such findings for apply. That rule presumed
// we could tell a proven model from a claimed one — which the codex echo test showed we cannot — so it
// withheld fixes for genuine defects on the strength of a tier that might itself be our own argument
// handed back to us. Identity is recorded, never acted on (../../../../docs/model-identity.md).
func TestPanel_WeakIdentitySupportIsAppliableAndLabeled(t *testing.T) {
	finding := func(title, file string) review.Finding {
		return review.Finding{ID: "x", Title: title, Kind: review.KindFail, Severity: review.SeverityHigh, File: file, Location: "1"}
	}
	weakOnly := finding("weak only", "weak.go")
	mixed := finding("mixed support", "mixed.go")
	adj := adjudication.Judge([]review.Finding{weakOnly, weakOnly, mixed}, nil) // duplicates dedup

	weak1 := review.SeatRef{SeatID: "reviewer", IdentityTier: review.SeatIdentitySelfReported}
	weak2 := review.SeatRef{SeatID: "reviewer-2", IdentityTier: review.SeatIdentityUnknown}
	strong := review.SeatRef{SeatID: "reviewer-3", IdentityTier: review.SeatIdentityVerified}
	p := panelResult{
		completed: []review.SeatRef{weak1, weak2, strong},
		support: map[string][]review.SeatRef{
			schema.Fingerprint(weakOnly): {weak1, weak2},
			schema.Fingerprint(mixed):    {weak2, strong},
		},
	}
	attachPanelProvenance(&adj, p)

	for i, f := range adj.Findings {
		d := adj.Decisions[i]
		switch f.Title {
		case "weak only":
			if d.AgreementCount != 2 {
				t.Errorf("finding lost its provenance through dedup: agreement %d, want 2", d.AgreementCount)
			}
			if d.ApplyRefusalReason != "" || d.Applyable != nil {
				t.Errorf("weak-identity support must NOT refuse a finding for apply, got applyable=%v reason=%q",
					d.Applyable, d.ApplyRefusalReason)
			}
			// The tiers are still on the record — that is the whole remaining job of identity here.
			for _, s := range d.SupportingSeats {
				if s.IdentityTier == "" {
					t.Errorf("supporting seat %s lost its identity tier: %+v", s.SeatID, s)
				}
			}
		case "mixed support":
			if d.ApplyRefusalReason != "" {
				t.Errorf("mixed support must not be refused either, got refusal %q", d.ApplyRefusalReason)
			}
		}
	}
}

// TestPanelBudget_RefusesBudgetBelowSeatCount: a run-level budget that cannot give every seat one
// round is a CONFIG ERROR, because quietly running fewer seats than requested is the silent
// degradation the panel exists to rule out.
func TestPanelBudget_RefusesBudgetBelowSeatCount(t *testing.T) {
	two := 2
	if _, err := newPanelBudget(&two, 4, 3); err == nil {
		t.Fatal("a budget of 2 with 3 seats must be refused")
	}
	b, err := newPanelBudget(nil, 4, 1)
	if err != nil {
		t.Fatalf("default budget: %v", err)
	}
	if b.total != 4 {
		t.Errorf("a panel of one gets %d rounds, want maxInnerIterations (4) — today's inner loop unchanged", b.total)
	}
	if b, err = newPanelBudget(nil, 8, review.MaxReviewerSeats); err != nil || b.total != defaultPanelRoundCeiling {
		t.Errorf("a full panel's default budget = %v (err %v), want the ceiling %d", b.total, err, defaultPanelRoundCeiling)
	}
}

// TestPanel_BudgetCapsRoundsNotSeats: when the budget is exhausted, seats stop STABILIZING (and say
// so in the roster) — they are never removed from the panel.
func TestPanel_BudgetCapsRoundsNotSeats(t *testing.T) {
	m := panelManager(t,
		seatFake{name: "seat-a", title: "issue A", file: "a.go"},
		seatFake{name: "seat-b", title: "issue B", file: "b.go"},
	)
	two := 2 // exactly one round per seat: enough to run every seat, not enough to stabilize
	m.Cfg.Review.MaxPanelRounds = &two
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "panel"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(out.Panel) != 2 {
		t.Fatalf("roster = %d seats, want 2 — the budget bounds ROUNDS, never the seat count", len(out.Panel))
	}
	for _, s := range out.Panel {
		if s.Rounds != 1 {
			t.Errorf("seat %q ran %d rounds under a 2-round budget, want exactly its guaranteed first round", s.SeatID, s.Rounds)
		}
	}
}
