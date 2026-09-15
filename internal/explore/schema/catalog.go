package schema

// This file holds the catalog mode's explorer schema and prompt, and the terminal CatalogOutput the host
// assembles from the canonicalization result. The mode's canonicalizing contract builds the output.

import (
	"fmt"
	"strings"
)

// CatalogOutput is the terminal output of the catalog mode: canonical clusters of the raw nominations,
// proposed distinguishing dimensions, and coverage notes. Catalog organizes without ranking, so the
// dimensions are suggestions, not decision criteria. It satisfies the mode package's ModeOutput contract.
type CatalogOutput struct {
	// Clusters are the canonical entities. Every raw nomination appears in exactly one cluster, and a
	// nomination from a single explorer is kept and marked single-source.
	Clusters []CatalogCluster `json:"clusters"`
	// ProposedDimensions are the distinguishing axes the canonicalizer extracted.
	ProposedDimensions []string `json:"proposedDimensions"`
	// CoverageNotes are the canonicalizer's notes on coverage/gaps across the nominations.
	CoverageNotes string `json:"coverageNotes"`
}

// CatalogCluster is one canonical entity: a human label + the raw nominations clustered under it.
type CatalogCluster struct {
	Name    string            `json:"name"`
	Members []CanonicalMember `json:"members"`
}

// CanonicalMember is one raw nomination within a cluster: its canonical ID, the text as nominated, the
// nominating explorer, and whether only one explorer backs the entity.
type CanonicalMember struct {
	CanonicalID    string           `json:"canonicalId"`
	RawNomination  string           `json:"rawNomination"`
	SourceExplorer ExplorerIdentity `json:"sourceExplorer"`
	SingleSource   bool             `json:"singleSource"`
}

// Summary returns a one-line summary of the cluster, member, single-source and dimension counts.
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

// Validate checks that the catalog has at least one cluster. Dimensions and notes may be empty.
func (o CatalogOutput) Validate() error {
	if len(o.Clusters) == 0 {
		return fmt.Errorf("catalog output has no clusters")
	}
	return nil
}

// catalogExplorerFields is the catalog explorer schema: candidate nominations and optional notes.
var catalogExplorerFields = []Field{
	{Name: "candidates", Type: TypeString, Required: true, Repeated: true},
	{Name: "notes", Type: TypeString, Required: false, Repeated: false},
}

// CatalogExplorerSchema returns a copy of the catalog explorer schema.
func CatalogExplorerSchema() Schema {
	fields := make([]Field, len(catalogExplorerFields))
	copy(fields, catalogExplorerFields)
	return Schema{Fields: fields}
}

// CatalogExplorerPrompt builds the catalog explorer prompt from raw, quoting the purpose and criteria
// verbatim, asking for broad enumeration, and rendering the schema's field names.
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
