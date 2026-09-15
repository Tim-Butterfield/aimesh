package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
)

// This file holds the MCP server's tool input and output schemas and its initialize instructions. The
// schemas are literal JSON so that oneOf, additionalProperties and required sets are exact.
//
// Input schemas are strict: additionalProperties is false and required fields are enforced. The server
// enforces the same rules itself, since clients may not validate. Output schemas require the governance
// block, identity caveats and panel echo, but allow additional properties so clients tolerate new fields.

// MaxPanelExplorers is the maximum number of explorers in a panel. Larger requests are refused, not
// clamped.
const MaxPanelExplorers = 16

// MinPanelExplorers is the minimum number of explorers in a panel.
const MinPanelExplorers = 2

// Wait-budget bounds, in seconds. The default stays well under the common 60-second MCP request timeout.
const (
	DefaultWaitSeconds = 25
	MaxWaitSeconds     = 120
)

// slotSchema is one panel seat, identified by adapter name, model and effort. It has no path or argument
// properties.
const slotSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["adapter", "model"],
  "properties": {
    "adapter": {"type": "string", "description": "An adapter this server was launched with, as reported by the list tool. Never a path or a binary name."},
    "model": {"type": "string", "description": "The exact model identifier this adapter's CLI accepts. Look it up for your adapter and the license it runs under; this server passes it through verbatim and does not validate it."},
    "effort": {"type": "string", "description": "Optional reasoning-effort label; a different effort is a genuinely different vantage, so it is part of the seat's identity."}
  }
}`

// panelSchema is the panel a call composes from the server's launch adapters: explorers and a collator.
var panelSchema = fmt.Sprintf(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["explorers", "collator"],
  "description": "Which explorers run. Required: every exploration composes its own panel from the adapters in the list tool.",
  "properties": {
    "explorers": {"type": "array", "minItems": %d, "maxItems": %d, "items": %s, "description": "The ordered explorer seats. Duplicated (adapter, model, effort) triples are rejected: an identical triple adds no independent vantage."},
    "collator": %s
  }
}`, MinPanelExplorers, MaxPanelExplorers, slotSchema, slotSchema)

// canonicalizersSchema is the optional explicit canonicalizer pair: exactly two seats (see
// roster.ValidateCanonicalizers).
var canonicalizersSchema = fmt.Sprintf(`{
  "type": "array",
  "minItems": 2,
  "maxItems": 2,
  "items": %s,
  "description": "The two identities that propose the canonicalization for a canonicalizing mode. A merge is HELD only if both propose it; a merge only one proposes is contested and resolved by splitting. Omit this entirely and the host derives them — slot a from the collator, slot b from the first explorer in the profile's preference order whose model differs from the collator's. Supply exactly two or none: one entry is refused. Compose-not-configure applies, as it does to panel seats."
}`, slotSchema)

// commonProps are the parameters every run-starting tool takes.
var commonProps = fmt.Sprintf(`
    "purpose": {"type": "string", "minLength": 1, "description": "What the panel is being asked to explore."},
    "criteria": {"type": "array", "minItems": 1, "items": {"type": "string", "minLength": 1}, "description": "The load-bearing constraints the response must satisfy. Never invented on your behalf: a run without criteria is refused."},
    "priorContext": {"type": "string", "description": "Optional prior context (e.g. an earlier exploration's result)."},
    "panel": %s,
    "canonicalizers": %s,
    "waitSeconds": {"type": "integer", "minimum": 1, "maximum": %d, "default": %d, "description": "How long to wait inline for the run to finish. If it finishes in time you get the full result; otherwise you get {runId, state:\"running\"} and poll explore_run_status / fetch explore_run_result."},
    "maxParallel": {"type": "integer", "minimum": 1, "description": "How many explorers may invoke their model CLI AT ONCE. Omitted: the whole panel runs in parallel. Lower it when the machine cannot host that many provider CLIs at once (each is a real subprocess; a local model also loads weights) or to stay under a provider rate limit. It bounds PARALLELISM only — every explorer in the panel still answers, so the result is unchanged and only the wall clock moves."},
    "dryRun": {"type": "boolean", "description": "Resolve everything and spend NOTHING. The panel convenes nobody: the task contract, the round contract, the terminal contract and the canonicalizer derivation all run, then the call stops before the identity pre-flight — an exploration's FIRST model call — and answers with 'dryRun': true and a 'shape' block: the panel, every stage that would be called, the exact model-call total, and the exact round-1 prompt every explorer would receive. Every configuration error a real run would raise is raised here too, for free, including a dual-canonicalizer plan that cannot yield two independent identities. Two things it does NOT do: it makes no claim that any adapter is reachable (proving that means calling it, which is what the pre-flight is), and it writes no run directory (there is no exploration to record)."},
    "verifyReadiness": {"type": "boolean", "description": "Before the run, ask every distinct adapter/model/effort the panel names whether it can do real work — one bounded one-token call each, which SPENDS. An agent blocked on login, folder trust or an identifier its license cannot use halts the run before the panel is paid for. With dryRun the calls are priced in shape and none is made."},
    "idempotencyKey": {"type": "string", "maxLength": 200, "description": "Optional caller-supplied key. Repeating a call with the same key returns the EXISTING run instead of spending again — use it when retrying."}`,
	panelSchema, canonicalizersSchema, MaxWaitSeconds, DefaultWaitSeconds)

// exploreInputSchema is the input schema of the explore tool, which starts a run in any mode. mode is
// required so the same call means the same thing on every install.
//
// Mode-specific parameters are described in three places: the oneOf branches here (see modeBranches),
// exploreToolDescription, and the agent guide. Clients handle conditional schemas inconsistently, so the
// prose copies make sure the model sees the rules; the handler enforces them.
var exploreInputSchema = fmt.Sprintf(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["purpose", "criteria", "mode", "panel"],
  "properties": {%s,
    "mode": {"type": "string", "enum": [%s], "description": "The exploration mode, and the thing that decides which other parameters are required. map: collate-only synthesis. synthesize: one composed best answer. catalog: enumerate + canonicalize. shortlist: host-tallied ballot over a confirmed candidate universe. ai-collab: mutual challenge between the explorers. challenge: blindly attack a supplied artifact — REQUIRES artifact. compare: evaluate a declared option set on declared axes — REQUIRES options + comparisonAxes. forecast: pool independent numeric estimates — REQUIRES target + unit + horizon. Supplying a parameter that belongs to a DIFFERENT mode is refused, never ignored."},
    "artifact": {"type": "string", "minLength": 1, "description": "REQUIRED by mode=challenge, and rejected for any other mode. The artifact under review — the text the panel attacks. Supplied inline; this server reads no files."},
    "options": {"type": "array", "minItems": 2, "uniqueItems": true, "items": {"type": "string", "minLength": 1}, "description": "REQUIRED by mode=compare, and rejected for any other mode. The DECLARED option set. Every explorer evaluates exactly these; nothing is added or dropped mid-run."},
    "comparisonAxes": {
      "type": "array", "minItems": 1,
      "description": "REQUIRED by mode=compare, and rejected for any other mode. The axes the options are measured on. Deliberately NOT named 'criteria': criteria are the constraints the whole exploration must satisfy, while these are the measured dimensions and pass/fail gates. Two same-named arrays with different meanings generate first-try errors.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name"],
        "properties": {
          "name": {"type": "string", "minLength": 1},
          "direction": {"type": "string", "enum": ["higher_is_better", "lower_is_better"], "description": "Required for a scored dimension (the host's dominance rule reads it); a pass/fail filter has no scale."},
          "role": {"type": "string", "enum": ["dimension", "filter"], "default": "dimension", "description": "A scored axis, or a pass/fail gate. A gate must be declared explicitly — promoting an axis to a requirement would exclude options nobody asked to exclude."},
          "weight": {"type": "number", "minimum": 0, "description": "A weight on EVERY scored axis is what licenses a scalar ranking. Without weights the result is the Pareto/trade-off view and says so."}
        }
      }
    },
    "target": {"type": "string", "minLength": 1, "description": "REQUIRED by mode=forecast, and rejected for any other mode. What is being estimated."},
    "unit": {"type": "string", "minLength": 1, "description": "REQUIRED by mode=forecast, and rejected for any other mode. The unit every estimate must be given in."},
    "horizon": {"type": "string", "minLength": 1, "description": "REQUIRED by mode=forecast, and rejected for any other mode. The period the estimate is for."},
    "conditioningEvent": {"type": "string", "description": "Optional, and only for mode=forecast: an \"assume this holds\" clause."}
  },
  "oneOf": [
    %s
  ]
}`, commonProps, quotedList(allModes), modeBranches())

const runIDInputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["runId"],
  "properties": {"runId": {"type": "string", "minLength": 1, "description": "A run id returned by one of the explore tools."}}
}`

const emptyInputSchema = `{"type": "object", "additionalProperties": false, "properties": {}}`

// governanceSchema is the governance block required on every complete result. countsEmitted is false for
// modes that produce no host-computed counts.
const governanceSchema = `{
    "type": "object",
    "required": ["countsEmitted"],
    "description": "The host-computed governance record. ALWAYS present. Counts, rankings and pooled numbers here are computed by the host over the blind responses — never asserted by a model. Surface it; never restate a count in your own words.",
    "properties": {
      "countsEmitted": {"type": "boolean"},
      "summary": {"type": "string"},
      "claims": {"type": "integer"},
      "claimsByLabel": {"type": "object"},
      "claimsWithheld": {"type": "integer", "description": "Claims the host declined to state definitively. Reporting a claim count without this beside it presents a contested result as a settled one."},
      "claimsContested": {"type": "integer"},
      "claimsHash": {"type": "string"},
      "countingPolicyHash": {"type": "string", "description": "Hash of the counting policy, frozen BEFORE any judgment was solicited."},
      "panelSize": {"type": "integer"},
      "respondents": {"type": "integer"},
      "quorumMet": {"type": "boolean"}
    }
  }`

const identityCaveatsSchema = `{
    "type": "array",
    "description": "Every seat whose model identity could NOT be verified, with the evidence tier behind it. ALWAYS present; an empty array is the positive statement that every seat was verified. Every listed seat PARTICIPATED FULLY — identity is recorded, never used to include or exclude a response, because an adapter can be asked to use a model but cannot be made to prove it did. Report these to the reader; do not down-weight or discard the findings they contributed to.",
    "items": {
      "type": "object",
      "required": ["role", "identityStatus"],
      "properties": {
        "role": {"type": "string", "enum": ["explorer", "collator", "canonicalizer"]},
        "adapter": {"type": "string"},
        "model": {"type": "string"},
        "effort": {"type": "string"},
        "identityStatus": {"type": "string", "enum": ["self_reported", "unknown", "mismatch"]},
        "identityEvidence": {"type": "string"},
        "caveat": {"type": "string"}
      }
    }
  }`

const panelEchoSchema = `{
    "type": "object",
    "required": ["requested", "executed"],
    "description": "The panel as REQUESTED and as EXECUTED. ALWAYS present, and always both halves: a seat that was dropped is only visible by comparing them.",
    "properties": {
      "requested": {
        "type": "object",
        "required": ["source"],
        "properties": {
          "source": {"type": "string", "enum": ["adhoc"]},
          "explorers": {"type": "array", "items": {"type": "object"}},
          "collator": {"type": "object"},
          "canonicalizers": {"type": "array", "items": {"type": "object"}, "description": "Present only when the call named them."}
        }
      },
      "executed": {
        "type": "object",
        "required": ["explorers", "collator", "selected", "configured", "canonicalizerSource"],
        "properties": {
          "explorers": {"type": "array", "items": {"type": "object"}},
          "collator": {"type": "object"},
          "canonicalizers": {"type": "array", "items": {"type": "object"}, "description": "The identities that proposed the canonicalization. Empty when the host derived them — canonicalizerSource says which."},
          "canonicalizerSource": {"type": "string", "enum": ["explicit", "derived"], "description": "WHO CHOSE the canonicalizers: explicit (the call or the profile named them) or derived (the host picked them — slot a the collator, slot b the first explorer by preference order that differs from it). ALWAYS present: two independent canonicalizers decide which merges hold, so which merges hold depends on who chose them, and a result that reported the identities without their provenance left that unanswerable."},
        "canonicalizerIndependence": {"type": "string", "enum": ["distinct_models", "shared_model"], "description": "WHAT THAT CHOICE BOUGHT, on a run that used two canonicalizers. distinct_models: the pair runs two different models, which is the evidence the merge-agreement rule was designed around. shared_model: both run the SAME model through different adapters — allowed, because a panel is configured deliberately, but every corroboration count in this result is then weaker evidence, since the two proposers share priors and their errors correlate. Report it alongside any count you surface; absent on a single-canonicalizer or non-canonicalizing mode."},
          "selected": {"type": "integer"},
          "configured": {"type": "integer"},
          "dropped": {"type": "integer"}
        }
      }
    }
  }`

// haltProps are the halt fields carried in structuredContent on an isError result. A JSON-RPC error is not
// used because clients often drop its data.
const haltProps = `
      "exitCode": {"type": "integer", "description": "The shared aimesh halt taxonomy exit code (3 config, 4 adapter, 5 model/identity, 7 policy/cap, 8 internal)."},
      "haltClass": {"type": "string"},
      "reasonCode": {"type": "string", "description": "The stable machine reason code. Branch on this, never on the message."},
      "failure": {"type": "object", "description": "The sanitized, capped breakdown: which seats were dropped and why."}`

// runResultSchema is the output schema of explore and explore_run_result: a oneOf with one branch per
// state (running, complete, halted, cancelled), each with a const state so exactly one branch matches.
// Governance and identity caveats are required only on complete; the panel echo is also required on running
// and cancelled. Review's result schema uses the same four states.
var runResultSchema = fmt.Sprintf(`{
  "oneOf": [
    {
      "type": "object",
      "required": ["runId", "state", "panel"],
      "description": "The run outlived the inline wait budget. Poll explore_run_status, then fetch explore_run_result.",
      "properties": {
        "runId": {"type": "string"},
        "state": {"const": "running"},
        "tool": {"type": "string"},
        "mode": {"type": "string"},
        "waitedSeconds": {"type": "integer"},
        "panel": %s
      }
    },
    {
      "type": "object",
      "required": ["runId", "state", "mode", "panel", "governance", "identityCaveats"],
      "properties": {
        "runId": {"type": "string"},
        "state": {"const": "complete"},
        "tool": {"type": "string"},
        "mode": {"type": "string"},
        "purpose": {"type": "string"},
        "criteria": {"type": "array", "items": {"type": "string"}},
        "priorContextSupplied": {"type": "boolean"},
        "captured": {"type": "boolean", "description": "Whether the run directory was written (the audit record for this run id)."},
        "panel": %s,
        "governance": %s,
        "identityCaveats": %s,
        "result": {"type": "object", "description": "The mode's own terminal detail."},
        "dryRun": {"type": "boolean", "description": "Present and true ONLY when the call asked for dryRun. The CALL completed; the exploration did not happen. Do not read the empty result as a finding: nothing was explored because nothing was called. It is exploremesh's form of reviewmesh's status 'planned', and it always travels with 'shape'."},
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
        "tool": {"type": "string"},
        "mode": {"type": "string"},
        "captured": {"type": "boolean"},
        "panel": %s,
        "identityCaveats": %s,%s
      }
    },
    {
      "type": "object",
      "required": ["runId", "state", "haltClass", "reasonCode", "panel"],
      "description": "The CALLER cancelled (or the session went away) and the run was killed. Nothing failed, and there is no result — but the panel echo and the taxonomy are still owed, because a cancelled exploration still spent whatever it spent before the cancel.",
      "properties": {
        "runId": {"type": "string"},
        "state": {"const": "cancelled"},
        "tool": {"type": "string"},
        "mode": {"type": "string"},
        "captured": {"type": "boolean"},
        "waitedSeconds": {"type": "integer"},
        "panel": %s,
        "identityCaveats": %s,%s
      }
    }
  ]
}`, panelEchoSchema, panelEchoSchema, governanceSchema, identityCaveatsSchema, runShapeSchema,
	panelEchoSchema, identityCaveatsSchema, haltProps,
	panelEchoSchema, identityCaveatsSchema, haltProps)

// runShapeSchema describes a dry run's shape block. modelCalls is a single exact number because the mode
// fixes the round count; only a halt lowers the real total.
const runShapeSchema = `{
    "type": "object",
    "required": ["mode", "explorers", "collator", "rounds", "policy", "modelCalls", "calls", "payload"],
    "description": "What this exploration WOULD do. Present only on a dryRun call. Every identity here RESOLVED, and no adapter was contacted: reachability is what the identity pre-flight establishes, and the dry run stops in front of it.",
    "properties": {
      "mode": {"type": "string"},
      "explorers": {"type": "array", "items": {"type": "object"}, "description": "The panel in attribution order. Every one is called in every round."},
      "collator": {"type": "object"},
      "canonicalizers": {"type": "array", "items": {"type": "object"}, "description": "The resolved canonicalizer roles (two on the dual path). Resolving them is what makes a dry run catch a panel that cannot supply two independent identities."},
      "canonicalizerProvenance": {"type": "string", "enum": ["explicit", "derived"]},
      "canonicalizerIndependence": {"type": "string", "enum": ["distinct_models", "shared_model"], "description": "shared_model means both canonicalizers would run the SAME model behind different adapters, making every corroboration count the run produces weaker evidence. Surfaced here because this is the moment it can still be changed for free."},
      "rounds": {"type": "integer", "description": "Fixed by the mode contract."},
      "policy": {"type": "object", "description": "The mode's governance grade: dual, confirm, ballot, and which terminal contract it declares. These flags decide most of what the run costs."},
      "maxParallel": {"type": "integer", "description": "Bounds wall clock, never cost."},
      "modelCalls": {"type": "integer", "description": "The EXACT total for a run that completes — not a range. Report it as given."},
      "calls": {"type": "array", "items": {"type": "object"}, "description": "The per-stage breakdown in execution order, including stages that cost nothing (a terminal collation the host composes in-process is listed with 0 calls, so it is visibly free rather than apparently forgotten)."},
      "payload": {"type": "object", "description": "What the blind round would carry: the exact prompt bytes every explorer receives, its length, the payload hash the run will record, and the response fields the app-owned schema requires. Rounds 2+ carry material derived from round 1 and cannot be shown — it does not exist yet."}
    }
  }`

var runStatusSchema = fmt.Sprintf(`{
  "type": "object",
  "required": ["runId", "state"],
  "properties": {
    "runId": {"type": "string"},
    "state": {"type": "string", "enum": ["running", "complete", "halted", "cancelled"]},
    "tool": {"type": "string"},
    "mode": {"type": "string"},
    "elapsedSeconds": {"type": "number"},
    "captured": {"type": "boolean"},
    "panel": %s
  }
}`, panelEchoSchema)

// listOutputSchema is the output schema of the list tool. It omits binary paths, launch arguments and
// environment, which would disclose the operator's setup to the client model.
const listOutputSchema = `{
  "type": "object",
  "required": ["adapters", "modes", "limits"],
  "properties": {
    "adapters": {
      "type": "array",
      "description": "The adapters this server was launched with. A panel seat may name only these. An adapter with available: false cannot be used until its CLI can be started; 'reason' says why.",
      "items": {
        "type": "object",
        "required": ["name", "kind", "available"],
        "properties": {
          "name": {"type": "string"},
          "kind": {"type": "string", "enum": ["shell", "fake"]},
          "available": {"type": "boolean"},
          "reason": {"type": "string"},
          "source": {"type": "string", "enum": ["flag", "env"], "description": "Where the operator named the adapter."},
          "identityEvidenceCapability": {"type": "string", "description": "The adapter's DECLARED evidence tier — not a live verdict. Proving a model's identity still takes a real call."}
        }
      }
    },
    "modes": {"type": "array", "items": {"type": "string"}},
    "limits": {"type": "object", "description": "The admission limits in force on this server."},
    "note": {"type": "string"}
  }
}`

const doctorOutputSchema = `{
  "type": "object",
  "required": ["ok", "checks", "protocolMode", "protocolEra"],
  "properties": {
    "ok": {"type": "boolean"},
    "checks": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["name", "ok"],
        "properties": {"name": {"type": "string"}, "ok": {"type": "boolean"}, "detail": {"type": "string"}}
      }
    },
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

// exploreToolDescription is the explore tool's description. A test checks that it states every rule in
// modeInputRules.
const exploreToolDescription = "Runs a blind panel of independent models over one question and returns a HOST-COMPUTED result. " +
	"SPENDS MONEY: it launches the configured model CLIs. " +
	"`mode` selects what the run means AND decides which other parameters are required: " +
	"map (collate-only synthesis), synthesize (one composed best answer), catalog (enumerate + canonicalize), " +
	"shortlist (a host-tallied ballot) and ai-collab (mutual challenge) need nothing beyond purpose + criteria; " +
	"challenge REQUIRES `artifact`; compare REQUIRES `options` + `comparisonAxes`; forecast REQUIRES `target` + `unit` + `horizon` " +
	"(with `conditioningEvent` optional). " +
	"A parameter belonging to a different mode is REFUSED, not ignored — an artifact passed to a map run fails rather than being dropped. " +
	"Job-shaped: if the run outlives waitSeconds you get a runId to poll with explore_run_status."

// uniformModes are the modes that take no parameters beyond the common ones.
var uniformModes = []string{mode.Map, mode.Synthesize, mode.Catalog, mode.Shortlist, mode.AICollab}

// allModes lists every mode the explore tool accepts, uniform modes first.
var allModes = append(append([]string{}, uniformModes...), mode.Challenge, mode.Compare, mode.Forecast)

// modeSpecificProps are the parameters that belong to only some modes.
var modeSpecificProps = []string{"artifact", "options", "comparisonAxes", "target", "unit", "horizon", "conditioningEvent"}

// modeInputRules defines which mode-specific parameters each mode requires or permits. Both the schema and
// the handler use it. A mode forbids every mode-specific parameter it neither requires nor permits, so a
// misplaced parameter is refused instead of silently ignored.
var modeInputRules = map[string]struct {
	requires []string
	permits  []string // allowed but not required
}{
	mode.Challenge: {requires: []string{"artifact"}},
	mode.Compare:   {requires: []string{"options", "comparisonAxes"}},
	mode.Forecast:  {requires: []string{"target", "unit", "horizon"}, permits: []string{"conditioningEvent"}},
}

// allowedFor returns the mode-specific parameters mode m requires or permits.
func allowedFor(m string) map[string]bool {
	out := map[string]bool{}
	r, ok := modeInputRules[m]
	if !ok {
		return out
	}
	for _, p := range r.requires {
		out[p] = true
	}
	for _, p := range r.permits {
		out[p] = true
	}
	return out
}

// modeBranches returns the oneOf branches for exploreInputSchema, generated from modeInputRules. Each branch
// fixes mode to a const, requires that mode's parameters and forbids the other mode-specific ones.
//
// It uses oneOf, not and anyOf rather than if/then because meshcore/jsonschema does not implement if/then,
// and vocabulary_test.go fails on keywords that validator would ignore.
func modeBranches() string {
	branches := make([]string, 0, len(allModes))
	for _, m := range allModes {
		allowed := allowedFor(m)

		required := append([]string{"mode"}, modeInputRules[m].requires...)
		branch := fmt.Sprintf(`"required": [%s], "properties": {"mode": {"const": %q}}`, quotedList(required), m)

		// Forbid the other parameters with not: {anyOf: [{required: [p]}, ...]}.
		forbidden := make([]string, 0, len(modeSpecificProps))
		for _, p := range modeSpecificProps {
			if !allowed[p] {
				forbidden = append(forbidden, fmt.Sprintf(`{"required": [%q]}`, p))
			}
		}
		if len(forbidden) > 0 {
			branch += fmt.Sprintf(`, "not": {"anyOf": [%s]}`, strings.Join(forbidden, ", "))
		}
		branches = append(branches, "{"+branch+"}")
	}
	return strings.Join(branches, ",\n    ")
}

func quotedList(ss []string) string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, `"`+s+`"`)
	}
	return strings.Join(out, ", ")
}

// raw returns s as a json.RawMessage, panicking if it is not valid JSON.
func raw(s string) json.RawMessage {
	var buf json.RawMessage
	if err := json.Unmarshal([]byte(s), new(any)); err != nil {
		panic("mcp: invalid schema literal: " + err.Error())
	}
	buf = json.RawMessage(s)
	return buf
}

// instructions is the server's initialize instructions, which describe how the tools fit together and how
// to report results.
const instructions = `exploremesh runs a blind panel of independent models over one question and returns a HOST-COMPUTED result.

How the tools relate:
- explore STARTS A RUN. It spawns model CLIs and SPENDS MONEY. Confirm with the user before repeating one.
- explore takes a REQUIRED mode, and the mode decides which other parameters are required:
    map / synthesize / catalog / shortlist / ai-collab  — nothing beyond purpose + criteria
    challenge                                           — also artifact
    compare                                             — also options + comparisonAxes
    forecast                                            — also target + unit + horizon (conditioningEvent optional)
  A parameter belonging to a different mode is REFUSED, not ignored: passing an artifact to a map
  run fails rather than quietly running a map that never saw it.
- Runs are JOB-SHAPED. If a run does not finish within waitSeconds you get {runId, state:"running"} — poll explore_run_status, then fetch explore_run_result. Do not re-issue the explore call: pass the same idempotencyKey and you will get the existing run back rather than paying twice.
- list and doctor are read-only reporting. Call list FIRST to see which adapters this server was launched with.

Composing a panel:
- Every explore call composes its own "panel": at least two explorers and a collator. Each seat names an adapter from list and the exact model identifier that adapter's CLI accepts. Look the identifier up for your adapter and the license it runs under; this server passes it through verbatim and does not validate it.
- Use verifyReadiness to check that the panel's agents can do real work before a full run, and dryRun to price one.

Rules that matter for how you report a result:
- COUNTS, RANKINGS AND POOLED NUMBERS ARE COMPUTED BY THE HOST over the blind responses. Report them as given. Never recompute, re-rank or restate them in your own words.
- ALWAYS surface the governance block and the identity caveats. A claim count without its withheld tally presents a contested result as a settled one; a synthesis from an unverifiable model must not read like one from a verified model.
- A shortlist is a BALLOT, not a consensus. A Pareto frontier has no winner. A forecast aggregate is host-pooled, not a model's answer. The result says which; say the same.
- The panel echo carries what was REQUESTED and what was EXECUTED. If they differ, say so.

Reading a result on MCP 2026-07-28 and later:
- resultType, state and task are THREE DIFFERENT THINGS and two of them are called "complete". resultType describes the REQUEST (the protocol's own field: "complete" means the response carries its final content); state describes the RUN (running / complete / halted / cancelled, inside structuredContent); a task, where one is offered, is a HANDLE to a run and not a state of one. A tool result carrying isError: true is still resultType "complete" — the request completed, the tool failed.
- THIS SERVER EMITS NO PROTOCOL LOG NOTIFICATIONS on this revision. The Logging feature is deprecated as of 2026-07-28 and its named migration for stdio servers is stderr, which is where this server's diagnostics go; setting _meta.io.modelcontextprotocol/logLevel is accepted and ignored rather than rejected. In-flight visibility lives on _meta.progressToken (notifications/progress) and on explore_run_status / explore_run_result, which are responses.

What this server will not do:
- It never changes configuration, and it reads none: its adapters come from its launch arguments, and a call can never introduce an adapter, a binary path or a launch argument.
- It reads no files. An artifact under review is supplied inline.
- A failed run comes back as a tool result with isError set and a machine-readable {exitCode, haltClass, reasonCode} payload. Read reasonCode, not the message text.`
