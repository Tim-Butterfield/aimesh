package mode

// This file holds the terminal collation contracts. Each contract's prompt spells out the exact output
// fields and asks for JSON only, and its parser validates the result into the mode's ModeOutput.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// ModeOutput is a mode's terminal output, such as schema.CollatorOutput for Map. It marshals as its
// concrete type.
type ModeOutput interface {
	// Summary returns a one-line summary of the output.
	Summary() string
}

// CollatorContract is a plain terminal collation: Prompt builds the collator prompt over the primary
// envelopes, and Parse validates the collator's output.
type CollatorContract interface {
	Prompt(primary []schema.Envelope) (string, error)
	Parse(raw []byte) (ModeOutput, error)
}

// mapCollator is the Map mode's contract. Its prompt labels each response with its `envelope#k` alias and
// asks every finding to cite the aliases it draws on; the host validates those citations afterwards
// (schema.ApplyCitations).
type mapCollator struct{}

// collatorEnvelopeWire is an envelope with its `envelope#k` alias, from schema.EnvelopeRef.
type collatorEnvelopeWire struct {
	Alias string `json:"envelope"`
	schema.Envelope
}

// citableEnvelopes pairs each primary envelope with its alias.
func citableEnvelopes(primary []schema.Envelope) []collatorEnvelopeWire {
	out := make([]collatorEnvelopeWire, len(primary))
	for i, env := range primary {
		out[i] = collatorEnvelopeWire{Alias: schema.EnvelopeRef(1, env.Order), Envelope: env}
	}
	return out
}

// Prompt builds the Map collator prompt.
func (mapCollator) Prompt(primary []schema.Envelope) (string, error) {
	b, err := json.Marshal(citableEnvelopes(primary))
	if err != nil {
		return "", fmt.Errorf("marshal explorer responses: %w", err)
	}
	aliases := schema.NewCitationIndex(primary).Aliases()
	return "You are the COLLATOR. Synthesize (do NOT tally — a single well-evidenced objection can outweigh " +
		"agreement) the verified explorer responses below into a SINGLE JSON object. Output ONLY the JSON " +
		"object — no prose before or after it, and no markdown code fences.\n\n" +
		"Each response below carries an \"envelope\" ALIAS. Those aliases are the ONLY citable sources in " +
		"this run: " + strings.Join(aliases, ", ") + ".\n\n" +
		"The JSON object MUST have EXACTLY these fields, fully populated (do not leave an array empty when " +
		"there is substance to report):\n" +
		"- \"synthesisSummary\": string — the integrated synthesis narrative.\n" +
		"- \"findings\": array of objects, each {\"statement\": string (the finding itself — never empty), " +
		"\"evidence\": string, \"confidence\": number 0..1, \"sources\": array of strings}.\n" +
		"  \"sources\" MUST cite the specific envelope alias(es) the finding is drawn from — one entry per " +
		"supporting response, written EXACTLY as the alias appears above (e.g. \"envelope#0\"), or narrowed " +
		"to a single claim as \"envelope#0/claims/2\" (the index into that response's \"claims\" array). " +
		"Cite every response that supports the finding. Do NOT invent an alias, do NOT cite an alias that is " +
		"not in the list above, and do NOT put prose, explorer names or model names in \"sources\" — anything " +
		"that is not one of those aliases will be discarded and the finding recorded as uncited.\n" +
		"- \"disagreementRegister\": array of objects, each {\"subject\": string, \"positions\": array of " +
		"{\"explorer\": {\"adapter\": string, \"model\": string, \"effort\": string}, \"stance\": string, " +
		"\"evidence\": string}, \"resolution\": string, \"residualRisk\": string}. Key EVERY position on the " +
		"full (adapter, model, effort) identity of the explorer that holds it.\n\nresponses:\n" + string(b) + "\n", nil
}

// Parse decodes the synthesis and requires a non-empty summary.
func (mapCollator) Parse(raw []byte) (ModeOutput, error) {
	var o schema.CollatorOutput
	obj, _, xerr := schema.ExtractJSONObject(raw)
	if xerr != nil {
		return nil, xerr
	}
	if err := json.Unmarshal(obj, &o); err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.SynthesisSummary) == "" {
		return nil, fmt.Errorf("synthesis output has an empty summary")
	}
	return o, nil
}

// synthesizeCollator is the Synthesize mode's contract: the collator chooses the strongest answer and grafts
// better elements from the others into it.
type synthesizeCollator struct{}

// Prompt builds the Synthesize collator prompt.
func (synthesizeCollator) Prompt(primary []schema.Envelope) (string, error) {
	b, err := json.Marshal(primary)
	if err != nil {
		return "", fmt.Errorf("marshal explorer responses: %w", err)
	}
	return "You are the COLLATOR. Each explorer below gave its single best complete answer to the task. " +
		"Do NOT tally or average. CHOOSE the strongest candidate answer and, where clearly beneficial, GRAFT " +
		"superior elements from the other answers into it to produce ONE composed best answer. Record which " +
		"explorer each grafted component came from, and record a minority report of the rejected alternatives " +
		"with why each was not chosen. Output ONLY a SINGLE JSON object — no prose before or after it, and no " +
		"markdown code fences.\n\n" +
		"The JSON object MUST have EXACTLY these fields, fully populated (do not leave an array empty when " +
		"there is substance to report):\n" +
		"- \"artifact\": string — the chosen/composed best answer (the complete deliverable; never empty).\n" +
		"- \"componentProvenance\": array of objects, each {\"component\": string (the grafted element), " +
		"\"fromExplorer\": {\"adapter\": string, \"model\": string, \"effort\": string}}. One entry per element " +
		"you grafted from an explorer other than the base answer.\n" +
		"- \"minorityReport\": array of objects, each {\"alternative\": string (the rejected answer/element), " +
		"\"fromExplorer\": {\"adapter\": string, \"model\": string, \"effort\": string}, \"whyRejected\": string}. " +
		"Key EVERY entry on the full (adapter, model, effort) identity of the explorer that held it.\n" +
		"- \"rationale\": string — why this candidate was chosen and why these grafts improve it.\n\nanswers:\n" +
		string(b) + "\n", nil
}

// Parse decodes and validates the composition.
func (synthesizeCollator) Parse(raw []byte) (ModeOutput, error) {
	var o schema.SynthesizeOutput
	obj, _, xerr := schema.ExtractJSONObject(raw)
	if xerr != nil {
		return nil, xerr
	}
	if err := json.Unmarshal(obj, &o); err != nil {
		return nil, err
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return o, nil
}

// CanonicalizingContract is a terminal collation that canonicalizes nominations. The pipeline extracts
// Nominations, calls one or two canonicalizers with CanonicalizerPrompt and ParseProposal, runs the
// confirmation round if the policy asks for it, and passes the resulting partition to Collate. The
// contract does not depend on the governance policy.
type CanonicalizingContract interface {
	// Nominations returns one nomination per candidate string in each primary envelope.
	Nominations(primary []schema.Envelope) []canon.Nomination
	// CanonicalizerPrompt builds the canonicalizer prompt over noms.
	CanonicalizerPrompt(noms []canon.Nomination) (string, error)
	// ParseProposal decodes a canonicalizer's output into a proposal attributed to decidedByCall and
	// identity.
	ParseProposal(raw []byte, noms []canon.Nomination, decidedByCall string, identity schema.ExplorerIdentity) (canon.Proposal, error)
	// Collate builds the mode's output from the partition.
	Collate(res canon.Result) (ModeOutput, error)
}

// CollateInput is the governance record a GovernedCollator reads. Every field is host-produced.
type CollateInput struct {
	Raw          schema.RawTask
	Partition    canon.Result
	Confirmation *canon.Confirmation
	// Governance holds the emitted claims, which a collator looks up rather than recomputing.
	Governance *govern.Report
	// Decision is the host tally, for a ballot-bearing mode.
	Decision *govern.Decision
	// Rounds are the executed rounds; a collator reads them for per-finding detail only.
	Rounds []round.Round
	Panel  govern.Panel
}

// GovernedCollator is implemented, in addition to CanonicalizingContract, by modes whose output needs the
// whole governance record. The pipeline uses it when present.
type GovernedCollator interface {
	CollateGoverned(in CollateInput) (ModeOutput, error)
}

// BallotContract declares a ballot-bearing mode's decision inputs and parses ballots. It has no method
// that ranks; the host freezes the inputs and tallies the ballots.
type BallotContract interface {
	// Criteria returns the decision criteria, derived from the task.
	Criteria(raw schema.RawTask) []govern.Criterion
	// Method returns the tally method.
	Method() govern.DecisionMethod
	// ShortlistSize returns how many candidates the shortlist holds for a universe of the given size.
	ShortlistSize(universeSize int) int
	// ParseBallot extracts one explorer's ballot from its envelope. An entry outside universe is an error.
	ParseBallot(env schema.Envelope, universe map[string]bool) (govern.Ballot, error)
}

// catalogCollator is the Catalog mode's canonicalizing contract.
type catalogCollator struct{}

// Nominations returns one nomination for each non-blank string in each envelope's "candidates".
func (catalogCollator) Nominations(primary []schema.Envelope) []canon.Nomination {
	var out []canon.Nomination
	for _, env := range primary {
		cands, _ := env.Response["candidates"].([]any)
		for _, c := range cands {
			s, ok := c.(string)
			if !ok || strings.TrimSpace(s) == "" {
				continue
			}
			out = append(out, canon.Nomination{
				Raw:            s,
				SourceExplorer: env.Identity,
				EnvelopeRef:    fmt.Sprintf("envelope#%d", env.Order),
			})
		}
	}
	return out
}

// catalogNominationWire is one nomination as shown to the canonicalizer, which clusters by index.
type catalogNominationWire struct {
	Index          int                     `json:"index"`
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
}

// CanonicalizerPrompt builds the Catalog canonicalizer prompt: cluster variants, keep every nomination
// index exactly once, and propose distinguishing dimensions.
func (catalogCollator) CanonicalizerPrompt(noms []canon.Nomination) (string, error) {
	wire := make([]catalogNominationWire, len(noms))
	for i, n := range noms {
		wire[i] = catalogNominationWire{Index: i, Raw: n.Raw, SourceExplorer: n.SourceExplorer}
	}
	arr, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("marshal nominations: %w", err)
	}
	lastIdx := "0"
	if len(noms) > 0 {
		lastIdx = strconv.Itoa(len(noms) - 1)
	}
	return "You are the CANONICALIZER — a role DISTINCT from the collator. Below are raw candidate " +
		"nominations gathered from INDEPENDENT explorers. CLUSTER the nominations that name the SAME underlying " +
		"candidate (synonyms/variants/near-duplicates) under one canonical entity; keep genuinely distinct " +
		"candidates in separate entities. You may CLUSTER duplicates but MUST NEVER DROP a nomination: every " +
		"nomination index 0.." + lastIdx + " MUST appear in EXACTLY one cluster (a singleton gets its own " +
		"cluster). Also extract the distinguishing DIMENSIONS (axes) that separate the candidates — these are " +
		"PROPOSED axes for the human, NOT decision criteria. Output ONLY a SINGLE JSON object — no prose before " +
		"or after it, and no markdown code fences.\n\n" +
		"The JSON object MUST have EXACTLY these fields, fully populated:\n" +
		"- \"clusters\": array of objects, each {\"canonicalId\": string (a stable slug for the entity), " +
		"\"name\": string (a human label for the entity), \"memberIndices\": array of integers (the indices of " +
		"the raw nominations clustered under it)}. Across ALL clusters, every nomination index appears exactly once.\n" +
		"- \"proposedDimensions\": array of strings — the distinguishing axes (PROPOSED, not criteria).\n" +
		"- \"coverageNotes\": string — notes on coverage/gaps across the nominations.\n\n" +
		"nominations:\n" + string(arr) + "\n", nil
}

// ParseProposal decodes the canonicalizer's output with parseClusterProposal. The surjectivity gate is
// enforced later by package canon.
func (catalogCollator) ParseProposal(raw []byte, _ []canon.Nomination, decidedByCall string, identity schema.ExplorerIdentity) (canon.Proposal, error) {
	return parseClusterProposal(raw, decidedByCall, identity)
}

// Collate builds the CatalogOutput from the partition: clusters with attributed members, proposed
// dimensions and coverage notes.
func (catalogCollator) Collate(res canon.Result) (ModeOutput, error) {
	out := schema.CatalogOutput{
		ProposedDimensions: res.ProposedDimensions,
		CoverageNotes:      res.CoverageNotes,
	}
	for _, c := range res.Clusters {
		cl := schema.CatalogCluster{Name: c.Name}
		for _, m := range c.Members {
			cl.Members = append(cl.Members, schema.CanonicalMember{
				CanonicalID:    m.CanonicalID,
				RawNomination:  m.RawNomination,
				SourceExplorer: m.SourceExplorer,
				SingleSource:   m.SingleSource,
			})
		}
		out.Clusters = append(out.Clusters, cl)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}
