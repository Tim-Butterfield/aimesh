// Package schema holds the JSON payload envelopes described in docs/schema/ and validates them
// structurally: required fields and enum membership.
//
// docs/schema/ is not read at runtime. Tests use meshcore/jsonschema to check the config and
// effective-config schemas against what the decoder accepts and what a run writes.
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

// ReviewerResult is the JSON a reviewer, cross_check or verifier call returns
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

// HostAdjudicationResult is the JSON a host-adjudication call returns: one adjudication per
// reviewer finding (docs/schema/host-adjudication.schema.json).
type HostAdjudicationResult struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Role          string                    `json:"role"`
	Phase         string                    `json:"phase"`
	Adjudications []review.HostAdjudication `json:"adjudications"`
}

// ParseHostAdjudication unmarshals and validates a host-adjudication response against the finding
// ids it must cover: exactly one adjudication per id, with no unknown or duplicate ids. Unknown
// top-level fields are ignored, as in ParseReviewerResult.
func ParseHostAdjudication(b []byte, findingIDs []string) (HostAdjudicationResult, error) {
	var r HostAdjudicationResult
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

// ValidateFor checks the envelope and that it adjudicates exactly the given finding ids, once each,
// with valid enum values.
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

// ParseReviewerResult unmarshals and validates a reviewer response. Unknown top-level fields are
// ignored, since models sometimes add a stray key and rejecting the result would discard a valid
// review; required fields and enum values are still enforced.
func ParseReviewerResult(b []byte) (ReviewerResult, error) {
	var r ReviewerResult
	if err := json.Unmarshal(b, &r); err != nil {
		return ReviewerResult{}, fmt.Errorf("reviewer result is not valid JSON for the schema: %w", err)
	}
	// An omitted or null `findings` would decode as "no findings", so require an array explicitly.
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

// NormalizeReviewerOutput recovers a single JSON object from model stdout: it strips one surrounding
// Markdown code fence and surrounding text when the object is unambiguous. It never repairs JSON.
// Empty output, arrays, prose without an object, unbalanced JSON and multiple objects are errors.
// changed reports whether anything was stripped.
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

// objectEnd returns the index just past the '}' closing the object that begins at s[0], respecting
// string literals and escapes. It returns an error when the object is unbalanced.
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

// stripFence removes one surrounding Markdown code fence and reports whether it did.
func stripFence(s string) (string, bool) {
	if !strings.HasPrefix(s, "```") {
		return s, false
	}
	_, after, ok := strings.Cut(s, "\n")
	if !ok {
		return s, false
	}
	body := after
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

// NormalizeLocation normalizes a model-authored location so small restatements do not change a
// finding's identity. A leading line number is bucketed to the nearest 10 below; otherwise
// non-alphanumeric runs collapse to "-" and are trimmed from both ends, so "f (lines 1-2)" and
// "f, lines 1-2" match. Different line ranges for the same place are not reconciled.
func NormalizeLocation(loc string) string {
	s := strings.ToLower(strings.TrimSpace(loc))
	if s == "" {
		return ""
	}
	m := regexp.MustCompile(`^(\d+)`).FindStringSubmatch(s)
	if m != nil {
		var n int
		fmt.Sscanf(m[1], "%d", &n)
		return fmt.Sprintf("L%d", (n/10)*10)
	}
	return strings.Trim(nonAlnum.ReplaceAllString(s, "-"), "-")
}

// Fingerprint returns a finding's host-computed identity for deduplication and selection: a hash of
// its file and normalized location.
//
// Title and kind are excluded because models relabel the same defect with a different kind, which
// would split one finding into two. SHA-1 is not a security choice: the inputs are visible in the
// finding, and the `sha1:` prefix is part of the public `select` vocabulary.
func Fingerprint(f review.Finding) string {
	identity := f.File + "|" + NormalizeLocation(f.Location)
	sum := sha1.Sum([]byte(identity))
	return fmt.Sprintf("sha1:%x", sum)
}

// StabilityKey is the finding identity for a seat's set-stability check across rounds. It equals
// Fingerprint and is named separately because the call site checks convergence rather than identity
// across seats.
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
