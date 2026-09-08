package evidence

// This file holds the DERIVED EVIDENCE SCHEMA (design §9): a RELATIONAL CORE plus exactly ONE
// variable-depth structure (`lineage_edges`). Both halves of that sentence were decisions:
//
//   - RELATIONAL, NOT A PROPERTY GRAPH. Rounds, envelopes, mentions, canonical entities, ballots, decisions
//     and claims all have fixed, known shapes with fixed relationships. Modeling them as generic
//     (node, edge, property) triples would throw away every constraint the database could otherwise enforce
//     for free — and the constraints are the point of exporting at all.
//   - ONE lineage table, because exactly one thing here IS variable-depth: how an element of the terminal
//     result traces back through the rounds to the blind responses it came from. That is a recursive-CTE
//     traversal, and it is the only one.
//
// Two schema rules follow from §9 and are applied without exception below:
//
//   - GOVERNANCE VALUES ARE REAL TYPED COLUMNS. A claim's k, its denominators, its label, its rules version
//     and its pinned hashes are columns — because the reason to export at all is to make governance SQL
//     runnable, and `json_extract(payload, '$.kOfPanel.k')` is not that. JSON appears in exactly two places,
//     both ORIGINAL PAYLOADS the export does not interpret: an envelope's response and the declared task.
//   - FK INTEGRITY ENFORCES GOVERNANCE INVARIANTS. The load-bearing one, named explicitly in §9: a
//     `ballot_entries` row references `canonical_entities` AT THE CONFIRMED REVISION, so a ballot naming a
//     candidate that is not in the confirmed universe is a CONSTRAINT VIOLATION at export time — not a
//     silent pass, and not a check some reader has to remember to run.
//
// Every table is STRICT (types are enforced, not advisory) and every foreign key is real —
// `PRAGMA foreign_keys=ON` is set on the connection and `foreign_key_check` + `integrity_check` run at
// finalize. A composite foreign key needs a UNIQUE parent key, which every referenced table's PRIMARY KEY
// provides.

// ExportSchemaVersion is the version of the schema below. It is stored in `explorations` (and as the
// database's user_version) so a consumer can tell which shape it is reading — the same discipline every
// other machine surface in exploremesh follows.
const ExportSchemaVersion = 1

// schemaSQL is the complete DDL, executed once at export. It is one string rather than a migration chain on
// purpose: this database is DERIVED and disposable. It is never migrated in place, because the system of
// record is the append-only run directory and a stale export is rebuilt, not patched.
const schemaSQL = `
-- The exploration itself: one row, carrying the run-level facts + the frozen panel outcome.
CREATE TABLE explorations (
  exploration_id          TEXT PRIMARY KEY,
  export_schema_version   INTEGER NOT NULL,
  manifest_schema_version INTEGER NOT NULL,
  status                  TEXT NOT NULL,
  fault                   TEXT NOT NULL,
  mode                    TEXT NOT NULL,
  purpose                 TEXT NOT NULL,
  payload_hash            TEXT NOT NULL,
  envelope_id_algo        TEXT NOT NULL,
  formulation_source      TEXT NOT NULL,
  collator_attempted      INTEGER NOT NULL,
  counting_policy_hash    TEXT NOT NULL,
  governance_rules_version TEXT NOT NULL,
  governance_claims_hash  TEXT NOT NULL,
  partition_revision_hash TEXT NOT NULL,
  degraded_class          TEXT NOT NULL,
  round_count             INTEGER NOT NULL,
  panel_selected          INTEGER NOT NULL,
  panel_dispatched        INTEGER NOT NULL,
  panel_eligible          INTEGER NOT NULL,
  technical_absence       INTEGER NOT NULL,
  deliberate_abstention   INTEGER NOT NULL,
  respondents             INTEGER NOT NULL,
  quorum                  INTEGER NOT NULL,
  quorum_met              INTEGER NOT NULL,
  tie_rule                TEXT NOT NULL,
  missing_response_policy TEXT NOT NULL,
  -- The ORIGINAL declared task, verbatim. One of the two JSON columns in the schema (see the file comment):
  -- the export does not interpret it, it preserves it.
  task_json               TEXT NOT NULL
) STRICT;

-- What the mode contract DID, derived from the run directory alone (never from the live registry — an
-- export must describe the run it is reading, not the code that happens to be compiled today).
CREATE TABLE mode_contracts (
  exploration_id   TEXT NOT NULL,
  mode             TEXT NOT NULL,
  terminal_kind    TEXT NOT NULL,   -- collate | canonicalizing | ballot | host_aggregate
  formulation_free INTEGER NOT NULL,
  executed_rounds  INTEGER NOT NULL,
  canonicalized    INTEGER NOT NULL,
  confirmed        INTEGER NOT NULL,
  balloted         INTEGER NOT NULL,
  PRIMARY KEY (exploration_id, mode),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

-- Every versioned rule the run's values were computed under, one row per component.
CREATE TABLE governance_rule_versions (
  exploration_id TEXT NOT NULL,
  component      TEXT NOT NULL,
  version        TEXT NOT NULL,
  PRIMARY KEY (exploration_id, component),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

-- Every identity that participated in any role. participant_id is the full (adapter, model, effort) triple,
-- because attribution keys on the whole triple and never on the model alone.
CREATE TABLE participants (
  exploration_id TEXT NOT NULL,
  participant_id TEXT NOT NULL,
  roles          TEXT NOT NULL,   -- comma-separated, sorted: explorer,collator,canonicalizer
  adapter        TEXT NOT NULL,
  model          TEXT NOT NULL,
  effort         TEXT NOT NULL,
  PRIMARY KEY (exploration_id, participant_id),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

-- The FROZEN panel: membership fixed before any judgment was solicited.
CREATE TABLE panel_members (
  exploration_id TEXT NOT NULL,
  participant_id TEXT NOT NULL,
  position       INTEGER NOT NULL,
  PRIMARY KEY (exploration_id, participant_id),
  FOREIGN KEY (exploration_id, participant_id) REFERENCES participants(exploration_id, participant_id)
) STRICT;

CREATE TABLE rounds (
  exploration_id       TEXT NOT NULL,
  round_index          INTEGER NOT NULL,
  round_id             TEXT NOT NULL,
  blind                INTEGER NOT NULL,
  payload_hash         TEXT NOT NULL,
  carried_kind         TEXT NOT NULL,
  carried_content_hash TEXT NOT NULL,
  carried_items        INTEGER NOT NULL,
  PRIMARY KEY (exploration_id, round_index),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

-- One row per model call the run RECORDED. round_index is NULL for a call that belongs to no round (the
-- identity pre-flight probe, the terminal collation).
CREATE TABLE calls (
  exploration_id TEXT NOT NULL,
  call_id        TEXT NOT NULL,
  role           TEXT NOT NULL,
  phase          TEXT NOT NULL,
  round_index    INTEGER,
  participant_id TEXT NOT NULL,
  PRIMARY KEY (exploration_id, call_id),
  FOREIGN KEY (exploration_id, participant_id) REFERENCES participants(exploration_id, participant_id),
  FOREIGN KEY (exploration_id, round_index) REFERENCES rounds(exploration_id, round_index)
) STRICT;

-- The identity verdict for one call: what was requested, what resolved, and how it was classified.
CREATE TABLE identity_records (
  exploration_id  TEXT NOT NULL,
  call_id         TEXT NOT NULL,
  participant_id  TEXT NOT NULL,
  requested_model TEXT NOT NULL,
  resolved_model  TEXT NOT NULL,
  status          TEXT NOT NULL,
  evidence_tier   TEXT NOT NULL,
  caveat          TEXT NOT NULL,
  PRIMARY KEY (exploration_id, call_id),
  FOREIGN KEY (exploration_id, call_id) REFERENCES calls(exploration_id, call_id),
  FOREIGN KEY (exploration_id, participant_id) REFERENCES participants(exploration_id, participant_id)
) STRICT;

-- The run directory's own file index, digests included: the bridge back to the system of record.
CREATE TABLE artifacts (
  exploration_id TEXT NOT NULL,
  path           TEXT NOT NULL,
  bytes          INTEGER NOT NULL,
  sha256         TEXT NOT NULL,
  PRIMARY KEY (exploration_id, path),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

-- Which blind envelopes a carried round artifact was projected FROM.
CREATE TABLE artifact_source_refs (
  exploration_id TEXT NOT NULL,
  round_index    INTEGER NOT NULL,
  content_hash   TEXT NOT NULL,
  envelope_ref   TEXT NOT NULL,
  PRIMARY KEY (exploration_id, round_index, envelope_ref),
  FOREIGN KEY (exploration_id, round_index) REFERENCES rounds(exploration_id, round_index)
) STRICT;

-- Every JSON-extraction repair the host applied before a body parsed (or failed to).
CREATE TABLE parse_repairs (
  exploration_id TEXT NOT NULL,
  subject_kind   TEXT NOT NULL,   -- envelope | dropped
  subject_id     TEXT NOT NULL,
  seq            INTEGER NOT NULL,
  repair         TEXT NOT NULL,
  PRIMARY KEY (exploration_id, subject_kind, subject_id, seq),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

CREATE TABLE envelopes (
  exploration_id    TEXT NOT NULL,
  envelope_ref      TEXT NOT NULL,   -- envelope#k (round 1) / rN:envelope#k (a later round)
  envelope_id       TEXT NOT NULL,
  round_index       INTEGER NOT NULL,
  panel_order       INTEGER NOT NULL,
  participant_id    TEXT NOT NULL,
  identity_status   TEXT NOT NULL,
  identity_evidence TEXT NOT NULL,
  identity_caveat   TEXT NOT NULL,
  payload_hash      TEXT NOT NULL,
  abstained         INTEGER NOT NULL,
  -- The ORIGINAL response payload, verbatim (the second and last JSON column).
  response_json     TEXT NOT NULL,
  PRIMARY KEY (exploration_id, envelope_ref),
  FOREIGN KEY (exploration_id, round_index) REFERENCES rounds(exploration_id, round_index),
  FOREIGN KEY (exploration_id, participant_id) REFERENCES participants(exploration_id, participant_id)
) STRICT;

-- A response that never became an envelope, with the audited reason + the raw-body provenance.
CREATE TABLE dropped_responses (
  exploration_id TEXT NOT NULL,
  seq            INTEGER NOT NULL,
  participant_id TEXT NOT NULL,
  reason         TEXT NOT NULL,
  raw_path       TEXT NOT NULL,
  captured_len   INTEGER NOT NULL,
  full_len       INTEGER NOT NULL,
  truncated      INTEGER NOT NULL,
  sha256         TEXT NOT NULL,
  full_sha256    TEXT NOT NULL,
  exit_code      INTEGER NOT NULL,
  stderr_excerpt TEXT NOT NULL,
  PRIMARY KEY (exploration_id, seq),
  FOREIGN KEY (exploration_id, participant_id) REFERENCES participants(exploration_id, participant_id)
) STRICT;

-- One MENTION = one raw nomination as one explorer wrote it in one envelope. It is the unit the
-- canonicalization ledger maps, and the unit lineage starts from.
CREATE TABLE mentions (
  exploration_id TEXT NOT NULL,
  mention_id     TEXT NOT NULL,
  raw_text       TEXT NOT NULL,
  envelope_ref   TEXT NOT NULL,
  participant_id TEXT NOT NULL,
  PRIMARY KEY (exploration_id, mention_id),
  FOREIGN KEY (exploration_id, envelope_ref) REFERENCES envelopes(exploration_id, envelope_ref),
  FOREIGN KEY (exploration_id, participant_id) REFERENCES participants(exploration_id, participant_id)
) STRICT;

-- The APPEND-ONLY merge-ledger, one row per revision. A confirmation revision is a NEW row; the superseded
-- revision is retained, exactly as on disk.
CREATE TABLE canonicalization_revisions (
  exploration_id         TEXT NOT NULL,
  revision               INTEGER NOT NULL,
  revision_hash          TEXT NOT NULL,
  prior_revision_hash    TEXT NOT NULL,
  confirmed              INTEGER NOT NULL,
  agreement_rule_version TEXT NOT NULL,
  row_count              INTEGER NOT NULL,
  PRIMARY KEY (exploration_id, revision),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

CREATE TABLE canonical_entities (
  exploration_id TEXT NOT NULL,
  revision       INTEGER NOT NULL,
  canonical_id   TEXT NOT NULL,
  name           TEXT NOT NULL,
  single_source  INTEGER NOT NULL,
  member_count   INTEGER NOT NULL,
  PRIMARY KEY (exploration_id, revision, canonical_id),
  FOREIGN KEY (exploration_id, revision) REFERENCES canonicalization_revisions(exploration_id, revision)
) STRICT;

CREATE TABLE canonical_map_entries (
  exploration_id     TEXT NOT NULL,
  revision           INTEGER NOT NULL,
  seq                INTEGER NOT NULL,
  mention_id         TEXT NOT NULL,
  canonical_id       TEXT NOT NULL,
  decided_by_call    TEXT NOT NULL,
  decided_by_adapter TEXT NOT NULL,
  decided_by_model   TEXT NOT NULL,
  agreed_by_count    INTEGER NOT NULL,
  PRIMARY KEY (exploration_id, revision, seq),
  FOREIGN KEY (exploration_id, mention_id) REFERENCES mentions(exploration_id, mention_id),
  FOREIGN KEY (exploration_id, revision, canonical_id) REFERENCES canonical_entities(exploration_id, revision, canonical_id)
) STRICT;

-- The binding confirmation round. canonical_id is NOT a foreign key here on purpose: a challenge targets the
-- PROVISIONAL revision, and a missing_item challenge names no entity at all.
CREATE TABLE challenges (
  exploration_id     TEXT NOT NULL,
  seq                INTEGER NOT NULL,
  challenge_type     TEXT NOT NULL,
  canonical_id       TEXT NOT NULL,
  other_canonical_id TEXT NOT NULL,
  raw_nomination     TEXT NOT NULL,
  reason             TEXT NOT NULL,
  participant_id     TEXT NOT NULL,
  PRIMARY KEY (exploration_id, seq),
  FOREIGN KEY (exploration_id, participant_id) REFERENCES participants(exploration_id, participant_id)
) STRICT;

CREATE TABLE resolutions (
  exploration_id TEXT NOT NULL,
  seq            INTEGER NOT NULL,
  challenge_seq  INTEGER,
  action         TEXT NOT NULL,
  detail         TEXT NOT NULL,
  rule_version   TEXT NOT NULL,
  PRIMARY KEY (exploration_id, seq),
  FOREIGN KEY (exploration_id, challenge_seq) REFERENCES challenges(exploration_id, seq)
) STRICT;

CREATE TABLE contested_mappings (
  exploration_id TEXT NOT NULL,
  seq            INTEGER NOT NULL,
  direction      TEXT NOT NULL,
  source         TEXT NOT NULL,
  reason         TEXT NOT NULL,
  rule_version   TEXT NOT NULL,
  PRIMARY KEY (exploration_id, seq),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

CREATE TABLE contested_mapping_entities (
  exploration_id TEXT NOT NULL,
  seq            INTEGER NOT NULL,
  position       INTEGER NOT NULL,
  canonical_id   TEXT NOT NULL,
  PRIMARY KEY (exploration_id, seq, position),
  FOREIGN KEY (exploration_id, seq) REFERENCES contested_mappings(exploration_id, seq)
) STRICT;

-- Criteria carry their ORIGIN + AGGREGATION METHOD (§4) and, for a fixed-space comparison, the DIRECTION,
-- ROLE and user WEIGHT that decide the host's Pareto rule — all as typed columns.
CREATE TABLE criteria (
  exploration_id     TEXT NOT NULL,
  criterion_id       TEXT NOT NULL,
  name               TEXT NOT NULL,
  origin             TEXT NOT NULL,
  aggregation_method TEXT NOT NULL,
  direction          TEXT NOT NULL,
  role               TEXT NOT NULL,
  weight             REAL NOT NULL,
  declared_in        TEXT NOT NULL,  -- decision_freeze | declared_task
  PRIMARY KEY (exploration_id, criterion_id),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

CREATE TABLE criterion_authorizations (
  exploration_id    TEXT NOT NULL,
  criterion_id      TEXT NOT NULL,
  actor             TEXT NOT NULL,
  authorized_at     TEXT NOT NULL,
  criterion_version TEXT NOT NULL,
  scope             TEXT NOT NULL,
  PRIMARY KEY (exploration_id, criterion_id),
  FOREIGN KEY (exploration_id, criterion_id) REFERENCES criteria(exploration_id, criterion_id)
) STRICT;

-- The criterion set as it was FROZEN, keyed by the frozen inputs hash: the evidence that the criteria
-- predate the judgments.
CREATE TABLE criterion_snapshots (
  exploration_id TEXT NOT NULL,
  snapshot_hash  TEXT NOT NULL,
  criterion_id   TEXT NOT NULL,
  position       INTEGER NOT NULL,
  PRIMARY KEY (exploration_id, snapshot_hash, criterion_id),
  FOREIGN KEY (exploration_id, criterion_id) REFERENCES criteria(exploration_id, criterion_id)
) STRICT;

CREATE TABLE decisions (
  exploration_id         TEXT PRIMARY KEY,
  inputs_hash            TEXT NOT NULL,
  method                 TEXT NOT NULL,
  rule_version           TEXT NOT NULL,
  rules_version          TEXT NOT NULL,
  universe_revision_hash TEXT NOT NULL,
  confirmed_revision     INTEGER NOT NULL,
  shortlist_size         INTEGER NOT NULL,
  quorum                 INTEGER NOT NULL,
  tie_rule               TEXT NOT NULL,
  missing_response       TEXT NOT NULL,
  policy_hash            TEXT NOT NULL,
  presentation_seed      TEXT NOT NULL,
  presentation_rule      TEXT NOT NULL,
  ballots_cast           INTEGER NOT NULL,
  quorum_met             INTEGER NOT NULL,
  tie_outcome            TEXT NOT NULL,
  rendering              TEXT NOT NULL,
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id),
  FOREIGN KEY (exploration_id, confirmed_revision) REFERENCES canonicalization_revisions(exploration_id, revision)
) STRICT;

-- The frozen candidate universe, in the exact PRESENTED order. Every row must name an entity of the
-- confirmed revision — enforced, not assumed.
CREATE TABLE ballot_universe_snapshots (
  exploration_id     TEXT NOT NULL,
  position           INTEGER NOT NULL,
  canonical_id       TEXT NOT NULL,
  confirmed_revision INTEGER NOT NULL,
  PRIMARY KEY (exploration_id, position),
  FOREIGN KEY (exploration_id) REFERENCES decisions(exploration_id),
  FOREIGN KEY (exploration_id, confirmed_revision, canonical_id) REFERENCES canonical_entities(exploration_id, revision, canonical_id)
) STRICT;

CREATE TABLE ballots (
  exploration_id TEXT NOT NULL,
  ballot_id      INTEGER NOT NULL,
  participant_id TEXT NOT NULL,
  envelope_ref   TEXT NOT NULL,
  PRIMARY KEY (exploration_id, ballot_id),
  FOREIGN KEY (exploration_id) REFERENCES decisions(exploration_id),
  FOREIGN KEY (exploration_id, participant_id) REFERENCES participants(exploration_id, participant_id),
  FOREIGN KEY (exploration_id, envelope_ref) REFERENCES envelopes(exploration_id, envelope_ref)
) STRICT;

-- THE governance invariant §9 names: a ballot entry referencing a canonical ID that is absent from the
-- CONFIRMED revision is a foreign-key violation, and the export fails rather than silently accepting it.
CREATE TABLE ballot_entries (
  exploration_id     TEXT NOT NULL,
  ballot_id          INTEGER NOT NULL,
  kind               TEXT NOT NULL,   -- ranking | approval
  position           INTEGER NOT NULL,
  canonical_id       TEXT NOT NULL,
  confirmed_revision INTEGER NOT NULL,
  PRIMARY KEY (exploration_id, ballot_id, kind, position),
  FOREIGN KEY (exploration_id, ballot_id) REFERENCES ballots(exploration_id, ballot_id),
  FOREIGN KEY (exploration_id, confirmed_revision, canonical_id) REFERENCES canonical_entities(exploration_id, revision, canonical_id)
) STRICT;

CREATE TABLE decision_entries (
  exploration_id     TEXT NOT NULL,
  canonical_id       TEXT NOT NULL,
  confirmed_revision INTEGER NOT NULL,
  rank               INTEGER NOT NULL,
  name               TEXT NOT NULL,
  score              INTEGER NOT NULL,
  approvals          INTEGER NOT NULL,
  support            INTEGER NOT NULL,
  tied               INTEGER NOT NULL,
  shortlisted        INTEGER NOT NULL,
  reason             TEXT NOT NULL,
  PRIMARY KEY (exploration_id, canonical_id),
  FOREIGN KEY (exploration_id) REFERENCES decisions(exploration_id),
  FOREIGN KEY (exploration_id, confirmed_revision, canonical_id) REFERENCES canonical_entities(exploration_id, revision, canonical_id)
) STRICT;

-- Every emitted governance claim, with its query id/version, params, pinned hashes, rules version, value and
-- both denominators as REAL COLUMNS (§9). partition_revision_hash carries the fixed-space "no partition"
-- statement verbatim for a Compare/Forecast run — an honest sentence rather than an empty string.
CREATE TABLE governance_claims (
  exploration_id          TEXT NOT NULL,
  claim_id                TEXT NOT NULL,
  seq                     INTEGER NOT NULL,
  query                   TEXT NOT NULL,
  subject                 TEXT NOT NULL,
  subject_label           TEXT NOT NULL,
  value                   INTEGER NOT NULL,
  k_panel                 INTEGER NOT NULL,
  m_panel                 INTEGER NOT NULL,
  k_respondents           INTEGER NOT NULL,
  m_respondents           INTEGER NOT NULL,
  label                   TEXT NOT NULL,
  definitive              INTEGER NOT NULL,
  formulation_hash        TEXT NOT NULL,
  partition_revision_hash TEXT NOT NULL,
  rules_version           TEXT NOT NULL,
  policy_hash             TEXT NOT NULL,
  baseline_round_id       TEXT NOT NULL,
  sensitivity_low         INTEGER,
  sensitivity_high        INTEGER,
  sensitivity_direction   TEXT,
  sensitivity_note        TEXT,
  PRIMARY KEY (exploration_id, claim_id),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

-- "the EXACT contributing source IDs" (§9), as rows rather than as a list inside a blob.
CREATE TABLE governance_claim_sources (
  exploration_id TEXT NOT NULL,
  claim_id       TEXT NOT NULL,
  envelope_ref   TEXT NOT NULL,
  PRIMARY KEY (exploration_id, claim_id, envelope_ref),
  FOREIGN KEY (exploration_id, claim_id) REFERENCES governance_claims(exploration_id, claim_id),
  FOREIGN KEY (exploration_id, envelope_ref) REFERENCES envelopes(exploration_id, envelope_ref)
) STRICT;

-- The quarantined model-prose namespace (§0 F-C). It is a separate table for the same reason it is a
-- separate field: so no query that reads a governance value can accidentally read a model's words.
CREATE TABLE collator_narrative (
  exploration_id TEXT NOT NULL,
  seq            INTEGER NOT NULL,
  adapter        TEXT NOT NULL,
  model          TEXT NOT NULL,
  effort         TEXT NOT NULL,
  phase          TEXT NOT NULL,
  prose          TEXT NOT NULL,
  PRIMARY KEY (exploration_id, seq),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

-- The addressable units of the result, and the ONE variable-depth structure over them.
CREATE TABLE artifact_elements (
  exploration_id TEXT NOT NULL,
  element_id     TEXT NOT NULL,
  kind           TEXT NOT NULL,   -- envelope | mention | canonical_entity | ballot | decision_entry | claim
  label          TEXT NOT NULL,
  round_index    INTEGER,
  PRIMARY KEY (exploration_id, element_id),
  FOREIGN KEY (exploration_id) REFERENCES explorations(exploration_id)
) STRICT;

CREATE TABLE lineage_edges (
  exploration_id    TEXT NOT NULL,
  parent_element_id TEXT NOT NULL,
  child_element_id  TEXT NOT NULL,
  relation          TEXT NOT NULL,
  PRIMARY KEY (exploration_id, parent_element_id, child_element_id, relation),
  FOREIGN KEY (exploration_id, parent_element_id) REFERENCES artifact_elements(exploration_id, element_id),
  FOREIGN KEY (exploration_id, child_element_id) REFERENCES artifact_elements(exploration_id, element_id)
) STRICT;

CREATE INDEX idx_lineage_parent ON lineage_edges(exploration_id, parent_element_id);
CREATE INDEX idx_lineage_child ON lineage_edges(exploration_id, child_element_id);
CREATE INDEX idx_claims_query ON governance_claims(exploration_id, query);
CREATE INDEX idx_envelopes_round ON envelopes(exploration_id, round_index);
`

// exportTables lists every table in a fixed order. It drives the canonical dump (and therefore the
// rebuild-and-compare invariant), so it is a declared list rather than a query against sqlite_master: the
// comparison must fail if a table is ADDED and not accounted for, which a self-discovering dump would hide.
var exportTables = []string{
	"explorations",
	"mode_contracts",
	"governance_rule_versions",
	"participants",
	"panel_members",
	"rounds",
	"calls",
	"identity_records",
	"artifacts",
	"artifact_source_refs",
	"parse_repairs",
	"envelopes",
	"dropped_responses",
	"mentions",
	"canonicalization_revisions",
	"canonical_entities",
	"canonical_map_entries",
	"challenges",
	"resolutions",
	"contested_mappings",
	"contested_mapping_entities",
	"criteria",
	"criterion_authorizations",
	"criterion_snapshots",
	"decisions",
	"ballot_universe_snapshots",
	"ballots",
	"ballot_entries",
	"decision_entries",
	"governance_claims",
	"governance_claim_sources",
	"collator_narrative",
	"artifact_elements",
	"lineage_edges",
}
