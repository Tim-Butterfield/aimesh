// Package evidence exports a captured run directory to a SQLite database for governance queries.
//
// The run directory is the system of record; the database is derived from it. Exporting the same run
// directory twice must give identical content, which Verify checks. To keep that true:
//
//   - nothing non-deterministic is written (no timestamps, absolute paths, map order or autoincrement ids),
//     and inserts run in a fixed order;
//   - a malformed artifact fails the export instead of being skipped;
//   - foreign keys, STRICT tables, foreign_key_check and integrity_check are enforced, so for example a
//     ballot naming a canonical ID missing from the confirmed revision fails the export.
//
// The driver is the pure-Go modernc.org/sqlite, since builds use CGO_ENABLED=0.
package evidence

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// Summary describes a finished export: the exploration it covers and the row count per table.
type Summary struct {
	RunDir        string         `json:"runDir"`
	DBPath        string         `json:"dbPath"`
	ExplorationID string         `json:"explorationId"`
	Mode          string         `json:"mode"`
	SchemaVersion int            `json:"schemaVersion"`
	Rows          map[string]int `json:"rows"`
	TotalRows     int            `json:"totalRows"`
}

// Export builds the evidence database at dbPath from the run directory at runDir.
//
// The destination is checked before anything is read (see resolveDestination) and the database is
// published with an atomic rename, so an interrupted export never leaves a truncated file.
func Export(runDir, dbPath string, opts Options) (Summary, error) {
	dest, err := resolveDestination(dbPath, opts)
	if err != nil {
		return Summary{}, err
	}
	// The run directory is user input, so problems reading it are configuration faults.
	rec, err := readRun(runDir)
	if err != nil {
		return Summary{}, fault.Wrap(fault.Config, "read the run directory", err)
	}
	sum, err := writeAtomic(runDir, rec, dest)
	if err != nil {
		return Summary{}, err
	}
	// Report the path as the caller wrote it, not the resolved path.
	sum.DBPath = dbPath
	return sum, nil
}

// build writes the evidence database at dbPath from rec. It performs no destination checks and is not
// atomic, so callers must pass a private path: writeAtomic's temporary file or Verify's scratch file.
func build(runDir string, rec *runRecord, dbPath string) (Summary, error) {
	db, err := open(dbPath)
	if err != nil {
		return Summary{}, err
	}
	defer db.Close()
	if _, err := db.Exec(schemaSQL); err != nil {
		return Summary{}, fmt.Errorf("create evidence schema: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return Summary{}, err
	}
	w := &writer{tx: tx, rec: rec, id: rec.Manifest.RunID}
	w.writeAll()
	if w.err != nil {
		_ = tx.Rollback()
		// A foreign-key failure means the input violates a governance invariant, so say so.
		if strings.Contains(strings.ToLower(w.err.Error()), "foreign key") {
			return Summary{}, fmt.Errorf("evidence export REFUSED %s: %w — a foreign-key violation means the run directory does not satisfy a governance invariant (the load-bearing one: a ballot entry, a universe snapshot or a decision entry naming a canonical ID that is ABSENT from the confirmed revision). The export fails rather than recording it", runDir, w.err)
		}
		return Summary{}, w.err
	}
	if err := tx.Commit(); err != nil {
		// Deferred foreign-key violations surface at commit.
		return Summary{}, fmt.Errorf("commit evidence export (a constraint violation here means the run directory does not satisfy a governance invariant — e.g. a ballot naming a canonical ID absent from the confirmed revision): %w", err)
	}
	if err := finalize(db); err != nil {
		return Summary{}, err
	}
	sum := Summary{
		RunDir: runDir, DBPath: dbPath, ExplorationID: rec.Manifest.RunID,
		Mode: effectiveMode(rec), SchemaVersion: ExportSchemaVersion, Rows: map[string]int{},
	}
	for _, t := range exportTables {
		n, cerr := countRows(db, t)
		if cerr != nil {
			return Summary{}, cerr
		}
		sum.Rows[t] = n
		sum.TotalRows += n
	}
	return sum, nil
}

// open opens the database with foreign keys enabled and a fixed page size and journal mode, so two exports
// of the same run produce the same bytes.
func open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=page_size(4096)&_pragma=journal_mode(DELETE)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Pragmas are per connection, so use exactly one.
	db.SetMaxOpenConns(1)
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		db.Close()
		return nil, fmt.Errorf("read foreign_keys pragma: %w", err)
	}
	if fk != 1 {
		db.Close()
		return nil, fmt.Errorf("evidence export: PRAGMA foreign_keys is OFF — refusing to build an evidence database whose constraints are advisory")
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(ExportSchemaVersion)); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// finalize runs foreign_key_check and integrity_check on the committed database.
func finalize(db *sql.DB) error {
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	var violations []string
	for rows.Next() {
		var table, parent string
		var rowid, fkid sql.NullInt64
		if serr := rows.Scan(&table, &rowid, &parent, &fkid); serr != nil {
			rows.Close()
			return serr
		}
		violations = append(violations, fmt.Sprintf("%s → %s (rowid %v)", table, parent, rowid.Int64))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(violations) > 0 {
		return fmt.Errorf("evidence export: foreign_key_check found %d violation(s): %s", len(violations), strings.Join(violations, "; "))
	}
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("evidence export: integrity_check reported %q", result)
	}
	return nil
}

// countRows returns the row count of table, which must come from exportTables.
func countRows(db *sql.DB, table string) (int, error) {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}

// writer writes a run record inside one transaction. It keeps the first error, and exec is a no-op once
// an error is set.
type writer struct {
	tx  *sql.Tx
	rec *runRecord
	id  string
	err error
	// participants holds every identity in the run, keyed by participantID.
	participants map[string]*participant
	// mentions maps (envelopeRef, rawText) to a mention id.
	mentions map[mentionKey]string
	// confirmedRevision is the partition revision the decision and claims were computed at.
	confirmedRevision int
	// elements is the set of declared lineage element ids.
	elements map[string]bool
}

type participant struct {
	id      string
	adapter string
	model   string
	effort  string
	roles   map[string]bool
}

type mentionKey struct{ envelopeRef, raw string }

// exec runs one statement unless an earlier one failed, and records the first error.
func (w *writer) exec(query string, args ...any) {
	if w.err != nil {
		return
	}
	if _, err := w.tx.Exec(query, args...); err != nil {
		w.err = fmt.Errorf("%s: %w", firstLine(query), err)
	}
}

// writeAll writes every table in a fixed dependency order, which both the foreign keys and deterministic
// output require.
func (w *writer) writeAll() {
	w.registerParticipants()
	w.writeExploration()
	w.writeModeContract()
	w.writeRuleVersions()
	w.writeParticipants()
	w.writeRounds()
	w.writeCalls()
	w.writeArtifacts()
	w.writeEnvelopes()
	w.writeDropped()
	w.writeCanonicalization()
	w.writeConfirmation()
	w.writeCriteria()
	w.writeDecision()
	w.writeClaims()
	w.writeNarrative()
	w.writeLineage()
}

// registerParticipants collects every identity referenced in the run, with its roles, before any table that
// references participants is written.
func (w *writer) registerParticipants() {
	w.participants = map[string]*participant{}
	add := func(id schema.ExplorerIdentity, role string) {
		if id.Adapter == "" && id.Model == "" {
			return
		}
		key := participantID(id)
		p, ok := w.participants[key]
		if !ok {
			p = &participant{id: key, adapter: id.Adapter, model: id.Model, effort: id.Effort, roles: map[string]bool{}}
			w.participants[key] = p
		}
		p.roles[role] = true
	}
	for _, e := range w.rec.Manifest.Explorers {
		add(e, "explorer")
	}
	add(w.rec.Manifest.Collator, "collator")
	for _, p := range w.rec.Preflight {
		add(p.Requested, roleOf(p.Role))
	}
	for _, r := range w.rec.Rounds {
		for _, env := range r.Envelopes {
			add(env.Identity, "explorer")
		}
	}
	for _, d := range w.rec.Manifest.Dropped {
		add(schema.ExplorerIdentity{Adapter: d.Adapter, Model: d.Model, Effort: d.Effort}, "explorer")
	}
	for _, row := range append(append([]canon.LedgerRow(nil), w.rec.Ledger...), w.rec.Provisional...) {
		add(row.SourceExplorer, "explorer")
		add(row.DecidedByIdentity, "canonicalizer")
		for _, a := range row.AgreedBy {
			add(a.Identity, "canonicalizer")
		}
	}
	if c := w.rec.Confirmation; c != nil {
		for _, ch := range c.Challenges {
			add(ch.By, "explorer")
		}
		for _, cl := range c.Revision.Canonicalizers {
			add(cl.Identity, "canonicalizer")
		}
	}
	if g := w.rec.Governance; g != nil {
		for _, m := range g.Panel.Members {
			add(m, "explorer")
		}
	}
	if d := w.rec.Decision; d != nil {
		for _, b := range d.Ballots {
			add(b.By, "explorer")
		}
	}
}

// roleOf maps a pre-flight role label, such as canonicalizer-a, to an export role.
func roleOf(role string) string {
	if strings.HasPrefix(role, "canonicalizer") {
		return "canonicalizer"
	}
	if role == "" {
		return "explorer"
	}
	return role
}

func (w *writer) writeExploration() {
	m := w.rec.Manifest
	task, _ := json.Marshal(w.rec.Task)
	var policy govern.CountingPolicy
	panel := govern.Panel{}
	if w.rec.Governance != nil {
		panel = w.rec.Governance.Panel
		policy = panel.Policy
	}
	rulesVersion := ""
	claimsHash := ""
	if w.rec.Governance != nil {
		rulesVersion, claimsHash = w.rec.Governance.RulesVersion, w.rec.Governance.ClaimsHash
	}
	w.exec(`INSERT INTO explorations (
		exploration_id, export_schema_version, manifest_schema_version, status, fault, mode, purpose,
		payload_hash, envelope_id_algo, formulation_source, collator_attempted, counting_policy_hash,
		governance_rules_version, governance_claims_hash, partition_revision_hash, degraded_class, round_count,
		panel_selected, panel_dispatched, panel_eligible, technical_absence, deliberate_abstention,
		respondents, quorum, quorum_met, tie_rule, missing_response_policy, task_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.id, ExportSchemaVersion, m.SchemaVersion, m.Status, m.Fault, effectiveMode(w.rec), w.rec.Task.Purpose,
		m.PayloadHash, m.EnvelopeIDAlgo, string(w.rec.Formulation.Source), b2i(w.rec.Formulation.CollatorAttempted),
		m.CountingPolicyHash, rulesVersion, claimsHash, m.PartitionRevisionHash, m.DegradedTerminalClass,
		len(w.rec.Rounds), panel.Selected, panel.Outcome.Dispatched, panel.Outcome.Eligible,
		panel.Outcome.TechnicalAbsence, panel.Outcome.DeliberateAbstention,
		panel.Respondents(), policy.Quorum, b2i(panel.QuorumMet()), string(policy.TieRule),
		string(policy.MissingResponse), string(task))
}

// writeModeContract records which stages the run performed, derived from the run directory rather than the
// current mode registry.
func (w *writer) writeModeContract() {
	canonicalized := len(w.rec.Ledger) > 0
	confirmed := w.rec.Confirmation != nil
	balloted := w.rec.Decision != nil
	terminal := "collate"
	switch {
	case balloted:
		terminal = "ballot"
	case canonicalized:
		terminal = "canonicalizing"
	case w.fixedSpace():
		terminal = "host_aggregate"
	}
	w.exec(`INSERT INTO mode_contracts (exploration_id, mode, terminal_kind, formulation_free, executed_rounds,
		canonicalized, confirmed, balloted) VALUES (?,?,?,?,?,?,?,?)`,
		w.id, effectiveMode(w.rec), terminal, b2i(!w.rec.Formulation.CollatorAttempted), len(w.rec.Rounds),
		b2i(canonicalized), b2i(confirmed), b2i(balloted))
}

// fixedSpace reports whether any recorded claim carries govern.FixedSpaceNoPartition.
func (w *writer) fixedSpace() bool {
	if w.rec.Governance == nil {
		return false
	}
	for _, c := range w.rec.Governance.Claims {
		if c.PartitionRevisionHash == govern.FixedSpaceNoPartition {
			return true
		}
	}
	return false
}

func (w *writer) writeRuleVersions() {
	m := w.rec.Manifest
	versions := map[string]string{}
	put := func(component, version string) {
		if strings.TrimSpace(version) != "" {
			versions[component] = version
		}
	}
	if w.rec.Governance != nil {
		put("governance", w.rec.Governance.RulesVersion)
		put("counting_policy", w.rec.Governance.Panel.Policy.RulesVersion)
	}
	put("canonicalization_agreement", m.AgreementRuleVersion)
	put("confirmation", m.ConfirmationRule)
	put("decision", m.DecisionRule)
	put("envelope_id", m.EnvelopeIDAlgo)
	if d := w.rec.Decision; d != nil {
		put("decision_inputs", d.Frozen.Inputs.RulesVersion)
		put("presentation_order", d.Frozen.Inputs.Presentation.RuleVersion)
	}
	for _, component := range sortedKeys(versions) {
		w.exec(`INSERT INTO governance_rule_versions (exploration_id, component, version) VALUES (?,?,?)`,
			w.id, component, versions[component])
	}
}

func (w *writer) writeParticipants() {
	for _, key := range sortedParticipantKeys(w.participants) {
		p := w.participants[key]
		w.exec(`INSERT INTO participants (exploration_id, participant_id, roles, adapter, model, effort)
			VALUES (?,?,?,?,?,?)`, w.id, p.id, strings.Join(sortedBoolKeys(p.roles), ","), p.adapter, p.model, p.effort)
	}
	// The frozen panel, in recorded order.
	if w.rec.Governance == nil {
		return
	}
	seen := map[string]bool{}
	for i, m := range w.rec.Governance.Panel.Members {
		key := participantID(m)
		if seen[key] || w.participants[key] == nil {
			continue
		}
		seen[key] = true
		w.exec(`INSERT INTO panel_members (exploration_id, participant_id, position) VALUES (?,?,?)`, w.id, key, i)
	}
}

func (w *writer) writeRounds() {
	for _, r := range w.rec.Rounds {
		kind, hash, items := "", "", 0
		if r.Carried != nil {
			kind, hash, items = string(r.Carried.Discriminant.Kind), r.Carried.ContentHash, len(r.Carried.Payload.Items)
		}
		w.exec(`INSERT INTO rounds (exploration_id, round_index, round_id, blind, payload_hash, carried_kind,
			carried_content_hash, carried_items) VALUES (?,?,?,?,?,?,?,?)`,
			w.id, r.Index, r.ID, b2i(r.Blind), r.PayloadHash, kind, hash, items)
		if r.Carried == nil {
			continue
		}
		for _, ref := range r.Carried.SourceEnvelopeRefs {
			w.exec(`INSERT OR IGNORE INTO artifact_source_refs (exploration_id, round_index, content_hash, envelope_ref)
				VALUES (?,?,?,?)`, w.id, r.Index, r.Carried.ContentHash, ref)
		}
	}
}

// writeCalls records the model calls the run directory shows: pre-flight probes, one call per envelope, and
// the collation if its output exists.
func (w *writer) writeCalls() {
	for _, p := range w.rec.Preflight {
		key := participantID(p.Requested)
		if w.participants[key] == nil {
			continue
		}
		callID := "preflight:" + p.Role
		w.exec(`INSERT INTO calls (exploration_id, call_id, role, phase, round_index, participant_id)
			VALUES (?,?,?,?,NULL,?)`, w.id, callID, roleOf(p.Role), schema.PhasePreflight, key)
		w.exec(`INSERT INTO identity_records (exploration_id, call_id, participant_id, requested_model,
			resolved_model, status, evidence_tier, caveat) VALUES (?,?,?,?,?,?,?,?)`,
			w.id, callID, key, p.Requested.Model, p.ResolvedModel, string(p.Status), "", p.Caveat)
	}
	for _, r := range w.rec.Rounds {
		phase := schema.PhaseExplore
		if w.rec.Decision != nil && r.Index == len(w.rec.Rounds) && r.Index > 1 {
			phase = schema.PhaseBallot
		}
		for _, env := range r.Envelopes {
			key := participantID(env.Identity)
			ref := schema.EnvelopeRef(r.Index, env.Order)
			callID := "call:" + ref
			w.exec(`INSERT INTO calls (exploration_id, call_id, role, phase, round_index, participant_id)
				VALUES (?,?,?,?,?,?)`, w.id, callID, "explorer", phase, r.Index, key)
			w.exec(`INSERT INTO identity_records (exploration_id, call_id, participant_id, requested_model,
				resolved_model, status, evidence_tier, caveat) VALUES (?,?,?,?,?,?,?,?)`,
				w.id, callID, key, env.Identity.Model, "", string(env.IdentityStatus),
				string(env.IdentityEvidence), env.IdentityCaveat)
		}
	}
	// Record the collate call only if its output was captured.
	collator := participantID(w.rec.Manifest.Collator)
	if w.participants[collator] != nil && w.hasArtifact("raw-synthesis.txt", "synthesis.json") {
		w.exec(`INSERT INTO calls (exploration_id, call_id, role, phase, round_index, participant_id)
			VALUES (?,?,?,?,NULL,?)`, w.id, "call:collate", "collator", schema.PhaseSynthesize, collator)
	}
}

// hasArtifact reports whether the manifest lists any of the named run-relative files.
func (w *writer) hasArtifact(names ...string) bool {
	for _, a := range w.rec.Manifest.Artifacts {
		if slices.Contains(names, a.Path) {
			return true
		}
	}
	return false
}

func (w *writer) writeArtifacts() {
	for _, a := range w.rec.Manifest.Artifacts {
		w.exec(`INSERT OR IGNORE INTO artifacts (exploration_id, path, bytes, sha256) VALUES (?,?,?,?)`,
			w.id, a.Path, a.Bytes, a.SHA256)
	}
}

func (w *writer) writeEnvelopes() {
	for _, r := range w.rec.Rounds {
		for _, env := range r.Envelopes {
			ref := schema.EnvelopeRef(r.Index, env.Order)
			body, _ := json.Marshal(env.Response)
			w.exec(`INSERT INTO envelopes (exploration_id, envelope_ref, envelope_id, round_index, panel_order,
				participant_id, identity_status, identity_evidence, identity_caveat, payload_hash, abstained,
				response_json) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				w.id, ref, env.ID, r.Index, env.Order, participantID(env.Identity), string(env.IdentityStatus),
				string(env.IdentityEvidence), env.IdentityCaveat, env.PayloadHash,
				b2i(schema.IsAbstention(env.Response)), string(body))
			for i, rep := range env.Repairs {
				w.exec(`INSERT INTO parse_repairs (exploration_id, subject_kind, subject_id, seq, repair)
					VALUES (?,?,?,?,?)`, w.id, "envelope", ref, i, rep)
			}
		}
	}
}

func (w *writer) writeDropped() {
	for i, d := range w.rec.Manifest.Dropped {
		key := participantID(schema.ExplorerIdentity{Adapter: d.Adapter, Model: d.Model, Effort: d.Effort})
		if w.participants[key] == nil {
			continue
		}
		w.exec(`INSERT INTO dropped_responses (exploration_id, seq, participant_id, reason, raw_path,
			captured_len, full_len, truncated, sha256, full_sha256, exit_code, stderr_excerpt)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.id, i, key, d.Reason, d.RawPath, d.CapturedLen, d.FullLen, b2i(d.Truncated),
			d.SHA256, d.FullSHA256, d.ExitCode, d.StderrExcerpt)
		for j, rep := range d.Repairs {
			w.exec(`INSERT INTO parse_repairs (exploration_id, subject_kind, subject_id, seq, repair)
				VALUES (?,?,?,?,?)`, w.id, "dropped", strconv.Itoa(i), j, rep)
		}
	}
}

// writeCanonicalization writes mentions, revisions, entities and map entries. When confirmation ran, both
// the provisional and confirmed revisions are written.
func (w *writer) writeCanonicalization() {
	w.mentions = map[mentionKey]string{}
	w.confirmedRevision = w.rec.Manifest.LedgerRevision
	if w.confirmedRevision == 0 {
		w.confirmedRevision = 1
	}
	provisionalRevision := max(w.confirmedRevision-1, 1)

	// Mention ids follow file order (confirmed ledger, then provisional) so they are stable across exports.
	envelopeRefs := w.envelopeRefSet()
	next := 0
	register := func(rows []canon.LedgerRow) {
		for _, row := range rows {
			k := mentionKey{row.EnvelopeRef, row.RawNomination}
			if _, seen := w.mentions[k]; seen || !envelopeRefs[row.EnvelopeRef] {
				continue
			}
			id := fmt.Sprintf("mention:%04d", next)
			next++
			w.mentions[k] = id
			w.exec(`INSERT INTO mentions (exploration_id, mention_id, raw_text, envelope_ref, participant_id)
				VALUES (?,?,?,?,?)`, w.id, id, row.RawNomination, row.EnvelopeRef, participantID(row.SourceExplorer))
		}
	}
	register(w.rec.Ledger)
	register(w.rec.Provisional)

	if len(w.rec.Provisional) > 0 {
		w.writeRevision(provisionalRevision, w.rec.Manifest.PriorRevisionHash, "", false, w.rec.Provisional)
	}
	if len(w.rec.Ledger) > 0 {
		w.writeRevision(w.confirmedRevision, w.rec.Manifest.PartitionRevisionHash, w.rec.Manifest.PriorRevisionHash,
			w.rec.Confirmation != nil, w.rec.Ledger)
	}
}

// writeRevision writes one ledger revision, its entities with member counts and single-source flags
// computed from rows, and its map entries in file order.
func (w *writer) writeRevision(revision int, hash, prior string, confirmed bool, rows []canon.LedgerRow) {
	w.exec(`INSERT INTO canonicalization_revisions (exploration_id, revision, revision_hash,
		prior_revision_hash, confirmed, agreement_rule_version, row_count) VALUES (?,?,?,?,?,?,?)`,
		w.id, revision, hash, prior, b2i(confirmed), w.rec.Manifest.AgreementRuleVersion, len(rows))

	order := []string{}
	members := map[string]int{}
	sources := map[string]map[schema.ExplorerIdentity]bool{}
	for _, row := range rows {
		if _, seen := members[row.CanonicalID]; !seen {
			order = append(order, row.CanonicalID)
			sources[row.CanonicalID] = map[schema.ExplorerIdentity]bool{}
		}
		members[row.CanonicalID]++
		sources[row.CanonicalID][row.SourceExplorer] = true
	}
	names := w.entityNames()
	for _, id := range order {
		w.exec(`INSERT INTO canonical_entities (exploration_id, revision, canonical_id, name, single_source,
			member_count) VALUES (?,?,?,?,?,?)`,
			w.id, revision, id, names[id], b2i(len(sources[id]) == 1), members[id])
	}
	for i, row := range rows {
		mention := w.mentions[mentionKey{row.EnvelopeRef, row.RawNomination}]
		if mention == "" {
			continue // the nomination's envelope was not recorded
		}
		w.exec(`INSERT INTO canonical_map_entries (exploration_id, revision, seq, mention_id, canonical_id,
			decided_by_call, decided_by_adapter, decided_by_model, agreed_by_count) VALUES (?,?,?,?,?,?,?,?,?)`,
			w.id, revision, i, mention, row.CanonicalID, row.DecidedByCall,
			row.DecidedByIdentity.Adapter, row.DecidedByIdentity.Model, len(row.AgreedBy))
	}
}

// entityNames maps canonical IDs to labels from the confirmation clusters, claim subject labels and decision
// entries, in that order of preference. IDs with no recorded label are left out.
func (w *writer) entityNames() map[string]string {
	names := map[string]string{}
	if w.rec.Confirmation != nil {
		for _, c := range w.rec.Confirmation.Revision.Clusters {
			names[c.CanonicalID] = c.Name
		}
	}
	if w.rec.Governance != nil {
		for _, c := range w.rec.Governance.Claims {
			if _, ok := names[c.Subject]; !ok && c.SubjectLabel != "" {
				names[c.Subject] = c.SubjectLabel
			}
		}
	}
	if w.rec.Decision != nil {
		for _, e := range w.rec.Decision.Entries {
			if _, ok := names[e.CanonicalID]; !ok {
				names[e.CanonicalID] = e.Name
			}
		}
	}
	return names
}

func (w *writer) writeConfirmation() {
	c := w.rec.Confirmation
	if c == nil {
		return
	}
	// seqOf lets each resolution reference the challenge it resolved.
	seqOf := map[string]int{}
	for i, ch := range c.Challenges {
		w.exec(`INSERT INTO challenges (exploration_id, seq, challenge_type, canonical_id, other_canonical_id,
			raw_nomination, reason, participant_id) VALUES (?,?,?,?,?,?,?,?)`,
			w.id, i, string(ch.Type), ch.CanonicalID, ch.OtherCanonicalID, ch.RawNomination, ch.Reason,
			participantID(ch.By))
		seqOf[challengeKey(ch)] = i
	}
	for i, r := range c.Resolutions {
		var challengeSeq any
		if seq, ok := seqOf[challengeKey(r.Challenge)]; ok {
			challengeSeq = seq
		}
		w.exec(`INSERT INTO resolutions (exploration_id, seq, challenge_seq, action, detail, rule_version)
			VALUES (?,?,?,?,?,?)`, w.id, i, challengeSeq, r.Action, r.Detail, r.RuleVersion)
	}
	for i, m := range c.Contested {
		w.exec(`INSERT INTO contested_mappings (exploration_id, seq, direction, source, reason, rule_version)
			VALUES (?,?,?,?,?,?)`, w.id, i, string(m.Direction), string(m.Source), m.Reason, m.RuleVersion)
		for j, id := range m.HeldCanonicalIDs {
			w.exec(`INSERT INTO contested_mapping_entities (exploration_id, seq, position, canonical_id)
				VALUES (?,?,?,?)`, w.id, i, j, id)
		}
	}
}

// challengeKey identifies a challenge by value, since a recorded resolution embeds its challenge rather than
// an index.
func challengeKey(c canon.Challenge) string {
	return strings.Join([]string{string(c.Type), c.CanonicalID, c.OtherCanonicalID, c.RawNomination,
		c.Reason, participantID(c.By)}, "\x00")
}

// writeCriteria writes criteria from the frozen decision inputs and from the task's compare criteria,
// merged by name, plus the frozen criterion snapshot.
func (w *writer) writeCriteria() {
	type row struct {
		name        string
		origin      string
		aggregation string
		direction   string
		role        string
		weight      float64
		declaredIn  string
		auth        *govern.Authorization
	}
	byName := map[string]*row{}
	order := []string{}
	get := func(name string) *row {
		r, ok := byName[name]
		if !ok {
			r = &row{name: name}
			byName[name] = r
			order = append(order, name)
		}
		return r
	}
	if d := w.rec.Decision; d != nil {
		for _, c := range d.Frozen.Inputs.Criteria {
			r := get(c.Name)
			r.origin, r.aggregation, r.declaredIn = string(c.Origin), string(c.AggregationMethod), "decision_freeze"
			r.auth = c.Authorization
		}
	}
	for _, c := range w.rec.Task.CompareCriteria {
		r := get(c.Name)
		r.direction, r.role, r.weight = string(c.Direction), string(c.EffectiveRole()), c.Weight
		if r.declaredIn == "" {
			r.origin, r.aggregation, r.declaredIn = string(govern.OriginUser), string(govern.AggregationEvidenceSynthesis), "declared_task"
		}
	}
	for _, name := range order {
		r := byName[name]
		w.exec(`INSERT INTO criteria (exploration_id, criterion_id, name, origin, aggregation_method,
			direction, role, weight, declared_in) VALUES (?,?,?,?,?,?,?,?,?)`,
			w.id, r.name, r.name, r.origin, r.aggregation, r.direction, r.role, r.weight, r.declaredIn)
		if r.auth != nil {
			w.exec(`INSERT INTO criterion_authorizations (exploration_id, criterion_id, actor, authorized_at,
				criterion_version, scope) VALUES (?,?,?,?,?,?)`,
				w.id, r.name, r.auth.Actor, r.auth.Timestamp, r.auth.CriterionVersion, r.auth.Scope)
		}
	}
	// The criteria in force under the frozen inputs hash.
	if d := w.rec.Decision; d != nil && d.Frozen.InputsHash != "" {
		for i, c := range d.Frozen.Inputs.Criteria {
			w.exec(`INSERT OR IGNORE INTO criterion_snapshots (exploration_id, snapshot_hash, criterion_id, position)
				VALUES (?,?,?,?)`, w.id, d.Frozen.InputsHash, c.Name, i)
		}
	}
}

func (w *writer) writeDecision() {
	d := w.rec.Decision
	if d == nil {
		return
	}
	in := d.Frozen.Inputs
	w.exec(`INSERT INTO decisions (exploration_id, inputs_hash, method, rule_version, rules_version,
		universe_revision_hash, confirmed_revision, shortlist_size, quorum, tie_rule, missing_response,
		policy_hash, presentation_seed, presentation_rule, ballots_cast, quorum_met, tie_outcome, rendering)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.id, d.Frozen.InputsHash, string(in.Method), d.RuleVersion, in.RulesVersion, in.UniverseRevisionHash,
		w.confirmedRevision, in.ShortlistSize, in.Quorum, string(in.TieRule), string(in.MissingResponse),
		in.PolicyHash, in.Presentation.Seed, in.Presentation.RuleVersion, d.Cast, b2i(d.QuorumMet),
		d.TieOutcome, d.Rendering)
	// The frozen universe in presented order. The foreign key requires each ID to exist in the confirmed
	// revision.
	for i, id := range in.Presentation.Order {
		w.exec(`INSERT INTO ballot_universe_snapshots (exploration_id, position, canonical_id, confirmed_revision)
			VALUES (?,?,?,?)`, w.id, i, id, w.confirmedRevision)
	}
	for i, b := range d.Ballots {
		w.exec(`INSERT INTO ballots (exploration_id, ballot_id, participant_id, envelope_ref) VALUES (?,?,?,?)`,
			w.id, i, participantID(b.By), b.EnvelopeRef)
		for j, id := range b.Ranking {
			w.exec(`INSERT INTO ballot_entries (exploration_id, ballot_id, kind, position, canonical_id,
				confirmed_revision) VALUES (?,?,?,?,?,?)`, w.id, i, "ranking", j, id, w.confirmedRevision)
		}
		for j, id := range b.Approved {
			w.exec(`INSERT INTO ballot_entries (exploration_id, ballot_id, kind, position, canonical_id,
				confirmed_revision) VALUES (?,?,?,?,?,?)`, w.id, i, "approval", j, id, w.confirmedRevision)
		}
	}
	for _, e := range d.Entries {
		w.exec(`INSERT INTO decision_entries (exploration_id, canonical_id, confirmed_revision, rank, name,
			score, approvals, support, tied, shortlisted, reason) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			w.id, e.CanonicalID, w.confirmedRevision, e.Rank, e.Name, e.Score, e.Approvals, e.Support,
			b2i(e.Tied), b2i(e.Shortlisted), e.Reason)
	}
}

// writeClaims writes each governance claim as typed columns, with one row per contributing envelope.
func (w *writer) writeClaims() {
	g := w.rec.Governance
	if g == nil {
		return
	}
	envelopeRefs := w.envelopeRefSet()
	for i, c := range g.Claims {
		claimID := claimID(c)
		var low, high any
		var direction, note any
		if c.Sensitivity != nil {
			low, high = c.Sensitivity.Low, c.Sensitivity.High
			direction, note = c.Sensitivity.Direction, c.Sensitivity.Note
		}
		w.exec(`INSERT OR IGNORE INTO governance_claims (exploration_id, claim_id, seq, query, subject,
			subject_label, value, k_panel, m_panel, k_respondents, m_respondents, label, definitive,
			formulation_hash, partition_revision_hash, rules_version, policy_hash, baseline_round_id,
			sensitivity_low, sensitivity_high, sensitivity_direction, sensitivity_note)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.id, claimID, i, c.Query, c.Subject, c.SubjectLabel, c.Value,
			c.KOfPanel.K, c.KOfPanel.M, c.KOfRespondents.K, c.KOfRespondents.M,
			string(c.Label), b2i(c.Label.Definitive()), c.FormulationHash, c.PartitionRevisionHash,
			c.RulesVersion, c.PolicyHash, c.BaselineRoundID, low, high, direction, note)
		for _, ref := range c.ContributingSourceIDs {
			if !envelopeRefs[ref] {
				continue // the envelope was not recorded
			}
			w.exec(`INSERT OR IGNORE INTO governance_claim_sources (exploration_id, claim_id, envelope_ref)
				VALUES (?,?,?)`, w.id, claimID, ref)
		}
	}
}

func (w *writer) writeNarrative() {
	if w.rec.Governance == nil {
		return
	}
	for i, n := range w.rec.Governance.CollatorNarrative {
		w.exec(`INSERT INTO collator_narrative (exploration_id, seq, adapter, model, effort, phase, prose)
			VALUES (?,?,?,?,?,?,?)`, w.id, i, n.Source.Adapter, n.Source.Model, n.Source.Effort, n.Phase, n.Prose)
	}
}

// writeLineage writes lineage elements and edges. Edges point from source to derived element, so recursive
// queries can walk either direction.
func (w *writer) writeLineage() {
	w.elements = map[string]bool{}
	element := func(id, kind, label string, roundIndex any) {
		if w.elements[id] {
			return
		}
		w.elements[id] = true
		w.exec(`INSERT INTO artifact_elements (exploration_id, element_id, kind, label, round_index)
			VALUES (?,?,?,?,?)`, w.id, id, kind, label, roundIndex)
	}
	edge := func(parent, child, relation string) {
		if !w.elements[parent] || !w.elements[child] {
			return // both ends must be declared elements
		}
		w.exec(`INSERT OR IGNORE INTO lineage_edges (exploration_id, parent_element_id, child_element_id, relation)
			VALUES (?,?,?,?)`, w.id, parent, child, relation)
	}

	for _, r := range w.rec.Rounds {
		for _, env := range r.Envelopes {
			ref := schema.EnvelopeRef(r.Index, env.Order)
			element("env:"+ref, "envelope", participantID(env.Identity), r.Index)
		}
	}
	// Mentions, then the entities they map to, per revision.
	for _, row := range append(append([]canon.LedgerRow(nil), w.rec.Ledger...), w.rec.Provisional...) {
		id, ok := w.mentions[mentionKey{row.EnvelopeRef, row.RawNomination}]
		if !ok {
			continue
		}
		element("mention:"+id, "mention", row.RawNomination, nil)
		edge("env:"+row.EnvelopeRef, "mention:"+id, "nominated")
	}
	names := w.entityNames()
	writeEntityEdges := func(revision int, rows []canon.LedgerRow) {
		for _, row := range rows {
			eid := entityElementID(revision, row.CanonicalID)
			element(eid, "canonical_entity", names[row.CanonicalID], nil)
			if id, ok := w.mentions[mentionKey{row.EnvelopeRef, row.RawNomination}]; ok {
				edge("mention:"+id, eid, "canonicalized_to")
			}
		}
	}
	if len(w.rec.Provisional) > 0 {
		writeEntityEdges(w.confirmedRevision-1, w.rec.Provisional)
	}
	writeEntityEdges(w.confirmedRevision, w.rec.Ledger)

	if d := w.rec.Decision; d != nil {
		for i, b := range d.Ballots {
			bid := fmt.Sprintf("ballot:%d", i)
			element(bid, "ballot", participantID(b.By), nil)
			edge("env:"+b.EnvelopeRef, bid, "cast_in")
		}
		for _, e := range d.Entries {
			did := "decision:" + e.CanonicalID
			element(did, "decision_entry", e.Name, nil)
			edge(entityElementID(w.confirmedRevision, e.CanonicalID), did, "tallied_as")
		}
	}
	if g := w.rec.Governance; g != nil {
		for _, c := range g.Claims {
			cid := "claim:" + claimID(c)
			element(cid, "claim", c.SubjectLabel, nil)
			edge(entityElementID(w.confirmedRevision, c.Subject), cid, "claimed_over")
			for _, ref := range c.ContributingSourceIDs {
				edge("env:"+ref, cid, "contributed_to")
			}
		}
	}
}

// envelopeRefSet returns the set of envelope refs recorded in the run.
func (w *writer) envelopeRefSet() map[string]bool {
	out := map[string]bool{}
	for _, r := range w.rec.Rounds {
		for _, env := range r.Envelopes {
			out[schema.EnvelopeRef(r.Index, env.Order)] = true
		}
	}
	return out
}

// participantID returns the adapter|model|effort key for id.
func participantID(id schema.ExplorerIdentity) string {
	return id.Adapter + "|" + id.Model + "|" + id.Effort
}

// entityElementID returns a canonical entity's lineage element id, qualified by revision.
func entityElementID(revision int, canonicalID string) string {
	return fmt.Sprintf("entity:%d:%s", revision, canonicalID)
}

// claimID returns a claim's id: its query and subject.
func claimID(c govern.Claim) string { return c.Query + "::" + c.Subject }

// effectiveMode returns the run's mode from the manifest, then the task, or "unknown".
func effectiveMode(rec *runRecord) string {
	if rec.Manifest.Mode != "" {
		return rec.Manifest.Mode
	}
	if rec.Task.Mode != "" {
		return rec.Task.Mode
	}
	return "unknown"
}

// b2i converts a bool to 0 or 1.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedParticipantKeys(m map[string]*participant) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// firstLine returns the start of a SQL statement, for use in error messages.
func firstLine(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexAny(q, "\n("); i > 0 {
		q = q[:i]
	}
	return strings.TrimSpace(q)
}
