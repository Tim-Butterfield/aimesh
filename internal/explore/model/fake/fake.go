// Package fake is exploremesh's deterministic in-process adapter for the CLI's fake path + tests.
// Unlike meshcore's review-shaped fake, it emits EXPLOREMESH-shaped output: a formulate payload,
// schema-valid explorer responses (varied per explorer so they genuinely disagree), and a collator
// synthesis. Scenarios drive the failure-path/identity tests. It implements meshcore/model.Adapter.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Scenario selects the fake's behavior for identity/failure-path coverage.
type Scenario string

const (
	Valid            Scenario = "valid"             // schema-valid response, verified identity
	SchemaInvalid    Scenario = "schema_invalid"    // explorer response missing a required field → dropped
	IdentityMismatch Scenario = "identity_mismatch" // reports a different model → mismatch → halt
	SelfReported     Scenario = "self_reported"     // self-report evidence, matching model → self_reported (weak)
	UnknownIdentity  Scenario = "unknown"           // reports no model → unknown (weak)
	InvokeError      Scenario = "invoke_error"      // returns a Go error → dropped/halt depending on role
	BadFormulate     Scenario = "bad_formulate"     // collator: malformed formulate output → deterministic fallback
	FencedResponse   Scenario = "fenced_response"   // explorer wraps VALID JSON in a ```json fence → recovered via extraction
	// CanonMergeAll is an AGGRESSIVELY MERGING canonicalizer: it proposes ONE cluster containing every
	// nomination. Paired with a normal canonicalizer under the dual merge-agreement rule (design §0 F-B) it
	// produces a CONTESTED merge — the case the whole dual rule exists for, since an aggressive merger is
	// precisely how a canonicalizer would manufacture corroboration if its proposal were authoritative.
	CanonMergeAll Scenario = "canon_merge_all"
	// ChallengeWrongMerge makes the explorer raise ONE typed `wrong_merge` challenge in the confirmation round
	// (design §4), against the first presented entity that groups nominations from >=2 explorers.
	ChallengeWrongMerge Scenario = "challenge_wrong_merge"
	// Abstain makes the explorer DELIBERATELY abstain (a schema-valid response carrying `"abstain": true`) —
	// distinct from a technical absence, and the case the dual denominators exist to keep honest (§1).
	Abstain Scenario = "abstain"
	// BallotReverse makes the explorer cast its ballot in the EXACT REVERSE of the presented order.
	// Paired with the default forward ballot it produces a total tie under the positional tally — the case the
	// frozen tie rule exists for, and the only way to reach it deliberately.
	BallotReverse Scenario = "ballot_reverse"
	// ChallengeRefute makes the explorer REFUTE every pooled finding in the cross-review round instead of
	// deepening it — so the "surviving strengths" half of a Challenge register has a real case.
	ChallengeRefute Scenario = "challenge_refute"
	// CompareSparse makes the explorer SKIP every cell of the LAST declared option and say so in
	// missingEvidence. It is the case the explicit missing-evidence reporting exists for: a cell nobody
	// could judge must be visibly empty rather than scored as zero, and the option must be carried as
	// incomparable rather than quietly losing the comparison.
	CompareSparse Scenario = "compare_sparse"
	// ForecastNonNumeric makes the explorer answer a forecast with a NARRATIVE estimate ("about one hundred")
	// instead of a number — the response the typed explorer schema must reject at the envelope boundary
	// rather than interpret.
	ForecastNonNumeric Scenario = "forecast_non_numeric"
	// CollatorCiteAll makes the Map collator cite a REAL `envelope#k` alias on every finding — and makes one
	// of them additionally CLAIM `"uncited": true` (C1). It is the fixture for the two halves of the
	// host-authoritative citation rule that the default fake cannot show at once: a fully-cited collation
	// carries no `uncited` finding, and a model's assertion ABOUT its own sourcing is overwritten by the
	// host rather than believed.
	CollatorCiteAll Scenario = "collator_cite_all"
)

// Adapter is a deterministic exploremesh fake keyed on the phase label + a scenario. `tag` varies
// explorer output so a panel of fakes produces a real disagreement to synthesize.
type Adapter struct {
	name     string
	tag      string
	scenario Scenario
}

// New builds a fake adapter named `name`. `tag` distinguishes this explorer's stance (e.g. "A"/"B").
func New(name, tag string, s Scenario) *Adapter { return &Adapter{name: name, tag: tag, scenario: s} }

func (a *Adapter) Name() string { return a.name }
func (a *Adapter) Available() (bool, string) {
	return true, "fake exploremesh adapter (" + a.name + ")"
}

// Evidence declares the ceiling this fake may be classified on — its strongest emitted tier
// (invocation_tag). Required so exploremesh's fail-closed CapEvidence does not clamp fake identities to
// `none` (which would fail the ≥2-verified minimum and break the suite + demo).
func (a *Adapter) Evidence() core.IdentityEvidence { return core.EvidenceInvocationTag }

func (a *Adapter) Invoke(_ context.Context, c model.Call) (model.Result, error) {
	if a.scenario == InvokeError {
		return model.Result{}, fault.New(fault.Internal, "fake: simulated invoke error")
	}
	// Identity: default verified (matching model, medium+ evidence); scenarios weaken/break it.
	actual, ev := c.Model, core.EvidenceInvocationTag
	switch a.scenario {
	case IdentityMismatch:
		actual = "fake-wrong-model-9"
	case SelfReported:
		ev = core.EvidenceSelfReport // matching model + self-report → self_reported
	case UnknownIdentity:
		actual = "" // no model reported → unknown
	}
	res := model.Result{ExitCode: 0, ActualModel: actual, Evidence: ev}
	// The fake is mode-aware without a mode field: the app-owned prompt renders the mode's exact contract,
	// so the fake keys on a distinctive phrase to emit the RIGHT explorer/collator shape for the mode
	// (Map vs Synthesize). This lets a `--mode synthesize` demo/test run end-to-end on fakes alone.
	switch c.Phase {
	case schema.PhaseFormulate:
		res.Stdout = a.formulate()
	case schema.PhaseExplore, schema.PhaseBallot:
		switch {
		case a.scenario == Abstain:
			res.Stdout = a.abstain()
		// The two adjudicative mediated rounds are checked BEFORE the generic mediated branch: a ballot and a
		// cross-review both carry the host's untrusted-data block, and they need different answer shapes.
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
		// Includes the identity PRE-FLIGHT probe (design §1): its body is irrelevant — the probe exists so the
		// adapter performs a real invocation and the identity engine gets evidence before the fan-out.
		res.Stdout = []byte("{}")
	}
	return res, nil
}

// isSynthesizePrompt reports whether the explorer prompt is the Synthesize mode's app-owned prompt (it
// asks for the explorer's single best COMPLETE answer — a phrase the Map prompt never contains).
func isSynthesizePrompt(prompt string) bool { return strings.Contains(prompt, "best COMPLETE answer") }

// isSynthesizeCollatorPrompt reports whether the collator prompt is the Synthesize contract (it renders
// the componentProvenance field — a name the Map collator prompt never contains).
func isSynthesizeCollatorPrompt(prompt string) bool {
	return strings.Contains(prompt, "componentProvenance")
}

// isCatalogPrompt reports whether the explorer prompt is the Catalog mode's app-owned prompt (it asks the
// explorer to "Enumerate as many DISTINCT candidates" — a phrase no other mode's prompt contains).
func isCatalogPrompt(prompt string) bool {
	return strings.Contains(prompt, "Enumerate as many DISTINCT")
}

// isMediatedRoundPrompt reports whether this is a LATER (mediated) explorer round: the host wraps the carried
// round artifact in its untrusted-data delimiters, which round 1 never has (design §6).
func isMediatedRoundPrompt(prompt string) bool {
	return strings.Contains(prompt, "BEGIN UNTRUSTED DATA")
}

// isFindingsPrompt reports whether the round-1 prompt asks for typed FINDINGS — the shape Challenge and the
// ai-collab composition share (they render the identical findings-object instruction, which is exactly the
// reuse the composition is meant to demonstrate).
func isFindingsPrompt(prompt string) bool {
	return strings.Contains(prompt, `Each entry of "findings" MUST be a JSON object`)
}

// isShortlistPrompt reports whether the round-1 prompt is Shortlist's ("enumerate the CANDIDATE OPTIONS" — a
// phrase no other mode's prompt contains).
func isShortlistPrompt(prompt string) bool {
	return strings.Contains(prompt, "Enumerate the CANDIDATE OPTIONS")
}

// isCrossReviewPrompt reports whether this is Challenge's round-2 mediated CROSS-REVIEW.
func isCrossReviewPrompt(prompt string) bool {
	return strings.Contains(prompt, "This is the CROSS-REVIEW round")
}

// isBallotPrompt reports whether this is Shortlist's round-2 BALLOT. It keys on the FROZEN DECISION header the
// host prepends, so the fake only ever answers with a ballot when the framing really was frozen first.
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
		// A schema-valid body, but wrapped in a markdown code fence (the real dogfood failure): the
		// pipeline must strip the fence and recover it rather than drop on "invalid character '`'".
		return []byte("```json\n" + string(b) + "\n```")
	}
	return b
}

// exploreSynthesize returns a schema-valid Synthesize-mode explorer response — one best complete answer
// varied by tag, so a panel of fakes gives the collator genuinely DISTINCT answers to select/compose.
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

// exploreCatalog returns a schema-valid Catalog-mode explorer response — a list of candidate nominations
// varied by tag so a panel of fakes produces genuine OVERLAP (a duplicate to merge) + UNIQUE singletons
// (a minority to carry). A ("Postgres","SQLite") and B ("Postgres","MySQL") share "Postgres" (merged,
// multi-source) and each carry a singleton (single-source). Other tags reuse the shared candidate + a
// unique one so a larger panel still yields a mergeable duplicate.
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

// --- The adjudicative modes (design §3 Challenge + Shortlist rows) ---

// exploreFindings returns a schema-valid round-1 CHALLENGE / ai-collab response: typed findings varied by tag
// so a panel of fakes produces genuine OVERLAP (a finding two reviewers independently raise → corroborated,
// with the two of them disagreeing on severity so the host's max-severity triage has something to do) and
// UNIQUE singletons (a minority finding that must survive into the register). With a 3-explorer panel the
// shared finding is 2-of-3, which is the case the dual denominators are most easily got wrong on.
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
		// The SAME defect at a different severity: the host register reports the MAX any blind source assigned,
		// so this escalates the shared finding to critical without averaging anyone's judgment away.
		crit := map[string]any{}
		for k, v := range shared {
			crit[k] = v
		}
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

// crossReview returns the round-2 CROSS-REVIEW response: it ECHOES every pooled canonical ref back with a
// stance + its own severity. Echoing is deliberate — a reviewer repeating a finding it has now SEEN is exactly
// the contamination the anti-echo invariant must neutralize (§0 F-A), and a fake that quietly declined to echo
// would make the anti-echo test prove nothing.
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

// exploreShortlist returns a schema-valid round-1 SHORTLIST response: candidate options varied by tag so the
// panel produces one shared candidate (merged, multi-source) plus per-explorer singletons — the same overlap
// shape the Catalog fake uses, because it is the shape that exercises both merge and minority carry-through.
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

// ballot returns the BALLOT response: a ranking over the canonical refs the host presented, plus approvals for
// its top half. The order varies deterministically by tag so a panel of fakes produces a real preference
// disagreement rather than unanimity — tag "A" votes the presented order, "B" rotates the last option to the
// front, and the BallotReverse scenario votes the exact reverse (which, against a forward ballot, produces the
// total tie the frozen tie rule exists for).
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

// pooledItem mirrors one entry of the host's projected round payload (ref + text), so a fake can react to the
// canonical entities it was actually shown rather than inventing IDs the host would reject.
type pooledItem struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
}

// pooledItems extracts the projected items out of the host's untrusted-data block.
func pooledItems(prompt string) []pooledItem {
	var payload struct {
		Items []pooledItem `json:"items"`
	}
	if body, ok := untrustedBlock(prompt); ok {
		_ = json.Unmarshal([]byte(body), &payload)
	}
	return payload.Items
}

// --- The FIXED-SPACE modes (design §3 Compare + Forecast rows) ---

// isComparePrompt reports whether the round-1 prompt is Compare's (it renders the declared option set as a
// machine-readable block — a marker no other mode's prompt contains).
func isComparePrompt(prompt string) bool {
	return strings.Contains(prompt, schema.CompareOptionSetMarker)
}

// isForecastPrompt reports whether the round-1 prompt is Forecast's (same idea: the declared estimation
// target block).
func isForecastPrompt(prompt string) bool {
	return strings.Contains(prompt, schema.ForecastTargetMarker)
}

// isCompareCollatorPrompt / isForecastCollatorPrompt report whether a PhaseSynthesize call is a FIXED-SPACE
// NARRATIVE call. They key on the host's "here is the finished result" block, so the fake can only ever
// answer with narrative when it really was shown one.
func isCompareCollatorPrompt(prompt string) bool {
	return strings.Contains(prompt, "host-computed comparison:")
}

func isForecastCollatorPrompt(prompt string) bool {
	return strings.Contains(prompt, "host-pooled forecast:")
}

// fixedSpaceTag picks the fake's PERSONA for one fixed-space call. It prefers a `-a`/`-b`/`-c` suffix on the
// REQUESTED MODEL and falls back to the adapter's construction tag.
//
// The model suffix is there for a real reason rather than convenience: the CLI registry keys adapters by
// NAME, so a demo panel of `fake` slots shares ONE adapter instance and would otherwise give every explorer
// the identical answer. A panel that cannot disagree cannot exercise the one thing these two modes exist to
// report — a per-cell disagreement, and an outlier estimate — so the persona keys on the per-call model,
// which really does differ across a panel. Every other mode's fake behavior is untouched.
func (a *Adapter) fixedSpaceTag(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, t := range []string{"a", "b", "c"} {
		if strings.HasSuffix(m, "-"+t) {
			return strings.ToUpper(t)
		}
	}
	return a.tag
}

// declaredCriterion mirrors one entry of the criteria block the compare prompt embeds.
type declaredCriterion struct {
	Name      string `json:"name"`
	Direction string `json:"direction"`
	Role      string `json:"role"`
}

// compareDeclaration recovers the DECLARED space out of the compare prompt: the option set and the criteria
// with their direction + role. The fake reads exactly what a real evaluator is shown, so a fake response can
// never name an option the host did not declare (which the host would — correctly — record as unrecognized).
func compareDeclaration(prompt string) (options []string, criteria []declaredCriterion) {
	decodeAfter(prompt, schema.CompareOptionSetMarker, &options)
	decodeAfter(prompt, schema.CompareCriteriaMarker, &criteria)
	return options, criteria
}

// decodeAfter decodes the FIRST JSON value following a marker. A json.Decoder is used rather than
// json.Unmarshal because a prompt carries several blocks and Unmarshal rejects the trailing text.
func decodeAfter(prompt, marker string, v any) {
	idx := strings.Index(prompt, marker)
	if idx < 0 {
		return
	}
	_ = json.NewDecoder(strings.NewReader(prompt[idx+len(marker):])).Decode(v)
}

// compareScoreGrid is the fake's deterministic option×criterion score table (rows = criterion index parity,
// columns = option index mod 3). The values are chosen so the HOST's Pareto frontier is non-trivial under
// MIXED directions: with an even criterion read higher-is-better and an odd one read lower-is-better,
// option 1 is DOMINATED by option 0 (9≥6 and 7≤8) while option 2 survives on the odd criterion (2 is the
// best low value) — a real frontier of two rather than "every option is optimal", which would prove nothing.
var compareScoreGrid = [2][3]float64{
	{9, 6, 3}, // criterion index EVEN
	{7, 8, 2}, // criterion index ODD
}

// compareScore is the fake's score for one (option, criterion) pair, plus the ONE deliberate disagreement:
// evaluator "B" scores the (first option, first criterion) cell four points lower than everyone else. That
// cell is the fixture for "a cell where explorers disagree is SURFACED as disagreement" — and it is placed
// on the frontier's leading option on purpose, so the split is impossible to miss and the conditional flag
// on a Pareto entry has a real case.
func compareScore(tag string, optionIdx, criterionIdx int) float64 {
	v := compareScoreGrid[criterionIdx%2][optionIdx%3]
	if tag == "B" && optionIdx == 0 && criterionIdx == 0 {
		return v - 4
	}
	return v
}

// exploreCompare returns a schema-valid round-1 COMPARE response over the DECLARED space it was shown: a
// value for every option×criterion cell, a gate verdict for every filter criterion (the LAST declared option
// fails every gate, unanimously across the panel — the fixture for "a filter criterion excludes an option"),
// and, under the CompareSparse scenario, no evaluation at all for the last option plus an explicit
// missing-evidence line for each of its cells.
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
				// The gate: the LAST declared option fails, everything else passes. Every evaluator says the
				// same, so the host's "exclusion requires no dissent" rule really does exclude it.
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

// exploreForecast returns a schema-valid round-1 FORECAST response: a numeric estimate + interval +
// reasoning, varied by tag so a three-explorer panel produces two close estimates and ONE far outlier — the
// fixture for "an outlier is identified WITH its rationale retained". Under ForecastNonNumeric the estimate
// is deliberately a STRING, which the typed explorer schema must reject rather than interpret.
func (a *Adapter) exploreForecast(model string) []byte {
	if a.scenario == ForecastNonNumeric {
		// A narrative estimate. `estimate` is a typed number in the app-owned schema, so this response is
		// schema-INVALID and the explorer is dropped with a reason — never parsed into a number.
		return []byte(`{"estimate":"about one hundred","low":"ninety","high":"one hundred and twenty","reasoning":"a narrative estimate, deliberately not machine-readable"}`)
	}
	tag := a.fixedSpaceTag(model)
	est, low, high, why := 100.0, 90.0, 120.0, "steady extrapolation of the current trend"
	switch tag {
	case "A":
	case "B":
		est, low, high, why = 110, 95, 130, "the same trend with a small allowance for seasonality"
	default:
		// The OUTLIER, and it has a reason. The host identifies it and carries this sentence with it — the
		// point being that a forecaster far from the panel may be the only one who noticed something.
		est, low, high, why = 300, 250, 400, "a step change nobody else priced in: the capacity constraint lifts inside the horizon, which roughly triples the ceiling"
	}
	b, _ := json.Marshal(map[string]any{
		"estimate": est, "low": low, "high": high, "confidence": 0.8,
		"assumptions": []any{"assumption of forecaster " + tag},
		"reasoning":   why + " (forecaster " + tag + ")",
	})
	return b
}

// compareNarrative / forecastNarrative return the FIXED-SPACE collator's narrative-only output. Note what
// they do NOT contain: a number, a ranking, a "corrected" aggregate. The shape the host asks for has no
// field for one, and the fake honors that — a fake that smuggled a number in would be testing a contract
// exploremesh does not offer.
func (a *Adapter) compareNarrative() []byte {
	b, _ := json.Marshal(map[string]any{
		"reading":           "the options trade off against each other rather than ordering cleanly (fake collator).",
		"disagreementNotes": []any{"the evaluators split on one cell; the record keeps both values."},
		"evidenceGaps":      []any{"some cells were not evidenced by every evaluator."},
		"cautions":          []any{"this is a Pareto view, not a ranking — no weights were declared."},
	})
	return b
}

func (a *Adapter) forecastNarrative() []byte {
	b, _ := json.Marshal(map[string]any{
		"reading":      "the panel clusters tightly with one far estimate (fake collator).",
		"outlierNotes": []any{"the high estimate rests on a step change the others did not price in."},
		"keyDrivers":   []any{"the trend assumption dominates the result."},
		"cautions":     []any{"the interval is the median of the stated bounds, not an envelope."},
	})
	return b
}

// abstain returns a schema-valid DELIBERATE ABSTENTION (design §1): the reserved `abstain` marker plus the
// required fields left empty. It is the honest "I decline to answer", tallied separately from a technical
// absence so a denominator can never quietly absorb it.
func (a *Adapter) abstain() []byte {
	resp := map[string]any{
		schema.AbstentionField: true,
		"candidates":           []any{},
		// The adjudicative schemas' required fields, present-and-empty: an abstention must be SCHEMA-VALID
		// in every mode (that is what makes it a recorded position rather than a dropped explorer), and the
		// host tells it apart from a real answer by the reserved marker, never by an empty field.
		"findings":    []any{},
		"assessments": []any{},
		"ranking":     []any{},
		"approved":    []any{},
		// The FIXED-SPACE schemas' required fields, present-and-empty for the same reason: an abstention
		// must be schema-VALID in every mode, and the host tells it apart from a real answer by the reserved
		// marker — never by a zero estimate, which is exactly the value a forecast must not silently pool.
		"evaluations":      []any{},
		"optionsEvaluated": []any{},
		"missingEvidence":  []any{},
		"estimate":         0,
		"low":              0,
		"high":             0,
		"reasoning":        "abstaining: outside this forecaster's competence",
		"claims":           []any{},
		"answer":           "",
		"rationale":        "",
		"evidence":         "abstaining: outside this explorer's competence",
		"confidence":       0,
		"sources":          []any{},
		"assumptions":      []any{},
		"uncertainties":    []any{},
		"notes":            "abstained",
	}
	b, _ := json.Marshal(resp)
	return b
}

// exploreMediated returns the LATER-round response (design §1): the fake echoes back the canonical items it was
// shown in the untrusted-data block as `candidates`, plus a refinement per item. Echoing is deliberate — it is
// exactly the contamination the ANTI-ECHO invariant must neutralize (§0 F-A): a later round repeating an item
// must not raise any independence count, because counts are computed over blind round-1 artifacts only.
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
		"candidates":    cands, // the echo (see the doc comment)
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

// untrustedBlock extracts the JSON payload the host embedded between its untrusted-data delimiters, so the fake
// consumes the carried artifact exactly the way a real model would read it.
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

// confirmEntityWire mirrors the provisional-entity shape the confirmation prompt embeds, so the fake can pick a
// real target for a typed challenge instead of inventing an ID (which the host would reject).
type confirmEntityWire struct {
	CanonicalID string `json:"canonicalId"`
	Name        string `json:"name"`
	Members     []struct {
		SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
	} `json:"members"`
}

// confirm returns the CONFIRMATION-round response (design §4). The default is an empty challenge list — the
// common, valid answer. Under the ChallengeWrongMerge scenario it raises exactly ONE typed `wrong_merge` against
// the first presented entity that groups nominations from two different explorers (the only kind of entity whose
// split can change a corroboration count), so the host rule has something real to adjudicate.
func (a *Adapter) confirm(prompt string) []byte {
	target := ""
	if idx := strings.Index(prompt, "provisional entities:\n"); a.scenario == ChallengeWrongMerge && idx >= 0 {
		var ents []confirmEntityWire
		_ = json.Unmarshal([]byte(prompt[idx+len("provisional entities:\n"):]), &ents)
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

// canonicalizeNominationWire mirrors the per-nomination shape the canonicalizer prompt embeds (index/raw/
// sourceExplorer) so the fake canonicalizer can recover the exact nomination indexing to cluster over.
type canonicalizeNominationWire struct {
	Index          int                     `json:"index"`
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
}

// canonicalize is the deterministic FAKE canonicalizer: it recovers the nominations JSON array embedded in
// the canonicalizer prompt and clusters them by EXACT raw text (exact synonyms merge; distinct names stay
// separate) — so it honors the surjectivity contract (every nomination lands in exactly one cluster,
// singletons survive) without any real model. It emits the exact proposal structure the contract parses.
func (a *Adapter) canonicalize(prompt string) []byte {
	const marker = "nominations:\n"
	var noms []canonicalizeNominationWire
	if idx := strings.Index(prompt, marker); idx >= 0 {
		_ = json.Unmarshal([]byte(prompt[idx+len(marker):]), &noms)
	}
	// Cluster by exact raw text, preserving first-seen order for a deterministic partition.
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
		// The AGGRESSIVE merger: every nomination in ONE entity. Under the dual merge-agreement rule this
		// proposal wins nothing it does not share with the other canonicalizer — its extra merges are recorded
		// as CONTESTED and resolved by SPLITTING (design §0 F-B).
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

// collatorAliases recovers the citable `envelope#k` aliases out of the Map collator prompt's labeled
// responses block (C1). The fake cites what it was actually SHOWN — never a hardcoded alias — for the same
// reason the ballot fake reads the host's projected refs: a fake that invented its citations would pass the
// host's validation for the wrong reason, and would keep passing after the prompt stopped teaching them.
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

// synthesize returns a fixed collator-output with a non-empty summary + a disagreement entry. The
// weak-response appendix is left empty — the pipeline fills it from the weak envelopes.
//
// Its CITATIONS (C1) deliberately exercise BOTH host outcomes in one default run: the first finding cites
// the real aliases it was shown (including one narrowed to a specific claim, `envelope#k/claims/0`, so the
// narrowing half of the grammar is exercised hermetically), and the second cites the legacy free-text
// "s1" — which is not an alias, so the host drops it and labels that finding uncited. Under the
// CollatorCiteAll scenario every finding cites a real alias and one lies about being uncited.
func (a *Adapter) synthesize(prompt string) []byte {
	aliases := collatorAliases(prompt)
	// Finding 1: cite everything shown, with the first ref narrowed to a claim index.
	cited := append([]string(nil), aliases...)
	if len(cited) > 0 {
		cited[0] += "/claims/0"
	}
	// Finding 2: free text, not an alias → dropped by the host → the finding is recorded uncited.
	second, claimsUncited := []string{"s1"}, false
	if a.scenario == CollatorCiteAll {
		second, claimsUncited = aliases, true
	}
	out := schema.CollatorOutput{
		SynthesisSummary: "Synthesis of the explorer panel (fake collator).",
		Findings: []schema.Finding{
			{Statement: "A well-evidenced conclusion", Evidence: "cross-explorer support", Confidence: 0.8, Sources: cited},
			// `Uncited` is HOST-owned; a collator that sets it is asserting something about its own
			// sourcing, and the host overwrites it either way.
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
