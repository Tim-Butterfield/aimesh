package schema

// This file holds the round-1 prompt of the shipped AI-COLLAB composition (design §11: "ai-collab
// reproduced"). The owner's ai-collab decomposes into three exploremesh stages — each agent SHORTLISTS its
// own findings blind, then the agents CHALLENGE each other's findings through the collator-mediated
// cross-review, then one terminal collation — so the composition needs exactly one artifact of its own: a
// blind round-1 prompt that asks for the agent's OWN findings about the task rather than an attack on a
// supplied artifact.
//
// Everything else it uses is the Challenge mode's, unchanged: the same findings schema, the same closed
// severity enum, the same round-2 CROSS-REVIEW contract, the same canonicalize+confirm governance and the
// same severity-triaged terminal register. That reuse is the point — if the composition needed its own copy
// of any of those, the claim that ai-collab decomposes into exploremesh's modes would be false.

import "strings"

// CollabFindingsPrompt is the deterministic, app-owned round-1 prompt of the ai-collab composition: shortlist
// YOUR OWN findings, blind. It renders the SAME findings schema Challenge round-1 uses (including the exact
// nested field names and the closed severity enum), because the two rounds produce the same kind of artifact
// — the only difference is where the material comes from: a supplied artifact there, the agent's own analysis
// here.
func CollabFindingsPrompt(raw RawTask) string {
	var b strings.Builder
	b.WriteString("You are one agent of a collaborating panel. Working ALONE — you are not being shown any ")
	b.WriteString("other agent's work — produce YOUR OWN shortlist of the most important findings for the ")
	b.WriteString("task below: the things that, if missed, would most damage the outcome. Prize concrete, ")
	b.WriteString("checkable findings over general commentary. A later round will show you the panel's ")
	b.WriteString("combined findings so the agents can challenge each other; this round is yours alone. ")
	b.WriteString("Respond with a SINGLE JSON object. Output ONLY the JSON object — no prose before or after ")
	b.WriteString("it, and no markdown code fences.\n\n")
	b.WriteString("Task:\n")
	b.WriteString(raw.Purpose)
	b.WriteString("\n\nCriteria the findings must be relevant to:\n")
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
	// An ai-collab run MAY still carry an artifact (the panel is often collaborating ON something); it is
	// simply not required, which is the one task-level difference from Challenge.
	if art := RenderArtifactUnderReview(raw.Artifact); art != "" {
		b.WriteString("\n")
		b.WriteString(art)
	}
	b.WriteString("\nThe JSON object MUST contain EXACTLY these fields:\n")
	b.WriteString(RenderSchema(ChallengeExplorerSchema()))
	b.WriteString("Each entry of \"findings\" MUST be a JSON object with EXACTLY these fields: " +
		"{\"statement\": string (the finding itself, a short self-contained claim — never empty), " +
		"\"severity\": string (EXACTLY one of " + severityList() + " — do not invent another value), " +
		"\"failureScenario\": string (the concrete situation in which this bites), " +
		"\"evidence\": string (what supports it)}.\n" +
		"\"notes\" is any brief context on your coverage.\n")
	return b.String()
}
