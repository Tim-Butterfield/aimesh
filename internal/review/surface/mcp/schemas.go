package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
)

// This file holds the DECLARED contract of the reviewmesh MCP server: the tool input schemas, the
// output schemas, and the `initialize.instructions` cross-tool contract. Like exploremesh's, they
// are literal JSON rather than reflection-derived — strict `oneOf`, `additionalProperties: false`
// and exact `required` sets are the whole point, and none of them survives a round trip through a
// derived schema.
//
// The same two rules govern them:
//
//   - INPUTS are strict, and the SERVER re-enforces every rule the schema states. A schema is
//     advisory to a client; a server that trusted it would be one malformed call away from running
//     something the caller did not ask for. On this server that matters more than on exploremesh,
//     because one of these tools writes.
//   - OUTPUTS declare what must ALWAYS be there. For a review the governance-bearing fields are the
//     findings' per-seat provenance, the identity caveats, the authority inclusion manifest and the
//     requested-vs-executed panel: all four are `required`, so "the governance block was dropped"
//     is a client-side-checkable schema violation rather than a promise in prose.
//
// And the rule that makes the second one real: EVERY payload this server emits is validated against
// the outputSchema its own tool declares, by a test (schemaconformance_test.go) that walks the
// result builders and by a wire-level test that re-reads the schema from `tools/list`. A declared
// schema nothing checks is a promise in prose wearing JSON — which is how three payloads came to
// match no branch of the `oneOf` they were declared under.

// Wait-budget bounds, matched to exploremesh so the two servers refuse the same magnitudes. The
// default sits well inside the ~60 s request timeout common in MCP clients.
const (
	DefaultWaitSeconds = 25
	MaxWaitSeconds     = 120
)

// MaxAuthorityDocs is the authority engine's own cap, DERIVED from it rather than restated beside
// it. The bound is repeated in the schema so a caller learns it from the declaration instead of
// from a refusal — which only helps if the two cannot drift, and they did: the schema advertised 16
// while the engine enforced 8, so every caller who trusted the declaration met a fail-closed
// refusal it had promised would not come. Taking the constant from the engine makes that
// impossible; TestAuthoritySchemaBoundMatchesEngineCap holds the wiring in place.
const MaxAuthorityDocs = authority.MaxDocs

// seatSchema is one panel seat, named by IDENTIFIER only. There is deliberately no path/args/binary
// property: those are configuration, and configuration over MCP is a non-goal.
const seatSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["adapter", "model"],
  "properties": {
    "adapter": {"type": "string", "description": "An adapter this server was launched with, as reported by the list tool. Never a path or a binary name."},
    "model": {"type": "string", "description": "The exact model identifier this adapter's CLI accepts, passed to it VERBATIM and reported back with modelSource: 'passthrough'. Look the identifier up for your adapter and the license it runs under: this server does not validate it, so a name the provider does not recognise fails in the provider's words once the run starts. Ask verifyReadiness to check it before a full run."},
    "effort": {"type": "string", "description": "Optional reasoning-effort label. A different effort is a genuinely different vantage, so it is part of a reviewer seat's identity. Accepted on 'reviewers[]' only."}
  }
}`

// panelSchema is the panel every call composes. It MUST name `author_remediator`: it is the
// host-adjudication seat, and defaulting it silently would mean the caller composed a panel whose
// adjudicator it never saw — the one seat whose judgment becomes the accepted set.
var panelSchema = fmt.Sprintf(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["reviewers", "author_remediator"],
  "description": "The panel, composed per call from the adapters this server was launched with. reviewers and author_remediator are required; cross_check and verifier run only when named, and a run without them lists them in skippedSteps. Composing one persists nothing.",
  "properties": {
    "reviewers": {"type": "array", "minItems": 1, "maxItems": %d, "items": %s, "description": "The ORDERED blind primary seats. Every seat reviews blind and in parallel; agreement is computed BY THE HOST over which seats reported a finding. Two seats with an identical (adapter, model, effort) triple are refused — an identical seat adds no independent vantage and would double-count as agreement."},
    "cross_check": %s,
    "verifier": %s,
    "author_remediator": %s
  }
}`, review.MaxReviewerSeats, seatSchema, seatSchema, seatSchema, seatSchema)

// authoritySchema is the P2 authority manifest as a caller declares it. Exactly one of
// `path`/`content` per document — the same declaration the CLI's `--authority-manifest` and the ACP
// surface's `_meta.reviewmesh.authority[]` take, because a governance declaration that meant
// different things on different surfaces would not be a governance declaration.
var authoritySchema = fmt.Sprintf(`{
  "type": "array",
  "maxItems": %d,
  "description": "The authority/context documents this review is judged AGAINST — requirements, a design doc, a spec. They are CONTEXT, never targets: never reviewed, never patched, never applied, and never placed in the containment copy. A document is embedded IN FULL or the run fails closed; it is never silently truncated.",
  "items": {
    "type": "object",
    "additionalProperties": false,
    "required": ["name"],
    "properties": {
      "name": {"type": "string", "minLength": 1, "description": "The stable label this document is referred to by, in the prompt and in the inclusion manifest. Unique within one request."},
      "path": {"type": "string", "description": "A filesystem path, read root-scoped (a .env or key file is refused, inside a trusted root or not). Exactly one of path/content. Allowed in every mode."},
      "content": {"type": "string", "description": "The document text supplied inline. Exactly one of path/content. REPORT MODE ONLY, and excluded from the host-adjudication prompt: otherwise a client model supplies the intent, its peers find deviations from it, and the host writes the result with no human artifact anywhere in the loop."},
      "mediaType": {"type": "string", "description": "Advisory type (e.g. text/markdown). Recorded in the manifest; never changes how bytes are handled."},
      "expectedHash": {"type": "string", "description": "Pins the document's FULL content (\"sha256:<hex>\"). A mismatch halts — this is what stops a path document changing between a report run and the remediation that acts on it."},
      "completeness": {"type": "string", "enum": ["requireFull", "ranges"], "default": "requireFull", "description": "requireFull: embed the whole document or refuse. ranges: embed exactly the declared byte ranges, recorded as an incomplete inclusion."},
      "ranges": {"type": "array", "items": {"type": "object", "additionalProperties": false, "required": ["start", "end"], "properties": {"start": {"type": "integer", "minimum": 0}, "end": {"type": "integer", "minimum": 0}}}, "description": "Half-open byte ranges [start, end). Required with completeness: ranges."}
    }
  }
}`, MaxAuthorityDocs)

// inlineWorkspaceSchema is the host-mediated read path: content supplied over the wire, materialized
// into a directory THIS PROCESS owns. It needs no trusted root precisely because it never touched
// this machine's filesystem — and for the same reason it can never be remediated.
var inlineWorkspaceSchema = fmt.Sprintf(`{
  "type": "object",
  "minProperties": 1,
  "maxProperties": %d,
  "additionalProperties": {"type": "string", "maxLength": %d},
  "description": "Workspace-relative path → file content, supplied inline. The server materializes it into a private temporary directory, reviews it, and deletes it. It is MODEL-AUTHORED input: results say which input mode was used, and an inline workspace can never be remediated (there is nothing real to write to).",
  "propertyNames": {"pattern": "^[^/\\\\][^\\u0000]*$"}
}`, maxInlineEntries, maxInlineEntryBytes)

// rootsArgSchema is the per-call EXTRA roots argument: directories a call reads beside its workspace.
// It is the tool-parameter form `client/roots` names as its migration ("passing directories or files
// via tool parameters").
const rootsArgSchema = `{
      "type": "array", "minItems": 1, "maxItems": 32,
      "items": {"type": "string", "minLength": 1},
      "description": "Extra ABSOLUTE directories this call reads beside its workspace — for example the project folder holding authority documents while the workspace is a temporary directory. Each follows the same rules as the workspace: absolute, not the filesystem root, a home directory, a system tree or a protected directory, and inside the operator's --root ceiling when one is set."
    }`

// commonReportProps are the run-forming parameters both review tools share.
var commonReportProps = fmt.Sprintf(`
    "roots": %s,
    "panel": %s,
    "authority": %s,
    "waitSeconds": {"type": "integer", "minimum": 1, "maximum": %d, "default": %d, "description": "How long to wait inline for the run. If it finishes in time you get the full result; otherwise you get {runId, state:\"running\"} and poll review_run_status / fetch review_run_result."},
    "maxParallel": {"type": "integer", "minimum": 1, "description": "How many reviewer seats may invoke their model CLI AT ONCE. Omitted: the whole panel runs in parallel. Lower it when the machine cannot host that many provider CLIs at once (each is a real subprocess; a local model also loads weights) or to stay under a provider rate limit. It bounds PARALLELISM only — every seat still reviews, so the findings are unchanged and only the wall clock moves."},
    "verifyReadiness": {"type": "boolean", "description": "Before dispatching anything, ask EVERY configured agent whether it can do real work: one bounded, one-token invocation per distinct adapter/model in a throwaway directory, all at once. It catches what a binary check cannot — a CLI that is installed but not logged in, blocked on folder trust, or handed a model the account cannot use — so a panel does not pay for its first seat's full prompt and then halt on its second. It SPENDS one call per agent, which is why it is opt-in; dryRun prices those calls and performs none of them. It cannot establish quota: a usage window can empty between the probe and the call, so quota is reported when it happens (signal 'quota_exhausted') rather than predicted."},
    "dryRun": {"type": "boolean", "description": "Resolve the plan and spend NOTHING. The run convenes no reviewer: it resolves the panel, the authority documents and the static preflight, then stops before the first model call and answers with status 'planned' and a 'shape' object — the seats it would convene, the lanes it would call, and the minimum/maximum model calls it is bounded by. Every configuration error a real run would raise (an unresolvable seat, a duplicate panel identity, a missing binary, an oversized authority document) is raised here too, for free. Use it to price a large panel before committing to it. It finds nothing because it looks at nothing: 'findings' is empty for that reason, not because the code is clean."},
    "idempotencyKey": {"type": "string", "maxLength": 200, "description": "Optional caller-supplied key. Repeating a call with the same key returns the EXISTING run instead of spending again — use it when retrying after a dropped connection."}`,
	rootsArgSchema, panelSchema, authoritySchema, MaxWaitSeconds, DefaultWaitSeconds)

// reportInputSchema is `review_report`. The top-level `oneOf` is the workspace XOR inlineWorkspace
// choice, re-enforced server-side; `panel` is required in both.
var reportInputSchema = fmt.Sprintf(`{
  "oneOf": [
    {
      "type": "object",
      "additionalProperties": false,
      "required": ["workspace", "panel"],
      "properties": {
        "workspace": {"type": "string", "minLength": 1, "description": "The ABSOLUTE directory to review. It is this call's root: files outside it (and outside any extra roots) are refused. It may not be the filesystem root, a home directory, a system tree or a protected directory, and it must lie inside the operator's --root ceiling when one is set."},%s
      }
    },
    {
      "type": "object",
      "additionalProperties": false,
      "required": ["inlineWorkspace", "panel"],
      "properties": {
        "inlineWorkspace": %s,%s
      }
    }
  ]
}`, commonReportProps, inlineWorkspaceSchema, commonReportProps)

// remediateInputSchema is `review_remediate`. It has one form: `fromRun` plus the workspace that run
// reviewed. `fromRun` is a value the caller already RECEIVED in a completed response, so a caller whose
// write call is cancelled still holds the handle to the run it named — basic/index says state spanning
// requests "MUST be referenced by an explicit identifier the client passes on each request". The
// `oneOf` makes `allowWrite: true` required exactly when `output` is `apply`.
var remediateInputSchema = fmt.Sprintf(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["fromRun", "workspace", "output"],
  "description": "Produce or apply the already-adjudicated accepted findings of a prior review_report run. Nothing is re-reviewed and nothing is re-judged. output=patch returns the complete diff and changes no project content; it is available on every server, so an agent can apply the change itself. output=apply writes the live workspace and is available only when the operator launched this server with --allow-writes.",
  "properties": {
    "fromRun": {"type": "string", "minLength": 1, "description": "The runId of a completed review_report run. Its accepted findings are the exact set that is patched or applied, and it is your durable handle: it survives cancellation of this call."},
    "workspace": {"type": "string", "minLength": 1, "description": "The ABSOLUTE workspace the source run reviewed. It locates that run's record, and it must be the same tree the run judged: a different workspace is refused."},
    "output": {"type": "string", "enum": ["patch", "apply"], "description": "patch: produce the complete diff and change no project content. apply: write the changes to the live workspace (requires --allow-writes at launch and allowWrite: true)."},
    "allowWrite": {"type": "boolean", "description": "Required, and literally true, for output=apply: a per-call confirmation that this call writes. Not needed for output=patch."},
    "select": {
      "type": "array",
      "minItems": 1,
      "items": {"type": "string", "minLength": 1},
      "description": "SELECTIVE APPLY. Write only the accepted findings whose HOST-COMPUTED fingerprint is listed. Take the values from review_report's accepted findings — the 'fingerprint' field, NEVER a finding's 'id' (an id is model-authored and is renumbered by the run, so keying a write set on one would let the model steer which finding you selected). Omit this to apply the whole accepted set. An empty array is REFUSED, not treated as 'apply everything'. A fingerprint that names no finding in the source run is dropped and reported in 'selection.unmatched' — nothing is ever fetched from another run. A selection can only NARROW."
    },
    "roots": %s,
    "waitSeconds": {"type": "integer", "minimum": 1, "maximum": %d, "default": %d},
    "idempotencyKey": {"type": "string", "maxLength": 200, "description": "Repeating a call with the same key returns the ORIGINAL receipt. A remediation is never applied twice for the same key, and never twice for the same fromRun."}
  },
  "oneOf": [
    {"properties": {"output": {"const": "patch"}}},
    {"required": ["allowWrite"], "properties": {"output": {"const": "apply"}, "allowWrite": {"const": true}}}
  ]
}`, rootsArgSchema, MaxWaitSeconds, DefaultWaitSeconds)

const runIDInputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["runId"],
  "properties": {"runId": {"type": "string", "minLength": 1, "description": "A runId returned by review_report or review_remediate."}}
}`

const emptyInputSchema = `{"type": "object", "additionalProperties": false, "properties": {}}`

// --- output schemas ---

// findingSchema is one adjudicated finding WITH its per-seat provenance. `supportingSeats`,
// `agreementCount` and `dissentingSeats` are host arithmetic over the blind panel: no model asserts
// them and no model is asked to count. `applyable: false` is the quarantine — a finding supported
// only by weak-identity seats, or traceable only to authority text, is reportable and never
// writable, whatever a client asks for afterwards.
const findingSchema = `{
    "type": "array",
    "description": "The adjudicated findings. 'disposition' is the host's DECISION — never infer \"this was fixed\" from a finding's presence.",
    "items": {
      "type": "object",
      "required": ["id", "fingerprint", "severity", "kind", "summary", "disposition"],
      "properties": {
        "id": {"type": "string", "description": "The finding's label. It is MODEL-AUTHORED and is renumbered by the run — never use it as a selector for review_remediate's 'select'. Use 'fingerprint'."},
        "fingerprint": {"type": "string", "description": "The HOST-COMPUTED identity of this finding (file + normalized location, hashed; kind is EXCLUDED because it is model-authored and a model relabelling its own finding would otherwise split it in two). This is the value review_remediate's 'select' takes: a model can change what a fingerprint names only by proposing a different finding, in the open."},
        "severity": {"type": "string"},
        "kind": {"type": "string"},
        "file": {"type": "string"},
        "location": {"type": "string"},
        "summary": {"type": "string"},
        "disposition": {"type": "string"},
        "sources": {"type": "array", "items": {"type": "string"}},
        "applyable": {"type": "boolean", "description": "Present, and false, ONLY when the write-path rule refused this finding. Its absence does not mean anything was written."},
        "applyRefusalReason": {"type": "string", "enum": ["authority_only", "no_workspace_evidence", "protected_path"], "description": "Why the host will not write this finding. The first two describe a finding that was never eligible to be applied. protected_path is different in kind: the finding targets a path the non-overridable write denylist protects (.env, .git/, agent client config, .aimesh/), so it is recorded as refused, every other accepted finding is still applied, and the run reports itself as NOT cleanly successful."},
        "supportingSeats": {
          "type": "array",
          "description": "Which blind seats reported this finding, with the identity tier the HOST verified for each.",
          "items": {"type": "object", "required": ["seatId", "adapter", "model", "identityTier"], "properties": {
            "seatId": {"type": "string"}, "adapter": {"type": "string"}, "model": {"type": "string"}, "identityTier": {"type": "string"}
          }}
        },
        "agreementCount": {"type": "integer", "description": "HOST-computed. Report it as given; never recount the array and never restate it in your own words."},
        "dissentingSeats": {"type": "array", "items": {"type": "string"}, "description": "Seats that RAN successfully and did not report this finding."},
        "distinctModels": {"type": "integer", "description": "How many different MODELS the supporting seats ran. NOT an adjusted agreement figure — agreementCount is unchanged and means what it always meant. This is a count of a different thing: '3 seats, 2 distinct models' states the situation without anyone interpreting a weighting."},
        "agreementIndependence": {"type": "string", "enum": ["distinct_models", "shared_model"], "description": "What those seats were WORTH. shared_model: at least two ran the same model, so their errors correlate and the extra agreement is worth less than the count suggests — it does NOT mean the finding is wrong. Report it alongside agreementCount whenever you surface the count. Only the model STRING is asserted; vendor, base family and weight lineage are not observable here and are deliberately not claimed."},
        "consensus": {"type": "string", "enum": ["unanimous", "majority", "contested"], "description": "What the seats that RAN did with this finding. contested: a minority reported it, or the seats split evenly. A SILENT SEAT IS NOT A SEAT THAT DISAGREED — seats are blind and each reports what IT found, so a seat missing from supportingSeats may have disagreed, may never have reached that file, or may have stopped when its own set stabilized. So contested is NOT evidence against the finding and does NOT make it less likely to be real: it marks the finding a human should read rather than skim. Do NOT drop, deprioritise or soften a contested finding, and do not describe it as disputed. Absent when no panel stood behind the finding, or when fewer than two seats completed."},
        "grounding": {
          "type": "object",
          "required": ["status"],
          "description": "HOST-COMPUTED, by reading files and executing nothing: whether this finding's CITATION points at something that exists. It is NOT a disposition and NOT a verification of the finding. 'grounded' means the file/line/symbol is there; it says nothing about whether the code there does what the finding claims. 'file_missing' means the pointer did not resolve — the finding was still raised, adjudicated and reported, because a reviewer that named the wrong file may have found a real defect one file over. Never drop, reorder or discount a finding on this field.",
          "properties": {
            "file": {"type": "string", "description": "The citation as the finding gave it."},
            "location": {"type": "string", "description": "The location as the finding gave it: a line, a range, a symbol, or free text."},
            "status": {"type": "string", "enum": ["grounded", "no_citation", "file_missing", "line_out_of_range", "symbol_absent", "location_unchecked", "unreadable"], "description": "no_citation: the finding names no file, which is ordinary and not a failure. location_unchecked: the file exists and the location was not in a shape this pass would check — it says nothing either way, and must not be reported as a problem. unreadable: the host could not look, which is a fact about the host, not the finding."},
            "detail": {"type": "string", "description": "What was checked and what happened, in a sentence."},
            "lineCount": {"type": "integer", "description": "The cited file's length when it was read, so a line-range result is checkable rather than asserted."}
          }
        }
      }
    }
  }`

const panelEchoSchema = `{
    "type": "object",
    "required": ["requested", "executed"],
    "description": "The panel as REQUESTED and as EXECUTED. ALWAYS present, and always both halves: a seat that halted is visible only by comparing them.",
    "properties": {
      "requested": {
        "type": "object",
        "required": ["source"],
        "properties": {
          "source": {"type": "string", "enum": ["adhoc"]},
          "reviewers": {"type": "array", "items": {"type": "object"}},
          "cross_check": {"type": "object"},
          "verifier": {"type": "object"},
          "author_remediator": {"type": "object"}
        }
      },
      "executed": {
        "type": "array",
        "description": "One entry per REQUESTED seat, in requested order — including a seat that halted or never started. N seats requested is N seats accounted for.",
        "items": {"type": "object", "required": ["seatId", "adapter", "model", "status"], "properties": {
          "seatId": {"type": "string"}, "index": {"type": "integer"}, "adapter": {"type": "string"},
          "model": {"type": "string"}, "effort": {"type": "string"}, "status": {"type": "string"},
          "rounds": {"type": "integer"}, "findings": {"type": "integer"},
          "identityTier": {"type": "string"}, "reasonCode": {"type": "string"}
        }}
      }
    }
  }`

const identityCaveatsSchema = `{
    "type": "array",
    "description": "Every lane that ran and returned valid output but whose model identity was NOT strongly verified. ALWAYS present; an empty array is the positive statement that every lane was verified. A weak-identity lane is not a peer of a verified one and must not be presented as one.",
    "items": {
      "type": "object",
      "required": ["role", "status"],
      "properties": {
        "role": {"type": "string"}, "adapter": {"type": "string"},
        "requestedModel": {"type": "string"}, "reportedModel": {"type": "string"},
        "evidence": {"type": "string"}, "status": {"type": "string"}
      }
    }
  }`

const authorityManifestSchema = `{
    "type": "array",
    "description": "The INCLUSION MANIFEST: per authority document, the source, the full-content and embedded-content hashes, the byte counts, and whether the inclusion was complete. ALWAYS present (empty when none was declared) — \"which intent was this judged against\" is a governance fact, never an inference from an absent key.",
    "items": {
      "type": "object",
      "required": ["name", "source", "fullHash", "embeddedHash", "complete"],
      "properties": {
        "name": {"type": "string"}, "source": {"type": "string", "enum": ["path", "inline"]},
        "mediaType": {"type": "string"}, "fullHash": {"type": "string"}, "embeddedHash": {"type": "string"},
        "bytesEmbedded": {"type": "integer"}, "bytesTotal": {"type": "integer"}, "complete": {"type": "boolean"},
        "ranges": {"type": "array", "items": {"type": "object"}}
      }
    }
  }`

const withheldSchema = `{
    "type": "array",
    "description": "Files a CONTAINMENT rule kept OUT of the reviewed set. ALWAYS present. A reviewer cannot object to a file it was never shown, so silence about a withheld file is not approval.",
    "items": {"type": "object", "required": ["path", "reason"], "properties": {
      "path": {"type": "string"}, "reason": {"type": "string"}, "rule": {"type": "string"},
      "detail": {"type": "string"}, "stage": {"type": "string", "enum": ["copy", "snippets"]}
    }}
  }`

// groundingSchema is the RUN-LEVEL tally of the non-executing citation check. It reads files and
// executes nothing, so it is not a capability grant and is always on.
//
// The `note` is required rather than optional, and that is the point of the schema entry. The
// tempting misreading of this block is "12 of 14 findings verified"; what it actually says is "12 of
// 14 findings point at a file, line or symbol that exists". A caller that surfaces the ratio without
// the qualifier has upgraded a floor into a corroboration, so the qualifier travels with the numbers
// instead of living in prose a caller may never read.
const groundingSchema = `{
    "type": "object",
    "required": ["checked", "grounded", "unresolved", "note"],
    "description": "What the host could verify about the findings' CITATIONS without executing anything: the cited file exists, the cited lines exist, the named symbol appears. It DROPS AND DOWNGRADES NOTHING — an unresolved citation is a label on a finding that was raised, adjudicated and reported exactly as any other, because a reviewer that named the wrong file may still have found a real defect one file over.",
    "properties": {
      "checked": {"type": "integer", "description": "Findings that carried a citation this pass examined."},
      "grounded": {"type": "integer", "description": "Of those, how many pointed at something that is there."},
      "unresolved": {"type": "integer", "description": "Of those, how many did not. Report this number WITH the note — it is a statement about the pointer, never about whether the finding is correct."},
      "noCitation": {"type": "integer", "description": "Findings naming no file at all. Deliberately not part of 'checked': a finding about the change as a whole has nothing to resolve."},
      "byStatus": {"type": "object", "description": "The breakdown by stable status code: grounded | no_citation | file_missing | line_out_of_range | symbol_absent | location_unchecked | unreadable."},
      "note": {"type": "string", "description": "The fixed sentence stating what grounding does NOT establish. Pass it on whenever you surface the counts."}
    }
  }`

// verificationSchema is the BOUNDED-EXECUTION record: the project's own build/test commands, run on
// the containment copy. Present only when the OPERATOR supplied commands at launch; a calling model
// cannot name a command and cannot cause one to run.
//
// `delta` is `required` and `note` is `required`, and both for the same reason: the two obvious
// misreadings of this block run in opposite directions. Green invites "the change is fine"; red
// invites "the findings were wrong". Neither follows, and the fields that say so travel with the ones
// that invite it.
const verificationSchema = `{
    "type": "object",
    "required": ["commands", "delta", "note"],
    "description": "What the PROJECT'S OWN build/test commands did, on the containment copy, before and after the accepted findings were applied to it. Nothing model-authored is ever executed and your working tree is never touched. IT GATES NOTHING: the second pass runs after the commit, so a failure structurally cannot have blocked anything, and no finding was dropped, downgraded or reordered by any of this.",
    "properties": {
      "commands": {"type": "array", "items": {"type": "string"}, "description": "The operator's command list, as given."},
      "before": {"type": "array", "items": {"$ref": "#/$defs/verificationResult"}, "description": "The baseline pass, taken on the copy before any edit reached it."},
      "after": {"type": "array", "items": {"$ref": "#/$defs/verificationResult"}, "description": "The second pass, with the edits applied. EMPTY on a report run, which writes nothing and so has no 'after'."},
      "delta": {"type": "string", "enum": ["unchanged_pass", "unchanged_fail", "fixed", "broken", "baseline_only", "not_comparable"], "description": "READ THIS FIRST. Absolute green is not the signal: real repositories routinely have a red suite, so 'unchanged_fail' is an ORDINARY outcome meaning the change did not make things worse. 'broken' is the one worth surfacing — and it still is not a verdict on any finding. 'not_comparable' means a command timed out or could not start, which is a fact about the budget or the host, not about the code."},
      "note": {"type": "string", "description": "The fixed sentence stating both limits. Pass it on whenever you surface the delta."}
    },
    "$defs": {
      "verificationResult": {
        "type": "object",
        "required": ["command", "ok", "exitCode"],
        "properties": {
          "command": {"type": "string"},
          "ok": {"type": "boolean"},
          "exitCode": {"type": "integer"},
          "timedOut": {"type": "boolean", "description": "Present and true only when the command hit its budget. Separate from a failure on purpose — a wall clock is not a defect in the code."},
          "unstartable": {"type": "string", "description": "Present only when the command could not be launched at all. A fact about this host."},
          "durationMs": {"type": "integer"},
          "output": {"type": "string", "description": "The TAIL of the combined streams, bounded. The tail is kept because build tools print their banner first and their error last."}
        }
      }
    }
  }`

// compositionSchema is the run-level record of WHAT THE PANEL WAS. It is always present for a run
// with a blind panel, and its `note` is required for the same reason the verification note is: the
// two misreadings run in opposite directions — shared_model invites discarding a real agreement,
// distinct_models invites treating one as proof — and the sentence that says neither follows has to
// travel with the numbers that invite them.
const compositionSchema = `{
    "type": "object",
    "required": ["seats", "distinctModels", "independence", "note"],
    "description": "What the blind panel WAS: each seat as the operator configured it, and how many distinct models they ran between them. It qualifies every agreementCount in this result WITHOUT changing any of them.",
    "properties": {
      "seats": {"type": "array", "items": {"type": "object"}, "description": "The panel in requested order: seatId, index, adapter, model, effort, and modelSource. modelSource is 'catalog' (a configured model key) or 'passthrough' (the caller composed a seat naming a model the configuration does not define, handed to the adapter verbatim). The ADAPTER is validated fail-closed either way and nothing about a pass-through seat's findings differs — but the configuration cannot describe that model, and whether it exists at all was the provider's answer. A passThroughHint, when present, names the configured key the model most plausibly meant."},
      "distinctModels": {"type": "integer", "description": "How many different models the panel ran between them."},
      "independence": {"type": "string", "enum": ["distinct_models", "shared_model"], "description": "shared_model means at least two seats ran the same model. Agreement between them is real but correlated."},
      "note": {"type": "string", "description": "The fixed sentence stating the limit in both directions. Pass it on whenever you surface an agreement count."}
    }
  }`

// reviewShapeSchema is the DRY RUN's disclosure: what the review WOULD do, before anything is spent.
//
// It is present exactly when `status` is `planned`, and it is the ONLY thing that makes a dry run
// worth calling: the run deliberately convenes nobody, so a caller that received `status: planned`
// and an empty finding set without this would have paid nothing and learned nothing. Read `findings`
// as "nothing was looked at", never as "the tree is clean" — the empty array is a consequence of the
// stop, not a result.
//
// THE CALL COUNTS ARE A RANGE, unlike exploremesh's single figure. A review iterates until
// adjudication converges, so the floor is what it cannot avoid and the ceiling is every configured
// cap multiplied out; a single number would have to understate the iterating run or overstate the
// ordinary one.
const reviewShapeSchema = `{
    "type": "object",
    "required": ["mode", "seats", "lanes", "minModelCalls", "maxModelCalls", "payload"],
    "description": "What this run WOULD do. Present only when status is 'planned' (a dryRun call). Every identity here RESOLVED and no adapter was contacted.",
    "properties": {
      "mode": {"type": "string", "description": "The mode that was PRICED — a dry run of an apply run prices an apply run and still writes nothing."},
      "surface": {"type": "string"},
      "seats": {"type": "array", "items": {"type": "object"}, "description": "The blind panel in REQUESTED order: seatId, adapter, model, effort. Every seat runs; maxParallel bounds only how many at once, so shortening this list is the only thing that removes a call."},
      "lanes": {"type": "array", "items": {"type": "object"}, "description": "Every other resolved role. A role ABSENT here is a call this run will not make. An 'execution: host' lane runs in-process and costs no model call."},
      "skippedSteps": {"type": "array", "items": {"type": "string", "enum": ["cross_check", "verifier"]}, "description": "The optional steps this run will not take because no seat was assigned to them. Report them with the result so a thinner review is not mistaken for a full one."},
      "maxParallel": {"type": "integer", "description": "Bounds wall clock, never cost."},
      "outerCycles": {"type": "integer"},
      "panelRounds": {"type": "integer", "description": "The budget the panel SHARES for one cycle — never a per-seat figure."},
      "minModelCalls": {"type": "integer", "description": "The floor: a run that converges on its first cycle with nothing contested."},
      "maxModelCalls": {"type": "integer", "description": "The ceiling: every configured cap multiplied out. Report the RANGE, not one end of it."},
      "readinessProbes": {"type": "integer", "description": "One-token readiness invocations before dispatch, counted in BOTH bounds because a run that enables them cannot make fewer. 0 unless asked for."},
      "writes": {"type": "boolean"},
      "deterministicHost": {"type": "boolean", "description": "The adjudication costs no model call."},
      "payload": {"type": "object", "description": "What every reviewer would be shown: the file count, the total bytes (sent once per seat per round), the full path list, and anything containment withheld."},
      "egress": {"type": "array", "items": {"type": "object"}, "description": "WHERE THE CONTENT WOULD GO, grouped by destination, each with the seats and roles that reach it. 'local: true' means nothing leaves the machine; 'known: false' is a user-defined adapter whose destination this tool cannot state — treat it as leaving. Surface this whenever you report what a run would cost: who receives the code is the other half of that question."}
    }
  }`

// dissentSchema is the run-level tally of the per-finding consensus labels: how much of this result
// the panel actually agreed on.
//
// Its `note` is required for the same reason the composition note is, and the misreading it guards
// against is sharper: "3 contested" reads as three doubtful findings to anyone who does not know that
// a blind seat's silence is not a vote. The sentence that says so has to travel with the number.
const dissentSchema = `{
    "type": "object",
    "required": ["panelled", "unanimous", "majority", "contested", "note"],
    "description": "How much of this result the blind panel agreed on. It qualifies NOTHING away: a contested finding is reported, adjudicated, applyable and counted exactly as a unanimous one is.",
    "properties": {
      "panelled": {"type": "integer", "description": "Findings with a describable panel behind them — the denominator. unanimous + majority + contested equals it."},
      "unanimous": {"type": "integer", "description": "Every seat that completed reported it."},
      "majority": {"type": "integer", "description": "More of the completed seats reported it than did not."},
      "contested": {"type": "integer", "description": "A minority reported it, or the seats split evenly. NOT a count of doubtful findings."},
      "note": {"type": "string", "description": "The fixed sentence stating what a silent seat does and does not mean. Pass it on whenever you surface the contested count."}
    }
  }`

// partialPanelSchema is present ONLY when a capacity failure — a provider out of quota, a wall clock
// reached — removed a seat and the run continued rather than discarding the seats that had answered.
//
// Its PRESENCE is the signal, which is why it is absent rather than zeroed on a full panel: a caller
// must not have to compare two numbers to notice that the panel it requested is not the panel that
// answered.
const partialPanelSchema = `{
    "type": "object",
    "required": ["configured", "answered", "lost", "note"],
    "description": "This review ran with FEWER SEATS than were configured. A provider ran out of capacity and the run continued instead of discarding the seats that had already answered — that is a fact about a billing relationship or a clock, never about whether the surviving findings are sound. Read every agreementCount against 'answered', NOT against the panel that was requested.",
    "properties": {
      "configured": {"type": "integer", "description": "Seats the panel was configured with."},
      "answered": {"type": "integer", "description": "Seats that actually produced a result. THIS is the denominator every agreement count in the result is over."},
      "lost": {"type": "array", "items": {"type": "object"}, "description": "Each seat the run continued without: seatId, adapter, model, signal (quota_exhausted | timeout) and its own actionable detail."},
      "note": {"type": "string", "description": "The fixed sentence stating what must not be concluded in either direction — neither that the result is untrustworthy, nor that it is as strong as a full panel's."}
    }
  }`

// scopeSchema is present ONLY when the review was NARROWED to part of the tree. Its presence is the
// signal, and its `note` is required, because the misreading here is the most damaging one this
// server can produce: a caller that sees a clean result without knowing four files were shown has
// been told the tree is clean when nobody looked at most of it.
const scopeSchema = `{
    "type": "object",
    "required": ["selected", "available", "files", "note"],
    "description": "This review was NARROWED: reviewers saw only the files listed here. It says NOTHING about the rest of the tree — the absence of a finding elsewhere means nobody looked, not that there is nothing there. Containment is unchanged; scope only decides what was shown.",
    "properties": {
      "selected": {"type": "integer", "description": "Files the scope admitted — what was actually reviewed."},
      "available": {"type": "integer", "description": "Files the tree holds, so the ratio is visible rather than inferred."},
      "paths": {"type": "array", "items": {"type": "string"}, "description": "The explicit paths or globs, as given. Works in any directory."},
      "changedSince": {"type": "string", "description": "The time window, as given (a duration or an RFC3339 stamp). Works in ANY directory, including one under no version control."},
      "vcsRef": {"type": "string", "description": "The version-control baseline, as given (diff | staged | a ref). Available in a repository only — one baseline among several, not the organising idea."},
      "files": {"type": "array", "items": {"type": "string"}, "description": "The selected set, sorted: exactly what a reviewer was shown."},
      "note": {"type": "string", "description": "The fixed sentence stating what this review is silent about. Pass it on whenever you report the result."}
    }
  }`

const countsSchema = `{
    "type": "object",
    "required": ["findings", "panelSeats", "panelSeatsCompleted"],
    "description": "HOST-computed totals. A consumer must never recount the arrays and reach a different number.",
    "properties": {
      "findings": {"type": "integer"},
      "bySeverity": {"type": "object"}, "byDisposition": {"type": "object"},
      "identityCaveats": {"type": "integer"}, "withheld": {"type": "integer"},
      "panelSeats": {"type": "integer"}, "panelSeatsCompleted": {"type": "integer"},
      "accepted": {"type": "integer", "description": "Findings in the ACCEPTED set — the exact set review_remediate would write."},
      "quarantined": {"type": "integer", "description": "Findings the write-path rule refused: reportable, never applyable."}
    }
  }`

// haltProps is the taxonomy payload a domain halt carries, on `structuredContent` of a result with
// `isError: true` — never a JSON-RPC error, whose `data` clients routinely flatten or drop.
const haltProps = `
      "exitCode": {"type": "integer", "description": "The shared aimesh halt taxonomy exit code (2 usage, 3 config, 4 adapter, 5 model/identity, 6 containment, 7 policy/cap, 8 internal)."},
      "haltClass": {"type": "string"},
      "reasonCode": {"type": "string", "description": "The stable machine reason code. Branch on this, never on the message text."},
      "failure": {"type": "object", "description": "The sanitized, capped identification of the failing lane."}`

// reportResultSchema is the declared outputSchema of `review_report`. It is a FOUR-branch `oneOf`
// keyed on `state`, which is what lets the governance-bearing fields be REQUIRED on the branch
// where a result exists instead of being softened to optional so that a "running" reply can
// validate.
//
// The branches are `running` / `complete` / `halted` / `cancelled`, and each `state` is a `const`
// rather than an `enum`, so exactly one branch can ever match. `cancelled` is its own branch
// because a cancelled call is genuinely a fourth shape: it is not a halt (nothing failed) and it
// has no result, but it does carry the panel echo and the taxonomy. It must NOT be emitted as the
// RUNNING shape with `state` overwritten — that is a payload matching no branch at all.
//
// `panel` is required on `running` and `cancelled` as well as on `complete`. The requested-vs-
// executed echo is the one governance fact that exists from the moment a run is admitted, so
// "which panel is this?" is answerable from the first reply rather than only from the last.
var reportResultSchema = fmt.Sprintf(`{
  "oneOf": [
    {
      "type": "object",
      "required": ["runId", "state", "panel"],
      "description": "The run outlived the inline wait budget. Poll review_run_status, then fetch review_run_result.",
      "properties": {
        "runId": {"type": "string", "description": "Opaque. It names this server's run record; it is never a filesystem path."},
        "state": {"const": "running"},
        "tool": {"type": "string"}, "mode": {"type": "string"},
        "waitedSeconds": {"type": "integer"},
        "panel": %s
      }
    },
    {
      "type": "object",
      "required": ["runId", "state", "mode", "status", "findings", "counts", "identityCaveats", "authority", "panel", "withheld"],
      "properties": {
        "runId": {"type": "string"},
        "state": {"const": "complete"},
        "tool": {"type": "string"},
        "mode": {"type": "string", "description": "The EFFECTIVE output mode. review_report is always 'report': it writes nothing."},
        "requestedMode": {"type": "string", "description": "Present only when a policy ceiling narrowed the mode — its presence IS the signal."},
        "status": {"type": "string"},
        "workspaceSource": {"type": "string", "enum": ["path", "inline"], "description": "Whether the reviewed content came from a trusted-root path or was supplied inline by the caller. Findings about model-authored inline content are not findings about the user's files."},
        "remediable": {"type": "boolean", "description": "Whether this run's accepted set can be handed to review_remediate --fromRun."},
        "findings": %s,
        "counts": %s,
        "identityCaveats": %s,
        "authority": %s,
        "withheld": %s,
        "panel": %s,
        "grounding": %s,
        "verification": %s,
        "composition": %s,
        "dissent": %s,
        "partialPanel": %s,
        "scope": %s,
        "shape": %s
      }
    },
    {
      "type": "object",
      "required": ["runId", "state", "exitCode", "haltClass", "reasonCode"],
      "description": "The run FAILED, or was refused before it started. Branch on reasonCode. A refusal that happened before any spend carries the taxonomy and nothing else — there was no panel to echo.",
      "properties": {
        "runId": {"type": "string"},
        "state": {"const": "halted"},
        "tool": {"type": "string"}, "mode": {"type": "string"},
        "panel": %s,
        "identityCaveats": %s,%s
      }
    },
    {
      "type": "object",
      "required": ["runId", "state", "haltClass", "reasonCode", "panel"],
      "description": "The CALLER cancelled (or the session went away) and the run was killed. Nothing failed, and there is no result — but the panel echo and the taxonomy are still owed, because a cancelled review still spent whatever it spent before the cancel.",
      "properties": {
        "runId": {"type": "string"},
        "state": {"const": "cancelled"},
        "tool": {"type": "string"}, "mode": {"type": "string"},
        "waitedSeconds": {"type": "integer"},
        "panel": %s,
        "identityCaveats": %s,%s
      }
    }
  ]
}`, panelEchoSchema, findingSchema, countsSchema, identityCaveatsSchema, authorityManifestSchema,
	withheldSchema, panelEchoSchema, groundingSchema, verificationSchema, compositionSchema, dissentSchema, partialPanelSchema, scopeSchema, reviewShapeSchema,
	panelEchoSchema, identityCaveatsSchema, haltProps,
	panelEchoSchema, identityCaveatsSchema, haltProps)

// The PARTIAL-REFUSAL contract, declared once and reused on every branch that can carry it. A
// finding whose target is a protected path is refused rather than halting the
// run; these three fields are how a caller learns that happened without reading the receipt.
const (
	applyOutcomeSchema = `{
      "type": "string",
      "enum": ["applied", "partial_refusal", "nothing_applied"],
      "description": "What this write actually did. \"partial_refusal\" means the write COMMITTED and at least one accepted finding was refused because its target is a protected path — the run is not a clean success, and isError is true. \"nothing_applied\" means the write committed and applied nothing (check counts.refused to tell \"nothing to do\" from \"everything refused\"). Absent on results that are not writes."
    }`
	applyCountsSchema = `{
      "type": "object",
      "required": ["applied", "refused"],
      "description": "Host-computed totals for this write. Never recount the arrays and reach a different number.",
      "properties": {
        "applied": {"type": "integer", "description": "Findings whose hunks reached the live tree (apply) or the produced diff (patch)."},
        "refused": {"type": "integer", "description": "Findings refused because their target is a PROTECTED PATH. Greater than zero means this run did not apply everything it was asked to, and isError is true."}
      }
    }`
	refusalsSchema = `{
      "type": "array",
      "description": "The findings refused for a protected path. NOTHING was written to any of these — the denylist is not overridable — and every other accepted finding was applied normally. Re-running the same remediation refuses them identically; it is not a retryable failure.",
      "items": {
        "type": "object",
        "required": ["fingerprint", "file", "reason"],
        "properties": {
          "fingerprint": {"type": "string", "description": "The HOST-COMPUTED identity of the finding. It is deliberately not the finding's id: an id is model-authored, and a model that could relabel findings could steer which one a follow-up selection names."},
          "file": {"type": "string"},
          "reason": {"type": "string", "enum": ["protected_path"]}
        }
      }
    }`

	// selectionSchema is the record of a SELECTIVE APPLY. All three lists are always present so
	// that "no selector was unmatched" is a stated fact rather than an absent key.
	selectionSchema = `{
      "type": "object",
      "description": "What the narrowing 'select' argument did. Present only when 'select' was supplied.",
      "required": ["requested", "matched", "unmatched"],
      "properties": {
        "requested": {"type": "array", "items": {"type": "string"}, "description": "The host-computed fingerprints this call asked to narrow the write set to, trimmed and deduped."},
        "matched": {"type": "array", "items": {"type": "string"}, "description": "Those that named a finding in the source run's accepted set. Only these were considered for writing."},
        "unmatched": {"type": "array", "items": {"type": "string"}, "description": "Those that named NOTHING. They wrote nothing and nothing was fetched for them — a selection can only narrow. If every selector is unmatched the call is REFUSED rather than writing nothing silently."}
      }
    }`
)

// remediateResultSchema is the declared outputSchema of `review_remediate`. The complete branch IS
// the receipt: what was intended, what was written, and whether anything reached the live tree.
var remediateResultSchema = fmt.Sprintf(`{
  "oneOf": [
    {
      "type": "object",
      "required": ["runId", "state"],
      "properties": {
        "runId": {"type": "string"}, "state": {"const": "running"},
        "tool": {"type": "string"}, "mode": {"type": "string"}, "waitedSeconds": {"type": "integer"}
      }
    },
    {
      "type": "object",
      "required": ["runId", "state", "mode", "receipt"],
      "properties": {
        "runId": {"type": "string"},
        "state": {"const": "complete"},
        "tool": {"type": "string"},
        "mode": {"type": "string", "enum": ["patch", "apply"]},
        "sourceRunId": {"type": "string", "description": "The review_report run whose accepted set was applied."},
        "outcome": %s,
        "counts": %s,
        "refusals": %s,
        "selection": %s,
        "receipt": {
          "type": "object",
          "required": ["runId", "mode", "status", "intended", "applied", "files", "committed"],
          "description": "The durable answer to \"what did this call actually write\". It is persisted to the run record on every path, including a cancelled call, and is the same object recorded there.",
          "properties": {
            "runId": {"type": "string"}, "sourceRunId": {"type": "string"},
            "mode": {"type": "string"}, "status": {"type": "string", "enum": ["complete", "halted", "cancelled"]},
            "reasonCode": {"type": "string"},
            "baseHashesVerified": {"type": "integer", "description": "How many targeted files were re-verified byte-for-byte against the review that judged them, before the write window opened."},
            "intended": {"type": "array", "description": "The journal: every hunk this call intended to write, recorded BEFORE the first edit was applied. Each carries replacementSha256 — the digest of the text it was about to write, since a byte count identifies nothing.", "items": {"type": "object"}},
            "intentSha256": {"type": "string", "description": "A canonical digest over the WHOLE ordered intent, so this receipt and the run record's journal are tied by value rather than by proximity."},
            "applied": {"type": "array", "items": {"type": "object"}},
            "notApplied": {"type": "array", "items": {"type": "object"}},
            "files": {"type": "array", "items": {"type": "string"}},
            "commitAttempted": {"type": "boolean", "description": "Whether the LIVE commit was entered. With committed:false it distinguishes \"nothing was tried\" from \"something was tried and rolled back\"."},
            "patchArtifact": {"type": "string", "description": "The run-record-relative name of the COMPLETE patch. The content is never inlined: a syntactically valid truncated patch is worse than a reference."},
            "patchSha256": {"type": "string"},
            "patchResource": {"type": "string", "description": "The MCP resource URI of the complete patch — fetch it with resources/read. It is opaque and run-scoped (aimesh://run/<runId>/patch): it names WHAT to fetch, never where it lives, because this surface reports no host paths. The same URI also rides the result as a resource_link content block."},
            "committed": {"type": "boolean", "description": "Whether anything reached the LIVE workspace. Always false in patch mode, and always false for a cancelled call."}
          }
        },
        "withheld": %s
      }
    },
    {
      "type": "object",
      "required": ["runId", "state", "exitCode", "haltClass", "reasonCode"],
      "properties": {
        "runId": {"type": "string"},
        "state": {"const": "halted"},
        "tool": {"type": "string"}, "mode": {"type": "string"},
        "sourceRunId": {"type": "string"},
        "outcome": %s,
        "counts": %s,
        "refusals": %s,
        "selection": %s,
        "receipt": {"type": "object", "description": "Present whenever the write window was reached."},%s
      }
    },
    {
      "type": "object",
      "required": ["runId", "state", "haltClass", "reasonCode"],
      "description": "The CALLER cancelled. 'committed: false' on the receipt is the fact that matters: a cancelled write window never reaches the live workspace.",
      "properties": {
        "runId": {"type": "string"},
        "state": {"const": "cancelled"},
        "tool": {"type": "string"}, "mode": {"type": "string"},
        "waitedSeconds": {"type": "integer"},
        "sourceRunId": {"type": "string"},
        "panel": %s,
        "identityCaveats": %s,
        "receipt": {"type": "object", "description": "Present whenever the write window was reached — including a cancelled call, which is the case a response cannot otherwise report."},%s
      }
    }
  ]
}`,
	applyOutcomeSchema, applyCountsSchema, refusalsSchema, selectionSchema, withheldSchema,
	applyOutcomeSchema, applyCountsSchema, refusalsSchema, selectionSchema, haltProps,
	panelEchoSchema, identityCaveatsSchema, haltProps)

// runResultSchema is the declared outputSchema of `review_run_result`, and it is deliberately NOT
// reportResultSchema. `review_run_result` fetches ANY run this server started, and a remediation run's
// result is a RECEIPT, not a review. Declaring it under the report schema would make every
// remediation fetched through `review_run_result` a payload its own tool said was impossible.
//
// The union is `anyOf`, not `oneOf`, and that is the honest operator: the two schemas overlap on
// the shapes they share (a pre-spend refusal carries the taxonomy and nothing that identifies which
// tool it came from), so demanding exactly one match would fail a payload that is correct under
// both. Each ARM is still an exclusive `oneOf` over `state`, so nothing inside either is loosened.
var runResultSchema = fmt.Sprintf(`{
  "description": "The full result of a run: a REVIEW result when the run was started by review_report, or a remediation RECEIPT when it was started by review_remediate. Branch on 'tool' to know which, and on 'state' within it.",
  "anyOf": [%s, %s]
}`, reportResultSchema, remediateResultSchema)

var runStatusSchema = fmt.Sprintf(`{
  "type": "object",
  "required": ["runId", "state"],
  "properties": {
    "runId": {"type": "string"},
    "state": {"type": "string", "enum": ["running", "complete", "halted", "cancelled"]},
    "tool": {"type": "string"}, "mode": {"type": "string"},
    "elapsedSeconds": {"type": "number"},
    "remediable": {"type": "boolean"},
    "outcome": %s,
    "refusedCount": {"type": "integer", "description": "How many findings this write refused for a protected path. A run with state \"complete\" and refusedCount > 0 is NOT a clean success — do not report it as one without fetching review_run_result."}
  }
}`, applyOutcomeSchema)

// listOutputSchema is the SANITIZED configuration projection. Note what is absent and cannot be
// added without changing the projection type: binary paths, launch arguments, environment. A tool
// result is inference input for a third party — shipping the operator's environment into it is a
// disclosure, not a convenience.
const listOutputSchema = `{
  "type": "object",
  "required": ["adapters", "modes", "limits", "remediation"],
  "properties": {
    "adapters": {
      "type": "array",
      "description": "The adapters this server was launched with. A panel seat may name only these. An adapter with available: false cannot be used until its CLI can be started; 'reason' says why.",
      "items": {
        "type": "object",
        "required": ["name", "kind", "available"],
        "properties": {
          "name": {"type": "string"}, "displayName": {"type": "string"},
          "kind": {"type": "string", "enum": ["shell", "fake"]},
          "available": {"type": "boolean"},
          "reason": {"type": "string"},
          "source": {"type": "string", "enum": ["flag", "env"], "description": "Where the operator named the adapter."},
          "identityEvidenceCapability": {"type": "string", "description": "The adapter's DECLARED evidence tier — not a live verdict. Proving a model's identity still takes a real call."}
        }
      }
    },
    "modes": {"type": "array", "items": {"type": "string"}},
    "limits": {"type": "object"},
    "remediation": {
      "type": "object",
      "required": ["writes", "diffAvailable"],
      "description": "Who performs writes on this server.",
      "properties": {
        "writes": {"type": "string", "enum": ["aimesh", "agent"], "description": "aimesh: the operator launched this server with --allow-writes, so review_remediate output=apply writes the workspace. agent: aimesh never changes project content here; apply the diff yourself."},
        "diffAvailable": {"type": "boolean", "description": "Always true: review_remediate output=patch supplies the complete diff on every server."},
        "note": {"type": "string"}
      }
    },
    "roots": {"type": "object", "required": ["ceiling"], "description": "How many --root ceiling directories bound the paths a call may declare (0 = no ceiling). The paths are NOT reported.", "properties": {"ceiling": {"type": "integer"}}}
  }
}`

// doctorOutputSchema is the readiness projection. Every field is a count, an enum or a boolean — never a
// path — because a tool result is inference input for a third party.
const doctorOutputSchema = `{
  "type": "object",
  "required": ["ok", "checks", "writes", "diffAvailable", "rootCeiling", "protocolMode", "protocolEra"],
  "properties": {
    "ok": {"type": "boolean", "description": "True when at least one adapter this server was launched with can be started."},
    "checks": {
      "type": "array",
      "items": {"type": "object", "required": ["name", "ok"], "properties": {
        "name": {"type": "string"}, "ok": {"type": "boolean"}, "detail": {"type": "string"}
      }}
    },
    "writes": {"type": "string", "enum": ["aimesh", "agent"], "description": "Who performs writes. aimesh: launched with --allow-writes, so review_remediate output=apply writes the workspace. agent: aimesh never changes project content here; ask for output=patch and apply the diff yourself."},
    "diffAvailable": {"type": "boolean", "description": "Always true: review_remediate output=patch supplies the complete diff on every server."},
    "verify": {"type": "object", "description": "The operator's --verify-cmd grant: how many project build/test commands run on each review's containment copy (0 = none), whether they also run on report runs, and the per-command timeout. The commands themselves are never reported, and their result gates nothing.", "properties": {"commands": {"type": "integer", "minimum": 0}, "baseline": {"type": "boolean"}, "timeoutSeconds": {"type": "integer", "minimum": 0}}},
    "rootCeiling": {"type": "integer", "minimum": 0, "description": "How many --root ceiling directories bound the paths a call may declare. 0 means no ceiling. The paths are never reported."},
    "protocolMode": {
      "type": "string",
      "enum": ["dual", "legacy"],
      "description": "The era posture the OPERATOR launched this process with. dual: it serves whichever MCP era the client opens with — the sessionless 2026-07-28 revision (per-request _meta) or a legacy initialize handshake — and commits to that one era for the life of the process. legacy: it is a pre-2026-07-28 server in every observable respect, server/discover included, which answers with method-not-found. A legacy-mode process is a documented compatibility fallback and is NOT a conformant 2026-07-28 deployment."
    },
    "protocolEra": {
      "type": "string",
      "enum": ["legacy", "modern"],
      "description": "Which revision family is serving THIS request. modern (2026-07-28 and later): stateless, no handshake, and this server emits NO notifications/message at all — the Logging feature is deprecated in that revision and this server writes diagnostics to stderr instead, which is the migration the specification names; use _meta.progressToken for in-flight progress. legacy (2025-06-18 and earlier): the initialize handshake, logging/setLevel and log notifications all behave as they always did."
    },
    "note": {"type": "string"}
  }
}`

// raw compacts a schema literal, failing loudly at startup if it is not valid JSON (a malformed
// schema would otherwise reach a client as an uninterpretable tool declaration).
func raw(s string) json.RawMessage {
	if err := json.Unmarshal([]byte(s), new(any)); err != nil {
		panic("mcp: invalid schema literal: " + err.Error())
	}
	return json.RawMessage(s)
}

// instructions is the cross-tool contract carried in `initialize.instructions`. It is the one place
// a client model reads BEFORE choosing a tool, so it states the rules no per-tool description can
// enforce on its own — above all the one that matters here: this server contains a write primitive.
const instructions = `reviewmesh runs a governed, blind panel of independent models over an artifact and returns a HOST-ADJUDICATED result.

How the tools relate:
- review_report STARTS A REVIEW. It spawns model CLIs and SPENDS MONEY, and it changes no project content. Confirm with the user before repeating one.
- review_remediate acts on a completed review_report run's accepted findings. Pass fromRun (that run's runId) and the absolute workspace it reviewed. output=patch returns the complete diff on every server; output=apply writes the workspace, needs allowWrite: true, and works only when the operator launched this server with --allow-writes. review_doctor reports "writes": "aimesh" when aimesh can apply, or "agent" when you apply the diff yourself.
- Runs are JOB-SHAPED. If a run does not finish within waitSeconds you get {runId, state:"running"} — poll review_run_status, then fetch review_run_result. Do not re-issue the call: pass the same idempotencyKey and you get the existing run back rather than paying twice (and, for remediation, rather than writing twice).
- list and doctor are read-only reporting. Call list FIRST to see which adapters this server was launched with.

Composing a panel:
- Every review composes its own "panel": reviewers[] and author_remediator are required; cross_check and verifier run only when named, and a run without them lists them in skippedSteps. author_remediator is the seat whose adjudication becomes the accepted set, so it is never defaulted.
- Each seat names an adapter from list and the exact model identifier that adapter's CLI accepts. Look the identifier up for your adapter and the license it runs under; this server passes it through verbatim and does not validate it. Use verifyReadiness to check a model before a full run, and dryRun to price one.

Scope:
- workspace is an ABSOLUTE directory, and it is the call's root. Use roots to add other absolute directories the call reads, such as a project folder holding authority documents. No path may be the filesystem root, a home directory, a system tree or a protected directory, and every path must lie inside the operator's --root ceiling when one is set. Use inlineWorkspace to review content you supply in the call itself.

Rules that matter for how you report a result:
- AGREEMENT COUNTS ARE COMPUTED BY THE HOST over the blind seats. Report them as given. Never recount supportingSeats, and never restate a count in your own words.
- ALWAYS surface the governance block: the per-finding supportingSeats/agreementCount, the identityCaveats, the authority inclusion manifest, the withheld files, and the requested-vs-executed panel. A finding from an unverifiable model must not read like one from a verified model, and a file nobody was shown is not a file nobody objected to.
- A finding with applyable: false is REPORTABLE AND NEVER WRITABLE — it is supported only by weak-identity seats, or it traces only to authority text. Do not present it as a pending fix.
- The authority manifest is what the artifact was judged AGAINST. If an inclusion is incomplete, say so.

Reading a result on MCP 2026-07-28 and later:
- resultType, state and task are THREE DIFFERENT THINGS and two of them are called "complete". resultType describes the REQUEST (the protocol's own field: "complete" means the response carries its final content); state describes the RUN (running / complete / halted / cancelled, inside structuredContent); a task, where one is offered, is a HANDLE to a run and not a state of one. A tool result carrying isError: true is still resultType "complete" — the request completed, the tool failed.
- THIS SERVER EMITS NO PROTOCOL LOG NOTIFICATIONS on this revision. The Logging feature is deprecated as of 2026-07-28 and its named migration for stdio servers is stderr, which is where this server's diagnostics go; setting _meta.io.modelcontextprotocol/logLevel is accepted and ignored rather than rejected. In-flight visibility lives on _meta.progressToken (notifications/progress) and on review_run_status / review_run_result, which are responses. No governance-relevant fact has ever existed only in a log line.

What this server will not do:
- It never changes configuration, and it reads none: its adapters and grants come from its launch arguments, and a call can never introduce an adapter, a binary path or a launch argument.
- It reads only inside the paths a call declares, and never the filesystem root, a home directory, a system tree or a protected path.
- A failed run comes back as a tool result with isError set and a machine-readable {exitCode, haltClass, reasonCode} payload. Branch on reasonCode, not on the message text.`
