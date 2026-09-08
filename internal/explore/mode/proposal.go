package mode

// This file holds the ONE decoder for a canonicalizer's proposed partition, shared by every canonicalizing
// mode. Each mode writes its own canonicalizer PROMPT (clustering candidate options and clustering findings
// are genuinely different judgments and are instructed differently), but they all ask for the same JSON
// shape — so they decode it in one place. A per-mode copy of this decoder would be a place for the
// surjectivity contract to drift: `memberIndices` is what the host's gate is checked over, and exactly one
// function should be responsible for getting it out of the model's bytes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// clusterProposalWire is the canonicalizer's raw output shape (memberIndices → canon.ProposedCluster.Members).
type clusterProposalWire struct {
	Clusters []struct {
		CanonicalID   string `json:"canonicalId"`
		Name          string `json:"name"`
		MemberIndices []int  `json:"memberIndices"`
	} `json:"clusters"`
	ProposedDimensions []string `json:"proposedDimensions"`
	CoverageNotes      string   `json:"coverageNotes"`
}

// parseClusterProposal decodes a canonicalizer's raw output into a canon.Proposal, stamping the deciding call
// ref + the SEPARATELY-verified canonicalizer identity onto it so every ledger row is attributable. It does
// NOT enforce surjectivity — that GATE is canon's host-checked invariant over the parsed proposal, a single
// authority no mode can weaken by parsing leniently.
func parseClusterProposal(raw []byte, decidedByCall string, identity schema.ExplorerIdentity) (canon.Proposal, error) {
	obj, _, xerr := schema.ExtractJSONObject(raw)
	if xerr != nil {
		return canon.Proposal{}, xerr
	}
	var w clusterProposalWire
	if err := json.Unmarshal(obj, &w); err != nil {
		return canon.Proposal{}, err
	}
	clusters := make([]canon.ProposedCluster, 0, len(w.Clusters))
	for _, c := range w.Clusters {
		clusters = append(clusters, canon.ProposedCluster{
			CanonicalID: c.CanonicalID,
			Name:        c.Name,
			Members:     c.MemberIndices,
		})
	}
	return canon.Proposal{
		Clusters:           clusters,
		ProposedDimensions: w.ProposedDimensions,
		CoverageNotes:      w.CoverageNotes,
		DecidedByCall:      decidedByCall,
		DecidedByIdentity:  identity,
	}, nil
}

// sha256Hex is the hex SHA-256 of b — used to digest a supplied artifact so a result can be tied to the exact
// bytes it was produced against without copying them into the machine surface.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
