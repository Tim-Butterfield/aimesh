package mcp

import (
	"encoding/json"
	"strings"
	"time"
)

// This file is the `io.modelcontextprotocol/tasks` EXTENSION: `resultType: "task"` on `tools/call`,
// and the three methods `tasks/get`, `tasks/update`, `tasks/cancel`.
//
// It is adopted under product decision D7 with the extension's EXPERIMENTAL status as an accepted,
// recorded risk (migration design §12.2). Two things bound that risk and they are both structural
// rather than aspirational:
//
//  1. THIS PACKAGE MINTS NO IDENTIFIER AND OWNS NO STATE. A task is a PROJECTION over run state the
//     application already keeps: `taskId == runId`, always. There is no second execution model here,
//     no task table, and no lifecycle this file drives. If the extension is withdrawn we delete a
//     projection; nothing underneath it moves.
//  2. THE JOB SHAPE STAYS. An application's `waitSeconds` → run id → poll surface is untouched, and
//     it is what a client that does NOT declare the extension keeps getting. That is OUR DECISION,
//     not a specification requirement — but it is
//     what makes the extension additive: a non-declaring client's transcript is unchanged except for the
//     one line `server/discover` gains.
//
// THE ONE RULE THAT CANNOT BE GOT WRONG, quoted verbatim from `extensions/tasks/overview`
// (fetched 2026-08-04): "Before returning a `CreateTaskResult`, verify that the client included the
// extension in its per-request capabilities. Never return a task to a client that did not declare
// support." It is enforced in exactly one place — Call.CreateTask — which returns false rather than
// marking anything, so a handler that ignores the answer still cannot produce a task.

// ExtensionTasks is the extension's identifier, in the `extensions` field of both the client's
// per-request capabilities and this server's `server/discover` capabilities.
//
// `basic/versioning` §Extension Negotiation: "Extensions are advertised in the `extensions` field of
// capabilities". The settings object is empty on both sides; the extension defines none.
const ExtensionTasks = "io.modelcontextprotocol/tasks"

// The five task statuses, quoted from the extension's `Task` interface (fetched 2026-08-04):
//
//	status: "working" | "input_required" | "completed" | "cancelled" | "failed";
//
// `completed`, `failed` and `cancelled` are terminal.
const (
	// TaskWorking — the operation is in progress.
	TaskWorking = "working"
	// TaskInputRequired is NEVER EMITTED by this transport: we issue no mid-flight request, so
	// there is nothing for a client to respond to. It is declared so the status set is complete
	// and so `tasks/update`'s contract has something to be vacuous about.
	TaskInputRequired = "input_required"
	// TaskCompleted — the request finished. It covers a DOMAIN FAILURE too, and that is the
	// extension's own rule rather than our convenience. Verbatim (fetched 2026-08-04): "The
	// `failed` status **MUST NOT** represent non-JSON-RPC errors like tool results with
	// `isError: true`. Errors within protocol method results **MUST** use `completed` status with
	// error details in the `result` field". So a governed HALT maps here, with `isError: true`
	// riding the embedded result — never to `failed`.
	TaskCompleted = "completed"
	// TaskFailed is for a JSON-RPC PROTOCOL error only.
	TaskFailed = "failed"
	// TaskCancelled — the operation was cancelled. See TaskProvider.CancelTask for what our
	// acknowledgement does and does not promise.
	TaskCancelled = "cancelled"
)

// maxStatusMessage bounds `statusMessage`, which the extension says "may be exposed to the end-user
// or model". A run's terminal rendering can be thousands of characters; a status line is a status
// line, and the full text is one `tasks/get` away in `result`.
const maxStatusMessage = 240

// TaskView is ONE task as the application projects it. Every field is the application's answer:
// this package formats, it does not decide.
type TaskView struct {
	// Status is one of the five constants above.
	Status string
	// StatusMessage is optional human/model-facing context. It is capped when rendered.
	StatusMessage string
	// CreatedAt / LastUpdatedAt are the task's timestamps, rendered as RFC 3339.
	CreatedAt     time.Time
	LastUpdatedAt time.Time
	// TTLMs is the extension's `ttlMs`: "Time-to-live duration from creation in integer
	// milliseconds, null for unlimited. The server may discard the task after the TTL elapses.
	// This value **MAY** change over the lifetime of a task." (fetched 2026-08-04)
	//
	// IT IS NOT A CACHING HINT. `tasks/get` is not in `server/utilities/caching`'s closed cacheable
	// set, so no hint is owed on it at all, and a `ttlMs: 0` here would declare a task with a
	// zero-millisecond lifetime — a handle we told the client to throw away. nil means unlimited
	// and marshals to JSON `null`, which is the honest value for a run that is still live.
	TTLMs *int64
	// PollIntervalMs is the extension's suggested polling interval. 0 omits the field.
	PollIntervalMs int
	// Result is what the ORIGINAL request would have returned synchronously. It is set only on
	// TaskCompleted, and it carries `isError` unchanged — that is how a domain halt travels.
	Result *CallToolResult
	// Err is the JSON-RPC error a TaskFailed task carries. Set only on TaskFailed.
	Err *RequestError
}

// TaskProvider is the seam between this transport and an application's run registry. It is
// deliberately tiny: three verbs, no lifecycle, no minting.
//
// A nil provider means this server has no tasks. It then advertises no extension, implements none of
// the three methods, and Call.CreateTask always answers false — so "the extension is off" is one
// condition with one consequence rather than three switches that can disagree.
type TaskProvider interface {
	// Task projects one task. ok=false means the id is unknown OR EXPIRED, which the extension
	// treats identically: "It is compliant behavior for a server to return an error stating the
	// task cannot be found if it has purged an expired task." (fetched 2026-08-04)
	Task(taskID string) (TaskView, bool)
	// CancelTask delivers the cancel signal for a task and reports whether the id was known.
	//
	// WHAT AN ACKNOWLEDGEMENT MEANS, AND ONLY THIS: the signal has been delivered to the run's
	// context. It is NOT a promise about the terminal status. The extension's own floor is
	// cooperative — "Honor them when possible, but cancellation is cooperative — the task may
	// still reach a non-`cancelled` terminal status" (fetched 2026-08-04) — and an application
	// that guarantees more (reviewmesh guarantees no byte reaches the workspace once cancellation
	// wins the write window) guarantees it about WRITES, not about the status word.
	CancelTask(taskID string) bool
}

// clientExtensions parses the `extensions` object out of a client's per-request capabilities.
//
// It is best-effort by design: capabilities we cannot read declare nothing, which is the
// fail-closed answer — an unparseable capability object must never be read as "the client opted in".
func clientExtensions(caps json.RawMessage) map[string]json.RawMessage {
	if len(caps) == 0 {
		return nil
	}
	var c struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(caps, &c) != nil {
		return nil
	}
	return c.Extensions
}

// DeclaredExtension reports whether the client declared a named extension on THIS request.
//
// It is per-request because the extension is: `basic/index` carries capabilities in every request's
// `_meta`, and a session-scoped answer would be exactly the prior-connection-state the modern
// revision deleted. Under the legacy era it is always false — a 2025-06-18 client cannot declare a
// 2026-07-28 extension, and pretending otherwise would let a legacy transcript grow a task.
func (e *RequestEnv) DeclaredExtension(name string) bool {
	if e == nil || e.Era != EraModern {
		return false
	}
	_, ok := e.Extensions[name]
	return ok
}

// CreateTask marks this call as TASK-AUGMENTED and reports whether the mark took.
//
// It answers false, and marks nothing, when any of three things is true: this server has no task
// provider, the request is not modern, or the client did not declare the extension. That is the
// enforcement point for the extension's "Never return a task to a client that did not declare
// support" — a handler that ignored the boolean would produce a nil result, not a task.
//
// The handler's contract is therefore: `if c.CreateTask(runID) { return nil, nil }`, and otherwise
// fall through to the job-shaped result it would have produced anyway.
func (c *Call) CreateTask(taskID string) bool {
	if c == nil || c.srv == nil || c.srv.Tasks == nil {
		return false
	}
	if strings.TrimSpace(taskID) == "" {
		return false
	}
	if !c.Env().DeclaredExtension(ExtensionTasks) {
		return false
	}
	c.taskID = taskID
	return true
}

// taskHandle is the id a handler marked with CreateTask ("" when it marked none).
func (c *Call) taskHandle() string {
	if c == nil {
		return ""
	}
	return c.taskID
}

// taskFields renders the `Task` interface's fields. It is shared by CreateTaskResult and
// GetTaskResult because they are the same object with a different envelope around it — which is
// what `type CreateTaskResult = Result & Task` and `type GetTaskResult = Result & DetailedTask` say.
func taskFields(taskID string, v TaskView) map[string]any {
	created, updated := v.CreatedAt, v.LastUpdatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if updated.IsZero() {
		updated = created
	}
	m := map[string]any{
		"taskId":        taskID,
		"status":        v.Status,
		"createdAt":     created.UTC().Format(time.RFC3339Nano),
		"lastUpdatedAt": updated.UTC().Format(time.RFC3339Nano),
	}
	// `ttlMs` is REQUIRED and nullable, so nil is emitted as an explicit null rather than omitted:
	// "null for unlimited" is a value, and an absent field is not it.
	if v.TTLMs == nil {
		m["ttlMs"] = nil
	} else {
		m["ttlMs"] = *v.TTLMs
	}
	if msg := strings.TrimSpace(v.StatusMessage); msg != "" {
		if len(msg) > maxStatusMessage {
			msg = msg[:maxStatusMessage-1] + "…"
		}
		m["statusMessage"] = msg
	}
	if v.PollIntervalMs > 0 {
		m["pollIntervalMs"] = v.PollIntervalMs
	}
	return m
}

// embeddedResult is what the original `tools/call` WOULD have returned synchronously, built with the
// same era envelope it would have carried — `resultType: "complete"` and `_meta.serverInfo`.
//
// It is built from a synthetic `tools/call` env rather than from the polling request's env, because
// the question the field answers is about the ORIGINAL request. `tools/call` is not a cacheable
// operation, so no caching hint is owed and none is produced.
func (s *Server) embeddedResult(env *RequestEnv, res *CallToolResult) any {
	if res == nil {
		res = ErrorResult("the task produced no result", nil)
	}
	if res.Content == nil {
		res.Content = []Content{}
	}
	version := ProtocolVersion20260728
	if env != nil && env.Version != "" {
		version = env.Version
	}
	return s.result(&RequestEnv{Method: "tools/call", Era: EraModern, Version: version}, res)
}

// createTaskResponse is the answer to a `tools/call` a handler chose to make task-augmented.
//
// `CreateTaskResult` is `Result & Task`, and the extension MUSTs `resultType: "task"` on it and
// MUST NOT allow that value anywhere else (both verified 2026-08-04). It also MUSTs that the task be
// resolvable before this frame is written: "A server **MUST NOT** return `CreateTaskResult` until the
// task is durably created — that is, until a `tasks/get` for the returned `taskId` would resolve."
// We meet the rule AS STATED by reading the projection back here, before answering — if it does not
// resolve, no handle is returned at all.
//
// We do NOT meet the rationale the overview gives for that rule ("Crash resilience. A task ID is a
// durable handle."). Our registries are in memory, so a `taskId` is durable for the life of the
// SERVER PROCESS and not across a restart of it. That is recorded here, in docs/mcp.md and in the
// design (§12.3) rather than quietly enjoyed.
func (s *Server) createTaskResponse(id json.RawMessage, env *RequestEnv, taskID string) *rpcResponse {
	v, ok := s.Tasks.Task(taskID)
	if !ok {
		// Unreachable through any handler that marked a live run, and a loud internal error rather
		// than a handle to nothing: a `CreateTaskResult` whose `tasks/get` would 404 is precisely
		// what the durable-creation MUST forbids.
		return errResp(id, CodeInternal, "internal error: a task handle was created for a run this server cannot project")
	}
	return okResp(id, s.resultAs(env, ResultTypeTask, taskFields(taskID, v)))
}

// taskIDParams is the params shape of all three task methods. `tasks/update` adds `inputResponses`.
type taskIDParams struct {
	TaskID         string          `json:"taskId"`
	InputResponses json.RawMessage `json:"inputResponses,omitempty"`
}

// taskIDOf decodes and validates the `taskId` every task method requires.
func taskIDOf(id json.RawMessage, method string, params json.RawMessage) (string, *rpcResponse) {
	var p taskIDParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return "", errResp(id, CodeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if strings.TrimSpace(p.TaskID) == "" {
		return "", errResp(id, CodeInvalidParams, "invalid params: "+method+" requires a `taskId` string")
	}
	return strings.TrimSpace(p.TaskID), nil
}

// unknownTask is the answer for a task id this server cannot resolve — unknown, or purged.
//
// `-32602` is the extension's own code for it, and it is one of the two places our behavior and the
// extension's text agree exactly: "Invalid or nonexistent `taskId`: `-32602` (Invalid params) —
// Servers **MUST** return this error for `tasks/get`" and "Servers **SHOULD** return this error for
// `tasks/update` and `tasks/cancel`."
func unknownTask(id json.RawMessage, taskID string) *rpcResponse {
	return errResp(id, CodeInvalidParams, "invalid params: unknown or expired taskId "+quoteJSON(taskID)+
		" — a task is retained for a bounded time and count, and this server's registry is in memory, so a task does not survive a restart of the server process")
}

func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"` + s + `"`
	}
	return string(b)
}

// getTask answers `tasks/get`.
//
// The result's `resultType` is `"complete"`, NOT `"task"`. That is a MUST and it is easy to get
// backwards: "The `resultType` field **MUST** be set to `"complete"` on this object as it is the
// standard result shape for the `tasks/get` request", against "Servers **MUST NOT** set `resultType`
// to `"task"` on result types other than `CreateTaskResult`" (both verified 2026-08-04). Only the
// `tools/call` answer carries `"task"`.
//
// No caching hint rides it: `tasks/get` is not in `server/utilities/caching`'s closed set, and
// emitting half of a model this result is not part of is how a lifetime field and a freshness field
// come to be confused for each other.
func (s *Server) getTask(env *RequestEnv, id json.RawMessage, params json.RawMessage) *rpcResponse {
	taskID, refusal := taskIDOf(id, "tasks/get", params)
	if refusal != nil {
		return refusal
	}
	v, ok := s.Tasks.Task(taskID)
	if !ok {
		return unknownTask(id, taskID)
	}
	fields := taskFields(taskID, v)
	switch v.Status {
	case TaskCompleted:
		fields["result"] = s.embeddedResult(env, v.Result)
	case TaskFailed:
		e := v.Err
		if e == nil {
			e = &RequestError{Code: CodeInternal, Message: "internal error"}
		}
		fields["error"] = map[string]any{"code": e.Code, "message": e.Message}
	}
	return okResp(id, s.result(env, fields))
}

// updateTask answers `tasks/update`.
//
// It is VACUOUS HERE, and honestly so: this transport issues no mid-flight request, so no task ever
// reaches `input_required` and there is never an outstanding `inputRequests` key to answer. The
// extension's own instruction for a server is "Accept `inputResponses` keyed to outstanding
// `inputRequests`. Acknowledge with an empty result. Ignore responses for unknown or
// already-satisfied keys." Every key we receive is unknown, so every one is ignored and the
// acknowledgement is the whole answer.
//
// It still validates the task id, because acknowledging an update to a task that does not exist
// would be a success message about nothing.
func (s *Server) updateTask(env *RequestEnv, id json.RawMessage, params json.RawMessage) *rpcResponse {
	taskID, refusal := taskIDOf(id, "tasks/update", params)
	if refusal != nil {
		return refusal
	}
	if _, ok := s.Tasks.Task(taskID); !ok {
		return unknownTask(id, taskID)
	}
	return okResp(id, s.result(env, map[string]any{}))
}

// cancelTask answers `tasks/cancel`.
//
// THE ACKNOWLEDGEMENT IS NOT A PROMISE ABOUT THE OUTCOME. It means the cancel signal has been
// delivered to the run's context, and nothing more. What an application may additionally guarantee
// is about WRITES, not about the terminal status: a cancellation delivered before a governed write
// window is entered guarantees no byte reaches the live workspace, and a cancellation that arrives
// after it is acknowledged identically while the task still reaches `completed`. The extension
// permits exactly that — "cancellation is cooperative — the task may still reach a non-`cancelled`
// terminal status."
//
// This is ALSO the only way to cancel a task. Verbatim: "The `notifications/cancelled` notification
// **MUST NOT** be used for task cancellation." Once a `CreateTaskResult` has been returned the
// originating `tools/call` is COMPLETE, so its request id is no longer in flight and a
// `notifications/cancelled` naming it reaches nothing — see handleCancelled, where that is a
// property of the code rather than a rule someone has to remember.
func (s *Server) cancelTask(env *RequestEnv, id json.RawMessage, params json.RawMessage) *rpcResponse {
	taskID, refusal := taskIDOf(id, "tasks/cancel", params)
	if refusal != nil {
		return refusal
	}
	if !s.Tasks.CancelTask(taskID) {
		return unknownTask(id, taskID)
	}
	return okResp(id, s.result(env, map[string]any{}))
}

// missingTasksCapability is the answer to a `tasks/*` request from a client that never declared the
// extension.
//
// WE EMIT `-32021`, AND THE EXTENSION SPECIFIES `-32003`. That is a KNOWN, DECLARED DEVIATION, not
// an oversight, and both texts were read before choosing (migration design §15.2):
//
//   - The extension: "Servers **MUST** return this error [`-32003`] for non-declaring clients issuing
//     `tasks/get`, `tasks/update`, and `tasks/cancel` requests."
//   - Core `basic/index` §`_meta`: "If processing a request requires a capability the client did not
//     include in `io.modelcontextprotocol/clientCapabilities`, the server **MUST** return a
//     `MissingRequiredClientCapabilityError` (`-32021`) whose `data.requiredCapabilities` lists the
//     missing capabilities."
//   - Core `basic/index` §Error Codes: "Apart from `-32002` (see below), receivers **MUST NOT**
//     assume any specific meaning for these codes." A conforming 2026-07-28 client is therefore
//     FORBIDDEN to read `-32003` as anything in particular, so emitting it would not merely break a
//     SHOULD NOT — it would fail to communicate the thing the extension wants communicated.
//
// Between two contradictory MUSTs we follow the stable, versioned core specification over an
// extension whose own repository is labelled experimental. The cost is concrete and on the record: a
// client hard-coded to `-32003` will not recognise this error. `docs/mcp.md` declares it.
func missingTasksCapability(id json.RawMessage, method string) *rpcResponse {
	return errResp(id, CodeMissingClientCapability, "missing required client capability: "+method+
		" is defined only by the "+ExtensionTasks+" extension, and this request's `_meta."+MetaKeyClientCapabilities+
		"` does not declare it. Declare `extensions: {\""+ExtensionTasks+"\": {}}` on every request, or use this server's job shape (waitSeconds → runId → run_status / run_result), which is the documented fallback and needs no extension.",
		map[string]any{"requiredCapabilities": []string{ExtensionTasks}})
}

// isTaskMethod reports whether a method belongs to the extension.
func isTaskMethod(method string) bool {
	switch method {
	case "tasks/get", "tasks/update", "tasks/cancel":
		return true
	}
	return false
}
