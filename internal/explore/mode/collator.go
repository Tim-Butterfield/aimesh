package mode

// This file holds the per-mode COLLATOR contract (design §3): the terminal collation is app-owned
// per mode, not one hardcoded shape. A CollatorContract renders the collator instruction (spelling out
// the exact nested output structure — the hard-won lesson from the Map fixes: always render the field
// names + "output ONLY JSON, no fences") and parses/validates the collator's raw output into the mode's
// own terminal ModeOutput. Map's contract is the plain synthesize prompt + parse + identity-policy
// behavior; Synthesize adds the select/compose contract. The registry pins one
// contract per ModeSpec; the pipeline resolves it and persists whichever mode output ran.

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

// ModeOutput is a mode's terminal collator output (design §3). Each mode has its OWN concrete output
// type (Map → schema.CollatorOutput; Synthesize → schema.SynthesizeOutput); the pipeline persists
// whichever ran behind this marker interface, and the surfaces render Summary() (with an optional
// type-switch for a mode's richer detail). A JSON round-trip of the concrete value is the machine surface.
type ModeOutput interface {
	// Summary returns a one-line human summary of the terminal output (for the CLI/manifest/ACP echo).
	Summary() string
}

// CollatorContract is a mode's app-owned terminal-collation behavior (design §3). Prompt builds the
// collator instruction over the primary envelopes — rendering the exact nested output structure
// explicitly. Parse decodes + validates the collator's raw output into the mode's ModeOutput. It
// depends only on schema (never the pipeline), so the pipeline resolves a contract from a ModeSpec
// without an import cycle.
type CollatorContract interface {
	Prompt(primary []schema.Envelope) (string, error)
	Parse(raw []byte) (ModeOutput, error)
}

// --- Map: the collate-only contract ---

// mapCollator is the Map mode's terminal contract. Its Prompt is the synthesize prompt plus the C1
// CITATION instruction:
// each explorer response is labeled with its prompt-facing `envelope#k` alias and every finding is required
// to cite the aliases it drew from. Nothing the collator answers with is trusted — the host validates the
// citations afterwards (schema.ApplyCitations) — but a model cannot cite a vocabulary it was never shown,
// so the prompt has to teach it first.
type mapCollator struct{}

// collatorEnvelopeWire is one primary envelope as the collator sees it: the full envelope plus its
// prompt-facing ALIAS. The alias is rendered by schema.EnvelopeRef — the same function the capture
// manifest's `envelope#k → Envelope.ID` table and canon/govern attribution use — so the citation
// vocabulary shown to the model is literally the one the host will validate against and record.
type collatorEnvelopeWire struct {
	Alias string `json:"envelope"`
	schema.Envelope
}

// citableEnvelopes labels the primary panel with its aliases for the prompt.
func citableEnvelopes(primary []schema.Envelope) []collatorEnvelopeWire {
	out := make([]collatorEnvelopeWire, len(primary))
	for i, env := range primary {
		out[i] = collatorEnvelopeWire{Alias: schema.EnvelopeRef(1, env.Order), Envelope: env}
	}
	return out
}

// Prompt hands the collator the verified explorer responses to synthesize (not tally), each labeled with
// its `envelope#k` alias, and requires every finding to CITE the aliases that support it.
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

// Parse decodes the collator's synthesis and enforces the non-empty-summary guard.
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

// --- Synthesize: select/compose the strongest answer (design §3) ---

// synthesizeCollator is the Synthesize mode's terminal contract: the collator SELECTS the strongest
// candidate and GRAFTS superior elements from the others into one composed artifact — it does NOT tally.
// The prompt renders the exact SynthesizeOutput structure so a real model emits the reserved fields.
type synthesizeCollator struct{}

// Prompt instructs the collator to select/compose over the candidate answers and renders the exact
// nested SynthesizeOutput field names (+ "output ONLY JSON, no fences").
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

// Parse decodes the collator's composition and enforces the non-empty-artifact guard.
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

// --- Catalog: enumerate broadly → CANONICALIZE → organize into clusters (design §3/§4) ---

// CanonicalizingContract is a mode's app-owned CANONICALIZING terminal collation (design §3/§4): unlike a
// plain CollatorContract (Prompt → one collate model call → Parse), its terminal step runs the raw
// explorer nominations through the canonicalization component (internal/canon). The pipeline resolves it
// from ModeSpec.Canonicalizing and drives the flow: it extracts the nominations (Nominations), builds a
// DECOUPLED canonicalizer model call around CanonicalizerPrompt/ParseProposal (a distinct call,
// SEPARATELY identity-verified — a strong-evidence mismatch halts, §0 F-A), hands it to canon.Canonicalize
// (which records the APPEND-ONLY, revision-hashed merge-ledger + enforces the surjectivity GATE), and
// finally Collate assembles the mode output as a deterministic VIEW over the canonical partition. Catalog
// is the only single-canonicalizer mode that uses it.
//
// The same contract serves the RANKING-GRADE path: when a mode's CanonicalizationPolicy sets Dual, the
// pipeline drives TWO of these calls (independent identities) into canon.CanonicalizeDual; when it sets
// Confirm, the pipeline runs the binding confirmation round over the provisional partition and hands
// Collate the CONFIRMED revision. A contract therefore never needs to know which governance grade it is
// running under — Catalog stays observe posture (single canonicalizer, no confirmation) either way.
type CanonicalizingContract interface {
	// Nominations extracts the raw candidate nominations from the verified primary envelopes (one per
	// candidate string per explorer), carrying each nomination's source explorer identity + envelope ref.
	Nominations(primary []schema.Envelope) []canon.Nomination
	// CanonicalizerPrompt renders the canonicalizer instruction over the nominations — spelling out the
	// exact proposal JSON structure ("output ONLY JSON, no fences": the map/synthesize lesson).
	CanonicalizerPrompt(noms []canon.Nomination) (string, error)
	// ParseProposal decodes the canonicalizer's raw output into a canon.Proposal, stamping the deciding
	// call ref + the SEPARATELY-verified canonicalizer identity onto it so every ledger row is attributable.
	ParseProposal(raw []byte, noms []canon.Nomination, decidedByCall string, identity schema.ExplorerIdentity) (canon.Proposal, error)
	// Collate assembles the terminal ModeOutput from the canonicalization result (a deterministic host
	// view over the append-only ledger; PROPOSED dimensions are labeled, never promoted to criteria).
	Collate(res canon.Result) (ModeOutput, error)
}

// CollateInput is the FULL governed record a terminal collation may read (design §0 F-C): the confirmed
// partition, the confirmation record, the emitted governance claims, the host tally, the recorded rounds and
// the frozen panel. Every field is a HOST artifact — there is nothing here a model asserted — which is what
// lets an adjudicative output be assembled as a pure view over it.
type CollateInput struct {
	Raw          schema.RawTask
	Partition    canon.Result
	Confirmation *canon.Confirmation
	// Governance carries the claims the host already emitted over the confirmed partition, so a collator
	// looks a claim up rather than recomputing (and possibly re-deriving) a count.
	Governance *govern.Report
	// Decision is the HOST tally, present only for a ballot-bearing mode.
	Decision *govern.Decision
	// Rounds are the executed rounds in order; Rounds[0] is the immutable blind baseline. A collator reads
	// them for per-finding DETAIL (severity, evidence, round-2 depth) — never to re-derive a count, which the
	// baseline type would refuse anyway.
	Rounds []round.Round
	Panel  govern.Panel
}

// GovernedCollator is the OPTIONAL richer terminal-collation seam. A mode whose output must carry the
// governance record — a claim-pinned register, a host-tallied ranking — implements it IN ADDITION to
// CanonicalizingContract, and the pipeline prefers it when present. It is an extra interface rather than a
// widened Collate signature for one reason: Catalog's partition-only Collate path stays untouched, so its
// observe-posture behavior holds by construction rather than by review.
type GovernedCollator interface {
	CollateGoverned(in CollateInput) (ModeOutput, error)
}

// BallotContract is a mode's app-owned BALLOT contract (design §3 Shortlist / §4). Note what it does
// NOT contain: there is no method that produces a ranking, an ordering, or a winner. It declares the frozen
// decision inputs (criteria, tally method, shortlist size) and parses ONE explorer's ballot; the host freezes,
// hashes, solicits and TALLIES. A ranking a mode contract could compute is a ranking a model could influence.
type BallotContract interface {
	// Criteria are the decision criteria, each carrying its origin + aggregationMethod (§4). They are frozen
	// and hashed before the ballot is solicited, so they are derived from the TASK — never from the tally.
	Criteria(raw schema.RawTask) []govern.Criterion
	// Method is the versioned HOST tally rule the decision is computed under.
	Method() govern.DecisionMethod
	// ShortlistSize is the frozen cut for a universe of the given size — how many candidates the shortlist
	// holds. Frozen with everything else, so the cut cannot be chosen after seeing which candidate it admits.
	ShortlistSize(universeSize int) int
	// ParseBallot lifts ONE explorer's ballot out of its recorded ballot-round envelope. universe is the
	// confirmed canonical ID set; an entry outside it is an ERROR (govern.Ballot.Validate), never a silent drop.
	ParseBallot(env schema.Envelope, universe map[string]bool) (govern.Ballot, error)
}

// catalogCollator is the Catalog mode's canonicalizing contract. It extracts candidate nominations from
// the blind round-1 envelopes, renders the exact canonicalizer proposal structure, parses the proposed
// partition, and assembles the CatalogOutput view over canon.Result (§4 host-derived organization).
type catalogCollator struct{}

// Nominations flattens the verified panel into raw nominations: each explorer's "candidates[]" entry
// becomes one nomination attributed to that explorer + its envelope ref. Blank/non-string entries are
// skipped (the schema already required candidates to be a string array, so this is defensive).
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

// catalogNominationWire is the per-nomination shape rendered into the canonicalizer prompt: the model
// clusters by INDEX, so the index is explicit alongside the raw text + source explorer.
type catalogNominationWire struct {
	Index          int                     `json:"index"`
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
}

// CanonicalizerPrompt renders the canonicalizer instruction: cluster synonyms/variants under one canonical
// entity, NEVER drop a nomination (every index appears exactly once — the surjectivity contract stated to
// the model), extract the distinguishing PROPOSED dimensions, and emit the exact JSON structure. It
// embeds the nominations as a JSON array (index/raw/sourceExplorer) so the mapping is unambiguous.
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
	return "You are the CANONICALIZER — a role DISTINCT from the collator (design §4). Below are raw candidate " +
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

// ParseProposal decodes the canonicalizer's raw output into a canon.Proposal, stamping the deciding call
// ref + the separately-verified canonicalizer identity onto it. It does NOT itself enforce surjectivity —
// that GATE is canon.Canonicalize's host-checked invariant over the parsed proposal (a single authority).
// The decode is the package-shared one (proposal.go): every canonicalizing mode asks its canonicalizer for
// the same proposal JSON, and the shape `memberIndices` arrives in is exactly what the gate is checked over,
// so exactly one function is responsible for reading it out of the model's bytes.
func (catalogCollator) ParseProposal(raw []byte, _ []canon.Nomination, decidedByCall string, identity schema.ExplorerIdentity) (canon.Proposal, error) {
	return parseClusterProposal(raw, decidedByCall, identity)
}

// Collate assembles the CatalogOutput as a deterministic VIEW over the canonicalization result: each
// canonical cluster + its attributed members (single-source carried through), the PROPOSED dimensions,
// and coverage notes. Validate requires >=1 cluster.
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
