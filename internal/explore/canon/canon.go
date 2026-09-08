// Package canon is exploremesh's canonicalization component (design §4): it maps the raw candidate
// nominations gathered from a blind explorer fan-out onto CANONICAL entities via a decoupled
// canonicalizer MODEL call, and records that judgment in an APPEND-ONLY, revision-hashed merge-ledger —
// the partition is made visible + attributable, never asserted as correct (design §0 F-B: entity
// resolution is RELOCATED + RECORDED, not eliminated).
//
// Two hard invariants hold BY CONSTRUCTION:
//   - The merge-ledger is APPEND-ONLY: rows are only ever appended (via the unexported append, called
//     once per nomination inside Canonicalize); the "current partition" is a VIEW over those rows
//     (partitionView), never an in-place rewrite. There is no exported setter or index-mutation path.
//   - The SURJECTIVITY GATE: every raw nomination lands in EXACTLY one ledger row, and every canonical
//     ID is reachable from >=1 nomination. A violation is an ERROR (never a silent pass) — the
//     mechanical form of minority carry-through: de-dup may CLUSTER, never DROP a nomination. A
//     singleton nomination therefore survives into its own canonical entity, tagged single-source.
//
// SCOPE: Canonicalize is the SINGLE decoupled-identity canonicalizer path — it is what the
// observe-posture Catalog mode uses. dual.go and confirm.go hold the two RANKING-GRADE governance layers a
// count/ballot-bearing mode needs:
//
//   - CanonicalizeDual (dual.go): TWO independent canonicalizer proposals, keeping ONLY merges BOTH
//     propose; a merge only one proposes is CONTESTED → SPLIT (the conservative direction — splitting can
//     undercount corroboration but can never manufacture it), with `agreedBy` on every ledger row.
//   - Confirm (confirm.go): the BINDING confirmation round — typed explorer challenges resolved by a
//     VERSIONED HOST rule (never by the canonicalizer, which must not review complaints about itself),
//     producing a NEW append-only ledger revision and, where a dispute stands, a CONTESTED mapping that
//     makes dependent counts conditional.
//
// canon depends on schema ONLY — the canonicalizer's identity verification is the CALLER's responsibility
// (the pipeline classifies each model call and halts on a strong-evidence mismatch), so canon stays free of
// the identity engine.
package canon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Nomination is one raw candidate proposed by one explorer in the blind round-1 fan-out (design §4):
// the raw text as the explorer wrote it, plus the SOURCE explorer identity + envelope ref so every
// ledger row is attributable. The nominations passed to Canonicalize are indexed by position — the
// canonicalizer proposes clusters by member index, and the surjectivity gate is checked over that index.
type Nomination struct {
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
	EnvelopeRef    string                  `json:"envelopeRef"`
}

// CanonicalizerCall is the DECOUPLED canonicalizer MODEL-call seam (design §0 F-A / §4): a distinct call
// whose identity is verified SEPARATELY from the collator (a strong-evidence mismatch halts). It
// proposes a partition over the nominations. The pipeline implements it with a real meshcore model call
// (isolated WorkDir, identity classified); tests implement it with a deterministic in-process fake — no
// real model needed. It may reuse the collator adapter but is NEVER the collate call itself.
type CanonicalizerCall interface {
	Propose(ctx context.Context, nominations []Nomination) (Proposal, error)
}

// Proposal is the canonicalizer's proposed partition + the distinguishing axes it extracted. Clusters
// reference nominations by INDEX (into the slice handed to Canonicalize) so the host can mechanically
// check surjectivity. DecidedByCall + DecidedByIdentity attribute the deciding call on every ledger row
// (the caller stamps them from the verified model call). ProposedDimensions/CoverageNotes are the
// canonicalizer's observations — for Catalog they are PROPOSED axes, never promoted to decision criteria.
type Proposal struct {
	Clusters           []ProposedCluster       `json:"clusters"`
	ProposedDimensions []string                `json:"proposedDimensions"`
	CoverageNotes      string                  `json:"coverageNotes"`
	DecidedByCall      string                  `json:"decidedByCall"`
	DecidedByIdentity  schema.ExplorerIdentity `json:"decidedByIdentity"`
}

// ProposedCluster is one proposed canonical entity: a stable ID + human label + the indices of the raw
// nominations the canonicalizer clustered under it. Members must reference in-range nomination indices
// and, across all clusters, partition the nominations EXACTLY (the surjectivity gate enforces this).
type ProposedCluster struct {
	CanonicalID string `json:"canonicalId"`
	Name        string `json:"name"`
	Members     []int  `json:"members"`
}

// Attribution is one canonicalizer CALL's attribution: the call reference plus the identity that call was
// verified as. It is the unit recorded in a row's AgreedBy, so "which canonicalizer(s) proposed this
// mapping" is answerable from the ledger alone (design §0 F-B).
type Attribution struct {
	Call     string                  `json:"call"`
	Identity schema.ExplorerIdentity `json:"identity"`
}

// LedgerRow is one raw nomination → canonical ID mapping decision (design §4). It is APPEND-ONLY — a row
// is never updated in place; a later revision (the confirmation round, confirm.go) builds a NEW ledger
// whose rows supersede these, and this row is retained unchanged. It records the deciding call + identity so
// the partition is attributable.
type LedgerRow struct {
	RawNomination     string                  `json:"rawNomination"`
	CanonicalID       string                  `json:"canonicalId"`
	SourceExplorer    schema.ExplorerIdentity `json:"sourceExplorer"`
	EnvelopeRef       string                  `json:"envelopeRef"`
	DecidedByCall     string                  `json:"decidedByCall"`
	DecidedByIdentity schema.ExplorerIdentity `json:"decidedByIdentity"`
	// AgreedBy names the canonicalizer call(s) that PROPOSED this mapping (design §0 F-B / §4).
	// It is populated on the DUAL path — where the deciding authority is the host merge-agreement rule and
	// the model proposals are the evidence, so DecidedByCall names the RULE and AgreedBy names the models.
	// It is nil (omitted) on the single-canonicalizer path, where DecidedByCall/Identity already name the
	// one deciding call — so a Catalog ledger row's bytes are unchanged.
	AgreedBy []Attribution `json:"agreedBy,omitempty"`
	// Revision is the ledger revision this row belongs to, present only from revision 2 onward (a
	// confirmation-round revision). Omitted on the provisional revision, so an unconfirmed (Catalog) row's
	// bytes are unchanged.
	Revision int `json:"revision,omitempty"`
}

// Ledger is the APPEND-ONLY, revision-hashed merge-ledger (design §4/§9). Its rows are unexported and
// there is NO exported way to mutate or replace a row — the only write path is the unexported append,
// invoked once per nomination inside Canonicalize. revisionHash is a hash CHAIN over the appended rows
// (each append folds the row bytes into the prior hash), so the head hash pins the exact partition every
// count/claim is computed at; any reordering or edit of the rows changes it. A stray in-place update is
// impossible by construction.
type Ledger struct {
	rows         []LedgerRow
	contested    []ContestedMerge
	revision     int
	prior        string
	revisionHash string
}

// newLedger starts a ledger revision. revision 1 with an empty prior is the PROVISIONAL revision (and the
// only one a single-canonicalizer run ever has); a later revision seeds its hash chain with the prior
// revision's head hash, so revisions are themselves chained — a confirmation revision is provably a
// successor of the exact partition it was computed from, and the prior revision is never touched.
func newLedger(revision int, prior string) *Ledger {
	return &Ledger{revision: revision, prior: prior, revisionHash: prior}
}

// append folds one row into the append-only ledger, extending the revision hash chain. Unexported: its only
// callers are this package's ledger builders, so no downstream code can grow or rewrite a ledger out of band.
func (l *Ledger) append(r LedgerRow) {
	l.rows = append(l.rows, r)
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(append([]byte(l.revisionHash), b...))
	l.revisionHash = hex.EncodeToString(sum[:])
}

// appendContested folds one CONTESTED-merge decision into the same append-only chain (design §0 F-B: the
// merge one canonicalizer proposed and the other did not is a recorded DECISION, not a discarded opinion).
// Contested records do not participate in the surjectivity gate — they describe merges that were REFUSED,
// not nomination→canonical mappings. Unexported, like append.
func (l *Ledger) appendContested(c ContestedMerge) {
	l.contested = append(l.contested, c)
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(append([]byte(l.revisionHash), b...))
	l.revisionHash = hex.EncodeToString(sum[:])
}

// Contested returns a COPY of the contested-merge decisions recorded in this revision (empty on the
// single-canonicalizer path).
func (l *Ledger) Contested() []ContestedMerge {
	out := make([]ContestedMerge, len(l.contested))
	copy(out, l.contested)
	return out
}

// Revision is this ledger's revision number (1 = the provisional partition; 2+ = a confirmation-round
// revision produced by the versioned host rule).
func (l *Ledger) Revision() int {
	if l.revision == 0 {
		return 1
	}
	return l.revision
}

// PriorRevisionHash is the head hash of the revision this one supersedes ("" for the first revision). The
// superseded revision is RETAINED — a revision is a new ledger, never an edit of the old one.
func (l *Ledger) PriorRevisionHash() string { return l.prior }

// Rows returns a COPY of the ledger rows in append order (a read-only view — mutating the copy cannot
// touch the ledger). It is the append-only system of record persisted as merge-ledger.jsonl (§9).
func (l *Ledger) Rows() []LedgerRow {
	out := make([]LedgerRow, len(l.rows))
	copy(out, l.rows)
	return out
}

// Len reports the number of ledger rows (one per raw nomination once the surjectivity gate has passed).
func (l *Ledger) Len() int { return len(l.rows) }

// RevisionHash returns the head of the ledger's revision hash chain — the partitionRevisionHash every
// count/claim computed over this partition must pin (design §4).
func (l *Ledger) RevisionHash() string { return l.revisionHash }

// MarshalJSON emits the ledger as {rows, revisionHash} — plus the revision chain + contested-merge
// decisions when they exist — so a Result serializes fully for --json / capture despite the append-only
// rows being unexported. The extra fields are omitempty, so a single-canonicalizer (Catalog) ledger
// serializes as {rows, revisionHash} alone.
func (l *Ledger) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Rows              []LedgerRow      `json:"rows"`
		RevisionHash      string           `json:"revisionHash"`
		Revision          int              `json:"revision,omitempty"`
		PriorRevisionHash string           `json:"priorRevisionHash,omitempty"`
		Contested         []ContestedMerge `json:"contestedMerges,omitempty"`
	}{l.rows, l.revisionHash, revisionIfSuperseding(l), l.prior, l.contested})
}

// revisionIfSuperseding reports the revision number for serialization ONLY when it supersedes another
// revision — the provisional revision omits it so its bytes are unchanged.
func revisionIfSuperseding(l *Ledger) int {
	if l.revision > 1 {
		return l.revision
	}
	return 0
}

// Member is one raw nomination within a canonical entity — the same attribution the ledger row carries,
// plus SingleSource: whether the entity is backed by exactly ONE distinct source explorer (minority
// carry-through, design §4). SingleSource is a property of the ENTITY, inherited by each member.
type Member struct {
	CanonicalID    string                  `json:"canonicalId"`
	RawNomination  string                  `json:"rawNomination"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
	EnvelopeRef    string                  `json:"envelopeRef"`
	SingleSource   bool                    `json:"singleSource"`
}

// Cluster is one confirmed canonical entity — a VIEW over the ledger (design §4): its members are the
// nominations mapped to this canonical ID. SingleSource marks an entity backed by a single distinct
// source explorer — a carried minority (salience, not corroboration; §0 F-B).
type Cluster struct {
	CanonicalID  string   `json:"canonicalId"`
	Name         string   `json:"name"`
	Members      []Member `json:"members"`
	SingleSource bool     `json:"singleSource"`
}

// Result is the canonicalization outcome (returned ONLY when the surjectivity gate passed): the
// append-only Ledger, the partition VIEW (Clusters), the PROPOSED distinguishing dimensions +
// coverage notes, and the partitionRevisionHash pinning the partition.
type Result struct {
	Clusters              []Cluster `json:"clusters"`
	ProposedDimensions    []string  `json:"proposedDimensions"`
	CoverageNotes         string    `json:"coverageNotes"`
	PartitionRevisionHash string    `json:"partitionRevisionHash"`
	Ledger                *Ledger   `json:"ledger"`
	// Canonicalizers names every canonicalizer call that contributed a PROPOSAL to this partition (one on
	// the single path, two on the dual path). omitempty keeps a Catalog result's bytes unchanged.
	Canonicalizers []Attribution `json:"canonicalizers,omitempty"`
	// AgreementRuleVersion is the versioned HOST rule that turned the proposal(s) into this partition — set
	// on the dual path (DualRuleVersion) and, after a confirmation round, the confirmation rule version.
	// Empty on the single-canonicalizer path, where the partition IS the single proposal.
	AgreementRuleVersion string `json:"agreementRuleVersion,omitempty"`
	// Nominations is the exact nomination list this partition was computed over, retained so a later
	// revision (the confirmation round) can re-partition the SAME inputs without re-deriving them, and so a
	// count can resolve a canonical ID back to its attributed blind round-1 sources. omitempty keeps a
	// single-canonicalizer result's bytes unchanged (the pipeline sets it only on the governed paths).
	Nominations []Nomination `json:"nominations,omitempty"`
}

// Canonicalize maps raw nominations onto canonical entities via the decoupled canonicalizer call, records
// the append-only merge-ledger, and enforces the surjectivity gate (design §4). It returns an error —
// never a silent pass — when the canonicalizer call fails/halts (identity mismatch propagates from the
// caller's Propose) or the proposed partition violates surjectivity (a dropped/double-counted nomination,
// an out-of-range member, an empty/duplicate canonical ID). On success the Result's ledger is complete
// (one row per nomination) and its partition view tags single-source entities.
func Canonicalize(ctx context.Context, nominations []Nomination, canonicalizer CanonicalizerCall) (Result, error) {
	prop, err := canonicalizer.Propose(ctx, nominations)
	if err != nil {
		// A canonicalizer identity halt / call failure propagates unchanged (the caller wraps it with a
		// fault halt class); %w keeps any underlying fault reachable via errors.As.
		return Result{}, fmt.Errorf("canonicalizer proposal: %w", err)
	}

	led, err := buildLedger(nominations, prop.Clusters, 1, "", func(r *LedgerRow) {
		r.DecidedByCall = prop.DecidedByCall
		r.DecidedByIdentity = prop.DecidedByIdentity
	})
	if err != nil {
		return Result{}, err
	}

	return Result{
		Clusters:              partitionView(nominations, prop.Clusters),
		ProposedDimensions:    prop.ProposedDimensions,
		CoverageNotes:         prop.CoverageNotes,
		PartitionRevisionHash: led.RevisionHash(),
		Ledger:                led,
	}, nil
}

// buildLedger builds ONE append-only ledger revision from a partition over the nominations and enforces the
// SURJECTIVITY GATE. It is the single authority for partition integrity, shared by every path that produces
// a partition (single canonicalizer, dual merge-agreement, confirmation revision) so the gate can never be
// bypassed by adding a path. stamp fills each row's attribution fields (deciding call / identity / agreedBy /
// revision), which is the only thing that differs between the paths.
func buildLedger(nominations []Nomination, clusters []ProposedCluster, revision int, prior string, stamp func(*LedgerRow)) (*Ledger, error) {
	// assigned[i] counts how many ledger rows claim nomination i — it must end EXACTLY 1 for every
	// nomination (the surjectivity gate). Reject an empty or duplicate canonical ID and an out-of-range
	// member index up front — a fabricated/unreachable ID or a phantom member would corrupt the partition.
	led := newLedger(revision, prior)
	assigned := make([]int, len(nominations))
	seenID := map[string]bool{}
	for _, c := range clusters {
		id := strings.TrimSpace(c.CanonicalID)
		if id == "" {
			return nil, fmt.Errorf("canonicalize: a proposed cluster has an empty canonical ID")
		}
		if seenID[id] {
			return nil, fmt.Errorf("canonicalize: duplicate canonical ID %q across clusters", id)
		}
		seenID[id] = true
		if len(c.Members) == 0 {
			// An unreachable canonical ID (no nomination maps to it) fails surjectivity — never manufacture
			// a candidate the panel did not nominate.
			return nil, fmt.Errorf("surjectivity gate: canonical ID %q has no nominations (every canonical ID must be reachable from >=1 nomination)", id)
		}
		for _, idx := range c.Members {
			if idx < 0 || idx >= len(nominations) {
				return nil, fmt.Errorf("surjectivity gate: cluster %q references out-of-range nomination index %d (have %d)", id, idx, len(nominations))
			}
			n := nominations[idx]
			row := LedgerRow{
				RawNomination:  n.Raw,
				CanonicalID:    c.CanonicalID,
				SourceExplorer: n.SourceExplorer,
				EnvelopeRef:    n.EnvelopeRef,
			}
			if revision > 1 {
				row.Revision = revision
			}
			if stamp != nil {
				stamp(&row)
			}
			led.append(row)
			assigned[idx]++
		}
	}

	// SURJECTIVITY GATE (hard invariant, host-checked): every nomination in EXACTLY one ledger row. A
	// count of 0 means the canonicalizer DROPPED a nomination (the failure minority carry-through exists
	// to catch — de-dup may cluster, never drop); a count >1 means it was double-counted.
	for i, count := range assigned {
		switch {
		case count == 0:
			return nil, fmt.Errorf("surjectivity gate: nomination %d (%q) was DROPPED by canonicalization — every nomination must map to exactly one canonical ID (de-dup may cluster, never drop)", i, nominations[i].Raw)
		case count > 1:
			return nil, fmt.Errorf("surjectivity gate: nomination %d (%q) appears in %d ledger rows — must be exactly one", i, nominations[i].Raw, count)
		}
	}
	return led, nil
}

// partitionView projects the proposed clusters into the confirmed partition, tagging minority
// carry-through: an entity whose members come from a SINGLE distinct source explorer is single-source
// (salience, not corroboration). It runs only after the surjectivity gate has passed, so every member
// index is valid.
func partitionView(noms []Nomination, clusters []ProposedCluster) []Cluster {
	out := make([]Cluster, 0, len(clusters))
	for _, c := range clusters {
		sources := map[schema.ExplorerIdentity]bool{}
		for _, idx := range c.Members {
			sources[noms[idx].SourceExplorer] = true
		}
		single := len(sources) == 1
		members := make([]Member, 0, len(c.Members))
		for _, idx := range c.Members {
			n := noms[idx]
			members = append(members, Member{
				CanonicalID:    c.CanonicalID,
				RawNomination:  n.Raw,
				SourceExplorer: n.SourceExplorer,
				EnvelopeRef:    n.EnvelopeRef,
				SingleSource:   single,
			})
		}
		out = append(out, Cluster{CanonicalID: c.CanonicalID, Name: c.Name, Members: members, SingleSource: single})
	}
	return out
}
