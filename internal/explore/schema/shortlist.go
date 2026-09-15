package schema

// This file holds the shortlist mode's round prompts and schemas: round 1 enumerates candidate options,
// and round 2 is a ballot over the confirmed canonical IDs.
//
// A ballot is cast over canonical IDs, so each entry resolves to one recorded candidate and a voter cannot
// add a new one. A ballot response has no field for a winner, score or tally; the host computes the
// ranking (govern.Tally) from decision inputs frozen and hashed before voting.
//
// The terminal ShortlistOutput lives in package mode, because it embeds govern.Claim and govern imports
// schema.

import "strings"

// shortlistExplorerFields is the shortlist round-1 explorer schema: candidate options and optional notes.
// It has the same shape as catalog's; the modes differ in what follows round 1.
var shortlistExplorerFields = []Field{
	{Name: "candidates", Type: TypeString, Required: true, Repeated: true},
	{Name: "notes", Type: TypeString, Required: false, Repeated: false},
}

// ShortlistExplorerSchema returns a fresh copy of the Shortlist round-1 explorer schema.
func ShortlistExplorerSchema() Schema {
	fields := make([]Field, len(shortlistExplorerFields))
	copy(fields, shortlistExplorerFields)
	return Schema{Fields: fields}
}

// ShortlistExplorerPrompt builds the round-1 prompt, which asks for candidate options broadly and tells
// the explorer this round is not the choice, so no explorer narrows the set the panel votes on.
func ShortlistExplorerPrompt(raw RawTask) string {
	var b strings.Builder
	b.WriteString("Enumerate the CANDIDATE OPTIONS for the following decision — as many DISTINCT, plausible ")
	b.WriteString("candidates as you can surface. This round is NOT the choice: do not rank, do not ")
	b.WriteString("recommend, and do not pre-filter to the obvious few. You will be asked to choose in a ")
	b.WriteString("later round, over the whole panel's combined candidate set. Respond with a SINGLE JSON ")
	b.WriteString("object. Output ONLY the JSON object — no prose before or after it, and no markdown code fences.\n\n")
	b.WriteString("Decision to be made:\n")
	b.WriteString(raw.Purpose)
	b.WriteString("\n\nCriteria the chosen option must satisfy:\n")
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
	b.WriteString(RenderSchema(ShortlistExplorerSchema()))
	b.WriteString("Where: \"candidates\" is your list of distinct candidate options (each a short, " +
		"self-contained label); \"notes\" is any brief context on your coverage.\n")
	return b.String()
}

// --- round 2: the ballot ---

// ballotFields is the ballot-response schema. `ranking` is required, best first; `approved` is a separate
// optional question tallied on its own; `rationale` is prose kept out of every governance field.
var ballotFields = []Field{
	{Name: "ranking", Type: TypeString, Required: true, Repeated: true},
	{Name: "approved", Type: TypeString, Required: false, Repeated: true},
	{Name: "rationale", Type: TypeString, Required: false, Repeated: false},
}

// BallotSchema returns a fresh copy of the ballot-response schema.
func BallotSchema() Schema {
	fields := make([]Field, len(ballotFields))
	copy(fields, ballotFields)
	return Schema{Fields: fields}
}

// BallotPrompt builds the ballot instruction. untrustedDataBlock is the host-framed list of confirmed
// candidates in their recorded random order, embedded verbatim. The pipeline prepends the frozen decision
// record the prompt refers to. The prompt tells the voter that position means nothing, that the host
// counts the ballots, and that it cannot add a candidate.
func BallotPrompt(raw RawTask, untrustedDataBlock string) string {
	var b strings.Builder
	b.WriteString("This is the BALLOT round. The block below is the CONFIRMED candidate universe: the whole ")
	b.WriteString("panel's candidates, grouped into canonical entities by a canonicalizer and confirmed by ")
	b.WriteString("the panel. They are presented in a RANDOMIZED order — position means nothing.\n\n")
	b.WriteString("Cast ONE ballot over these candidates, under the criteria and the decision rules frozen ")
	b.WriteString("above. Rank the candidates you can judge, best first, using their \"ref\" values EXACTLY ")
	b.WriteString("as written in the block. You may rank a subset. You may NOT add a candidate that is not ")
	b.WriteString("in the block — the universe is closed, and an unrecognized entry is rejected.\n\n")
	b.WriteString("Do NOT compute or state a result. Do NOT report which candidate \"wins\", how many ")
	b.WriteString("reviewers prefer it, or that there is a consensus. The HOST tallies the ballots; your ")
	b.WriteString("ballot is one informed preference under this shared framing, and it is recorded as that.\n\n")
	b.WriteString("Decision to be made:\n")
	b.WriteString(raw.Purpose)
	b.WriteString("\n\nCriteria the chosen option must satisfy:\n")
	for _, c := range raw.Criteria {
		b.WriteString("- ")
		b.WriteString(c)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(untrustedDataBlock)
	b.WriteString("\nRespond with a SINGLE JSON object. Output ONLY the JSON object — no prose before or ")
	b.WriteString("after it, and no markdown code fences. It MUST contain EXACTLY these fields:\n")
	b.WriteString(RenderSchema(BallotSchema()))
	b.WriteString("Where: \"ranking\" is the candidate \"ref\" values in YOUR preference order, best first " +
		"(a subset is fine; no duplicates); \"approved\" is the subset of \"ref\" values you consider " +
		"acceptable at all (a separate question from the ranking); \"rationale\" is a brief statement of why " +
		"you ranked them this way.\n")
	return b.String()
}

// BallotResponse is one explorer's ballot as raw strings. govern.Ballot.Validate checks that each entry
// names a confirmed candidate.
type BallotResponse struct {
	Ranking   []string
	Approved  []string
	Rationale string
}

// ParseBallotResponse reads the ballot from a validated response, trimming blanks and removing repeated
// entries while preserving order.
func ParseBallotResponse(response map[string]any) BallotResponse {
	return BallotResponse{
		Ranking:   dedupeStrings(stringList(response, "ranking")),
		Approved:  dedupeStrings(stringList(response, "approved")),
		Rationale: strings.TrimSpace(stringField(response, "rationale")),
	}
}

// stringList reads a repeated field from a decoded response, skipping blanks and formatting non-string
// elements as stringField does.
func stringList(response map[string]any, name string) []string {
	raw, _ := response[name].([]any)
	out := make([]string, 0, len(raw))
	for i := range raw {
		v := strings.TrimSpace(stringField(map[string]any{"v": raw[i]}, "v"))
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// dedupeStrings removes duplicates, preserving first-appearance order.
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
