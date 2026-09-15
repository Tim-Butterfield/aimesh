// Package verify classifies a call's model-identity evidence into a verification status. It is a
// pure function of the evidence tier, the requested model and the actual model.
//
// A classification is recorded and shown; it never decides whether a response is used, ranked or
// refused, because an adapter can be asked to use a model but cannot prove it did (see
// docs/model-identity.md). A weak self-report is recorded as `self_reported`, never `verified`; an
// unmatched self-report or missing evidence is `unknown`; a divergence proven by strong evidence is
// `mismatch`. An empty actual model is always `unknown`.
package verify

import (
	"encoding/json"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

// Normalize lower-cases and trims a model string for comparison.
func Normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Matches reports whether a non-empty actual model equals the requested one (exact,
// normalized). Provider-specific alias equivalence is handled separately, per adapter.
func Matches(requested, actual string) bool {
	if strings.TrimSpace(actual) == "" {
		return false
	}
	return Normalize(requested) == Normalize(actual)
}

// MatchesForAdapter reports whether actual names the same model as requested under the adapter's
// alias rules. It is the rule ClassifyIdentity applies, exported so identity parsers use the same
// rule when choosing among reported models.
func MatchesForAdapter(requested, actual, adapter string) bool {
	return matchesForAdapter(requested, actual, adapter)
}

// matchesForAdapter is exact matching plus adapter-specific equivalence: `claude-code` matches within
// an Anthropic family and version (`opus` matches `claude-opus-4-8[1m]`), and `agy-cli` compares
// canonical display names. Other adapters use exact matching.
func matchesForAdapter(requested, actual, adapter string) bool {
	if Matches(requested, actual) {
		return true
	}
	if adapter == "claude-code" {
		return claudeFamilyMatch(requested, actual)
	}
	if adapter == "agy-cli" {
		// agy is requested as a display name (e.g. "Gemini 3.1 Pro (High)") and self-reports the model
		// + effort separately; compare on canonical surface form only (case/spacing/punct/effort-suffix).
		return agyCanon(requested) != "" && agyCanon(requested) == agyCanon(actual)
	}
	return false
}

// claudeFamilyMatch reports whether two Anthropic model strings name the same model:
// same family (opus/sonnet/haiku) and — when BOTH carry a version — the same version. A
// bare alias (`opus`, no version) is a wildcard over that family; a different family
// (`sonnet` vs `opus`) never matches. Conservative: it does not match across versions.
func claudeFamilyMatch(requested, actual string) bool {
	rf, rv, rok := canonicalClaude(requested)
	af, av, aok := canonicalClaude(actual)
	if !rok || !aok || rf != af {
		return false
	}
	if rv != "" && av != "" && rv != av {
		return false
	}
	return true
}

// canonicalClaude extracts (family, version) from an Anthropic model string by tokens, not
// substrings, so it does not over-accept: the family must be a standalone token
// (`opus`/`sonnet`/`haiku`) preceded only by an optional `claude` brand token. It drops a
// trailing `[...]`/`(...)` context-window suffix and normalizes `.`/spaces to `-`.
// ok=false for non-Anthropic strings ("notopus", "gpt-opus", "gpt-5.5", …).
func canonicalClaude(s string) (family, version string, ok bool) {
	x := strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(x, "[("); i >= 0 {
		x = x[:i]
	}
	x = strings.ReplaceAll(x, ".", "-")
	x = strings.ReplaceAll(x, " ", "-")
	var toks []string
	for t := range strings.SplitSeq(x, "-") {
		if t != "" {
			toks = append(toks, t)
		}
	}
	for i, t := range toks {
		if t == "opus" || t == "sonnet" || t == "haiku" {
			// The family may be the first token (bare alias `opus`), or preceded by the
			// `claude` brand plus optional generation/version tokens (`claude-3-5-sonnet-…`,
			// `claude-opus-4-8`). It must not follow a different brand (`gpt-opus`).
			if i == 0 || toks[0] == "claude" {
				return t, strings.Join(toks[i+1:], "-"), true
			}
			return "", "", false
		}
	}
	return "", "", false
}

// evidenceRank orders identity evidence from weakest (0) to strongest. Only strong tiers (rank at
// least strongEvidenceRank) can produce `verified` or `mismatch`.
var evidenceRank = map[core.IdentityEvidence]int{
	"": 0, core.EvidenceNone: 0,
	core.EvidenceSelfReport:    1,
	core.EvidenceInvocationTag: 2, core.EvidenceCLIStatus: 3,
	core.EvidenceTrace: 4, core.EvidenceEnvelope: 5,
}

const strongEvidenceRank = 2 // invocation_tag and above are strong evidence

// isStrong reports whether an evidence tier is authoritative enough to verify identity (and therefore
// to prove a mismatch on a non-match).
func isStrong(ev core.IdentityEvidence) bool { return evidenceRank[ev] >= strongEvidenceRank }

// CapEvidence clamps a produced evidence tier to a declared ceiling, so an adapter is never
// classified on stronger evidence than its registered recipe can produce.
func CapEvidence(ceiling, produced core.IdentityEvidence) core.IdentityEvidence {
	if evidenceRank[produced] > evidenceRank[ceiling] {
		return ceiling
	}
	return produced
}

// ClassifyIdentity maps evidence, requested model, actual model and adapter to a verification
// status; ok is false when identity was not verified. The status is a label and never a reason to
// drop or refuse output (see docs/model-identity.md). adapter selects alias rules; see
// matchesForAdapter.
//
//   - no actual reported              → (unknown, false)
//   - strong evidence, match          → (verified, true)
//   - strong evidence, non-match      → (mismatch, false)
//   - self_report evidence, match     → (self_reported, true)
//   - self_report evidence, non-match → (unknown, false)
//   - none or unrecognized evidence   → (unknown, false)
//
// A mismatch requires strong evidence, because a self-report naming a different model is inherently
// uncertain.
func ClassifyIdentity(ev core.IdentityEvidence, requested, actual, adapter string) (status string, ok bool) {
	if strings.TrimSpace(actual) == "" {
		return core.VerifUnknown, false // no model reported: unknown, not a mismatch
	}
	matches := matchesForAdapter(requested, actual, adapter)
	switch {
	case isStrong(ev):
		if matches {
			return core.VerifVerified, true
		}
		return core.VerifMismatch, false // strong evidence of a different model
	case ev == core.EvidenceSelfReport:
		if matches {
			return core.VerifSelfReported, true
		}
		return core.VerifUnknown, false // a non-matching self-report is ambiguous
	default: // none or unrecognized evidence is never verified and never a mismatch
		return core.VerifUnknown, false
	}
}

// SelfReport is a structured weak self-report captured from an adapter's response to an identity
// probe (e.g. Devin's `-p '…output JSON {result, model, effort}…'`). Every field is model-generated
// and never authoritative; Raw preserves the original object text for auditing.
type SelfReport struct {
	Model  string
	Effort string
	Raw    string
}

// ParseSelfReportJSON extracts a structured self-report from the first JSON object in the output that
// carries a non-empty `model` field (and optionally `effort`) — the shape an identity probe asks for.
// Probe responses may wrap the object in prose, so it scans from the first "{" to the last "}".
// Returns ok=false when no such object with a model is present. Deterministic; a weak signal only.
func ParseSelfReportJSON(stdout []byte) (SelfReport, bool) {
	s := string(stdout)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return SelfReport{}, false
	}
	var obj struct {
		Model  string `json:"model"`
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &obj); err != nil {
		return SelfReport{}, false
	}
	if strings.TrimSpace(obj.Model) == "" {
		return SelfReport{}, false
	}
	return SelfReport{Model: strings.TrimSpace(obj.Model), Effort: strings.TrimSpace(obj.Effort), Raw: s[start : end+1]}, true
}

// firstJSONObject extracts the single balanced top-level JSON object from output that may be wrapped
// in ``` fences or prose. It does not repair JSON, and returns nil when there is no object or when a
// second top-level object follows the first.
func firstJSONObject(raw []byte) []byte {
	s := string(raw)
	if i := strings.Index(s, "```"); i >= 0 { // strip a leading ```lang fence + a trailing ``` fence
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		s = rest
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
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
				// A second top-level object is rejected; trailing prose is tolerated.
				if strings.IndexByte(s[i+1:], '{') >= 0 {
					return nil
				}
				return []byte(s[start : i+1])
			}
		}
	}
	return nil
}

// ParseSelfReportEnvelope extracts a weak self-report from either an identity wrapper
// `{"reviewmeshIdentity":{model,effort,source}, "result":{…}}` or a bare `{model,effort}` object,
// tolerating fences and prose. ok is false when no model is present.
func ParseSelfReportEnvelope(stdout []byte) (SelfReport, bool) {
	obj := firstJSONObject(stdout)
	if obj == nil {
		return SelfReport{}, false
	}
	var wrap struct {
		Identity struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
			Source string `json:"source"`
		} `json:"reviewmeshIdentity"`
	}
	if json.Unmarshal(obj, &wrap) == nil && strings.TrimSpace(wrap.Identity.Model) != "" {
		return SelfReport{Model: strings.TrimSpace(wrap.Identity.Model), Effort: strings.TrimSpace(wrap.Identity.Effort), Raw: string(obj)}, true
	}
	return ParseSelfReportJSON(obj)
}

// ExtractWrappedResult returns the inner `result` object from a self-report wrapper, or nil for any
// other response, so the caller falls back to raw stdout. The wrapper must carry both the identity
// and the result.
func ExtractWrappedResult(stdout []byte) []byte {
	obj := firstJSONObject(stdout)
	if obj == nil {
		return nil
	}
	var wrap struct {
		Identity json.RawMessage `json:"reviewmeshIdentity"`
		Result   json.RawMessage `json:"result"`
	}
	if json.Unmarshal(obj, &wrap) != nil || len(wrap.Identity) == 0 || len(wrap.Result) == 0 {
		return nil
	}
	return wrap.Result
}

// AgyModelString joins a self-reported model + effort into a single comparable string (e.g. model
// "Gemini 3.1 Pro" + effort "High" → "Gemini 3.1 Pro High"), which agyCanon then compares against the
// requested display-name arg (e.g. "Gemini 3.1 Pro (High)").
func AgyModelString(sr SelfReport) string { return strings.TrimSpace(sr.Model + " " + sr.Effort) }

// agyCanon canonicalizes an Agy model string for self-report comparison: it lower-cases, drops a
// trailing "thinking" effort suffix ("High Thinking" equals "High"), and strips spacing and
// punctuation. The version, family and effort level are kept, so a different model or level still
// does not match.
func agyCanon(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, " )")        // drop a trailing parenthetical/space so the suffix is exposed
	s = strings.TrimSuffix(s, "thinking") // "…high thinking" → "…high"
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
