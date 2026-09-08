package capture

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/model/fake"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

func panel(t *testing.T, exScenarios []fake.Scenario, coll fake.Scenario) (pipeline.Registry, roster.Plan) {
	t.Helper()
	reg := pipeline.Registry{}
	tags := []string{"A", "B", "C", "D"}
	var exs []roster.Explorer
	for i, sc := range exScenarios {
		name := "explorer-" + tags[i]
		reg[name] = fake.New(name, tags[i], sc)
		exs = append(exs, roster.Explorer{Adapter: name, Model: "model-" + tags[i], Effort: "high"})
	}
	reg["collator"] = fake.New("collator", "C", coll)
	plan, err := roster.Roster{Explorers: exs, Collator: roster.Collator{Adapter: "collator", Model: "collator-model"}}.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return reg, plan
}

func run(t *testing.T, reg pipeline.Registry, plan roster.Plan) (pipeline.Result, error) {
	t.Helper()
	return pipeline.Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}}, pipeline.Options{}, nil)
}

func readManifest(t *testing.T, dir string) ManifestV1 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m ManifestV1
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

func TestDump_Complete(t *testing.T) {
	reg, plan := panel(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rn, err := audit.NewRun(t.TempDir(), "", time.Now())
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	if err := Dump(Input{Run: rn, Plan: plan, Result: res, Status: "complete"}); err != nil {
		t.Fatalf("dump: %v", err)
	}
	m := readManifest(t, rn.Dir)
	if m.SchemaVersion != ManifestSchemaVersion || m.Status != "complete" {
		t.Errorf("manifest header: %+v", m)
	}
	if len(m.Envelopes) != 2 {
		t.Fatalf("expected 2 envelope aliases, got %d", len(m.Envelopes))
	}
	seen := map[string]bool{}
	for _, a := range m.Envelopes {
		if a.ID == "" {
			t.Error("empty envelope ID")
		}
		if seen[a.ID] {
			t.Errorf("duplicate envelope ID %q (IDs must be unique within a run)", a.ID)
		}
		seen[a.ID] = true
		if a.Alias != "envelope#"+strconv.Itoa(a.Order) {
			t.Errorf("alias/order mismatch: %+v", a)
		}
	}
	for i := range 2 {
		if _, err := os.Stat(filepath.Join(rn.Dir, "calls", "envelope-"+strconv.Itoa(i), "raw.txt")); err != nil {
			t.Errorf("missing raw.txt for explorer %d: %v", i, err)
		}
	}
	for _, f := range []string{"synthesis.json", "formulation.json", "synthesize-prompt.txt", "raw-synthesis.txt"} {
		if _, err := os.Stat(filepath.Join(rn.Dir, f)); err != nil {
			t.Errorf("missing artifact %s: %v", f, err)
		}
	}
	if len(m.Artifacts) == 0 {
		t.Error("manifest has no artifact digests")
	}
	for _, a := range m.Artifacts {
		if a.SHA256 == "" || a.Bytes < 0 {
			t.Errorf("bad artifact entry: %+v", a)
		}
	}
}

// TestDump_DroppedRawCaptured pins the dropped-explorer capture: a dropped explorer must be diagnosable from the
// dump — a bounded raw-body file plus a structured DropRecord (reason, lengths, digest) in the manifest.
// The SchemaInvalid fake returns a real (but schema-failing) body, so a raw body IS captured.
func TestDump_DroppedRawCaptured(t *testing.T) {
	reg, plan := panel(t, []fake.Scenario{fake.Valid, fake.Valid, fake.SchemaInvalid}, fake.Valid)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("run should survive one dropped explorer: %v", err)
	}
	rn, err := audit.NewRun(t.TempDir(), "", time.Now())
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	if err := Dump(Input{Run: rn, Plan: plan, Result: res, Status: "complete"}); err != nil {
		t.Fatalf("dump: %v", err)
	}
	m := readManifest(t, rn.Dir)
	if len(m.Dropped) != 1 {
		t.Fatalf("expected 1 drop record, got %d", len(m.Dropped))
	}
	d := m.Dropped[0]
	if d.Reason == "" || d.SHA256 == "" {
		t.Errorf("drop record missing reason/sha256: %+v", d)
	}
	if d.CapturedLen <= 0 || d.FullLen <= 0 {
		t.Errorf("drop record should carry a captured body length: %+v", d)
	}
	if d.RawPath == "" {
		t.Fatal("drop record has no rawPath")
	}
	raw, rerr := os.ReadFile(filepath.Join(rn.Dir, d.RawPath))
	if rerr != nil {
		t.Fatalf("read captured raw body: %v", rerr)
	}
	if len(raw) != d.CapturedLen {
		t.Errorf("captured file len %d != record capturedLen %d", len(raw), d.CapturedLen)
	}
}

// TestDump_CatalogLedger pins the append-only ledger capture: a catalog run persists merge-ledger.jsonl
// (one JSON row per raw nomination) + the canonicalizer prompt/raw output, and the manifest records the
// partition revision hash + surjectivity + ledger row count.
func TestDump_CatalogLedger(t *testing.T) {
	reg, plan := panel(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := pipeline.Run(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: "catalog"},
		pipeline.Options{}, nil)
	if err != nil {
		t.Fatalf("catalog run: %v", err)
	}
	rn, err := audit.NewRun(t.TempDir(), "", time.Now())
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	if err := Dump(Input{Run: rn, Plan: plan, Result: res, Status: "complete"}); err != nil {
		t.Fatalf("dump: %v", err)
	}
	// merge-ledger.jsonl exists, one JSON object per line (4 nominations).
	b, rerr := os.ReadFile(filepath.Join(rn.Dir, "merge-ledger.jsonl"))
	if rerr != nil {
		t.Fatalf("read merge-ledger.jsonl: %v", rerr)
	}
	lines := 0
	for _, ln := range splitNonEmptyLines(string(b)) {
		var row map[string]any
		if jerr := json.Unmarshal([]byte(ln), &row); jerr != nil {
			t.Errorf("ledger line is not a JSON object: %q (%v)", ln, jerr)
		}
		if row["canonicalId"] == nil || row["rawNomination"] == nil {
			t.Errorf("ledger row missing canonicalId/rawNomination: %v", row)
		}
		lines++
	}
	if lines != 4 {
		t.Errorf("expected 4 ledger rows (one per nomination), got %d", lines)
	}
	for _, f := range []string{"canonicalizer-prompt.txt", "raw-canonicalization.txt"} {
		if _, serr := os.Stat(filepath.Join(rn.Dir, f)); serr != nil {
			t.Errorf("missing catalog artifact %s: %v", f, serr)
		}
	}
	m := readManifest(t, rn.Dir)
	if m.PartitionRevisionHash == "" || m.Surjectivity != "holds" || m.LedgerRows != 4 {
		t.Errorf("manifest canonicalization fields wrong: hash=%q surjectivity=%q rows=%d", m.PartitionRevisionHash, m.Surjectivity, m.LedgerRows)
	}
}

// TestDump_GovernedRun_PersistsGovernanceArtifacts pins the governance capture surface (design §4/§9): a run under the
// ranking-grade policy persists the CONFIRMED merge-ledger AND the superseded provisional one (a revision is a
// new entry — the prior revision must stay readable), the confirmation record (presentation order + typed
// challenges + versioned resolutions), the pre-flight verdicts, the recorded rounds, and every emitted
// governance claim with its inputs. The manifest indexes the revision chain + the frozen policy hash.
func TestDump_GovernedRun_PersistsGovernanceArtifacts(t *testing.T) {
	// A canonicalizer that merges everything + an explorer that challenges it ⇒ a real confirmation revision.
	reg, plan := panel(t, []fake.Scenario{fake.ChallengeWrongMerge, fake.Valid}, fake.CanonMergeAll)
	cat, _ := mode.Lookup(mode.Catalog)
	spec := cat
	spec.Name = "example-governed"
	spec.Canonicalization = mode.CanonicalizationPolicy{Confirm: true}

	res, err := pipeline.RunSpec(context.Background(), reg, plan,
		schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}},
		spec, pipeline.Options{}, nil)
	if err != nil {
		t.Fatalf("governed run: %v", err)
	}
	rn, nerr := audit.NewRun(t.TempDir(), "", time.Now())
	if nerr != nil {
		t.Fatalf("new run: %v", nerr)
	}
	if derr := Dump(Input{Run: rn, Plan: plan, Result: res, Status: "complete"}); derr != nil {
		t.Fatalf("dump: %v", derr)
	}
	for _, f := range []string{
		"merge-ledger.jsonl", "merge-ledger-provisional.jsonl", "confirmation.json",
		"confirmation-prompt.txt", "preflight.json", "rounds.json", "governance-claims.json",
	} {
		if _, serr := os.Stat(filepath.Join(rn.Dir, f)); serr != nil {
			t.Errorf("missing governance artifact %s: %v", f, serr)
		}
	}
	// The PROVISIONAL revision is retained alongside the confirmed one, and they differ (a split happened).
	prov, _ := os.ReadFile(filepath.Join(rn.Dir, "merge-ledger-provisional.jsonl"))
	confirmed, _ := os.ReadFile(filepath.Join(rn.Dir, "merge-ledger.jsonl"))
	if len(prov) == 0 || string(prov) == string(confirmed) {
		t.Error("the superseded provisional revision must be retained and must differ from the confirmed one")
	}
	m := readManifest(t, rn.Dir)
	if m.LedgerRevision != 2 || m.PriorRevisionHash == "" {
		t.Errorf("the manifest must index the revision chain: rev=%d prior=%q", m.LedgerRevision, m.PriorRevisionHash)
	}
	if m.ConfirmationRule == "" || m.Challenges != 1 {
		t.Errorf("the manifest must record the confirmation rule + challenge count: %+v", m)
	}
	if m.CountingPolicyHash == "" || m.GovernanceClaimsHash == "" || m.GovernanceClaims == 0 {
		t.Errorf("the manifest must record the frozen policy hash + the emitted claims: %+v", m)
	}
	// Every persisted claim carries its inputs (§9).
	b, rerr := os.ReadFile(filepath.Join(rn.Dir, "governance-claims.json"))
	if rerr != nil {
		t.Fatalf("read governance claims: %v", rerr)
	}
	var report struct {
		Claims []map[string]any `json:"claims"`
	}
	if uerr := json.Unmarshal(b, &report); uerr != nil {
		t.Fatalf("parse governance claims: %v", uerr)
	}
	if len(report.Claims) == 0 {
		t.Fatal("no claims persisted")
	}
	for _, c := range report.Claims {
		for _, field := range []string{"query", "value", "kOfPanel", "kOfRespondents", "formulationHash",
			"partitionRevisionHash", "rulesVersion", "contributingSourceIds", "label"} {
			if _, ok := c[field]; !ok {
				t.Errorf("persisted claim missing %q: %v", field, c)
			}
		}
	}
}

// TestDump_DegradedRun_PersistsDegradedArtifact: when the collator is lost after the fan-out the run still
// produces a terminal artifact, and the dump records it (with its mode class in the manifest).
func TestDump_DegradedRun_PersistsDegradedArtifact(t *testing.T) {
	reg, plan := panel(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	reg["collator"] = deadCollator{reg["collator"]}
	res, rerr := run(t, reg, plan)
	if rerr == nil {
		t.Fatal("a collator that dies after the fan-out must halt")
	}
	rn, nerr := audit.NewRun(t.TempDir(), "", time.Now())
	if nerr != nil {
		t.Fatalf("new run: %v", nerr)
	}
	if derr := Dump(Input{Run: rn, Plan: plan, Result: res, Status: "halted", Fault: rerr.Error()}); derr != nil {
		t.Fatalf("dump: %v", derr)
	}
	if _, serr := os.Stat(filepath.Join(rn.Dir, "degraded.json")); serr != nil {
		t.Fatalf("the degraded terminal artifact must be captured: %v", serr)
	}
	m := readManifest(t, rn.Dir)
	if m.DegradedTerminalClass != string(schema.EmergentSpace) {
		t.Errorf("the manifest must record the degraded artifact's mode class, got %q", m.DegradedTerminalClass)
	}
	b, _ := os.ReadFile(filepath.Join(rn.Dir, "degraded.json"))
	if !strings.Contains(string(b), schema.UncollatedLabel) {
		t.Errorf("an emergent-space degraded artifact must carry the uncollated label: %s", b)
	}
}

// deadCollator passes the identity pre-flight and then fails every later call — a collator lost AFTER the
// fan-out, which is exactly when the degraded terminal artifact must be produced.
type deadCollator struct{ inner model.Adapter }

func (d deadCollator) Name() string                    { return d.inner.Name() }
func (d deadCollator) Available() (bool, string)       { return d.inner.Available() }
func (d deadCollator) Evidence() core.IdentityEvidence { return core.EvidenceInvocationTag }
func (d deadCollator) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	if c.Phase == schema.PhasePreflight {
		return d.inner.Invoke(ctx, c)
	}
	return model.Result{ExitCode: 1}, context.DeadlineExceeded
}

// splitNonEmptyLines splits s on newlines and drops empty trailing lines.
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

func TestDump_Halted(t *testing.T) {
	// A schema-invalid explorer response drops that seat and leaves a one-response panel, which halts —
	// the dump must still record it, with status=halted, the fault, and the surviving envelope.
	// (An identity mismatch deliberately does NOT halt; see the pipeline's identity tests.)
	reg, plan := panel(t, []fake.Scenario{fake.Valid, fake.SchemaInvalid}, fake.Valid)
	res, rerr := run(t, reg, plan)
	if rerr == nil {
		t.Fatal("expected a halt from the one-response panel")
	}
	rn, err := audit.NewRun(t.TempDir(), "", time.Now())
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	if err := Dump(Input{Run: rn, Plan: plan, Result: res, Status: "halted", Fault: rerr.Error()}); err != nil {
		t.Fatalf("dump: %v", err)
	}
	m := readManifest(t, rn.Dir)
	if m.Status != "halted" || m.Fault == "" {
		t.Errorf("halted manifest should carry status+fault: %+v", m)
	}
}
