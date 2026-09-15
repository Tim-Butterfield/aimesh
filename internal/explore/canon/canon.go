// Package canon maps the raw nominations from a blind explorer fan-out onto canonical entities, using a
// canonicalizer model call, and records the result in an append-only, hash-chained merge ledger. The
// partition is recorded and attributable, not asserted to be correct.
//
//   - The ledger is append-only; the current partition is a view over its rows.
//   - The surjectivity gate requires every nomination to appear in exactly one row and every canonical ID
//     to have at least one nomination, so de-duplication can cluster but never drop a nomination.
//
// Canonicalize runs a single canonicalizer. CanonicalizeDual (dual.go) keeps only merges two independent
// canonicalizers both propose, splitting the rest. Confirm (confirm.go) resolves explorer challenges with a
// host rule and produces a new revision.
//
// Identity verification of the canonicalizer call is the caller's responsibility.
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

// Nomination is one raw candidate from one explorer's blind round-1 response, with its source explorer and
// envelope ref. Clusters refer to nominations by their index in the slice passed to Canonicalize.
type Nomination struct {
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
	EnvelopeRef    string                  `json:"envelopeRef"`
}

// CanonicalizerCall proposes a partition over nominations. The pipeline implements it with a model call
// separate from the collate call; tests use a fake.
type CanonicalizerCall interface {
	Propose(ctx context.Context, nominations []Nomination) (Proposal, error)
}

// Proposal is a canonicalizer's proposed partition. Clusters refer to nominations by index.
// DecidedByCall and DecidedByIdentity attribute the call on every ledger row. ProposedDimensions and
// CoverageNotes are observations, not decision criteria.
type Proposal struct {
	Clusters           []ProposedCluster       `json:"clusters"`
	ProposedDimensions []string                `json:"proposedDimensions"`
	CoverageNotes      string                  `json:"coverageNotes"`
	DecidedByCall      string                  `json:"decidedByCall"`
	DecidedByIdentity  schema.ExplorerIdentity `json:"decidedByIdentity"`
}

// ProposedCluster is one proposed canonical entity: an ID, a label and the indices of its nominations.
// Across all clusters the members must partition the nominations exactly.
type ProposedCluster struct {
	CanonicalID string `json:"canonicalId"`
	Name        string `json:"name"`
	Members     []int  `json:"members"`
}

// Attribution identifies a canonicalizer call and the identity it was verified as.
type Attribution struct {
	Call     string                  `json:"call"`
	Identity schema.ExplorerIdentity `json:"identity"`
}

// LedgerRow records one nomination's mapping to a canonical ID and the call that decided it. Rows are
// never updated; a later revision is a new ledger.
type LedgerRow struct {
	RawNomination     string                  `json:"rawNomination"`
	CanonicalID       string                  `json:"canonicalId"`
	SourceExplorer    schema.ExplorerIdentity `json:"sourceExplorer"`
	EnvelopeRef       string                  `json:"envelopeRef"`
	DecidedByCall     string                  `json:"decidedByCall"`
	DecidedByIdentity schema.ExplorerIdentity `json:"decidedByIdentity"`
	// AgreedBy names the canonicalizer calls that proposed this mapping. It is set when a host rule decided
	// the row (dual path or confirmation), in which case DecidedByCall names the rule.
	AgreedBy []Attribution `json:"agreedBy,omitempty"`
	// Revision is the ledger revision, set only from revision 2 onward.
	Revision int `json:"revision,omitempty"`
}

// Ledger is the append-only merge ledger. Rows are unexported and only this package appends them. Each
// append extends a hash chain, so the head hash identifies the exact partition.
type Ledger struct {
	rows         []LedgerRow
	contested    []ContestedMerge
	revision     int
	prior        string
	revisionHash string
}

// newLedger starts a ledger revision whose hash chain is seeded with the prior revision's head hash.
// Revision 1 with an empty prior is the provisional revision.
func newLedger(revision int, prior string) *Ledger {
	return &Ledger{revision: revision, prior: prior, revisionHash: prior}
}

// append adds a row and extends the hash chain.
func (l *Ledger) append(r LedgerRow) {
	l.rows = append(l.rows, r)
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(append([]byte(l.revisionHash), b...))
	l.revisionHash = hex.EncodeToString(sum[:])
}

// appendContested records a refused merge and extends the hash chain. Contested records are not part of
// the surjectivity gate.
func (l *Ledger) appendContested(c ContestedMerge) {
	l.contested = append(l.contested, c)
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(append([]byte(l.revisionHash), b...))
	l.revisionHash = hex.EncodeToString(sum[:])
}

// Contested returns a copy of the refused merges recorded in this revision.
func (l *Ledger) Contested() []ContestedMerge {
	out := make([]ContestedMerge, len(l.contested))
	copy(out, l.contested)
	return out
}

// Revision returns the revision number: 1 for the provisional partition, 2 or more after confirmation.
func (l *Ledger) Revision() int {
	if l.revision == 0 {
		return 1
	}
	return l.revision
}

// PriorRevisionHash returns the head hash of the revision this one supersedes, or "".
func (l *Ledger) PriorRevisionHash() string { return l.prior }

// Rows returns a copy of the rows in append order.
func (l *Ledger) Rows() []LedgerRow {
	out := make([]LedgerRow, len(l.rows))
	copy(out, l.rows)
	return out
}

// Len returns the number of rows.
func (l *Ledger) Len() int { return len(l.rows) }

// RevisionHash returns the head of the hash chain, which claims record as partitionRevisionHash.
func (l *Ledger) RevisionHash() string { return l.revisionHash }

// MarshalJSON encodes the ledger as {rows, revisionHash}, adding the revision, prior hash and contested
// merges when present.
func (l *Ledger) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Rows              []LedgerRow      `json:"rows"`
		RevisionHash      string           `json:"revisionHash"`
		Revision          int              `json:"revision,omitempty"`
		PriorRevisionHash string           `json:"priorRevisionHash,omitempty"`
		Contested         []ContestedMerge `json:"contestedMerges,omitempty"`
	}{l.rows, l.revisionHash, revisionIfSuperseding(l), l.prior, l.contested})
}

// revisionIfSuperseding returns the revision number when it is above 1, else 0 so JSON omits it.
func revisionIfSuperseding(l *Ledger) int {
	if l.revision > 1 {
		return l.revision
	}
	return 0
}

// Member is one nomination within a canonical entity. SingleSource is inherited from the entity.
type Member struct {
	CanonicalID    string                  `json:"canonicalId"`
	RawNomination  string                  `json:"rawNomination"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
	EnvelopeRef    string                  `json:"envelopeRef"`
	SingleSource   bool                    `json:"singleSource"`
}

// Cluster is one canonical entity, a view over the ledger. SingleSource marks an entity backed by only one
// distinct explorer.
type Cluster struct {
	CanonicalID  string   `json:"canonicalId"`
	Name         string   `json:"name"`
	Members      []Member `json:"members"`
	SingleSource bool     `json:"singleSource"`
}

// Result is a canonicalization that passed the surjectivity gate: the ledger, the partition view, the
// proposed dimensions and notes, and the partition revision hash.
type Result struct {
	Clusters              []Cluster `json:"clusters"`
	ProposedDimensions    []string  `json:"proposedDimensions"`
	CoverageNotes         string    `json:"coverageNotes"`
	PartitionRevisionHash string    `json:"partitionRevisionHash"`
	Ledger                *Ledger   `json:"ledger"`
	// Canonicalizers names the calls that proposed this partition, when a host rule combined proposals.
	Canonicalizers []Attribution `json:"canonicalizers,omitempty"`
	// AgreementRuleVersion names the host rule that produced the partition; empty for a single proposal.
	AgreementRuleVersion string `json:"agreementRuleVersion,omitempty"`
	// Nominations are the inputs the partition was computed over, kept so a confirmation revision can
	// re-partition them and counts can trace entities to their sources.
	Nominations []Nomination `json:"nominations,omitempty"`
}

// Canonicalize asks canonicalizer for a partition of nominations, records it in a new ledger, and
// enforces the surjectivity gate. It returns an error if the call fails or the partition drops,
// duplicates or invents a nomination or ID.
func Canonicalize(ctx context.Context, nominations []Nomination, canonicalizer CanonicalizerCall) (Result, error) {
	prop, err := canonicalizer.Propose(ctx, nominations)
	if err != nil {
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

// buildLedger builds a ledger revision from clusters and enforces the surjectivity gate. Every path that
// produces a partition goes through it. stamp sets each row's attribution fields.
func buildLedger(nominations []Nomination, clusters []ProposedCluster, revision int, prior string, stamp func(*LedgerRow)) (*Ledger, error) {
	// assigned[i] counts the rows that claim nomination i; each must end at exactly 1.
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

	// Surjectivity gate: 0 means a dropped nomination, more than 1 a double-counted one.
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

// partitionView builds the clusters for a partition, marking entities with a single distinct source. It
// must run after the surjectivity gate, which guarantees every member index is valid.
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
