package mcp

import (
	"fmt"
	"time"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file projects the run registry as `io.modelcontextprotocol/tasks` tasks. A task id is the run id,
// and a task is a read of the same record explore_run_status and explore_run_result read; nothing here
// keeps state. Explorations write nothing, so cancelling a task stops spend and promises nothing about the
// terminal status. The registry is in memory, so a task id lasts only as long as the server process.

// TaskPollIntervalMs is the `pollIntervalMs` this server suggests.
const TaskPollIntervalMs = 2000

// taskProvider adapts the run registry to the transport's TaskProvider seam.
type taskProvider struct{ s *Server }

// resolveTask returns the record a task id names, with its remaining lifetime. A finished run past its
// retention TTL is treated as gone, because eviction is lazy and an expired task may be reported as not
// found.
func (t taskProvider) resolveTask(taskID string) (*record, *int64, bool) {
	rec := t.s.runs.get(taskID)
	if rec == nil {
		return nil, nil, false
	}
	state, _, _, _ := rec.snapshot()
	if state == StateRunning {
		// A running record is never evicted, so its lifetime is reported as null.
		return rec, nil, true
	}
	_, end := rec.timestamps()
	remaining := finishedTTL - time.Since(end)
	if remaining <= 0 {
		return nil, nil, false
	}
	ms := max(remaining.Milliseconds(), 1)
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
		// Complete and halted runs are both `completed`: the tasks extension reserves `failed` for
		// protocol errors, so a halt is a completed task whose result has isError set.
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

// taskHandOff reports whether this call answered with a task handle instead of the job shape: the run must
// still be running, and Call.CreateTask accepts only a modern request from a client that declared the
// tasks extension.
func taskHandOff(c *proto.Call, rec *record) bool {
	if state, _, _, _ := rec.snapshot(); state != StateRunning {
		return false
	}
	return c.CreateTask(rec.ID)
}
