# Real agy-cli self-report capture (sanitized)

**Captured:** 2026-07-07, this batch (Tim-authorized bounded real call). **agy version:** 1.0.16.

**Command:** `agy --model "Gemini 3.1 Pro (High)" -p '<JSON {result,model,effort,source} self-report prompt asking for the ACTUAL model/effort, never the requested arg>'`

**Returned (stdout, verbatim — already free of paths/tokens/IDs/usernames):** see `stdout.txt`.

**Findings:**
- agy-cli **CAN** emit a parseable, structured JSON self-report — the exact `{result, model, effort, source}` shape.
- Reported `model: "Gemini 3.1 Pro"` + `effort: "High"` **matches** the requested display name `"Gemini 3.1 Pro (High)"` after canonical normalization (`verify.agyCanon`) → classifies **`self_reported`** (weak, never verified).
- A SECOND capture with the slug arg `--model gemini-3-pro` self-reported a DIFFERENT model (`"Gemini 3.5 Flash"`) → a weak non-match → **`unknown`** (correctly NOT a mismatch halt). This is why normalization is EXACT (case/spacing/punct/effort-suffix only), never fuzzy.
- Stability: two runs both returned well-formed JSON of this shape. Parseable + stable enough to support a weak `self_reported` parser.

This capture is the **probe/bare** shape. In normal review the model is asked (via the gated prompt) to emit the WRAPPER `{reviewmeshIdentity:{model,effort,source}, result:<ReviewerResult>}`; `verify.ParseSelfReportEnvelope` accepts BOTH shapes. Parsing of both is unit-tested (`TestParseSelfReportEnvelope`, `TestAgy_SelfReportWrapper`, `TestAgySelfReportMatching`); this file preserves the raw real evidence. No `expected-identity.json` here on purpose — the shared `TestIdentityFixtures` uses the line-based `ParseSelfReport`, not the JSON envelope parser.
