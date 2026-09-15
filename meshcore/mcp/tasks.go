package mcp

import (
	"encoding/json"
	"strings"
	"time"
)

// This file implements the `io.modelcontextprotocol/tasks` extension: `resultType: "task"` on
// `tools/call` and the methods `tasks/get`, `tasks/update` and `tasks/cancel`.
//
// The extension is experimental, and two properties bound that risk:
//
//  1. This package mints no identifier and owns no state. A task projects run state the application
//     already keeps (taskId equals runId), so withdrawing the extension removes only a projection.
//  2. The job shape (waitSeconds, run id, polling) is unchanged for clients that do not declare the
//     extension, so the extension is additive.
//
// A task is never returned to a client that did not declare the extension. Call.CreateTask is the
// single enforcement point, and it returns false without marking anything.

// ExtensionTasks is the extension identifier used in the `extensions` field of client and server
// capabilities. Its settings object is empty on both sides.
const ExtensionTasks = "io.modelcontextprotocol/tasks"

// The five task statuses from the extension's Task interface. `completed`, `failed` and `cancelled`
// are terminal.
const (
	// TaskWorking — the operation is in progress.
	TaskWorking = "working"
	// TaskInputRequired is never emitted: this transport sends no mid-flight requests. It is declared
	// for completeness.
	TaskInputRequired = "input_required"
	// TaskCompleted — the request finished, including with a domain failure: the extension requires
	// tool results with `isError: true` to use `completed`, never `failed`.
	TaskCompleted = "completed"
	// TaskFailed is for a JSON-RPC protocol error only.
	TaskFailed = "failed"
	// TaskCancelled — the operation was cancelled. See TaskProvider.CancelTask for what our
	// acknowledgement does and does not promise.
	TaskCancelled = "cancelled"
)

// maxStatusMessage bounds `statusMessage`, which the extension says "may be exposed to the end-user
// or model". A run's terminal rendering can be thousands of characters; a status line is a status
// line, and the full text is one `tasks/get` away in `result`.
const maxStatusMessage = 240

// TaskView is one task as the application projects it; this package only formats it.
type TaskView struct {
	// Status is one of the five constants above.
	Status string
	// StatusMessage is optional human- and model-facing context, capped when rendered.
	StatusMessage string
	// CreatedAt / LastUpdatedAt are the task's timestamps, rendered as RFC 3339.
	CreatedAt     time.Time
	LastUpdatedAt time.Time
	// TTLMs is the extension's `ttlMs`, the lifetime from creation in milliseconds. nil means
	// unlimited and marshals to null. It is not a caching hint.
	TTLMs *int64
	// PollIntervalMs is the extension's suggested polling interval. 0 omits the field.
	PollIntervalMs int
	// Result is what the original request would have returned synchronously, set only on
	// TaskCompleted. A domain failure keeps its `isError`.
	Result *CallToolResult
	// Err is the JSON-RPC error a TaskFailed task carries. Set only on TaskFailed.
	Err *RequestError
}

// TaskProvider is the seam between this transport and an application's run registry. A nil provider
// disables the extension: no advertisement, no task methods, and CreateTask always false.
type TaskProvider interface {
	// Task projects one task; ok is false for an unknown or expired id, which the extension treats
	// alike.
	Task(taskID string) (TaskView, bool)
	// CancelTask delivers the cancel signal to the task's run and reports whether the id was known.
	// It promises nothing about the terminal status: cancellation is cooperative.
	CancelTask(taskID string) bool
}

// clientExtensions parses the `extensions` object out of a client's per-request capabilities.
// Unreadable capabilities declare nothing.
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

// DeclaredExtension reports whether the client declared the named extension on this request.
// Capabilities are per request in the modern era; on the legacy era it is always false.
func (e *RequestEnv) DeclaredExtension(name string) bool {
	if e == nil || e.Era != EraModern {
		return false
	}
	_, ok := e.Extensions[name]
	return ok
}

// CreateTask marks this call as task-augmented and reports whether the mark took. It returns false
// without marking when the server has no task provider, the request is not modern, or the client did
// not declare the extension. Handlers use `if c.CreateTask(runID) { return nil, nil }` and otherwise
// return their ordinary result.
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
	// `ttlMs` is required and nullable, so nil is emitted as an explicit null.
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

// embeddedResult is what the original `tools/call` would have returned synchronously, with the envelope
// it would have carried. It uses a `tools/call` env rather than the polling request's, since it
// describes the original request.
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

// createTaskResponse answers a task-augmented `tools/call` with `resultType: "task"`. The extension
// requires the task to resolve through `tasks/get` before the handle is returned, so the projection is
// read back first. Handles last for the life of the server process, not across restarts (see
// docs/mcp.md).
func (s *Server) createTaskResponse(id json.RawMessage, env *RequestEnv, taskID string) *rpcResponse {
	v, ok := s.Tasks.Task(taskID)
	if !ok {
		// A handle that `tasks/get` could not resolve is never returned.
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

// unknownTask answers a task id this server cannot resolve, with the extension's -32602.
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

// getTask answers `tasks/get`. Its `resultType` is `"complete"`; only the `tools/call` answer carries
// `"task"`. No caching hint is included, since `tasks/get` is not cacheable.
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

// updateTask answers `tasks/update`. No task reaches `input_required` here, so every input response is
// ignored and an empty acknowledgement is returned once the task id is validated.
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

// cancelTask answers `tasks/cancel`, the only way to cancel a task (see handleCancelled). The
// acknowledgement means the cancel signal reached the run's context; the task may still reach a
// non-cancelled terminal status.
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

// missingTasksCapability answers a `tasks/*` request from a client that did not declare the extension.
// It returns the core specification's -32021 rather than the extension's -32003, because the core
// specification requires -32021 for a missing client capability and forbids clients from assigning
// meaning to other codes. docs/mcp.md records the deviation.
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
