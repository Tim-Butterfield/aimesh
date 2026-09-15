package schema

// This file holds the round-1 prompt of the ai-collab mode. Each agent lists its own findings blind, the
// agents then challenge each other's findings in a mediated cross-review, and one collation ends the run.
// Everything after round 1 reuses the challenge mode: its findings schema, severity values, cross-review
// round, canonicalization and confirmation, and terminal register.

import "strings"

// CollabFindingsPrompt builds the ai-collab round-1 prompt, which asks each agent for its own findings
// about the task. It renders the challenge findings schema, including the nested fields and severity
// values.
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
	// Unlike challenge, ai-collab accepts an artifact without requiring one.
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
