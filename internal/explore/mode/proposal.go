package mode

// This file holds the decoder for a canonicalizer's proposed partition, shared by every canonicalizing mode.
// Keeping one decoder means memberIndices, which the surjectivity check runs over, is extracted in one place.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// clusterProposalWire is the JSON shape a canonicalizer returns.
type clusterProposalWire struct {
	Clusters []struct {
		CanonicalID   string `json:"canonicalId"`
		Name          string `json:"name"`
		MemberIndices []int  `json:"memberIndices"`
	} `json:"clusters"`
	ProposedDimensions []string `json:"proposedDimensions"`
	CoverageNotes      string   `json:"coverageNotes"`
}

// parseClusterProposal decodes a canonicalizer's output into a canon.Proposal attributed to decidedByCall and
// identity. Surjectivity is checked by package canon, not here.
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

// sha256Hex returns the hex-encoded SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
