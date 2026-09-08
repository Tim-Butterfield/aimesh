// Package schema holds the JSON payload envelopes that mirror docs/schema/ and
// practical (Go-level) validation for them: structural validation (required
// fields + enum membership) against the typed envelopes, which is what a running
// binary enforces.
//
// Nothing here reads docs/schema/ at runtime, and that is deliberate — the
// schemas are a reference declaration, not a loaded artifact. What closes the gap
// is test-time validation: meshcore/jsonschema is a small draft-2020-12
// validator, and tests use it to hold the config schema and the effective-config
// schema to what the decoder accepts and to what a real run actually writes. The
// remaining files in docs/schema/ are not yet checked that way.
package schema

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// ReviewerResult is the JSON a reviewer/cross_check/verifier call returns
// (docs/schema/reviewer-result.schema.json).
type ReviewerResult struct {
	SchemaVersion int              `json:"schemaVersion"`
	Role          string           `json:"role"`
	Phase         string           `json:"phase"`
	Summary       string           `json:"summary,omitempty"`
	Verdict       string           `json:"verdict"`
	Findings      []review.Finding `json:"findings"`
}

var (
	validKind          = newSet("pass", "fail", "ambiguity", "inconsistency", "gap", "risk")
	validSeverity      = newSet("info", "low", "medium", "high", "critical")
	validVerdict       = newSet("approve", "approve_with_suggestions", "request_changes", "block")
	validRevRole       = newSet("reviewer", "cross_check", "verifier", "author_remediator")
	validRevPhase      = newSet("semantic_iterate", "semantic_cross_check", "semantic_verify", "semantic_author_review")
	validSource        = newSet("reviewer", "evidence", "author_self_review")
	validValidity      = newSet("valid", "invalid")
	validDecisionState = newSet("applied", "invalid", "skipped", "already_addressed",
		"upstream_conflict_deferred", "withheld_class_e", "reported_valid", "reported_invalid")
)

// HostAdjudicationResult is the JSON a host-adjudication call returns: a wrapper
// object carrying one adjudication per reviewer finding
// (docs/schema/host-adjudication.schema.json; prompts.md → Host adjudication prompt).
type HostAdjudicationResult struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Role          string                    `json:"role"`
	Phase         string                    `json:"phase"`
	Adjudications []review.HostAdjudication `json:"adjudications"`
}

// ParseHostAdjudication unmarshals + validates a host-adjudication response against
// the reviewer finding ids it must cover (exactly one per id, no unknowns, no dups).
func ParseHostAdjudication(b []byte, findingIDs []string) (HostAdjudicationResult, error) {
	var r HostAdjudicationResult
	// Tolerate unknown top-level fields (see ParseReviewerResult): a benign extra key must not
	// discard an otherwise-valid adjudication. ValidateFor still enforces every real constraint
	// (schemaVersion/role/phase, exact finding-id coverage, enum membership, required reasoning).
	if err := json.Unmarshal(b, &r); err != nil {
		return HostAdjudicationResult{}, fmt.Errorf("host adjudication is not valid JSON for the schema: %w", err)
	}
	var probe struct {
		Adjudications *json.RawMessage `json:"adjudications"`
	}
	_ = json.Unmarshal(b, &probe)
	if probe.Adjudications == nil {
		return HostAdjudicationResult{}, fmt.Errorf("host adjudication missing required \"adjudications\" array")
	}
	if t := bytes.TrimSpace(*probe.Adjudications); len(t) == 0 || t[0] != '[' {
		return HostAdjudicationResult{}, fmt.Errorf("host adjudication \"adjudications\" must be an array")
	}
	if err := r.ValidateFor(findingIDs); err != nil {
		return HostAdjudicationResult{}, err
	}
	return r, nil
}

// ValidateFor checks the host-adjudication envelope + that it adjudicates exactly
// the given finding ids (one each, no unknown, no duplicates) with valid enums.
func (r HostAdjudicationResult) ValidateFor(findingIDs []string) error {
	if r.SchemaVersion != 1 {
		return fmt.Errorf("schemaVersion must be 1, got %d", r.SchemaVersion)
	}
	if r.Role != "author_remediator" {
		return fmt.Errorf("invalid host role %q (want author_remediator)", r.Role)
	}
	if r.Phase != "semantic_adjudicate" {
		return fmt.Errorf("invalid host phase %q (want semantic_adjudicate)", r.Phase)
	}
	want := newSet(findingIDs...)
	seen := map[string]bool{}
	for i, a := range r.Adjudications {
		if a.FindingID == "" {
			return fmt.Errorf("adjudication[%d]: missing findingId", i)
		}
		if !want.has(a.FindingID) {
			return fmt.Errorf("adjudication for unknown finding id %q", a.FindingID)
		}
		if seen[a.FindingID] {
			return fmt.Errorf("duplicate adjudication for finding id %q", a.FindingID)
		}
		seen[a.FindingID] = true
		if !validValidity.has(a.Validity) {
			return fmt.Errorf("finding %s: invalid validity %q", a.FindingID, a.Validity)
		}
		if !validDecisionState.has(string(a.DecisionState)) {
			return fmt.Errorf("finding %s: invalid/empty decisionState %q", a.FindingID, a.DecisionState)
		}
		if a.SeverityAdjusted != "" && !validSeverity.has(string(a.SeverityAdjusted)) {
			return fmt.Errorf("finding %s: invalid severityAdjusted %q", a.FindingID, a.SeverityAdjusted)
		}
		if strings.TrimSpace(a.Reasoning) == "" {
			return fmt.Errorf("finding %s: missing reasoning", a.FindingID)
		}
	}
	for _, id := range findingIDs {
		if !seen[id] {
			return fmt.Errorf("missing adjudication for finding id %q", id)
		}
	}
	return nil
}

// ParseReviewerResult unmarshals and validates a reviewer response. Unknown
// top-level fields are TOLERATED (ignored), not fatal: real models occasionally
// emit a benign extra key (e.g. a top-level "source":"reviewer" echo), and
// rejecting the whole result over one stray field discarded a valid, substantive
// review — then the single corrective retry could replace it with an empty
// approve (net finding loss). Every real constraint still holds — required
// fields, enum membership (Validate), and the required "findings" array probe
// below — so tolerating extras loses no integrity while matching the host's
// "judge the content, don't choke on noise" posture. Raw stdout is preserved in
// the audit regardless.
func ParseReviewerResult(b []byte) (ReviewerResult, error) {
	var r ReviewerResult
	if err := json.Unmarshal(b, &r); err != nil {
		return ReviewerResult{}, fmt.Errorf("reviewer result is not valid JSON for the schema: %w", err)
	}
	// `findings` is required and must be an array (an omitted/null value decodes to a
	// nil slice, which would otherwise pass as "no findings" and bypass the retry).
	var probe struct {
		Findings *json.RawMessage `json:"findings"`
	}
	_ = json.Unmarshal(b, &probe)
	if probe.Findings == nil {
		return ReviewerResult{}, fmt.Errorf("reviewer result missing required \"findings\" array")
	}
	if t := bytes.TrimSpace(*probe.Findings); len(t) == 0 || t[0] != '[' {
		return ReviewerResult{}, fmt.Errorf("reviewer result \"findings\" must be an array")
	}
	if err := r.Validate(); err != nil {
		return ReviewerResult{}, err
	}
	return r, nil
}

// NormalizeReviewerOutput conservatively recovers a single JSON object from a real
// model's stdout before parsing. It strips a single surrounding Markdown code fence
// and extracts the first top-level JSON object when there is surrounding text — but
// only when unambiguous. It NEVER repairs JSON or guesses fields. It rejects: empty
// output, arrays, prose without an object, partial/unbalanced JSON, and multiple
// top-level objects. `changed` reports whether anything was stripped/extracted (the
// caller keeps the original stdout in the audit regardless).
func NormalizeReviewerOutput(raw []byte) (normalized []byte, changed bool, err error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil, false, fmt.Errorf("reviewer output is empty")
	}
	if body, ok := stripFence(s); ok {
		s = strings.TrimSpace(body)
		changed = true
	}
	if strings.HasPrefix(s, "[") {
		return nil, changed, fmt.Errorf("reviewer output is a JSON array; expected one object")
	}
	i := strings.IndexByte(s, '{')
	if i < 0 {
		return nil, changed, fmt.Errorf("reviewer output contains no JSON object")
	}
	end, e := objectEnd(s[i:])
	if e != nil {
		return nil, changed, e
	}
	obj := s[i : i+end]
	if i > 0 {
		changed = true // leading text trimmed
	}
	if after := strings.TrimSpace(s[i+end:]); after != "" {
		if strings.IndexByte(after, '{') >= 0 {
			return nil, changed, fmt.Errorf("reviewer output has multiple JSON objects; expected exactly one")
		}
		changed = true // trailing text trimmed
	}
	return []byte(obj), changed, nil
}

// objectEnd returns the index just past the '}' that closes the object beginning at
// s[0] (which must be '{'), respecting string literals and escapes. Error if unbalanced.
func objectEnd(s string) (int, error) {
	depth, inStr, esc := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("reviewer output has unbalanced/partial JSON (no matching '}')")
}

// stripFence removes a single surrounding Markdown code fence (```...``` or ```json)
// when the whole string is fenced; returns (inner, true) if it stripped one.
func stripFence(s string) (string, bool) {
	if !strings.HasPrefix(s, "```") {
		return s, false
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s, false
	}
	body := s[nl+1:]
	if idx := strings.LastIndex(body, "```"); idx >= 0 {
		return body[:idx], true
	}
	return s, false
}

// Validate checks required fields and enum membership.
func (r ReviewerResult) Validate() error {
	if r.SchemaVersion != 1 {
		return fmt.Errorf("schemaVersion must be 1, got %d", r.SchemaVersion)
	}
	if !validRevRole.has(r.Role) {
		return fmt.Errorf("invalid role %q", r.Role)
	}
	if !validRevPhase.has(r.Phase) {
		return fmt.Errorf("invalid or missing phase %q", r.Phase)
	}
	if !validVerdict.has(r.Verdict) {
		return fmt.Errorf("invalid verdict %q", r.Verdict)
	}
	for i, f := range r.Findings {
		if f.ID == "" {
			return fmt.Errorf("finding[%d]: missing id", i)
		}
		if !validKind.has(string(f.Kind)) {
			return fmt.Errorf("finding %s: invalid kind %q", f.ID, f.Kind)
		}
		if !validSeverity.has(string(f.Severity)) {
			return fmt.Errorf("finding %s: invalid severity %q", f.ID, f.Severity)
		}
		if strings.TrimSpace(f.Title) == "" {
			return fmt.Errorf("finding %s: missing title", f.ID)
		}
		if f.Source != "" && !validSource.has(f.Source) {
			return fmt.Errorf("finding %s: invalid source %q", f.ID, f.Source)
		}
	}
	return nil
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// NormalizeLocation buckets a location so small line drift doesn't change identity
// (docs/design.md → Finding identity & rubrics).
//
// `location` is MODEL-AUTHORED FREE TEXT, so the normalization has to absorb the ways a model
// restates the same place. Measured 2026-08-11 on a real two-vendor run, one seat produced BOTH
// "loadPriorDispositions (lines 163-225)" and "loadPriorDispositions, lines 163-225" for the same
// defect: after the non-alphanumeric collapse those differ only by a TRAILING separator, left
// behind by the closing parenthesis. Trimming separators from both ends is what makes those one
// identity rather than two.
//
// WHAT IT STILL DOES NOT DO, stated so nobody reads more into it: it does not reconcile different
// line ranges for the same place. "(lines 177-181, 210-216)" and "(lines 177-217)" remain distinct
// identities. Merging those is range arithmetic — host-computed entity resolution — and it is a
// deliberate, separate decision rather than something smuggled into a normalizer.
func NormalizeLocation(loc string) string {
	s := strings.ToLower(strings.TrimSpace(loc))
	if s == "" {
		return ""
	}
	// a leading "a-b" or "a" line range → bucket the start line to nearest 10
	m := regexp.MustCompile(`^(\d+)`).FindStringSubmatch(s)
	if m != nil {
		var n int
		fmt.Sscanf(m[1], "%d", &n)
		return fmt.Sprintf("L%d", (n/10)*10)
	}
	return strings.Trim(nonAlnum.ReplaceAllString(s, "-"), "-")
}

// Fingerprint is the deterministic identity of a finding for DEDUP: file|normalizedLocation, with
// both title and KIND excluded.
//
// KIND USED TO BE PART OF THIS, on the reasoning that merging two findings of different kind at one
// location would drop a genuinely distinct issue. Measured evidence overturned it. On a real
// two-vendor run (2026-08-11, run dir 20260811T130527-1690) a single seat reported the same defect
// at a BYTE-IDENTICAL file and location twice, once as `inconsistency` and once as `fail` — so a
// model relabelling its own finding defeated its own dedup. Across 20 findings the agreement count
// was 1 for every single one, on a run where two independent vendors demonstrably agreed.
//
// `kind` is model-authored, and this codebase already refuses model-authored values as identity:
// the whole reason a fingerprint exists instead of `Finding.ID` is that the id is the model's to
// choose. Kind is the model's to choose too. The conservatism it was meant to buy was illusory,
// because the same relabelling that would "protect" a distinct issue also splits an identical one.
//
// This makes Fingerprint identical to what StabilityKey already computed — which is itself
// corroboration, since that key excluded kind years earlier for exactly this observed drift.
//
// SHA-1 is deliberate and is not a security choice: the input is a file path and a location, both
// model-authored and both already visible in the finding, so there is nothing to keep secret and no
// collision an attacker could profit from — the property the fingerprint provides is that the HOST
// computed it, not that it is hard to forge. The `sha1:` prefix is part of the public `--select`
// vocabulary, so the algorithm is pinned by compatibility as well.
func Fingerprint(f review.Finding) string {
	identity := f.File + "|" + NormalizeLocation(f.Location)
	sum := sha1.Sum([]byte(identity))
	return fmt.Sprintf("sha1:%x", sum)
}

// StabilityKey is the finding identity for the inner-loop SET-STABILITY check (methodology §
// Set-stability via content tuple). It excluded kind because "a reviewer often reports the SAME
// underlying issue with a drifting kind across iterations (ambiguity↔inconsistency)".
//
// That observation turned out to describe DEDUP just as well as stability, so Fingerprint now
// excludes kind too and the two identities have converged. It stays a named function because the
// call site means something different by it — convergence of a seat's own set, not identity across
// seats — and one delegating to the other is how they are kept from drifting apart again.
func StabilityKey(f review.Finding) string { return Fingerprint(f) }

type set map[string]struct{}

func newSet(vs ...string) set {
	s := make(set, len(vs))
	for _, v := range vs {
		s[v] = struct{}{}
	}
	return s
}

func (s set) has(v string) bool { _, ok := s[v]; return ok }
