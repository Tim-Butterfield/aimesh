package canon

// This file is the DUAL-CANONICALIZER MERGE-AGREEMENT (design §0 F-B + §4): the ranking-grade replacement
// for a single canonicalizer's partition authority.
//
// The problem it closes: every emergent-space count rides on a partition ("is 'Postgres' the same candidate
// as 'Postgres+Citus'?"). Merge → "3/5"; split → "2/5". Both are honest arithmetic and they disagree, so
// whoever controls the partition controls the agenda. Recording the partition (the merge ledger) makes that
// power VISIBLE; it does not divide it.
//
// The rule: run TWO INDEPENDENT canonicalizer calls (each separately identity-verified by the caller) and
// keep ONLY the merges BOTH propose. Formally the held partition is the INTERSECTION of the two proposed
// equivalence relations — every class is A_i ∩ B_j — which is itself an equivalence relation, so the result
// is always a well-formed partition and never needs transitive repair. A merge only ONE canonicalizer
// proposes is CONTESTED and resolved by SPLITTING: that direction can UNDERCOUNT corroboration (two names
// for one thing may be counted as two candidates with one source each) but it can NEVER MANUFACTURE
// corroboration (it cannot turn one explorer's nomination into two explorers' agreement). Undercounting is
// recoverable — the contested merge is recorded, the confirmation round can raise it, and a count over a
// contested mapping is emitted as a range. Manufactured corroboration is not recoverable: it is a false
// claim about independent support.
//
// Every held mapping row records `agreedBy` (both proposing calls + identities); the DECIDING authority on
// this path is the versioned HOST rule, not either model (§0 F-C: machine governance fields carry
// host-produced values only), so DecidedByCall names the rule and DecidedByIdentity is deliberately left
// zero — no model decided it.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// DualRuleVersion is the VERSIONED host rule that turns two independent canonicalizer proposals into one
// held partition. It is persisted on every row's deciding call and on the Result, so a partition can always
// be re-derived from the two recorded proposals by the exact rule that produced it (§0 F-C).
const DualRuleVersion = "host-dual-merge-agreement@v1"

// ContestedMerge is a merge exactly ONE canonicalizer proposed (design §0 F-B). The host rule resolves it in
// the conservative direction — the nominations are kept SPLIT — and the refused merge is recorded as a
// first-class decision in the same append-only chain, so a reader can see precisely which corroboration the
// partition may be undercounting and who proposed it.
type ContestedMerge struct {
	// ProposedBy is the canonicalizer call that wanted these nominations in one entity.
	ProposedBy Attribution `json:"proposedBy"`
	// ProposedCanonicalID/Name are that canonicalizer's own label for the merged entity.
	ProposedCanonicalID string `json:"proposedCanonicalId"`
	ProposedName        string `json:"proposedName"`
	// RawNominations are the raw texts the proposing canonicalizer wanted together (in nomination order).
	RawNominations []string `json:"rawNominations"`
	// SplitInto are the HELD canonical IDs those nominations actually landed in (>=2 — that is what makes the
	// merge contested).
	SplitInto []string `json:"splitInto"`
	// Resolution + RuleVersion record the host decision and the rule version that made it.
	Resolution  string `json:"resolution"`
	RuleVersion string `json:"ruleVersion"`
}

// ResolutionSplit is the only resolution DualRuleVersion ever applies to a contested merge.
const ResolutionSplit = "split"

// CanonicalizeDual runs TWO INDEPENDENT canonicalizer calls over the same nominations and returns the
// partition both agree on (design §0 F-B). Both proposals are validated for partition integrity FIRST (each
// must itself be a surjective partition — an invalid proposal is a canonicalizer failure, not something to
// silently intersect around), then the held partition is their intersection, then the surjectivity gate runs
// over the held partition exactly as on the single path. Errors name WHICH canonicalizer produced the bad
// proposal so a halt is diagnosable. A call failure (including an identity halt, which the caller raises
// inside Propose) propagates unchanged.
func CanonicalizeDual(ctx context.Context, nominations []Nomination, first, second CanonicalizerCall) (Result, error) {
	propA, err := first.Propose(ctx, nominations)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalizer A proposal: %w", err)
	}
	propB, err := second.Propose(ctx, nominations)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalizer B proposal: %w", err)
	}
	assignA, err := assignments(nominations, propA.Clusters)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalizer A: %w", err)
	}
	assignB, err := assignments(nominations, propB.Clusters)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalizer B: %w", err)
	}

	attrA := Attribution{Call: propA.DecidedByCall, Identity: propA.DecidedByIdentity}
	attrB := Attribution{Call: propB.DecidedByCall, Identity: propB.DecidedByIdentity}
	agreed := intersect(nominations, propA.Clusters, propB.Clusters, assignA, assignB)

	led, err := buildLedger(nominations, agreed, 1, "", func(r *LedgerRow) {
		// The DECIDING authority is the host rule; the two model proposals are the evidence (AgreedBy).
		r.DecidedByCall = "host:" + DualRuleVersion
		r.AgreedBy = []Attribution{attrA, attrB}
	})
	if err != nil {
		return Result{}, err
	}
	// Record every merge one canonicalizer proposed that the agreement REFUSED — from both directions, so
	// neither canonicalizer's rejected merges are silently privileged.
	for _, cm := range contestedMerges(nominations, propA.Clusters, agreed, attrA) {
		led.appendContested(cm)
	}
	for _, cm := range contestedMerges(nominations, propB.Clusters, agreed, attrB) {
		led.appendContested(cm)
	}

	return Result{
		Clusters:              partitionView(nominations, agreed),
		ProposedDimensions:    mergeDimensions(propA.ProposedDimensions, propB.ProposedDimensions),
		CoverageNotes:         mergeCoverageNotes(propA.CoverageNotes, propB.CoverageNotes),
		PartitionRevisionHash: led.RevisionHash(),
		Ledger:                led,
		Canonicalizers:        []Attribution{attrA, attrB},
		AgreementRuleVersion:  DualRuleVersion,
		Nominations:           append([]Nomination(nil), nominations...),
	}, nil
}

// assignments validates ONE proposed partition (non-empty unique canonical IDs, in-range members, every
// nomination in exactly one cluster) and returns the cluster INDEX each nomination was assigned to. It is
// the pre-intersection integrity check: intersecting a proposal that already dropped or double-counted a
// nomination would hide that canonicalizer's failure behind the agreement rule.
func assignments(noms []Nomination, clusters []ProposedCluster) ([]int, error) {
	out := make([]int, len(noms))
	for i := range out {
		out[i] = -1
	}
	seenID := map[string]bool{}
	for ci, c := range clusters {
		id := strings.TrimSpace(c.CanonicalID)
		if id == "" {
			return nil, fmt.Errorf("proposed cluster %d has an empty canonical ID", ci)
		}
		if seenID[id] {
			return nil, fmt.Errorf("duplicate canonical ID %q across clusters", id)
		}
		seenID[id] = true
		if len(c.Members) == 0 {
			return nil, fmt.Errorf("surjectivity gate: canonical ID %q has no nominations (every canonical ID must be reachable from >=1 nomination)", id)
		}
		for _, idx := range c.Members {
			if idx < 0 || idx >= len(noms) {
				return nil, fmt.Errorf("surjectivity gate: cluster %q references out-of-range nomination index %d (have %d)", id, idx, len(noms))
			}
			if out[idx] != -1 {
				return nil, fmt.Errorf("surjectivity gate: nomination %d (%q) appears in more than one cluster — must be exactly one", idx, noms[idx].Raw)
			}
			out[idx] = ci
		}
	}
	for i, ci := range out {
		if ci == -1 {
			return nil, fmt.Errorf("surjectivity gate: nomination %d (%q) was DROPPED by canonicalization — every nomination must map to exactly one canonical ID (de-dup may cluster, never drop)", i, noms[i].Raw)
		}
	}
	return out, nil
}

// intersect builds the AGREED partition: one class per non-empty A_i ∩ B_j, in the order the classes are
// first encountered walking A's clusters (deterministic, and stable under re-runs of the same proposals).
// A class that IS an entire A cluster (i.e. A_i ⊆ B_j — nothing was contested about it) keeps A's canonical
// ID and label unchanged, so an agreed partition reads like a normal one. A class that is a SPLIT of an A
// cluster gets a composite ID + label naming BOTH canonicalizers' entities, because that is the honest
// description of what it is: the part of A's entity that B also grouped together.
func intersect(noms []Nomination, a, b []ProposedCluster, assignA, assignB []int) []ProposedCluster {
	type key struct{ ai, bi int }
	var order []key
	members := map[key][]int{}
	for i := range noms {
		k := key{assignA[i], assignB[i]}
		if _, seen := members[k]; !seen {
			order = append(order, k)
		}
		members[k] = append(members[k], i)
	}
	// How many agreed classes each A cluster split into (1 = uncontested within A).
	splits := map[int]int{}
	for _, k := range order {
		splits[k.ai]++
	}
	out := make([]ProposedCluster, 0, len(order))
	for _, k := range order {
		ac, bc := a[k.ai], b[k.bi]
		id, name := ac.CanonicalID, ac.Name
		if splits[k.ai] > 1 {
			// Contested: A merged these with others, B did not. Name the intersection after both entities so
			// the label itself records that this is an agreement-narrowed group, not either model's cluster.
			id = ac.CanonicalID + "~" + bc.CanonicalID
			name = ac.Name + " / " + bc.Name
		}
		out = append(out, ProposedCluster{CanonicalID: id, Name: name, Members: members[k]})
	}
	return out
}

// contestedMerges reports, for one canonicalizer's proposal, every cluster the agreement rule SPLIT: the
// merge that canonicalizer proposed and the other refused. One record per split cluster (not per pair) keeps
// the ledger proportional to the disagreement while still naming every raw nomination involved and every
// held canonical ID they ended up in.
func contestedMerges(noms []Nomination, proposed, agreed []ProposedCluster, by Attribution) []ContestedMerge {
	// held[nominationIndex] = the agreed canonical ID it landed in.
	held := map[int]string{}
	for _, c := range agreed {
		for _, idx := range c.Members {
			held[idx] = c.CanonicalID
		}
	}
	var out []ContestedMerge
	for _, c := range proposed {
		ids := map[string]bool{}
		for _, idx := range c.Members {
			ids[held[idx]] = true
		}
		if len(ids) < 2 {
			continue // fully agreed — nothing was refused
		}
		raws := make([]string, 0, len(c.Members))
		for _, idx := range c.Members {
			raws = append(raws, noms[idx].Raw)
		}
		split := make([]string, 0, len(ids))
		for id := range ids {
			split = append(split, id)
		}
		sort.Strings(split) // deterministic record
		out = append(out, ContestedMerge{
			ProposedBy:          by,
			ProposedCanonicalID: c.CanonicalID,
			ProposedName:        c.Name,
			RawNominations:      raws,
			SplitInto:           split,
			Resolution:          ResolutionSplit,
			RuleVersion:         DualRuleVersion,
		})
	}
	return out
}

// mergeDimensions unions the two canonicalizers' PROPOSED dimensions, preserving A's order then appending
// B's extras. They are proposed axes for the human (never decision criteria), so a union loses nothing and
// keeping both is more honest than picking one canonicalizer's list.
func mergeDimensions(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if strings.TrimSpace(s) == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// mergeCoverageNotes keeps BOTH canonicalizers' coverage notes, each labeled by its author. They are model
// prose about coverage, so they are attributed rather than blended into one unattributed paragraph.
func mergeCoverageNotes(a, b string) string {
	switch {
	case strings.TrimSpace(a) == "" && strings.TrimSpace(b) == "":
		return ""
	case strings.TrimSpace(b) == "":
		return "canonicalizer A: " + a
	case strings.TrimSpace(a) == "":
		return "canonicalizer B: " + b
	default:
		return "canonicalizer A: " + a + "\ncanonicalizer B: " + b
	}
}
