package schema

// This file holds the SHORTLIST mode's app-owned round artifacts (design §3 Shortlist row / §4): the
// FIXED round-1 "enumerate the candidate options" prompt + schema, and the FIXED round-2 BALLOT prompt +
// schema over the CONFIRMED canonical IDs.
//
// The ballot is the mode's whole point and its whole risk. Three things keep it honest, and two of them live
// here:
//
//   - A ballot is cast over CANONICAL IDs, never over free text. The option set handed to a voter is the
//     confirmed partition in its persisted randomized presentation order, so a ballot entry resolves to
//     exactly one recorded entity and a voter cannot introduce a candidate the panel never nominated.
//   - A ballot RESPONSE CANNOT CARRY A RESULT. Its schema has a ranking, an approval set and a rationale —
//     there is no field for "the winner", a score, or a tally. The ranking is computed by the HOST from the
//     ballots (govern.Tally); a ranking asserted by a model is not a ranking the artifacts support.
//   - (in internal/govern) the decision inputs are FROZEN + HASHED before the ballot is solicited.
//
// The terminal ShortlistOutput lives in internal/mode, because each ranked entry embeds the host-computed
// govern.Claim and govern sits ABOVE schema in the import graph.

import "strings"

// shortlistExplorerFields is the Shortlist mode's FIXED round-1 explorer schema: the distinct candidate
// OPTIONS the explorer would consider, plus optional notes. It is deliberately the same SHAPE as Catalog's
// (enumerate broadly) — the difference between the two modes is not the round-1 schema but what happens
// afterwards: Catalog organizes and stops, Shortlist confirms the universe and puts it to a ballot.
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

// ShortlistExplorerPrompt is the deterministic, app-owned round-1 prompt: enumerate the candidate options
// BROADLY and blindly. It says explicitly that this round is not the choice — an explorer that pre-filters
// to its own favourite here would narrow the universe the whole panel later votes over, which is precisely
// the agenda-setting power the confirmed-universe-then-ballot split exists to remove.
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

// --- Round 2: the explicit BALLOT over the confirmed canonical IDs (design §3/§4) ---

// ballotFields is the FIXED ballot-response schema. `ranking` is required and ordered (best first);
// `approved` is an optional, separate question ("which of these would you accept at all?") tallied on its own
// rather than folded into the score; `rationale` is model prose, quarantined into the collatorNarrative
// namespace by the host and never merged into a machine governance field (§0 F-C). There is deliberately no
// field for a winner, a score, or a tally.
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

// BallotPrompt builds the ballot instruction. untrustedDataBlock is the host's ALREADY-FRAMED digest of the
// CONFIRMED canonical universe in its persisted randomized presentation order — embedded verbatim, never
// re-framed. frozenHeader is the host-rendered FROZEN DECISION record (criteria, method, quorum, tie rule,
// shortlist size and their hash), prepended by the pipeline rather than by this contract; it is passed in so
// the instruction can point at it explicitly.
//
// The prompt states the honest status of what it is soliciting: an INFORMED PREFERENCE UNDER SHARED FRAMING.
// A voter is told the position order means nothing, that the host does the counting, and that inventing a
// candidate is not available to it.
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

// BallotResponse is one explorer's ballot lifted out of a validated round-2 response — still raw strings at
// this point, because whether each entry names a candidate in the confirmed universe is the HOST's check
// (govern.Ballot.Validate), not this parser's guess.
type BallotResponse struct {
	Ranking   []string
	Approved  []string
	Rationale string
}

// ParseBallotResponse lifts the ballot out of ONE validated response, trimming blanks and de-duplicating the
// ranking (a repeated entry is a single preference stated twice, not two preferences) while preserving order.
func ParseBallotResponse(response map[string]any) BallotResponse {
	return BallotResponse{
		Ranking:   dedupeStrings(stringList(response, "ranking")),
		Approved:  dedupeStrings(stringList(response, "approved")),
		Rationale: strings.TrimSpace(stringField(response, "rationale")),
	}
}

// stringList reads a repeated string field out of a decoded response, skipping blanks and rendering a
// non-string element mechanically (the same rule stringField follows).
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
