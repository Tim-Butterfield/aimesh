package schema

// This file holds the Catalog mode's app-owned artifacts (design §3): the FIXED explorer schema +
// deterministic "enumerate broadly" explorer prompt, and the terminal CatalogOutput the host assembles
// as a VIEW over the canonicalization result (design §4). Catalog is FORMULATION-FREE like Map/Synthesize
// — the collator authors NO round-1 schema (§5): both the explorer prompt and the explorer schema are
// app-owned. Its terminal collation runs through the canonicalization component (canon), not a plain
// collate call, so the assembly of this type lives in the mode's CanonicalizingContract, not here.

import (
	"fmt"
	"strings"
)

// CatalogOutput is the FIXED, exploremesh-owned terminal output of the Catalog mode (design §3): the
// canonical CLUSTERS (each raw nomination de-duplicated onto a canonical entity, minority carry-through
// preserved), the PROPOSED distinguishing dimensions, and coverage notes. Catalog is OBSERVE posture —
// it enumerates + organizes, it does NOT rank — so ProposedDimensions are labeled PROPOSED axes for the
// human, NEVER promoted to decision criteria (§4). It satisfies the mode-package ModeOutput contract via
// Summary().
type CatalogOutput struct {
	// Clusters are the canonical entities the canonicalizer proposed over the raw nominations. Every raw
	// nomination appears in exactly one cluster (the surjectivity gate); a singleton survives tagged
	// single-source (minority carry-through). At least one cluster is required (Validate).
	Clusters []CatalogCluster `json:"clusters"`
	// ProposedDimensions are the distinguishing axes the canonicalizer extracted — PROPOSED for the human
	// to consider, NOT decision criteria (Catalog does not rank; §3/§4).
	ProposedDimensions []string `json:"proposedDimensions"`
	// CoverageNotes are the canonicalizer's notes on coverage/gaps across the nominations.
	CoverageNotes string `json:"coverageNotes"`
}

// CatalogCluster is one canonical entity: a human label + the raw nominations clustered under it.
type CatalogCluster struct {
	Name    string            `json:"name"`
	Members []CanonicalMember `json:"members"`
}

// CanonicalMember is one raw nomination within a canonical cluster, attributed to its source explorer
// (design §4): the canonical ID it was mapped to, the raw text as the explorer nominated it, the source
// explorer identity, and whether its canonical entity is backed by a single distinct source (a carried
// minority — salience, not corroboration).
type CanonicalMember struct {
	CanonicalID    string           `json:"canonicalId"`
	RawNomination  string           `json:"rawNomination"`
	SourceExplorer ExplorerIdentity `json:"sourceExplorer"`
	SingleSource   bool             `json:"singleSource"`
}

// Summary returns the one-line human summary of a Catalog result — the cluster/member/single-source
// tally + the count of proposed dimensions. Implements the mode-package ModeOutput contract.
func (o CatalogOutput) Summary() string {
	members, single := 0, 0
	for _, c := range o.Clusters {
		members += len(c.Members)
		for _, m := range c.Members {
			if m.SingleSource {
				single++
			}
		}
	}
	return fmt.Sprintf("cataloged %d nomination(s) into %d cluster(s) (%d single-source member(s)); %d proposed dimension(s)",
		members, len(o.Clusters), single, len(o.ProposedDimensions))
}

// Validate checks the catalog is usable: at least one cluster (a canonicalization that produced no
// canonical entity has nothing to organize). Proposed dimensions + coverage notes may legitimately be
// empty, so they are not required.
func (o CatalogOutput) Validate() error {
	if len(o.Clusters) == 0 {
		return fmt.Errorf("catalog output has no clusters")
	}
	return nil
}

// catalogExplorerFields is the Catalog mode's FIXED explorer-response schema (design §3): a list of
// distinct candidate nominations (required) + optional notes. It is app-owned (never collator-authored,
// §5) and is NOT the Map minimum schema — a formulation-free mode's explorer schema is its own contract.
var catalogExplorerFields = []Field{
	{Name: "candidates", Type: TypeString, Required: true, Repeated: true},
	{Name: "notes", Type: TypeString, Required: false, Repeated: false},
}

// CatalogExplorerSchema returns a fresh copy of the Catalog explorer schema (mirroring MinimumSchema's
// copy-per-call contract so a caller can never mutate the shared baseline).
func CatalogExplorerSchema() Schema {
	fields := make([]Field, len(catalogExplorerFields))
	copy(fields, catalogExplorerFields)
	return Schema{Fields: fields}
}

// CatalogExplorerPrompt derives the deterministic, app-owned explorer prompt for the Catalog mode from
// raw_task, preserving every purpose + criterion verbatim (add nothing, drop nothing). It asks the
// explorer to ENUMERATE BROADLY — coverage + novelty over polish (design §3) — and renders the exact
// fixed schema field names (RenderSchema) — the hard-won Map lesson: a prompt that merely says "match the
// schema" gives the model nothing to match, so it improvises names and fails validation.
func CatalogExplorerPrompt(raw RawTask) string {
	var b strings.Builder
	b.WriteString("Enumerate as many DISTINCT candidates/possibilities for the following task as you can — ")
	b.WriteString("prize COVERAGE and NOVELTY over polish. Cast a wide net; do not pre-filter to the ")
	b.WriteString("obvious few. Respond with a SINGLE JSON object. Output ONLY the JSON object — no prose ")
	b.WriteString("before or after it, and no markdown code fences.\n\n")
	b.WriteString("Purpose:\n")
	b.WriteString(raw.Purpose)
	b.WriteString("\n\nCriteria the candidates should be relevant to:\n")
	for _, c := range raw.Criteria {
		b.WriteString("- ")
		b.WriteString(c)
		b.WriteString("\n")
	}
	if strings.TrimSpace(raw.PriorContext) != "" {
		b.WriteString("\nPrior context:\n")
		b.WriteString(raw.PriorContext)
		b.WriteString("\n")
	}
	b.WriteString("\nThe JSON object MUST contain EXACTLY these fields:\n")
	b.WriteString(RenderSchema(CatalogExplorerSchema()))
	b.WriteString("Where: \"candidates\" is your list of distinct candidate names/possibilities (as many as " +
		"you can surface — each a short, self-contained label); \"notes\" is any brief context on your coverage.\n")
	return b.String()
}
