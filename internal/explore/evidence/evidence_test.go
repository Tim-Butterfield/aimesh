package evidence

// Tests for the DERIVED evidence export (design §9). Four of them are the contract:
//
//	a captured run exports, and the integrity gates pass
//	rebuild-and-compare: exporting the same directory twice is identical, in content AND in bytes
//	a DOCTORED ballot naming a canonical ID absent from the confirmed revision is REJECTED
//	lineage_edges is traversable by a recursive CTE
//
// They run entirely on deterministic in-process fakes: no real model CLI, no network, and every path under
// t.TempDir().

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"

	"github.com/Tim-Butterfield/aimesh/internal/explore/capture"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/model/fake"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// TestMain isolates the app home so nothing in this package can touch a real config or artifact dir.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "evidence-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("AIMESH_HOME", home)
	_ = os.Setenv("AIMESH_HOME", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// capturedRun runs one exploration on fakes and writes it to a fresh run directory, returning the path.
func capturedRun(t *testing.T, raw schema.RawTask) string {
	t.Helper()
	reg := pipeline.Registry{}
	var exs []roster.Explorer
	for _, tag := range []string{"A", "B", "C"} {
		name := "explorer-" + tag
		reg[name] = fake.New(name, tag, fake.Valid)
		exs = append(exs, roster.Explorer{Adapter: name, Model: "model-" + tag, Effort: "high"})
	}
	reg["collator"] = fake.New("collator", "C", fake.Valid)
	r := roster.Roster{Explorers: exs, Collator: roster.Collator{Adapter: "collator", Model: "collator-model"}}
	plan, err := r.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	res, rerr := pipeline.Run(context.Background(), reg, plan, raw, pipeline.Options{}, nil)
	status, faultMsg := "complete", ""
	if rerr != nil {
		status, faultMsg = "halted", rerr.Error()
	}
	run, aerr := audit.NewRun(t.TempDir(), "", time.Now())
	if aerr != nil {
		t.Fatalf("new run: %v", aerr)
	}
	if derr := capture.Dump(capture.Input{
		Run: run, Plan: plan, Result: res, Status: status, Fault: faultMsg, Task: raw,
	}); derr != nil {
		t.Fatalf("dump: %v", derr)
	}
	return run.Dir
}

func shortlistTask() schema.RawTask {
	return schema.RawTask{Purpose: "pick a store", Criteria: []string{"operability"}, Mode: mode.Shortlist}
}

func compareTask() schema.RawTask {
	return schema.RawTask{
		Purpose: "choose a store", Criteria: []string{"operability"}, Mode: mode.Compare,
		Options: []string{"alpha", "beta", "gamma"},
		CompareCriteria: []schema.CompareCriterion{
			{Name: "throughput", Direction: schema.HigherIsBetter, Role: schema.RoleDimension, Weight: 2},
			{Name: "cost", Direction: schema.LowerIsBetter, Role: schema.RoleDimension, Weight: 1},
			{Name: "on-prem support", Role: schema.RoleFilter},
		},
	}
}

// openDB opens an exported database read-only for assertions.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&mode=ro")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func scalar[T any](t *testing.T, db *sql.DB, query string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}

// TestExport_CapturedRunExportsWithIntegrityGates is the baseline: a real captured run exports, the
// integrity gates pass (Export runs foreign_key_check + integrity_check itself), and the relational core is
// populated rather than empty.
func TestExport_CapturedRunExportsWithIntegrityGates(t *testing.T) {
	dir := capturedRun(t, shortlistTask())
	out := filepath.Join(t.TempDir(), "evidence.db")
	sum, err := Export(dir, out, Options{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if sum.ExplorationID == "" || sum.Mode != mode.Shortlist || sum.SchemaVersion != ExportSchemaVersion {
		t.Errorf("summary: %+v", sum)
	}
	for _, table := range []string{
		"explorations", "mode_contracts", "participants", "panel_members", "rounds", "calls",
		"identity_records", "artifacts", "envelopes", "mentions", "canonicalization_revisions",
		"canonical_entities", "canonical_map_entries", "criteria", "criterion_snapshots", "decisions",
		"ballot_universe_snapshots", "ballots", "ballot_entries", "decision_entries", "governance_claims",
		"governance_claim_sources", "artifact_elements", "lineage_edges",
	} {
		if sum.Rows[table] == 0 {
			t.Errorf("table %s is empty in a shortlist export", table)
		}
	}
	db := openDB(t, out)
	// STRICT + foreign keys are properties of the FILE, so they are checked on the file a reader opens.
	if got := scalar[int](t, db, "PRAGMA user_version"); got != ExportSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, ExportSchemaVersion)
	}
	strictTables := scalar[int](t, db,
		`SELECT COUNT(*) FROM pragma_table_list WHERE schema='main' AND type='table' AND strict=0 AND name NOT LIKE 'sqlite_%'`)
	if strictTables != 0 {
		t.Errorf("%d table(s) are not STRICT", strictTables)
	}
	// Governance values are TYPED COLUMNS, not JSON: the point of exporting is runnable governance SQL.
	withheld := scalar[int](t, db,
		`SELECT COUNT(*) FROM governance_claims WHERE definitive=0`)
	corroborated := scalar[int](t, db,
		`SELECT COUNT(*) FROM governance_claims WHERE label=? AND k_panel>=2 AND m_panel>0`, string(govern.LabelCorroborated))
	if corroborated == 0 {
		t.Error("a shortlist run must record at least one corroborated claim with real denominators")
	}
	t.Logf("claims: %d corroborated, %d withheld", corroborated, withheld)
	// The ballot really did run over the CONFIRMED universe — expressed as SQL, which is the deliverable.
	orphans := scalar[int](t, db, `
		SELECT COUNT(*) FROM ballot_entries be
		LEFT JOIN canonical_entities ce
		  ON ce.exploration_id = be.exploration_id
		 AND ce.revision = be.confirmed_revision
		 AND ce.canonical_id = be.canonical_id
		WHERE ce.canonical_id IS NULL`)
	if orphans != 0 {
		t.Errorf("%d ballot entries name a candidate outside the confirmed universe", orphans)
	}
}

// TestExport_RebuildAndCompare is §9's derivability invariant: exporting the same run directory twice yields
// identical CONTENT and identical BYTES. It is the property that makes the database honest — it says the
// export is a pure function of the append-only run directory, which stays the system of record.
func TestExport_RebuildAndCompare(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  schema.RawTask
	}{
		{"shortlist", shortlistTask()},
		{"compare", compareTask()},
		{"map", schema.RawTask{Purpose: "explore X", Criteria: []string{"c1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := capturedRun(t, tc.raw)
			tmp := t.TempDir()
			first := filepath.Join(tmp, "first.db")
			second := filepath.Join(tmp, "second.db")
			if _, err := Export(dir, first, Options{}); err != nil {
				t.Fatalf("first export: %v", err)
			}
			if _, err := Export(dir, second, Options{}); err != nil {
				t.Fatalf("second export: %v", err)
			}
			d1, err := CanonicalDump(first)
			if err != nil {
				t.Fatalf("dump: %v", err)
			}
			d2, err := CanonicalDump(second)
			if err != nil {
				t.Fatalf("dump: %v", err)
			}
			if d1 != d2 {
				t.Errorf("two exports of the same run directory differ in CONTENT:\n%s", firstDiff(d1, d2))
			}
			s1, err := FileDigest(first)
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			s2, err := FileDigest(second)
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if s1 != s2 {
				t.Errorf("two exports of the same run directory differ in BYTES: %s vs %s", s1, s2)
			}
			// Verify is the runnable form an auditor uses: it rebuilds and compares both ways.
			if err := Verify(dir, first); err != nil {
				t.Errorf("Verify: %v", err)
			}
			// …and it leaves NOTHING beside the database it checked: the scratch rebuild lives in a
			// temporary directory of its own, so a verified export is still exactly one file.
			entries, rerr := os.ReadDir(tmp)
			if rerr != nil {
				t.Fatalf("read dir: %v", rerr)
			}
			for _, e := range entries {
				if e.Name() != "first.db" && e.Name() != "second.db" {
					t.Errorf("Verify left %q beside the export", e.Name())
				}
			}
		})
	}
}

// firstDiff renders the first differing line of two dumps (a whole-dump diff is unreadable in a failure).
func firstDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			return fmt.Sprintf("line %d:\n  A: %s\n  B: %s", i+1, la[i], lb[i])
		}
	}
	return fmt.Sprintf("one dump is longer than the other (%d vs %d lines)", len(la), len(lb))
}

// TestExport_DoctoredBallotIsRejected is §9's named invariant: a ballot row referencing a canonical ID that
// is ABSENT from the confirmed revision must be a CONSTRAINT VIOLATION, not a silent pass. The run directory
// is doctored exactly the way a tampered or corrupted record would be, and the export must refuse it.
func TestExport_DoctoredBallotIsRejected(t *testing.T) {
	dir := capturedRun(t, shortlistTask())
	path := filepath.Join(dir, "decision.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read decision.json: %v", err)
	}
	var decision map[string]any
	if jerr := json.Unmarshal(b, &decision); jerr != nil {
		t.Fatalf("parse decision.json: %v", jerr)
	}
	ballots, _ := decision["ballots"].([]any)
	if len(ballots) == 0 {
		t.Fatal("the shortlist run recorded no ballot to doctor")
	}
	first, _ := ballots[0].(map[string]any)
	ranking, _ := first["ranking"].([]any)
	first["ranking"] = append(ranking, "canon-this-candidate-was-never-confirmed")
	doctored, _ := json.MarshalIndent(decision, "", "  ")
	if werr := os.WriteFile(path, doctored, 0o644); werr != nil {
		t.Fatalf("write doctored decision.json: %v", werr)
	}

	out := filepath.Join(t.TempDir(), "doctored.db")
	_, eerr := Export(dir, out, Options{})
	if eerr == nil {
		t.Fatal("a ballot naming a canonical ID absent from the confirmed revision MUST be rejected, not exported")
	}
	msg := strings.ToLower(eerr.Error())
	if !strings.Contains(msg, "foreign key") || !strings.Contains(msg, "governance invariant") {
		t.Errorf("the refusal must name the violated invariant, got: %v", eerr)
	}
}

// TestExport_LineageIsRecursiveCTETraversable pins the one variable-depth structure (§9): a recursive CTE
// over lineage_edges walks from a BLIND round-1 envelope through the mention it produced, the canonical
// entity that mention was mapped to, and out to the decision entry the entity was tallied as.
func TestExport_LineageIsRecursiveCTETraversable(t *testing.T) {
	dir := capturedRun(t, shortlistTask())
	out := filepath.Join(t.TempDir(), "lineage.db")
	if _, err := Export(dir, out, Options{}); err != nil {
		t.Fatalf("export: %v", err)
	}
	db := openDB(t, out)
	const traverse = `
		WITH RECURSIVE reach(element_id, depth) AS (
		  SELECT element_id, 0 FROM artifact_elements
		   WHERE exploration_id = (SELECT exploration_id FROM explorations)
		     AND kind = 'envelope' AND round_index = 1
		  UNION
		  SELECT e.child_element_id, r.depth + 1
		    FROM lineage_edges e JOIN reach r ON e.parent_element_id = r.element_id
		)
		SELECT COALESCE(MAX(r.depth), -1)
		  FROM reach r JOIN artifact_elements a ON a.element_id = r.element_id
		 WHERE a.kind = 'decision_entry'`
	depth := scalar[int](t, db, traverse)
	if depth < 3 {
		t.Fatalf("a decision entry must be reachable from a blind round-1 envelope at depth >= 3 (envelope → mention → entity → decision), got %d", depth)
	}
	// And the same table answers the inverse question: what is a given result element built on.
	const provenance = `
		WITH RECURSIVE back(element_id) AS (
		  SELECT element_id FROM artifact_elements WHERE kind = 'decision_entry'
		  UNION
		  SELECT e.parent_element_id FROM lineage_edges e JOIN back b ON e.child_element_id = b.element_id
		)
		SELECT COUNT(*) FROM back b JOIN artifact_elements a ON a.element_id = b.element_id
		 WHERE a.kind = 'envelope'`
	if n := scalar[int](t, db, provenance); n == 0 {
		t.Error("walking lineage_edges backwards from a decision entry must reach the envelopes it rests on")
	}
}

// TestExport_FixedSpaceRunRecordsNoPartitionAndTypedCriteria pins the fixed-space half of the export: a fixed-space
// run's claims carry the explicit "no partition" statement (never an empty hash), and the DECLARED
// comparison criteria land as REAL TYPED COLUMNS — direction, role and weight — which is what makes the
// host's Pareto rule auditable in SQL.
func TestExport_FixedSpaceRunRecordsNoPartitionAndTypedCriteria(t *testing.T) {
	dir := capturedRun(t, compareTask())
	out := filepath.Join(t.TempDir(), "compare.db")
	sum, err := Export(dir, out, Options{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if sum.Mode != mode.Compare {
		t.Errorf("mode = %q, want compare", sum.Mode)
	}
	db := openDB(t, out)
	if kind := scalar[string](t, db, `SELECT terminal_kind FROM mode_contracts`); kind != "host_aggregate" {
		t.Errorf("terminal_kind = %q, want host_aggregate", kind)
	}
	// No canonicalization ran at all — the structural claim of the fixed-space modes, asserted against the export.
	for _, table := range []string{"canonicalization_revisions", "canonical_entities", "canonical_map_entries", "mentions", "ballots", "decisions"} {
		if sum.Rows[table] != 0 {
			t.Errorf("a fixed-space run must produce no %s rows, got %d", table, sum.Rows[table])
		}
	}
	blank := scalar[int](t, db, `SELECT COUNT(*) FROM governance_claims WHERE partition_revision_hash = ''`)
	if blank != 0 {
		t.Errorf("%d fixed-space claim(s) carry an EMPTY partition hash — the export must record the honest statement instead", blank)
	}
	stated := scalar[int](t, db, `SELECT COUNT(*) FROM governance_claims WHERE partition_revision_hash = ?`, govern.FixedSpaceNoPartition)
	if stated == 0 {
		t.Error("a fixed-space claim must carry the explicit no-partition statement")
	}
	// The declared criteria, as typed columns a governance query can actually use.
	dims := scalar[int](t, db, `SELECT COUNT(*) FROM criteria WHERE role='dimension' AND direction IN ('higher_is_better','lower_is_better')`)
	if dims != 2 {
		t.Errorf("expected 2 scored dimensions with a declared direction, got %d", dims)
	}
	gates := scalar[int](t, db, `SELECT COUNT(*) FROM criteria WHERE role='filter'`)
	if gates != 1 {
		t.Errorf("expected 1 filter gate, got %d", gates)
	}
	weight := scalar[float64](t, db, `SELECT weight FROM criteria WHERE criterion_id='throughput'`)
	if weight != 2 {
		t.Errorf("the user's declared weight must survive as a REAL column, got %v", weight)
	}
	// The per-cell claims are reachable from the blind envelopes that produced them.
	sources := scalar[int](t, db, `SELECT COUNT(*) FROM governance_claim_sources`)
	if sources == 0 {
		t.Error("a fixed-space claim must record its exact contributing source envelopes")
	}
}

// TestExport_MissingManifestIsRefused pins the capture layer's own signal: the manifest is written last and
// renamed into place, so a directory without one is an UNFINISHED run and there is nothing complete to
// export.
func TestExport_MissingManifestIsRefused(t *testing.T) {
	dir := capturedRun(t, shortlistTask())
	if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	_, err := Export(dir, filepath.Join(t.TempDir(), "x.db"), Options{})
	if err == nil {
		t.Fatal("a run directory with no manifest must be refused")
	}
	if !strings.Contains(err.Error(), "did not finish writing") {
		t.Errorf("the refusal must explain what a missing manifest means, got: %v", err)
	}
}

// TestExport_MalformedArtifactIsRefused pins the second refusal: the export never repairs its input. A
// present-but-corrupt artifact fails the export rather than being skipped, because a database that silently
// omitted what it could not read would look complete and would not be.
func TestExport_MalformedArtifactIsRefused(t *testing.T) {
	dir := capturedRun(t, shortlistTask())
	if err := os.WriteFile(filepath.Join(dir, "rounds.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Export(dir, filepath.Join(t.TempDir(), "x.db"), Options{})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("a malformed artifact must be refused, got: %v", err)
	}
}
