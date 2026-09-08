REAL claude-code review smoke (sanitized — session_id/uuid/costs/cache/durations removed; no secrets/paths).
A real `--output-format json` review envelope can list MULTIPLE models in `modelUsage`: a tiny
`claude-haiku-4-5-20251001` helper (14 output tokens) plus the PRIMARY `claude-opus-4-8[1m]` (170 output
tokens) that produced the answer. `parseClaudeEnvelope` selects the primary model by max output tokens.
`result` carries the reviewer JSON, which `parseClaudePayload` unwraps for the Manager. End-to-end smoke
verified: identityEvidence=envelope, verificationStatus=verified, reviewer-result.json = unwrapped JSON.
