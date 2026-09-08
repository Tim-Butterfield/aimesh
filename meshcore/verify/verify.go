// Package verify is the ModelVerifier utility: it classifies a call's model-identity
// evidence into a verification status. It is a pure function of (evidence tier,
// requested model, actual model) — no I/O — callable from any layer.
//
// WHAT A CLASSIFICATION IS FOR. It is recorded and shown to a reader. It is never consulted to decide
// whether a response is used, ranked, counted, or refused — the content of an answer determines its
// worth, not a label about who produced it. Nothing here should grow a caller that drops output on an
// identity verdict; see ../../docs/model-identity.md for why (in short: an adapter can be asked to use
// a model but cannot be made to prove it did, and the one adapter this repo declared at a strong tier
// turned out to be echoing the argument we passed it).
//
// The governing rule for the label itself: a weak self-report signal is recorded as `self_reported`,
// never silently as `verified`. A self-report that does not clearly match, or no/weak evidence with no
// confirmable model, is `unknown`. A proven STRONG-evidence divergence is `mismatch`, which callers
// surface as a prominent caveat. Classification returns `unknown` for an empty actual regardless of tier.
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

// MatchesForAdapter reports whether an actual model string names the SAME model as the requested one,
// using the adapter's alias rules. It is the single matching rule ClassifyIdentity applies, exported so
// an adapter's identity PARSER can ask the same question while choosing which reported model to return
// (e.g. picking the requested model out of an envelope that also lists an auxiliary one) — a parser that
// reimplemented matching could disagree with the verifier and manufacture a mismatch.
func MatchesForAdapter(requested, actual, adapter string) bool {
	return matchesForAdapter(requested, actual, adapter)
}

// matchesForAdapter is exact-match, plus adapter/provider-aware alias equivalence. Today
// only `claude-code` adds equivalence (Anthropic family/version), so a requested alias
// (`opus`) or short id (`claude-opus-4-8`) matches the envelope's `claude-opus-4-8[1m]`.
// It is NOT a global "contains" loosening: non-claude adapters stay on exact matching.
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

// canonicalClaude extracts (family, version) from an Anthropic model string by TOKENS
// (not substring), so it does not over-accept: the family must be a standalone token
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
			// `claude-opus-4-8`). It must NOT be preceded by a different brand (`gpt-opus`)
			// or be a substring fragment (`notopus`, excluded by token matching above).
			if i == 0 || toks[0] == "claude" {
				return t, strings.Join(toks[i+1:], "-"), true
			}
			return "", "", false
		}
	}
	return "", "", false
}

// evidenceRank orders identity evidence from weakest (0) to strongest, for capping. STRONG tiers
// (rank ≥ strongEvidenceRank) are the only ones that can produce `verified` — or, on a non-match, a
// proven `mismatch` halt.
var evidenceRank = map[core.IdentityEvidence]int{
	"": 0, core.EvidenceNone: 0,
	core.EvidenceSelfReport:    1,
	core.EvidenceInvocationTag: 2, core.EvidenceCLIStatus: 3,
	core.EvidenceTrace: 4, core.EvidenceEnvelope: 5,
}

const strongEvidenceRank = 2 // invocation_tag and above are STRONG (authoritative enough to verify/mismatch)

// isStrong reports whether an evidence tier is authoritative enough to verify identity (and therefore
// to prove a mismatch on a non-match).
func isStrong(ev core.IdentityEvidence) bool { return evidenceRank[ev] >= strongEvidenceRank }

// CapEvidence clamps a produced evidence tier to a declared CEILING: an adapter can never be
// classified on stronger evidence than its registered recipe is allowed to produce. This is the
// evidence-authority boundary — a self_report/none adapter cannot inflate its Result.Evidence to a
// strong tier and fake `verified`; only a code-owned recipe (parser + declared ceiling) change can.
func CapEvidence(ceiling, produced core.IdentityEvidence) core.IdentityEvidence {
	if evidenceRank[produced] > evidenceRank[ceiling] {
		return ceiling
	}
	return produced
}

// ClassifyIdentity maps (evidence, requested, actual, adapter) → (verificationStatus, ok).
// ok=false means identity was NOT verified. It is a LABEL, not a verdict on the response: no caller
// may drop, downgrade, or refuse output because of what this returns (see ../../docs/model-identity.md).
// `adapter` enables adapter-specific alias equivalence (only `claude-code` today); all others use exact
// normalized matching.
//
//   - no actual reported                              → (unknown, false)
//   - STRONG evidence, match                          → (verified, true)
//   - STRONG evidence, non-match                      → (mismatch, false)   // a PROVEN wrong model
//   - self_report evidence, match                     → (self_reported, true)  // weaker, accepted
//   - self_report evidence, non-match                 → (unknown, false)    // WEAK + uncertain
//   - none/unrecognized evidence                      → (unknown, false)    // no implicit trust, never verified
//
// A MISMATCH is reserved for STRONG evidence: a weak self-report that names a different model is
// inherently uncertain (display-name vs slug, model self-honesty), so it is recorded as `unknown` (the
// reported value is preserved by the caller as a caveat signal) rather than as a proven divergence.
func ClassifyIdentity(ev core.IdentityEvidence, requested, actual, adapter string) (status string, ok bool) {
	if strings.TrimSpace(actual) == "" {
		return core.VerifUnknown, false // no model reported → identity UNKNOWN (not a mismatch)
	}
	matches := matchesForAdapter(requested, actual, adapter)
	switch {
	case isStrong(ev):
		if matches {
			return core.VerifVerified, true
		}
		return core.VerifMismatch, false // STRONG evidence + different model → proven mismatch → halt
	case ev == core.EvidenceSelfReport:
		if matches {
			return core.VerifSelfReported, true
		}
		return core.VerifUnknown, false // WEAK self-report + non-match → ambiguous → caveat, not halt
	default: // none/unrecognized evidence → UNKNOWN (never silently verified, never a mismatch halt)
		return core.VerifUnknown, false
	}
}

// SelfReport is a structured WEAK self-report captured from an adapter's response to an identity
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

// firstJSONObject conservatively extracts THE single balanced top-level JSON object from output that
// may be wrapped in ``` fences or light prose. It does NOT repair JSON and, like
// schema.NormalizeReviewerOutput, REJECTS multiple top-level objects (a second object after the first
// → nil) so a self-report response is held to the same single-object rule as a bare result. Returns nil
// if no single balanced object is found. Kept dependency-free for the identity path (design review G2).
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
				// Reject a SECOND top-level object after the first (trailing content containing a `{`),
				// matching schema.NormalizeReviewerOutput's multiple-object rule; plain trailing prose
				// without another object is tolerated (as it is for a bare result).
				if strings.IndexByte(s[i+1:], '{') >= 0 {
					return nil
				}
				return []byte(s[start : i+1])
			}
		}
	}
	return nil
}

// ParseSelfReportEnvelope extracts a WEAK self-report from a response in EITHER shape: the review-path
// WRAPPER `{"reviewmeshIdentity":{model,effort,source}, "result":{…}}` OR the bare probe object
// `{model,effort,…}`. Fence/prose tolerant (firstJSONObject). ok=false when no usable model is present.
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

// ExtractWrappedResult returns the inner `result` object bytes from a self-report WRAPPER, or nil when
// the response is not a wrapper (a bare result) — so the caller falls back to raw stdout for schema
// parsing. Requires BOTH reviewmeshIdentity and result to be present. Fence/prose tolerant.
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

// agyCanon canonicalizes an Agy model string for WEAK self-report comparison — lower-cases, drops a
// TRAILING "thinking" effort-suffix word (the synonym "High Thinking" ≡ "High" — only in the effort/
// suffix position, never a "Thinking" token inside the model name), and strips all spacing/punctuation
// (incl. parentheses). It canonicalizes ONLY surface form; it NEVER drops the version, provider family,
// or the effort LEVEL, so a real different model or effort level stays a non-match (design review G4).
func agyCanon(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, " )")        // drop a trailing parenthetical/space so the suffix is exposed
	s = strings.TrimSuffix(s, "thinking") // "…high thinking" → "…high" (effort-suffix synonym ONLY)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ParseSelfReport extracts a model identifier from a self-report response (the reply to
// a prompt like "Output only your model identifier/name"). It takes the first non-empty
// line and strips surrounding quotes/backticks and a leading label — a deterministic,
// best-effort parse of an inherently weak signal. Returns "" if nothing usable.
func ParseSelfReport(stdout []byte) string {
	for raw := range strings.SplitSeq(string(stdout), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// strip a leading label like "model:" / "I am " (best-effort)
		for _, pfx := range []string{"model:", "model =", "model is", "i am", "i'm", "my model identifier is", "my model is", "identifier:"} {
			if low := strings.ToLower(line); strings.HasPrefix(low, pfx) {
				line = strings.TrimSpace(line[len(pfx):])
				break
			}
		}
		line = strings.Trim(line, "`\"' .")
		if line != "" {
			return line
		}
	}
	return ""
}
