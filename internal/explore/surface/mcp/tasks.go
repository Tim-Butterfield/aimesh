package mcp

import (
	"fmt"
	"time"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the `io.modelcontextprotocol/tasks` PROJECTION over this server's run registry
// (migration design §12, product decision D7). It is the same projection reviewmesh carries, over
// the same job shape, and it is deliberately a sibling rather than shared code: the two registries
// are separate types in separate modules, and the adapter is small enough that a shared abstraction
// would cost more than the duplication it removed.
//
// `taskId == runId`. Nothing here mints an identifier, drives a lifecycle, or keeps state. A task is
// a READ of a `*record` — the same record `explore_run_status` and `explore_run_result` read.
//
// exploremesh has NO WRITE PATH, so the cancellation contract here is simpler than reviewmesh's and
// is exactly the extension's own: cancelling stops spend, and the acknowledgement promises delivery
// of the signal and nothing about the terminal status.
//
// The registry is in memory, so a `taskId` is durable for the life of THIS SERVER PROCESS only.

// TaskPollIntervalMs is the `pollIntervalMs` this server suggests — see reviewmesh's copy for why
// two seconds.
const TaskPollIntervalMs = 2000

// taskProvider adapts the run registry to the transport's TaskProvider seam.
type taskProvider struct{ s *Server }

// resolveTask returns the record a task id names, treating a terminal run whose retention TTL has
// elapsed as GONE. Eviction is lazy, so without this a task could stay resolvable long past the
// `ttlMs` we advertised and every later poll would have to report a zero lifetime — which tells a
// conforming client to throw the handle away. The extension permits discarding an expired task and
// answering "cannot be found".
func (t taskProvider) resolveTask(taskID string) (*record, *int64, bool) {
	rec := t.s.runs.get(taskID)
	if rec == nil {
		return nil, nil, false
	}
	state, _, _, _ := rec.snapshot()
	if state == StateRunning {
		// A RUNNING record is never evicted, so there is no lifetime bound to advertise. `null` is
		// the honest value.
		return rec, nil, true
	}
	_, end := rec.timestamps()
	remaining := finishedTTL - time.Since(end)
	if remaining <= 0 {
		return nil, nil, false
	}
	ms := remaining.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return rec, &ms, true
}

// Task projects one run as a task.
func (t taskProvider) Task(taskID string) (proto.TaskView, bool) {
	rec, ttl, ok := t.resolveTask(taskID)
	if !ok {
		return proto.TaskView{}, false
	}
	state, _, text, _ := rec.snapshot()
	start, end := rec.timestamps()
	v := proto.TaskView{
		CreatedAt:      start,
		LastUpdatedAt:  end,
		TTLMs:          ttl,
		PollIntervalMs: TaskPollIntervalMs,
	}
	switch state {
	case StateRunning:
		v.Status, v.LastUpdatedAt = proto.TaskWorking, time.Now()
		v.StatusMessage = fmt.Sprintf("%s is running (mode %s), %.0fs elapsed", rec.Tool, rec.Mode, elapsed(rec))
	case StateCancelled:
		v.Status, v.StatusMessage = proto.TaskCancelled, text
	default:
		// `complete` AND `halted` both land here. The extension: "The `failed` status **MUST NOT**
		// represent non-JSON-RPC errors like tool results with `isError: true`. Errors within
		// protocol method results **MUST** use `completed` status with error details in the
		// `result` field." A domain halt is not a protocol fault, so it is `completed` + `isError`.
		v.Status, v.StatusMessage = proto.TaskCompleted, text
		v.Result = t.s.payloadFor(rec)
	}
	return v, true
}

// CancelTask delivers the cancel signal to a run and reports whether the id was known. A terminal
// run answers true: cancelling something already finished is a no-op, not an error, and `-32602` is
// reserved for an id that names nothing.
func (t taskProvider) CancelTask(taskID string) bool {
	rec, _, ok := t.resolveTask(taskID)
	if !ok {
		return false
	}
	if state, _, _, _ := rec.snapshot(); state == StateRunning && rec.cancel != nil {
		rec.cancel()
	}
	return true
}

// taskHandOff reports whether THIS call answered with a task handle instead of the job shape. See
// reviewmesh's copy: the run must still be running, and Call.CreateTask must accept — which it does
// only for a modern request from a client that declared the extension.
func taskHandOff(c *proto.Call, rec *record) bool {
	if state, _, _, _ := rec.snapshot(); state != StateRunning {
		return false
	}
	return c.CreateTask(rec.ID)
}
