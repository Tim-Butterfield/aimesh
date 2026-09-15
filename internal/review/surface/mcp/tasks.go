package mcp

import (
	"fmt"
	"time"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file projects the run registry as the io.modelcontextprotocol/tasks extension, with
// taskId == runId. A task is a read of the same record review_run_status and review_run_result read;
// there is no second execution model, so removing the extension would touch only this file.
//
// The registry is in memory, so a task id lasts for the life of the server process, not across a
// restart; an unknown id is answered with invalid params. docs/mcp.md documents this.
//
// Clients that do not declare the extension keep the job shape: waitSeconds, then
// {runId, state: "running"}, then review_run_status and review_run_result.

// TaskPollIntervalMs is the pollIntervalMs this server suggests. Reviews take tens of seconds to
// minutes, so two seconds is responsive without excessive polling. It is a hint, not enforced.
const TaskPollIntervalMs = 2000

// taskProvider adapts the run registry to the transport's TaskProvider interface. Every answer is a
// read of the registry.
type taskProvider struct{ s *Server }

// resolveTask returns the record a task id names and its remaining lifetime. A terminal run whose
// retention TTL has elapsed is reported as not found, since registry eviction is lazy and a resolvable
// task must not advertise ttlMs 0; the extension allows discarding expired tasks.
func (t taskProvider) resolveTask(taskID string) (*record, *int64, bool) {
	rec := t.s.runs.get(taskID)
	if rec == nil {
		return nil, nil, false
	}
	state, _, _, _ := rec.snapshot()
	if state == StateRunning {
		// A running record is never evicted, so there is no lifetime to advertise; 0 would tell the client
		// to discard the handle.
		return rec, nil, true
	}
	_, end := rec.timestamps()
	remaining := finishedTTL - time.Since(end)
	if remaining <= 0 {
		return nil, nil, false
	}
	ms := max(remaining.Milliseconds(),
		// Round sub-millisecond remainders up to 1, so a resolvable task never advertises zero.
		1)
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
		// A cancelled task carries no result; the run directory's receipt records what was written.
		v.Status, v.StatusMessage = proto.TaskCancelled, text
	default:
		// Complete and halted runs both map to completed. The extension requires tool results with
		// isError: true to use completed rather than failed, which is reserved for JSON-RPC faults. A partial
		// refusal also lands here with isError; structuredContent's state and outcome distinguish them.
		v.Status, v.StatusMessage = proto.TaskCompleted, text
		v.Result = t.s.payloadFor(rec)
	}
	return v, true
}

// CancelTask delivers the cancel signal to a run and reports whether the id was known.
//
// Acknowledgement means only that the signal reached the run's context. writeWindow.enter reads that
// context under the lock that marks the window entered, so either cancellation wins and nothing is
// committed (the task ends cancelled), or the writer wins, the commit completes, and the task ends
// completed. Cancellation is binding for writes and cooperative for terminal status, which the
// extension permits.
//
// It returns true for a terminal run too; cancelling a finished run is a no-op.
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

// taskHandOff reports whether this call answered with a task handle instead of the job shape. The run
// must still be running, and Call.CreateTask must accept, which it does only for a modern request from
// a client that declared the extension.
func taskHandOff(c *proto.Call, rec *record) bool {
	if state, _, _, _ := rec.snapshot(); state != StateRunning {
		return false
	}
	return c.CreateTask(rec.ID)
}
