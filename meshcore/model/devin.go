package model

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// knownDevinBrands are the leading brand tokens accepted at the start of a Devin display model name,
// so an ambiguous partial name such as "Opus 4.8" is rejected rather than prefixed with "Claude".
var knownDevinBrands = map[string]bool{
	"claude": true, "gpt": true, "gemini": true, "deepseek": true,
	"glm": true, "kimi": true, "swe": true, "adaptive": true,
}

// RenderDevinModelArg converts a Devin display model name plus a separate effort level into the
// `--model` slug the Devin CLI expects: lowercase, spaces and periods replaced with hyphens, repeated
// hyphens collapsed, with the normalized effort appended. The display name must begin with a known brand
// token (Claude, GPT, Gemini, ...); a partial name such as "Opus 4.8" is rejected.
//
//	"Claude Opus 4.8" + "medium"          -> "claude-opus-4-8-medium"
//	"GPT-5.4"         + "Medium Thinking" -> "gpt-5-4-medium"
//	"Gemini 3.1 Pro"  + "High Thinking"   -> "gemini-3-1-pro-high"
//	"Opus 4.8"        + "medium"          -> error (ambiguous)
func RenderDevinModelArg(displayModel, effort string) (string, error) {
	name := strings.TrimSpace(displayModel)
	if name == "" {
		return "", fault.New(fault.Config, "devin model is required (the full display model name from Devin IDE/Cascade, e.g. \"Claude Opus 4.8\")")
	}
	// Leading brand token (split on spaces/hyphens) must be recognized. A separator-only
	// value (e.g. "-" / "---") yields no fields → ambiguous, not a panic.
	fields := strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == '-' })
	if len(fields) == 0 || !knownDevinBrands[strings.ToLower(fields[0])] {
		return "", fault.New(fault.Config, fmt.Sprintf("ambiguous Devin model %q; use the full display model name from Devin IDE/Cascade, such as \"Claude %s\"", displayModel, name))
	}

	slug := slugifyDevin(name)
	eff, err := normalizeDevinEffort(effort)
	if err != nil {
		return "", err
	}
	if eff != "" {
		slug += "-" + eff
	}
	return slug, nil
}

// slugifyDevin lowercases and replaces spaces + periods with hyphens, collapsing and
// trimming hyphens. No spaces or periods can survive.
func slugifyDevin(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer(" ", "-", ".", "-").Replace(s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

// normalizeDevinEffort lowercases the effort, strips a trailing "thinking" word, and
// accepts only low/medium/high (or empty). Anything else is an error.
func normalizeDevinEffort(effort string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(effort))
	e = strings.TrimSpace(strings.ReplaceAll(e, "thinking", ""))
	switch e {
	case "", "low", "medium", "high":
		return e, nil
	default:
		return "", fault.New(fault.Config, fmt.Sprintf("unsupported Devin effort %q; use low, medium, or high", effort))
	}
}
