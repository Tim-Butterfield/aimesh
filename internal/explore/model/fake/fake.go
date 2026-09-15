// Package fake provides a deterministic in-process model.Adapter for tests and demos. It returns
// explore-shaped output for every phase, varied per explorer so a panel disagrees, and Scenario values
// select failure and identity cases.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Scenario selects the fake's behavior.
type Scenario string

// Scenarios.
const (
	Valid            Scenario = "valid"             // schema-valid response, verified identity
	SchemaInvalid    Scenario = "schema_invalid"    // explorer response missing a required field
	IdentityMismatch Scenario = "identity_mismatch" // reports a different model
	SelfReported     Scenario = "self_reported"     // self-reported evidence for the requested model
	UnknownIdentity  Scenario = "unknown"           // reports no model
	InvokeError      Scenario = "invoke_error"      // Invoke returns an error
	BadFormulate     Scenario = "bad_formulate"     // malformed formulate output
	FencedResponse   Scenario = "fenced_response"   // valid JSON wrapped in a ```json fence
	// CanonMergeAll proposes one cluster containing every nomination, producing a contested merge when
	// paired with a normal canonicalizer.
	CanonMergeAll Scenario = "canon_merge_all"
	// ChallengeWrongMerge raises one wrong_merge challenge against the first entity with nominations from
	// two or more explorers.
	ChallengeWrongMerge Scenario = "challenge_wrong_merge"
	// Abstain returns a schema-valid deliberate abstention.
	Abstain Scenario = "abstain"
	// BallotReverse ranks the presented options in reverse, producing a full tie against a forward ballot.
	BallotReverse Scenario = "ballot_reverse"
	// ChallengeRefute refutes every pooled finding in the cross-review round.
	ChallengeRefute Scenario = "challenge_refute"
	// CompareSparse skips every cell of the last declared option and reports missing evidence for each.
	CompareSparse Scenario = "compare_sparse"
	// ForecastNonNumeric returns a text estimate, which the forecast schema rejects.
	ForecastNonNumeric Scenario = "forecast_non_numeric"
	// CollatorCiteAll makes the Map collator cite a real alias on every finding and falsely mark one finding
	// uncited, which the host overrides.
	CollatorCiteAll Scenario = "collator_cite_all"
)

// Adapter is a deterministic fake whose output depends on the call's phase and prompt and on its scenario.
// tag varies explorer output so a panel disagrees.
type Adapter struct {
	name     string
	tag      string
	scenario Scenario
}

// New returns a fake adapter named name. tag distinguishes this explorer's answers, such as "A" or "B".
func New(name, tag string, s Scenario) *Adapter { return &Adapter{name: name, tag: tag, scenario: s} }

func (a *Adapter) Name() string { return a.name }
func (a *Adapter) Available() (bool, string) {
	return true, "fake exploremesh adapter (" + a.name + ")"
}

// Evidence returns the fake's identity-evidence ceiling. Without it the pipeline would cap fake identities
// at `none`.
func (a *Adapter) Evidence() core.IdentityEvidence { return core.EvidenceInvocationTag }

func (a *Adapter) Invoke(_ context.Context, c model.Call) (model.Result, error) {
	if a.scenario == InvokeError {
		return model.Result{}, fault.New(fault.Internal, "fake: simulated invoke error")
	}
	actual, ev := c.Model, core.EvidenceInvocationTag
	switch a.scenario {
	case IdentityMismatch:
		actual = "fake-wrong-model-9"
	case SelfReported:
		ev = core.EvidenceSelfReport
	case UnknownIdentity:
		actual = ""
	}
	res := model.Result{ExitCode: 0, ActualModel: actual, Evidence: ev}
	// The fake recognizes each mode by a phrase in the mode's prompt.
	switch c.Phase {
	case schema.PhaseFormulate:
		res.Stdout = a.formulate()
	case schema.PhaseExplore, schema.PhaseBallot:
		switch {
		case a.scenario == Abstain:
			res.Stdout = a.abstain()
		// Ballot and cross-review prompts also contain the untrusted-data block, so check them first.
		case isBallotPrompt(c.Prompt):
			res.Stdout = a.ballot(c.Prompt)
		case isCrossReviewPrompt(c.Prompt):
			res.Stdout = a.crossReview(c.Prompt)
		case isMediatedRoundPrompt(c.Prompt):
			res.Stdout = a.exploreMediated(c.Prompt)
		case isComparePrompt(c.Prompt):
			res.Stdout = a.exploreCompare(c.Prompt, c.Model)
		case isForecastPrompt(c.Prompt):
			res.Stdout = a.exploreForecast(c.Model)
		case isFindingsPrompt(c.Prompt):
			res.Stdout = a.exploreFindings()
		case isShortlistPrompt(c.Prompt):
			res.Stdout = a.exploreShortlist()
		case isSynthesizePrompt(c.Prompt):
			res.Stdout = a.exploreSynthesize()
		case isCatalogPrompt(c.Prompt):
			res.Stdout = a.exploreCatalog()
		default:
			res.Stdout = a.explore()
		}
	case schema.PhaseSynthesize:
		switch {
		case isCompareCollatorPrompt(c.Prompt):
			res.Stdout = a.compareNarrative()
		case isForecastCollatorPrompt(c.Prompt):
			res.Stdout = a.forecastNarrative()
		case isSynthesizeCollatorPrompt(c.Prompt):
			res.Stdout = a.synthesizeCompose()
		default:
			res.Stdout = a.synthesize(c.Prompt)
		}
	case schema.PhaseCanonicalize:
		res.Stdout = a.canonicalize(c.Prompt)
	case schema.PhaseConfirm:
		res.Stdout = a.confirm(c.Prompt)
	default:
		// Includes the pre-flight probe, whose body is not parsed.
		res.Stdout = []byte("{}")
	}
	return res, nil
}

// isSynthesizePrompt reports whether prompt is the Synthesize explorer prompt.
func isSynthesizePrompt(prompt string) bool { return strings.Contains(prompt, "best COMPLETE answer") }

// isSynthesizeCollatorPrompt reports whether prompt is the Synthesize collator prompt.
func isSynthesizeCollatorPrompt(prompt string) bool {
	return strings.Contains(prompt, "componentProvenance")
}

// isCatalogPrompt reports whether prompt is the Catalog explorer prompt.
func isCatalogPrompt(prompt string) bool {
	return strings.Contains(prompt, "Enumerate as many DISTINCT")
}

// isMediatedRoundPrompt reports whether prompt is a later round's, which carries an untrusted-data block.
func isMediatedRoundPrompt(prompt string) bool {
	return strings.Contains(prompt, "BEGIN UNTRUSTED DATA")
}

// isFindingsPrompt reports whether prompt asks for typed findings, as Challenge and ai-collab do.
func isFindingsPrompt(prompt string) bool {
	return strings.Contains(prompt, `Each entry of "findings" MUST be a JSON object`)
}

// isShortlistPrompt reports whether prompt is the Shortlist explorer prompt.
func isShortlistPrompt(prompt string) bool {
	return strings.Contains(prompt, "Enumerate the CANDIDATE OPTIONS")
}

// isCrossReviewPrompt reports whether prompt is Challenge's cross-review round.
func isCrossReviewPrompt(prompt string) bool {
	return strings.Contains(prompt, "This is the CROSS-REVIEW round")
}

// isBallotPrompt reports whether prompt is Shortlist's ballot round, including the frozen decision header.
func isBallotPrompt(prompt string) bool {
	return strings.Contains(prompt, "This is the BALLOT round") && strings.Contains(prompt, "FROZEN DECISION INPUTS")
}

// formulate returns an explorer_task_payload: the minimum schema plus one additive field (a valid
// collator expansion), with a prompt.
func (a *Adapter) formulate() []byte {
	if a.scenario == BadFormulate {
		return []byte("{ this is not valid json")
	}
	exp := schema.MinimumSchema()
	exp.Fields = append(exp.Fields, schema.Field{Name: "tradeoffs", Type: schema.TypeString, Required: false, Repeated: true})
	p := schema.ExplorerTaskPayload{
		FinalPrompt:    "Explore the task and respond as JSON matching the schema.",
		ExpandedSchema: exp,
	}
	b, _ := json.Marshal(p)
	return b
}

// explore returns a schema-valid response varied by tag (so explorers disagree), or a deliberately
// schema-invalid one for the drop scenario.
func (a *Adapter) explore() []byte {
	if a.scenario == SchemaInvalid {
		// missing the required "confidence" field → ValidateResponse rejects → explorer dropped.
		return []byte(`{"claims":["x"],"evidence":"e","sources":["s"],"assumptions":[],"uncertainties":[]}`)
	}
	resp := map[string]any{
		"claims":        []any{"claim from " + a.tag},
		"evidence":      "evidence from explorer " + a.tag,
		"confidence":    0.75,
		"sources":       []any{"source-" + a.tag},
		"assumptions":   []any{"assumption-" + a.tag},
		"uncertainties": []any{"open question from " + a.tag},
		"tradeoffs":     []any{"tradeoff-" + a.tag},
	}
	b, _ := json.Marshal(resp)
	if a.scenario == FencedResponse {
		return []byte("```json\n" + string(b) + "\n```")
	}
	return b
}

// exploreSynthesize returns a schema-valid Synthesize-mode explorer response — one best complete answer
// varied by tag, so a panel of fakes gives the collator distinct answers to choose from.
func (a *Adapter) exploreSynthesize() []byte {
	resp := map[string]any{
		"answer":      "best answer from explorer " + a.tag,
		"rationale":   "rationale from explorer " + a.tag,
		"assumptions": []any{"assumption-" + a.tag},
		"confidence":  0.8,
	}
	b, _ := json.Marshal(resp)
	return b
}

// synthesizeCompose returns a fixed Synthesize-mode collator output: a non-empty composed artifact plus
// a populated component-provenance + minority report (so a synthesize run demonstrates the full shape).
func (a *Adapter) synthesizeCompose() []byte {
	out := schema.SynthesizeOutput{
		Artifact: "Composed best answer: explorer A's approach, grafted with explorer B's edge-case handling.",
		ComponentProvenance: []schema.ProvenanceEntry{
			{Component: "edge-case handling", FromExplorer: schema.ExplorerIdentity{Adapter: "codex-cli", Model: "gpt-5-codex", Effort: "medium"}},
		},
		MinorityReport: []schema.MinorityEntry{
			{Alternative: "a wholly different approach", FromExplorer: schema.ExplorerIdentity{Adapter: "claude-code", Model: "opus", Effort: "high"}, WhyRejected: "weaker against the stated criteria"},
		},
		Rationale: "chose the strongest candidate and grafted one clearly superior element (no tally).",
	}
	b, _ := json.Marshal(out)
	return b
}

// exploreCatalog returns a Catalog explorer response. Every tag nominates "Postgres" (a shared candidate)
// plus one unique candidate.
func (a *Adapter) exploreCatalog() []byte {
	var cands []any
	switch a.tag {
	case "A":
		cands = []any{"Postgres", "SQLite"}
	case "B":
		cands = []any{"Postgres", "MySQL"}
	default:
		cands = []any{"Postgres", "candidate-" + a.tag}
	}
	resp := map[string]any{"candidates": cands, "notes": "coverage notes from explorer " + a.tag}
	b, _ := json.Marshal(resp)
	return b
}

// exploreFindings returns a Challenge or ai-collab round-1 response. Tags A and B share one finding at
// different severities; each tag also has unique findings.
func (a *Adapter) exploreFindings() []byte {
	shared := map[string]any{
		"statement":       "Race condition on shutdown",
		"severity":        "high",
		"failureScenario": "two shutdown paths run concurrently and the second frees state the first is still reading",
		"evidence":        "the shutdown handler takes no lock (reviewer " + a.tag + ")",
	}
	var findings []any
	switch a.tag {
	case "A":
		findings = []any{shared, map[string]any{
			"statement":       "No input validation on the config path",
			"severity":        "medium",
			"failureScenario": "a malformed config path is passed straight to the loader",
			"evidence":        "no validation between parse and load",
		}}
	case "B":
		// The shared finding at a higher severity.
		crit := map[string]any{}
		maps.Copy(crit, shared)
		crit["severity"] = "critical"
		findings = []any{crit, map[string]any{
			"statement":       "Missing rate limit on the public endpoint",
			"severity":        "medium",
			"failureScenario": "an unauthenticated caller can exhaust the worker pool",
			"evidence":        "no limiter on the public handler",
		}}
	default:
		findings = []any{
			map[string]any{"statement": "Unbounded retry loop in " + a.tag, "severity": "low",
				"failureScenario": "a permanent failure retries forever", "evidence": "no attempt cap"},
			map[string]any{"statement": "Undocumented failure mode in " + a.tag, "severity": "low",
				"failureScenario": "an operator cannot tell a partial failure from a success", "evidence": "no error taxonomy"},
		}
	}
	b, _ := json.Marshal(map[string]any{"findings": findings, "notes": "coverage notes from reviewer " + a.tag})
	return b
}

// crossReview returns a cross-review response that echoes every pooled item with a stance and severity.
// The echo lets tests confirm that later rounds cannot raise counts.
func (a *Adapter) crossReview(prompt string) []byte {
	stance := "deepens"
	if a.scenario == ChallengeRefute {
		stance = "refutes"
	}
	assessments := make([]any, 0, 4)
	for _, it := range pooledItems(prompt) {
		assessments = append(assessments, map[string]any{
			"ref":      it.Ref,
			"stance":   stance,
			"severity": "high",
			"depth":    "depth added by reviewer " + a.tag + " on " + it.Text,
			"evidence": "second-look evidence from reviewer " + a.tag,
		})
	}
	b, _ := json.Marshal(map[string]any{"assessments": assessments, "notes": "cross-review notes from " + a.tag})
	return b
}

// exploreShortlist returns a Shortlist round-1 response with the same overlap as exploreCatalog.
func (a *Adapter) exploreShortlist() []byte {
	var cands []any
	switch a.tag {
	case "A":
		cands = []any{"Postgres", "SQLite"}
	case "B":
		cands = []any{"Postgres", "MySQL"}
	default:
		cands = []any{"Postgres", "candidate-" + a.tag}
	}
	b, _ := json.Marshal(map[string]any{"candidates": cands, "notes": "coverage notes from explorer " + a.tag})
	return b
}

// ballot returns a ballot ranking the presented refs and approving the top half. Tag A keeps the presented
// order, tag B moves the last option first, and BallotReverse reverses it.
func (a *Adapter) ballot(prompt string) []byte {
	items := pooledItems(prompt)
	refs := make([]string, 0, len(items))
	for _, it := range items {
		refs = append(refs, it.Ref)
	}
	switch {
	case a.scenario == BallotReverse:
		for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
			refs[i], refs[j] = refs[j], refs[i]
		}
	case a.tag == "B" && len(refs) > 1:
		refs = append([]string{refs[len(refs)-1]}, refs[:len(refs)-1]...)
	}
	ranking := make([]any, 0, len(refs))
	for _, r := range refs {
		ranking = append(ranking, r)
	}
	approved := make([]any, 0, len(refs))
	for i := 0; i < (len(refs)+1)/2; i++ {
		approved = append(approved, refs[i])
	}
	b, _ := json.Marshal(map[string]any{
		"ranking": ranking, "approved": approved,
		"rationale": "preference of voter " + a.tag + " under the frozen criteria",
	})
	return b
}

// pooledItem is one item of the host's projected round payload.
type pooledItem struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
}

// pooledItems returns the items in prompt's untrusted-data block.
func pooledItems(prompt string) []pooledItem {
	var payload struct {
		Items []pooledItem `json:"items"`
	}
	if body, ok := untrustedBlock(prompt); ok {
		_ = json.Unmarshal([]byte(body), &payload)
	}
	return payload.Items
}

// isComparePrompt reports whether prompt is the Compare explorer prompt.
func isComparePrompt(prompt string) bool {
	return strings.Contains(prompt, schema.CompareOptionSetMarker)
}

// isForecastPrompt reports whether prompt is the Forecast explorer prompt.
func isForecastPrompt(prompt string) bool {
	return strings.Contains(prompt, schema.ForecastTargetMarker)
}

// isCompareCollatorPrompt reports whether prompt is the Compare narrative prompt.
func isCompareCollatorPrompt(prompt string) bool {
	return strings.Contains(prompt, "host-computed comparison:")
}

// isForecastCollatorPrompt reports whether prompt is the Forecast narrative prompt.
func isForecastCollatorPrompt(prompt string) bool {
	return strings.Contains(prompt, "host-pooled forecast:")
}

// fixedSpaceTag returns the persona for a fixed-space call: the `-a`, `-b` or `-c` suffix of the requested
// model, else the adapter's tag. The CLI shares one fake instance across a panel, so the per-call model is
// what lets its members disagree.
func (a *Adapter) fixedSpaceTag(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, t := range []string{"a", "b", "c"} {
		if strings.HasSuffix(m, "-"+t) {
			return strings.ToUpper(t)
		}
	}
	return a.tag
}

// declaredCriterion is one entry of the criteria block in the Compare prompt.
type declaredCriterion struct {
	Name      string `json:"name"`
	Direction string `json:"direction"`
	Role      string `json:"role"`
}

// compareDeclaration returns the options and criteria declared in the Compare prompt.
func compareDeclaration(prompt string) (options []string, criteria []declaredCriterion) {
	decodeAfter(prompt, schema.CompareOptionSetMarker, &options)
	decodeAfter(prompt, schema.CompareCriteriaMarker, &criteria)
	return options, criteria
}

// decodeAfter decodes the first JSON value after marker into v. A Decoder is used because text follows the
// value.
func decodeAfter(prompt, marker string, v any) {
	_, after, ok := strings.Cut(prompt, marker)
	if !ok {
		return
	}
	_ = json.NewDecoder(strings.NewReader(after)).Decode(v)
}

// compareScoreGrid holds scores by criterion index parity (rows) and option index mod 3 (columns). With an
// even criterion higher-is-better and an odd one lower-is-better, option 0 dominates option 1 and option 2
// stays on the frontier.
var compareScoreGrid = [2][3]float64{
	{9, 6, 3}, // even criterion index
	{7, 8, 2}, // odd criterion index
}

// compareScore returns the score for an option and criterion. Evaluator B scores the first option on the
// first criterion four points lower, creating one split cell on the frontier.
func compareScore(tag string, optionIdx, criterionIdx int) float64 {
	v := compareScoreGrid[criterionIdx%2][optionIdx%3]
	if tag == "B" && optionIdx == 0 && criterionIdx == 0 {
		return v - 4
	}
	return v
}

// exploreCompare returns a Compare response covering every declared cell. The last option fails every
// filter. Under CompareSparse the last option is skipped and reported as missing evidence.
func (a *Adapter) exploreCompare(prompt, model string) []byte {
	tag := a.fixedSpaceTag(model)
	options, criteria := compareDeclaration(prompt)
	evaluations := make([]any, 0)
	evaluated := make([]any, 0, len(options))
	missing := make([]any, 0)
	for oi, opt := range options {
		sparse := a.scenario == CompareSparse && oi == len(options)-1
		if !sparse {
			evaluated = append(evaluated, opt)
		}
		for ci, crit := range criteria {
			if sparse {
				missing = append(missing, opt+" / "+crit.Name+": this evaluator has no evidence about "+opt)
				continue
			}
			cell := map[string]any{
				"option": opt, "criterion": crit.Name,
				"rationale": "evaluation of " + opt + " on " + crit.Name + " by evaluator " + tag,
			}
			if crit.Role == string(schema.RoleFilter) {
				verdict := string(schema.VerdictPass)
				if oi == len(options)-1 {
					verdict = string(schema.VerdictFail)
				}
				cell["value"] = verdict
			} else {
				cell["value"] = compareScore(tag, oi, ci)
			}
			evaluations = append(evaluations, cell)
		}
	}
	b, _ := json.Marshal(map[string]any{
		"evaluations": evaluations, "optionsEvaluated": evaluated,
		"missingEvidence": missing, "notes": "coverage notes from evaluator " + tag,
	})
	return b
}

// exploreForecast returns a Forecast response. Tags A and B give close estimates and any other tag gives a
// far outlier with its reasoning. Under ForecastNonNumeric the estimate is text.
func (a *Adapter) exploreForecast(model string) []byte {
	if a.scenario == ForecastNonNumeric {
		return []byte(`{"estimate":"about one hundred","low":"ninety","high":"one hundred and twenty","reasoning":"a narrative estimate, deliberately not machine-readable"}`)
	}
	tag := a.fixedSpaceTag(model)
	est, low, high, why := 100.0, 90.0, 120.0, "steady extrapolation of the current trend"
	switch tag {
	case "A":
	case "B":
		est, low, high, why = 110, 95, 130, "the same trend with a small allowance for seasonality"
	default:
		est, low, high, why = 300, 250, 400, "a step change nobody else priced in: the capacity constraint lifts inside the horizon, which roughly triples the ceiling"
	}
	b, _ := json.Marshal(map[string]any{
		"estimate": est, "low": low, "high": high, "confidence": 0.8,
		"assumptions": []any{"assumption of forecaster " + tag},
		"reasoning":   why + " (forecaster " + tag + ")",
	})
	return b
}

// compareNarrative returns the Compare collator's narrative output, which contains no numbers.
func (a *Adapter) compareNarrative() []byte {
	b, _ := json.Marshal(map[string]any{
		"reading":           "the options trade off against each other rather than ordering cleanly (fake collator).",
		"disagreementNotes": []any{"the evaluators split on one cell; the record keeps both values."},
		"evidenceGaps":      []any{"some cells were not evidenced by every evaluator."},
		"cautions":          []any{"this is a Pareto view, not a ranking — no weights were declared."},
	})
	return b
}

// forecastNarrative returns the Forecast collator's narrative output, which contains no numbers.
func (a *Adapter) forecastNarrative() []byte {
	b, _ := json.Marshal(map[string]any{
		"reading":      "the panel clusters tightly with one far estimate (fake collator).",
		"outlierNotes": []any{"the high estimate rests on a step change the others did not price in."},
		"keyDrivers":   []any{"the trend assumption dominates the result."},
		"cautions":     []any{"the interval is the median of the stated bounds, not an envelope."},
	})
	return b
}

// abstain returns a deliberate abstention: the reserved abstention marker with every mode's required
// fields present and empty, so it is schema-valid in every mode.
func (a *Adapter) abstain() []byte {
	resp := map[string]any{
		schema.AbstentionField: true,
		"candidates":           []any{},
		"findings":             []any{},
		"assessments":          []any{},
		"ranking":              []any{},
		"approved":             []any{},
		"evaluations":          []any{},
		"optionsEvaluated":     []any{},
		"missingEvidence":      []any{},
		"estimate":             0,
		"low":                  0,
		"high":                 0,
		"reasoning":            "abstaining: outside this forecaster's competence",
		"claims":               []any{},
		"answer":               "",
		"rationale":            "",
		"evidence":             "abstaining: outside this explorer's competence",
		"confidence":           0,
		"sources":              []any{},
		"assumptions":          []any{},
		"uncertainties":        []any{},
		"notes":                "abstained",
	}
	b, _ := json.Marshal(resp)
	return b
}

// exploreMediated returns a later-round response that echoes the items it was shown as `candidates`, so
// tests can confirm the echo does not raise independence counts.
func (a *Adapter) exploreMediated(prompt string) []byte {
	var payload struct {
		Items []struct {
			Ref  string `json:"ref"`
			Text string `json:"text"`
		} `json:"items"`
	}
	if body, ok := untrustedBlock(prompt); ok {
		_ = json.Unmarshal([]byte(body), &payload)
	}
	cands := make([]any, 0, len(payload.Items))
	refs := make([]any, 0, len(payload.Items))
	for _, it := range payload.Items {
		cands = append(cands, it.Text)
		refs = append(refs, it.Ref)
	}
	resp := map[string]any{
		"candidates":    cands,
		"echoedRefs":    refs,
		"refinements":   []any{"depth added by explorer " + a.tag},
		"severity":      "medium",
		"confidence":    0.7,
		"notes":         "later-round refinement from explorer " + a.tag,
		"claims":        []any{"refined claim from " + a.tag},
		"evidence":      "later-round evidence from " + a.tag,
		"sources":       []any{"source-" + a.tag},
		"assumptions":   []any{},
		"uncertainties": []any{},
	}
	b, _ := json.Marshal(resp)
	return b
}

// untrustedBlock returns the JSON payload between the untrusted-data delimiters in prompt.
func untrustedBlock(prompt string) (string, bool) {
	const begin, end = "BEGIN UNTRUSTED DATA -----", "----- END UNTRUSTED DATA"
	i := strings.Index(prompt, begin)
	j := strings.Index(prompt, end)
	if i < 0 || j <= i {
		return "", false
	}
	block := prompt[i+len(begin) : j]
	if k := strings.Index(block, "{"); k >= 0 {
		return block[k:], true
	}
	return "", false
}

// confirmEntityWire is one provisional entity in the confirmation prompt.
type confirmEntityWire struct {
	CanonicalID string `json:"canonicalId"`
	Name        string `json:"name"`
	Members     []struct {
		SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
	} `json:"members"`
}

// confirm returns a confirmation-round response: no challenges, or under ChallengeWrongMerge one wrong_merge
// against the first entity with nominations from two explorers.
func (a *Adapter) confirm(prompt string) []byte {
	target := ""
	if _, after, ok := strings.Cut(prompt, "provisional entities:\n"); a.scenario == ChallengeWrongMerge && ok {
		var ents []confirmEntityWire
		_ = json.Unmarshal([]byte(after), &ents)
		for _, e := range ents {
			sources := map[schema.ExplorerIdentity]bool{}
			for _, m := range e.Members {
				sources[m.SourceExplorer] = true
			}
			if len(sources) > 1 {
				target = e.CanonicalID
				break
			}
		}
	}
	if target == "" {
		return []byte(`{"challenges":[]}`)
	}
	out := map[string]any{"challenges": []any{map[string]any{
		"type":        "wrong_merge",
		"canonicalId": target,
		"reason":      "these nominations name different things (fake explorer " + a.tag + ")",
	}}}
	b, _ := json.Marshal(out)
	return b
}

// canonicalizeNominationWire is one nomination in the canonicalizer prompt.
type canonicalizeNominationWire struct {
	Index          int                     `json:"index"`
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
}

// canonicalize clusters the prompt's nominations by exact raw text, in first-seen order.
func (a *Adapter) canonicalize(prompt string) []byte {
	const marker = "nominations:\n"
	var noms []canonicalizeNominationWire
	if _, after, ok := strings.Cut(prompt, marker); ok {
		_ = json.Unmarshal([]byte(after), &noms)
	}
	order := []string{}
	byRaw := map[string][]int{}
	for _, n := range noms {
		if _, seen := byRaw[n.Raw]; !seen {
			order = append(order, n.Raw)
		}
		byRaw[n.Raw] = append(byRaw[n.Raw], n.Index)
	}
	type cluster struct {
		CanonicalID   string `json:"canonicalId"`
		Name          string `json:"name"`
		MemberIndices []int  `json:"memberIndices"`
	}
	clusters := make([]cluster, 0, len(order))
	if a.scenario == CanonMergeAll {
		all := make([]int, 0, len(noms))
		for _, n := range noms {
			all = append(all, n.Index)
		}
		clusters = append(clusters, cluster{CanonicalID: "canon-all", Name: "everything", MemberIndices: all})
	} else {
		for i, raw := range order {
			clusters = append(clusters, cluster{
				CanonicalID:   fmt.Sprintf("canon-%d", i),
				Name:          raw,
				MemberIndices: byRaw[raw],
			})
		}
	}
	out := map[string]any{
		"clusters":           clusters,
		"proposedDimensions": []string{"maturity", "operational-complexity"},
		"coverageNotes":      "fake canonicalizer: clustered nominations by exact name match.",
	}
	b, _ := json.Marshal(out)
	return b
}

// collatorAliases returns the `envelope#k` aliases in the Map collator prompt's responses block.
func collatorAliases(prompt string) []string {
	var envs []struct {
		Envelope string `json:"envelope"`
	}
	decodeAfter(prompt, "responses:\n", &envs)
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		if e.Envelope != "" {
			out = append(out, e.Envelope)
		}
	}
	return out
}

// synthesize returns a Map collator output with a summary, two findings and a disagreement entry. The first
// finding cites every alias shown (the first narrowed to `/claims/0`); the second cites "s1", which is not an
// alias, so the host marks it uncited. Under CollatorCiteAll both findings cite real aliases and the second
// falsely claims to be uncited.
func (a *Adapter) synthesize(prompt string) []byte {
	aliases := collatorAliases(prompt)
	cited := append([]string(nil), aliases...)
	if len(cited) > 0 {
		cited[0] += "/claims/0"
	}
	second, claimsUncited := []string{"s1"}, false
	if a.scenario == CollatorCiteAll {
		second, claimsUncited = aliases, true
	}
	out := schema.CollatorOutput{
		SynthesisSummary: "Synthesis of the explorer panel (fake collator).",
		Findings: []schema.Finding{
			{Statement: "A well-evidenced conclusion", Evidence: "cross-explorer support", Confidence: 0.8, Sources: cited},
			// The host overwrites Uncited regardless of what the collator sets.
			{Statement: "A conclusion the collator did not source", Evidence: "asserted without attribution",
				Confidence: 0.5, Sources: second, Uncited: claimsUncited},
		},
		DisagreementRegister: []schema.DisagreementEntry{
			{
				Subject: "primary approach",
				Positions: []schema.Position{
					{Explorer: schema.ExplorerIdentity{Adapter: "claude-code", Model: "opus", Effort: "high"}, Stance: "approach A", Evidence: "e-A"},
					{Explorer: schema.ExplorerIdentity{Adapter: "codex-cli", Model: "gpt-5-codex", Effort: "medium"}, Stance: "approach B", Evidence: "e-B"},
				},
				Resolution:   "surfaced to the human (pure-judgment dispute)",
				ResidualRisk: "approach choice unresolved",
			},
		},
	}
	b, _ := json.Marshal(out)
	return b
}
