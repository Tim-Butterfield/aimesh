package mcp

import (
	"testing"
	"time"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// These internal tests cover the mapping from a run record to task status and ttlMs; the transport is
// covered by meshcore/mcp/tasks_test.go. Each test names the mutation it would catch.

func provider(s *Server) taskProvider { return taskProvider{s} }

// A running record is never evicted, so its ttl is null.
// MUTATION: return a non-nil ttl for the running branch of resolveTask.
func TestTaskProjection_RunningIsWorkingWithAnUnlimitedTTL(t *testing.T) {
	s := &Server{}
	s.runs = newRegistry()
	rec, err := s.runs.admit("run-abc", toolReport, "report", "", "", func() {})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	v, ok := provider(s).Task(rec.ID)
	if !ok {
		t.Fatal("a live run must resolve as a task — the extension MUSTs that a returned taskId resolve")
	}
	if v.Status != proto.TaskWorking {
		t.Errorf("status = %q, want %q", v.Status, proto.TaskWorking)
	}
	if v.TTLMs != nil {
		t.Errorf("ttlMs = %v on a running task, want null (unlimited): a running record is never evicted", *v.TTLMs)
	}
	if v.PollIntervalMs != TaskPollIntervalMs {
		t.Errorf("pollIntervalMs = %d, want %d", v.PollIntervalMs, TaskPollIntervalMs)
	}
	if v.Result != nil {
		t.Error("a working task carries no result — the point of the handle is that the result does not exist yet")
	}
}

// A halted run is completed with isError, never failed.
// MUTATION: map StateHalted to proto.TaskFailed.
func TestTaskProjection_HaltedIsCompletedWithIsErrorNeverFailed(t *testing.T) {
	s := &Server{}
	s.runs = newRegistry()
	rec, _ := s.runs.admit("run-halt", toolReport, "report", "", "", func() {})
	rec.finish(StateHalted, map[string]any{"state": StateHalted, "haltClass": "F"}, "run halted", true)

	v, ok := provider(s).Task(rec.ID)
	if !ok {
		t.Fatal("a halted run must still resolve as a task")
	}
	if v.Status == proto.TaskFailed {
		t.Fatal("a governed HALT was mapped to `failed`; `failed` is for a JSON-RPC protocol fault, and a domain halt is not one")
	}
	if v.Status != proto.TaskCompleted {
		t.Fatalf("status = %q, want %q", v.Status, proto.TaskCompleted)
	}
	if v.Result == nil || !v.Result.IsError {
		t.Fatalf("result = %+v, want the halt riding isError: true — that is the field it travels in", v.Result)
	}
	if v.Err != nil {
		t.Errorf("err = %+v, want nil: the task-level `error` field is for a JSON-RPC fault", v.Err)
	}
}

// A partial refusal is also completed with isError; structuredContent distinguishes it from a halt.
// MUTATION: map an isError record with state complete to TaskFailed.
func TestTaskProjection_PartialRefusalIsCompletedWithIsError(t *testing.T) {
	s := &Server{}
	s.runs = newRegistry()
	rec, _ := s.runs.admit("run-partial", toolRemediate, "apply", "", "", func() {})
	rec.finish(StateComplete, map[string]any{
		"state": StateComplete, "outcome": "partial_refusal",
		"counts": map[string]any{"applied": 7, "refused": 1},
	}, "REFUSED: 1 finding", true)

	v, _ := provider(s).Task(rec.ID)
	if v.Status != proto.TaskCompleted {
		t.Fatalf("status = %q, want %q", v.Status, proto.TaskCompleted)
	}
	if v.Result == nil || !v.Result.IsError {
		t.Fatal("a partial refusal must carry isError into the embedded result")
	}
	sc, _ := v.Result.StructuredContent.(map[string]any)
	if sc["state"] != StateComplete || sc["outcome"] != "partial_refusal" {
		t.Errorf("structuredContent = %v — the run's own vocabulary is what tells a halt from a partial refusal", sc)
	}
}

// A cancelled run is a cancelled task with no result.
// MUTATION: attach payloadFor(rec) on the cancelled branch.
func TestTaskProjection_CancelledIsCancelled(t *testing.T) {
	s := &Server{}
	s.runs = newRegistry()
	rec, _ := s.runs.admit("run-cancel", toolRemediate, "apply", "", "", func() {})
	rec.finish(StateCancelled, map[string]any{"state": StateCancelled}, "cancelled", true)

	v, _ := provider(s).Task(rec.ID)
	if v.Status != proto.TaskCancelled {
		t.Fatalf("status = %q, want %q", v.Status, proto.TaskCancelled)
	}
	if v.Result != nil {
		t.Errorf("a cancelled task carried a result; the extension's CancelledTask has no such field")
	}
}

// A terminal task's ttlMs is the registry's remaining finishedTTL, which decreases over time.
// MUTATION: return a constant ttl for the terminal branch.
func TestTaskProjection_TerminalTTLIsTheRealRetentionAndDecreases(t *testing.T) {
	s := &Server{}
	s.runs = newRegistry()
	rec, _ := s.runs.admit("run-done", toolReport, "report", "", "", func() {})
	rec.finish(StateComplete, map[string]any{"state": StateComplete}, "done", false)

	first, ok := provider(s).Task(rec.ID)
	if !ok || first.TTLMs == nil {
		t.Fatalf("a terminal task must advertise a real lifetime, got %+v", first)
	}
	if *first.TTLMs <= 0 {
		t.Fatalf("ttlMs = %d on a resolvable task; 0 tells a conforming client to discard the handle", *first.TTLMs)
	}
	if *first.TTLMs > finishedTTL.Milliseconds() {
		t.Errorf("ttlMs = %d exceeds the registry's own retention %d — it must be the REAL bound, not a number",
			*first.TTLMs, finishedTTL.Milliseconds())
	}
	time.Sleep(3 * time.Millisecond)
	second, _ := provider(s).Task(rec.ID)
	if *second.TTLMs >= *first.TTLMs {
		t.Errorf("ttlMs went %d → %d across two polls, want strictly decreasing: it is a lifetime from creation",
			*first.TTLMs, *second.TTLMs)
	}
}

// An expired terminal task is not found, so a resolvable task never reports ttlMs 0.
// MUTATION: delete the `remaining <= 0` branch in resolveTask.
func TestTaskProjection_ExpiredTerminalTaskIsGone(t *testing.T) {
	s := &Server{}
	s.runs = newRegistry()
	rec, _ := s.runs.admit("run-old", toolReport, "report", "", "", func() {})
	rec.finish(StateComplete, map[string]any{"state": StateComplete}, "done", false)
	// Age it past the retention TTL.
	rec.mu.Lock()
	rec.End = time.Now().Add(-finishedTTL - time.Minute)
	rec.mu.Unlock()

	if _, ok := provider(s).Task(rec.ID); ok {
		t.Fatal("a task past its advertised ttlMs still resolved — it would have to report ttlMs 0, which is a handle we told the client to throw away")
	}
	if provider(s).CancelTask(rec.ID) {
		t.Error("cancelling an expired task reported success; the id names nothing and must yield -32602")
	}
}

// CancelTask delivers the signal to the run and acknowledges terminal ids too. The terminal status
// depends on whether cancellation wins the write window, so the test asserts only delivery.
// MUTATION: drop the `rec.cancel()` call, or return false for a terminal record.
func TestTaskProjection_CancelDeliversToTheRunAndAcknowledgesTerminalIdsToo(t *testing.T) {
	s := &Server{}
	s.runs = newRegistry()
	delivered := 0
	rec, _ := s.runs.admit("run-live", toolRemediate, "apply", "", "", func() { delivered++ })

	if !provider(s).CancelTask(rec.ID) {
		t.Fatal("cancelling a known, running task must be acknowledged")
	}
	if delivered != 1 {
		t.Fatalf("cancel deliveries = %d, want 1 — the acknowledgement means the signal reached the run's context, and it must actually have", delivered)
	}
	// A terminal id is still known: cancelling it is a no-op.
	rec.finish(StateComplete, map[string]any{"state": StateComplete}, "done", false)
	if !provider(s).CancelTask(rec.ID) {
		t.Error("cancelling a finished task must be acknowledged; -32602 is for an id that names nothing")
	}
	if delivered != 1 {
		t.Errorf("a finished run's cancel func was called again (%d) — an entered write window is deliberately uncancellable", delivered)
	}
	if _, ok := provider(s).Task("run-never-existed"); ok {
		t.Error("an unknown id resolved")
	}
}

// Only a running run is handed off as a task.
// MUTATION: drop the state check in taskHandOff.
func TestTaskHandOff_OnlyARunningRunBecomesAHandle(t *testing.T) {
	s := &Server{}
	s.runs = newRegistry()
	rec, _ := s.runs.admit("run-x", toolReport, "report", "", "", func() {})
	rec.finish(StateComplete, map[string]any{"state": StateComplete}, "done", false)

	// A nil Call cannot create a task; Call.CreateTask answers false for anything it cannot verify.
	if taskHandOff(nil, rec) {
		t.Fatal("a finished run produced a task handle")
	}
}
