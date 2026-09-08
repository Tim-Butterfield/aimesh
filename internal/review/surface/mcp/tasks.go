package mcp

import (
	"fmt"
	"time"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the `io.modelcontextprotocol/tasks` PROJECTION over this server's run registry
// (migration design §12, product decision D7).
//
// THE WHOLE DESIGN IS ONE LINE: `taskId == runId`. No second identifier is minted, no second
// execution model exists, and nothing here drives a lifecycle. A task is a READ of a `*record` —
// the same record `review_run_status` and `review_run_result` read — rendered in the extension's vocabulary. That
// is what bounds the accepted risk of adopting an experimental extension: if it is withdrawn we
// delete this file and the run model underneath is untouched.
//
// WHAT WE DO NOT HAVE, stated here rather than discovered. The registry is entirely in memory
// (runs.go): `admitLocked` inserts a `*record` into maps and nothing is persisted. So a `taskId` is
// durable for the life of THIS SERVER PROCESS and not across a restart of it. We meet the
// extension's durable-creation rule as the extension STATES it — "until a `tasks/get` for the
// returned `taskId` would resolve", which is true the instant the record is in the map, before the
// id is returned — and we do NOT meet the rationale its overview gives for the rule ("Crash
// resilience. A task ID is a durable handle."). A task id from a restarted server is answered
// `-32602`, which is the honest answer and not a silently-empty task. docs/mcp.md says so.
//
// The JOB SHAPE IS NOT REPLACED. A client that does not declare the extension sees exactly the
// behavior it saw before this file existed: `waitSeconds` → `{runId, state:"running"}` →
// `review_run_status` / `review_run_result`. That is our decision (the extension is opt-in per client, so a modern
// client that does not opt in still needs it), not a specification requirement.

// TaskPollIntervalMs is the `pollIntervalMs` this server suggests. It matches the guidance the job
// shape already gives a caller ("poll review_run_status, then fetch review_run_result"): a governed review is
// measured in tens of seconds to minutes, so a two-second cadence is responsive without turning a
// long run into thousands of round trips.
//
// The extension calls it a suggestion and says "Clients **SHOULD** honor this value to avoid
// overwhelming the server"; nothing here enforces it, because a rate limit that lived in a hint
// would be a limit in name only.
const TaskPollIntervalMs = 2000

// taskProvider adapts the run registry to the transport's TaskProvider seam. It is a value type over
// the Server because it holds nothing of its own: every answer is a read of the registry.
type taskProvider struct{ s *Server }

// resolveTask returns the record a task id names, applying the ONE rule that makes `ttlMs`
// meaningful: a terminal run whose retention TTL has elapsed is reported as GONE rather than as a
// task with a zero lifetime.
//
// Eviction in the registry is lazy — it happens when another run is released, not on a timer — so
// without this check a task could remain resolvable for an unbounded time after the `ttlMs` we
// advertised had run out, and every poll after that point would have to report `ttlMs: 0`. The
// extension permits exactly this reading: "The server **may** discard the task after the TTL
// elapses", and "It is compliant behavior for a server to return an error stating the task cannot be
// found if it has purged an expired task."
//
// The remaining lifetime is returned with the record so the two cannot be computed twice and differ.
func (t taskProvider) resolveTask(taskID string) (*record, *int64, bool) {
	rec := t.s.runs.get(taskID)
	if rec == nil {
		return nil, nil, false
	}
	state, _, _, _ := rec.snapshot()
	if state == StateRunning {
		// A RUNNING record is never evicted (runs.go), so there is no lifetime bound to advertise.
		// `null` is the honest value; `0` would tell a conforming client to throw the handle away.
		return rec, nil, true
	}
	_, end := rec.timestamps()
	remaining := finishedTTL - time.Since(end)
	if remaining <= 0 {
		return nil, nil, false
	}
	ms := remaining.Milliseconds()
	if ms < 1 {
		// Sub-millisecond remainders round to zero, and a resolvable task must never advertise a
		// zero lifetime. One millisecond is the truthful floor for "still here, expiring now".
		ms = 1
	}
	return rec, &ms, true
}

// Task projects one run as a task. See the state table in the migration design §12.4 — the row that
// matters most is the one it is easiest to get wrong.
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
		// A CancelledTask carries no `result` field in the extension's type, so none is attached.
		// The durable answer to "what did it write" is the run directory's receipt, which is where
		// it has always been — a cancelled call may receive no response at all.
		v.Status, v.StatusMessage = proto.TaskCancelled, text
	default:
		// `complete` AND `halted` both land here, and that is the extension's rule rather than our
		// convenience: "The `failed` status **MUST NOT** represent non-JSON-RPC errors like tool
		// results with `isError: true`. Errors within protocol method results **MUST** use
		// `completed` status with error details in the `result` field."
		//
		// So a governed HALT is `completed` + `isError: true` on the embedded result, never
		// `failed`. `failed` is for a JSON-RPC protocol fault, and a domain halt is not one. The
		// same row covers a PARTIAL REFUSAL (`state: "complete"`, `outcome: "partial_refusal"`),
		// which also rides `isError: true` — the two are told apart one level down, in
		// `structuredContent.state` and `outcome`, where the run's own vocabulary is authoritative.
		v.Status, v.StatusMessage = proto.TaskCompleted, text
		v.Result = t.s.payloadFor(rec)
	}
	return v, true
}

// CancelTask delivers the cancel signal to a run and reports whether the id was known.
//
// WHAT THE ACKNOWLEDGEMENT MEANS — and the answer is deliberately narrower than "it was cancelled":
// the signal has been delivered to the run's context. The linearization is decided elsewhere, once,
// under a lock: `writeWindow.enter` (manager/review/remediate.go) reads that context INSIDE the lock
// that also marks the window entered, so exactly one of two things is true afterwards.
//
//   - Cancellation won `enter`: no commit runs for this run, ever; the isolated copy is discarded
//     and the live workspace is unchanged. The run reaches `cancelled`, and so does the task.
//   - The writer won `enter`: the commit runs to completion, a later cancellation cannot retract it,
//     and the run reaches `complete` (or `halted`) — so the TASK REACHES `completed`, not
//     `cancelled`. The extension explicitly permits this: "cancellation is cooperative — the task
//     may still reach a non-`cancelled` terminal status."
//
// So our contract is BINDING WITH RESPECT TO WRITES and COOPERATIVE WITH RESPECT TO TERMINAL STATUS,
// which is strictly stronger than the extension's floor on the half that matters for governance and
// exactly equal to it on the half that does not.
//
// It returns true for a TERMINAL run too. Cancelling something already finished is a no-op, not an
// error: the id is known, and `-32602` is for an id that names nothing.
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

// taskHandOff reports whether THIS call answered with a task handle instead of a job-shaped payload.
//
// Two conditions, and both are necessary. The run must still be RUNNING — a finished run's real
// result is strictly better than a handle to it, and the extension leaves the choice to the server
// per request ("The server decides per-request whether to create a task"). And Call.CreateTask must
// accept, which it does only for a modern request from a client that declared the extension: that is
// the single enforcement point for "Never return a task to a client that did not declare support."
func taskHandOff(c *proto.Call, rec *record) bool {
	if state, _, _, _ := rec.snapshot(); state != StateRunning {
		return false
	}
	return c.CreateTask(rec.ID)
}
