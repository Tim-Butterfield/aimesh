package canon

// This file implements dual-canonicalizer merge agreement.
//
// Two independent canonicalizers each propose a partition, and only merges both propose are kept: the held
// partition is the intersection of the two equivalence relations (every class is A_i ∩ B_j), which is
// always a valid partition. A merge only one proposes is contested and split. Splitting can undercount
// corroboration but never manufacture it, and the contested merge is recorded so the confirmation round and
// the counts can account for it.
//
// Rows record both proposals in AgreedBy. The host rule decides, so DecidedByCall names the rule and
// DecidedByIdentity is left zero.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// DualRuleVersion identifies the merge-agreement rule. It is recorded on every row and on the Result.
const DualRuleVersion = "host-dual-merge-agreement@v1"

// ContestedMerge records a merge only one canonicalizer proposed, which the rule split.
type ContestedMerge struct {
	// ProposedBy is the canonicalizer call that proposed the merge.
	ProposedBy Attribution `json:"proposedBy"`
	// ProposedCanonicalID and ProposedName are that canonicalizer's label for the merged entity.
	ProposedCanonicalID string `json:"proposedCanonicalId"`
	ProposedName        string `json:"proposedName"`
	// RawNominations are the nominations it proposed merging, in nomination order.
	RawNominations []string `json:"rawNominations"`
	// SplitInto are the held canonical IDs those nominations landed in (at least two).
	SplitInto []string `json:"splitInto"`
	// Resolution and RuleVersion record the host's decision and rule.
	Resolution  string `json:"resolution"`
	RuleVersion string `json:"ruleVersion"`
}

// ResolutionSplit is the resolution DualRuleVersion applies to every contested merge.
const ResolutionSplit = "split"

// CanonicalizeDual asks both canonicalizers for a partition of nominations and returns the partition they
// agree on. Each proposal must itself pass the surjectivity checks, and errors name the canonicalizer at
// fault. Call errors, including identity halts, are returned wrapped.
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
		r.DecidedByCall = "host:" + DualRuleVersion
		r.AgreedBy = []Attribution{attrA, attrB}
	})
	if err != nil {
		return Result{}, err
	}
	// Record the refused merges of both canonicalizers.
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

// assignments validates one proposed partition and returns the cluster index of each nomination. A
// proposal that drops or double-counts a nomination is an error, so intersection cannot hide it.
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

// intersect builds the agreed partition: one class per non-empty A_i ∩ B_j, in order of first appearance.
// A class equal to a whole A cluster keeps A's ID and label; a class that splits an A cluster gets an ID
// and label combining both canonicalizers' entities.
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
	// splits counts the agreed classes each A cluster became.
	splits := map[int]int{}
	for _, k := range order {
		splits[k.ai]++
	}
	out := make([]ProposedCluster, 0, len(order))
	for _, k := range order {
		ac, bc := a[k.ai], b[k.bi]
		id, name := ac.CanonicalID, ac.Name
		if splits[k.ai] > 1 {
			id = ac.CanonicalID + "~" + bc.CanonicalID
			name = ac.Name + " / " + bc.Name
		}
		out = append(out, ProposedCluster{CanonicalID: id, Name: name, Members: members[k]})
	}
	return out
}

// contestedMerges returns one record for each cluster in proposed that the agreement split.
func contestedMerges(noms []Nomination, proposed, agreed []ProposedCluster, by Attribution) []ContestedMerge {
	held := map[int]string{} // nomination index to agreed canonical ID

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
			continue
		}
		raws := make([]string, 0, len(c.Members))
		for _, idx := range c.Members {
			raws = append(raws, noms[idx].Raw)
		}
		split := make([]string, 0, len(ids))
		for id := range ids {
			split = append(split, id)
		}
		sort.Strings(split)
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

// mergeDimensions returns the union of both proposals' dimensions: a's in order, then b's additions.
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

// mergeCoverageNotes joins both canonicalizers' coverage notes, each labeled with its author.
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
