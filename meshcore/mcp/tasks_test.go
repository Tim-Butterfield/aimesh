package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the `io.modelcontextprotocol/tasks` EXTENSION on the wire (D7, migration design §12).
//
// AGAINST A TREE WITHOUT THE EXTENSION EVERY TEST HERE FAILS TO COMPILE, and that is stated rather than
// dressed up: `mcp.ExtensionTasks`, `mcp.TaskProvider`, `mcp.TaskView`, `mcp.ResultTypeTask`,
// `Server.Tasks` and `Call.CreateTask` did not exist, because the extension did not. A test that
// cannot compile against the old tree proves the API is new and proves nothing about behavior, so
// each test below additionally states the MUTATION it would catch — the one-line change to the new
// code that makes it fail. Those mutations are what stand in for a behavioral red, and they were
// each run — a precedent kept because naming and running the mutation has caught a false negative before.
//
// The one test here that CAN fail behaviorally against the old tree is
// TestTasks_DiscoverAdvertisesTheExtension: `server/discover` existed, and its `capabilities` object
// had no `extensions` key.

// --- a fake provider, so the transport is the unit under test ---

type fakeTask struct {
	view mcp.TaskView
	ok   bool
}

type fakeTasks struct {
	mu        sync.Mutex
	tasks     map[string]fakeTask
	cancelled []string
}

func newFakeTasks() *fakeTasks { return &fakeTasks{tasks: map[string]fakeTask{}} }

func (f *fakeTasks) put(id string, v mcp.TaskView) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[id] = fakeTask{view: v, ok: true}
}

func (f *fakeTasks) Task(id string) (mcp.TaskView, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return mcp.TaskView{}, false
	}
	return t.view, t.ok
}

func (f *fakeTasks) CancelTask(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tasks[id]; !ok {
		return false
	}
	f.cancelled = append(f.cancelled, id)
	return true
}

func (f *fakeTasks) cancels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}

// workingView is a task that is still running: unlimited lifetime, a poll interval, no result.
func workingView() mcp.TaskView {
	return mcp.TaskView{
		Status: mcp.TaskWorking, StatusMessage: "running",
		CreatedAt: time.Now().Add(-time.Second), LastUpdatedAt: time.Now(),
		TTLMs: nil, PollIntervalMs: 2000,
	}
}

// ttl returns a pointer to n, for the terminal-lifetime field.
func ttl(n int64) *int64 { return &n }

// tasksMeta is a modern `_meta` that DECLARES the tasks extension.
func tasksMeta() map[string]any {
	return map[string]any{
		mcp.MetaKeyProtocolVersion: modern,
		mcp.MetaKeyClientCapabilities: map[string]any{
			"extensions": map[string]any{mcp.ExtensionTasks: map[string]any{}},
		},
	}
}

func tasksParams(params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = tasksMeta()
	return params
}

// taskServer builds a server whose `slow` tool answers with a TASK HANDLE when the caller declared
// the extension, and with the ordinary job-shaped result when it did not. That branch is the whole
// contract a real application implements, so the fake implements it the same way.
func taskServer(f *fakeTasks, handle string) *mcp.Server {
	return newServer(func(s *mcp.Server) {
		s.Tasks = f
		s.Register(mcp.Tool{Name: "slow", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				if c.CreateTask(handle) {
					return nil, nil
				}
				return mcp.Result("still running", map[string]any{"runId": handle, "state": "running"}), nil
			})
	})
}

// --- the advertisement ---

// TestTasks_DiscoverAdvertisesTheExtension is the ONE wire delta the extension makes visible to a
// client that never opts in, and it is required rather than incidental: advertising the extension in
// `server/discover` is HOW a client learns it may declare it.
//
// This test CAN fail behaviorally against the old tree — `server/discover` existed and its
// capabilities object had no `extensions` key at all.
// MUTATION: delete the `caps["extensions"] = …` block in era.go's discover.
func TestTasks_DiscoverAdvertisesTheExtension(t *testing.T) {
	f := newFakeTasks()
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "server/discover", modernParams(modern, nil))
	if resp.Error != nil {
		t.Fatalf("server/discover: %+v", resp.Error)
	}
	caps, _ := resp.Result["capabilities"].(map[string]any)
	exts, _ := caps["extensions"].(map[string]any)
	settings, ok := exts[mcp.ExtensionTasks].(map[string]any)
	if !ok {
		t.Fatalf("capabilities.extensions = %v, want %q declared — a client cannot opt into an extension it is never told about", caps["extensions"], mcp.ExtensionTasks)
	}
	if len(settings) != 0 {
		t.Errorf("extension settings = %v, want the empty object: the tasks extension defines none", settings)
	}
}

// TestTasks_NoProviderAdvertisesNothing — one condition, one consequence. A server with no task
// provider must not advertise an extension it cannot service.
// MUTATION: drop the `if s.Tasks != nil` guard around the advertisement.
func TestTasks_NoProviderAdvertisesNothing(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	resp, _ := c.call(t, "server/discover", modernParams(modern, nil))
	caps, _ := resp.Result["capabilities"].(map[string]any)
	if _, present := caps["extensions"]; present {
		t.Fatalf("capabilities.extensions = %v on a server with no task provider — advertising an unserviceable extension is worse than not having one", caps["extensions"])
	}
}

// --- never return a task to a non-declaring client ---

// TestTasks_ADeclaringClientGetsACreateTaskResult pins the `tools/call` answer: `resultType: "task"`
// (a MUST, and the only result that may carry it) plus the Task fields.
// MUTATION: change `ResultTypeTask` to `ResultTypeComplete` in createTaskResponse.
func TestTasks_ADeclaringClientGetsACreateTaskResult(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", workingView())
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "tools/call", tasksParams(map[string]any{"name": "slow", "arguments": map[string]any{}}))
	if resp.Error != nil {
		t.Fatalf("tools/call: %+v", resp.Error)
	}
	if resp.Result["resultType"] != mcp.ResultTypeTask {
		t.Fatalf("resultType = %v, want %q — the extension MUSTs it on a CreateTaskResult and MUST NOT allow it anywhere else",
			resp.Result["resultType"], mcp.ResultTypeTask)
	}
	if resp.Result["taskId"] != "run-1" {
		t.Errorf("taskId = %v, want the run id — taskId == runId, and no second identifier is ever minted", resp.Result["taskId"])
	}
	if resp.Result["status"] != mcp.TaskWorking {
		t.Errorf("status = %v, want %q", resp.Result["status"], mcp.TaskWorking)
	}
	// `ttlMs` is REQUIRED and nullable. A working task has no lifetime bound, and `null` is the
	// value that says so — `0` would tell a conforming client to throw the handle away at once.
	v, present := resp.Result["ttlMs"]
	if !present {
		t.Errorf("ttlMs is absent; it is required and nullable, and \"null for unlimited\" is a VALUE")
	}
	if v != nil {
		t.Errorf("ttlMs = %v on a working task, want null", v)
	}
	if _, ok := resp.Result["content"]; ok {
		t.Errorf("a CreateTaskResult carried tool `content` — the point of a task handle is that the result does not exist yet: %v", resp.Result)
	}
}

// TestTasks_ANonDeclaringClientNeverGetsATask is the extension's hardest rule ("Never return a task
// to a client that did not declare support") and the safety property that lets the extension ship
// additively: the same handler, the same tool, the same run — and the job shape comes back.
// MUTATION: make Call.CreateTask skip its DeclaredExtension check.
func TestTasks_ANonDeclaringClientNeverGetsATask(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", workingView())
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "tools/call", modernParams(modern, map[string]any{"name": "slow", "arguments": map[string]any{}}))
	if resp.Error != nil {
		t.Fatalf("tools/call: %+v", resp.Error)
	}
	if resp.Result["resultType"] != mcp.ResultTypeComplete {
		t.Fatalf("resultType = %v, want %q — a client that declared nothing must see exactly today's behavior",
			resp.Result["resultType"], mcp.ResultTypeComplete)
	}
	if _, leaked := resp.Result["taskId"]; leaked {
		t.Fatalf("a taskId reached a client that never declared the extension: %v", resp.Result)
	}
	sc, _ := resp.Result["structuredContent"].(map[string]any)
	if sc["state"] != "running" {
		t.Errorf("structuredContent = %v, want the job shape", sc)
	}
}

// TestTasks_DeclaredExtensionIsFalseOnTheLegacyEra pins the era clause DIRECTLY, and it exists
// because the wire test below does NOT pin it.
//
// Deleting `e.Era != EraModern` from DeclaredExtension leaves
// TestTasks_ALegacyClientNeverGetsATask PASSING, because a legacy request's
// env never has `Extensions` filled at all (envFor does not parse them), so the map lookup fails for
// a second, independent reason. Two redundant guards are the right design — but a test that cannot
// tell which one is holding is not testing either of them. This one builds the env by hand, with
// extensions present and the era legacy, so only the era clause can produce the answer.
// MUTATION: delete the `e.Era != EraModern` clause in DeclaredExtension.
func TestTasks_DeclaredExtensionIsFalseOnTheLegacyEra(t *testing.T) {
	env := &mcp.RequestEnv{
		Era:        mcp.EraLegacy,
		Extensions: map[string]json.RawMessage{mcp.ExtensionTasks: json.RawMessage(`{}`)},
	}
	if env.DeclaredExtension(mcp.ExtensionTasks) {
		t.Fatal("a LEGACY request declared a 2026-07-28 extension — a 2025-06-18 client has no way to make that declaration, and honouring one would let a legacy transcript grow a task")
	}
	modernEnv := &mcp.RequestEnv{
		Era:        mcp.EraModern,
		Extensions: map[string]json.RawMessage{mcp.ExtensionTasks: json.RawMessage(`{}`)},
	}
	if !modernEnv.DeclaredExtension(mcp.ExtensionTasks) {
		t.Fatal("a modern request that declared the extension was not recognised — the era clause must reject the legacy era, not every era")
	}
	if modernEnv.DeclaredExtension("io.example/other") {
		t.Fatal("an undeclared extension read as declared")
	}
}

// TestTasks_ALegacyClientNeverGetsATask — a 2025-06-18 client cannot declare a 2026-07-28 extension.
// It is the END-TO-END half; the clause that makes it true in isolation is pinned by
// TestTasks_DeclaredExtensionIsFalseOnTheLegacyEra above.
// MUTATION: make Call.CreateTask skip its DeclaredExtension check.
func TestTasks_ALegacyClientNeverGetsATask(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", workingView())
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()
	handshake(t, c)

	resp, _ := c.call(t, "tools/call", map[string]any{
		"name": "slow", "arguments": map[string]any{},
		"_meta": map[string]any{
			mcp.MetaKeyClientCapabilities: map[string]any{
				"extensions": map[string]any{mcp.ExtensionTasks: map[string]any{}},
			},
		},
	})
	if resp.Error != nil {
		t.Fatalf("tools/call: %+v", resp.Error)
	}
	if _, leaked := resp.Result["taskId"]; leaked {
		t.Fatalf("a legacy session produced a task handle: %v", resp.Result)
	}
	if _, leaked := resp.Result["resultType"]; leaked {
		t.Fatalf("a legacy result grew a modern field: %v", resp.Result)
	}
}

// --- tasks/get ---

// TestTasksGet_CarriesResultTypeCompleteNotTask is the MUST that is easiest to get backwards.
// Verbatim from the extension (fetched 2026-08-04): "The `resultType`
// field MUST be set to `"complete"` on this object as it is the standard result shape for the
// `tasks/get` request", against "Servers MUST NOT set `resultType` to `"task"` on result types other
// than `CreateTaskResult`".
// MUTATION: use resultAs(env, ResultTypeTask, …) in getTask.
func TestTasksGet_CarriesResultTypeCompleteNotTask(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", workingView())
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "tasks/get", tasksParams(map[string]any{"taskId": "run-1"}))
	if resp.Error != nil {
		t.Fatalf("tasks/get: %+v", resp.Error)
	}
	if resp.Result["resultType"] != mcp.ResultTypeComplete {
		t.Fatalf("resultType = %v, want %q — only a CreateTaskResult may carry \"task\"", resp.Result["resultType"], mcp.ResultTypeComplete)
	}
	// `tasks/get` is NOT in `server/utilities/caching`'s closed cacheable set, so no hint is owed —
	// and emitting half of that model on a result carrying the extension's own `ttlMs` is exactly
	// how a lifetime field and a freshness field come to be confused for each other.
	if _, present := resp.Result["cacheScope"]; present {
		t.Errorf("cacheScope rode a tasks/* result: %v", resp.Result)
	}
}

// TestTasksGet_TerminalTTLIsPositiveAndStrictlyDecreases — `ttlMs` is TASK LIFETIME FROM CREATION,
// not a caching hint, and the extension permits it to change over the task's life. A resolvable task
// must never advertise `0`: that declares a handle the server may discard immediately.
// MUTATION: return a constant ttl from the provider, or floor it at 0 instead of 1.
func TestTasksGet_TerminalTTLIsPositiveAndStrictlyDecreases(t *testing.T) {
	f := newFakeTasks()
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	deadline := time.Now().Add(3600 * time.Millisecond)
	put := func() {
		f.put("run-1", mcp.TaskView{
			Status: mcp.TaskCompleted, CreatedAt: time.Now().Add(-time.Second), LastUpdatedAt: time.Now(),
			TTLMs: ttl(time.Until(deadline).Milliseconds()), PollIntervalMs: 2000,
			Result: mcp.Result("done", map[string]any{"state": "complete"}),
		})
	}
	put()
	first, _ := c.call(t, "tasks/get", tasksParams(map[string]any{"taskId": "run-1"}))
	a, ok := first.Result["ttlMs"].(float64)
	if !ok || a <= 0 {
		t.Fatalf("ttlMs = %v on a resolvable terminal task, want a positive lifetime — 0 tells a conforming client to discard the handle", first.Result["ttlMs"])
	}
	time.Sleep(5 * time.Millisecond)
	put()
	second, _ := c.call(t, "tasks/get", tasksParams(map[string]any{"taskId": "run-1"}))
	b, _ := second.Result["ttlMs"].(float64)
	if b >= a {
		t.Errorf("ttlMs went %v → %v across two polls, want strictly decreasing: it is a lifetime from creation, not a constant", a, b)
	}
	if b <= 0 {
		t.Errorf("ttlMs = %v while the task is still resolvable", b)
	}
}

// TestTasksGet_HaltedMapsToCompletedWithIsErrorNotFailed is the state-table row the design flags as
// the one to get right, and the extension states it as a MUST: "The `failed` status MUST NOT
// represent non-JSON-RPC errors like tool results with `isError: true`. Errors within protocol method
// results MUST use `completed` status with error details in the `result` field."
// MUTATION: map a halted record to TaskFailed in the application's provider.
func TestTasksGet_HaltedMapsToCompletedWithIsErrorNotFailed(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", mcp.TaskView{
		Status: mcp.TaskCompleted, StatusMessage: "halted",
		CreatedAt: time.Now().Add(-time.Second), LastUpdatedAt: time.Now(), TTLMs: ttl(600000),
		Result: mcp.ErrorResult("run halted", map[string]any{"state": "halted", "haltClass": "F"}),
	})
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "tasks/get", tasksParams(map[string]any{"taskId": "run-1"}))
	if resp.Result["status"] != mcp.TaskCompleted {
		t.Fatalf("status = %v, want %q — a governed halt is a COMPLETED request whose tool failed, not a JSON-RPC fault",
			resp.Result["status"], mcp.TaskCompleted)
	}
	res, _ := resp.Result["result"].(map[string]any)
	if res == nil {
		t.Fatalf("a completed task carried no `result`: %v", resp.Result)
	}
	if res["isError"] != true {
		t.Fatalf("result.isError = %v, want true — the halt has to travel, and this is the field it travels in", res["isError"])
	}
	// The embedded result is what the original `tools/call` WOULD have returned, envelope included.
	if res["resultType"] != mcp.ResultTypeComplete {
		t.Errorf("embedded resultType = %v, want %q", res["resultType"], mcp.ResultTypeComplete)
	}
	sc, _ := res["structuredContent"].(map[string]any)
	if sc["state"] != "halted" {
		t.Errorf("the run's own vocabulary must survive into structuredContent: %v", sc)
	}
	if _, present := resp.Result["error"]; present {
		t.Errorf("a domain halt produced a task-level `error`: %v — that field is for a JSON-RPC fault", resp.Result["error"])
	}
}

// TestTasks_UnknownOrExpiredTaskIsInvalidParams. "Invalid or nonexistent `taskId`: -32602 (Invalid
// params) — Servers MUST return this error for `tasks/get`", SHOULD for update and cancel. It is also
// the honest answer for a task id from a server that has since restarted, because the registry is in
// memory and a restart loses it — a silently-empty task would be the alternative.
// MUTATION: return an empty TaskView with ok=true for an unknown id.
func TestTasks_UnknownOrExpiredTaskIsInvalidParams(t *testing.T) {
	f := newFakeTasks()
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()
	for _, m := range []string{"tasks/get", "tasks/update", "tasks/cancel"} {
		resp, _ := c.call(t, m, tasksParams(map[string]any{"taskId": "run-gone"}))
		if resp.Error == nil || resp.Error.Code != mcp.CodeInvalidParams {
			t.Fatalf("%s on an unknown taskId: %+v, want -32602", m, resp.Error)
		}
		if !strings.Contains(resp.Error.Message, "restart") {
			t.Errorf("%s: the message must name the in-memory limitation a caller will otherwise discover: %q", m, resp.Error.Message)
		}
	}
}

// --- the capability gate ---

// TestTasks_NonDeclaringClientGetsMinus32021 asserts OUR DECLARED BEHAVIOR, not extension
// conformance.
//
// CONFLICT: core 2026-07-28 basic/index MUSTs -32021 for a missing declared capability
// ("the server MUST return a MissingRequiredClientCapabilityError (-32021) whose
// data.requiredCapabilities lists the missing capabilities"); ext-tasks
// specification/draft/tasks MUSTs -32003 for the same condition ("Servers MUST return this error
// for non-declaring clients issuing tasks/get, tasks/update, and tasks/cancel requests"). We follow
// core — it is directly on point, and core additionally forbids receivers from ascribing any meaning
// to -32003, so emitting -32003 to a conforming modern client would not merely break a SHOULD NOT,
// it would fail to communicate. The deviation is declared in docs/mcp.md. This asserts our declared behavior, NOT extension conformance. Revisit if
// ext-tasks moves.
//
// MUTATION: delete the validateModernCapabilities call from admit.
func TestTasks_NonDeclaringClientGetsMinus32021(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", workingView())
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	for _, m := range []string{"tasks/get", "tasks/update", "tasks/cancel"} {
		resp, _ := c.call(t, m, modernParams(modern, map[string]any{"taskId": "run-1"}))
		if resp.Error == nil {
			t.Fatalf("%s from a non-declaring client succeeded: %v", m, resp.Result)
		}
		if resp.Error.Code != mcp.CodeMissingClientCapability {
			t.Fatalf("%s error code = %d, want %d (our declared behavior; see the CONFLICT note above)",
				m, resp.Error.Code, mcp.CodeMissingClientCapability)
		}
		if resp.Error.Code == -32003 {
			t.Fatalf("%s emitted -32003, which a conforming 2026-07-28 client is forbidden to interpret", m)
		}
		req, _ := resp.Error.Data["requiredCapabilities"].([]any)
		if len(req) != 1 || req[0] != mcp.ExtensionTasks {
			t.Errorf("%s data.requiredCapabilities = %v, want [%q] — the code is only useful with the list core specifies",
				m, resp.Error.Data["requiredCapabilities"], mcp.ExtensionTasks)
		}
	}
}

// TestTasks_TheCapabilityRefusalDoesNotLatchTheEra — the admission order is the design, and a request
// we are about to refuse has no business choosing the process's era. A `tasks/get` refused for a
// missing capability must leave a following LEGACY client servable.
// MUTATION: move validateModernCapabilities to after latchEra (or into serveModern).
func TestTasks_TheCapabilityRefusalDoesNotLatchTheEra(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", workingView())
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "tasks/get", modernParams(modern, map[string]any{"taskId": "run-1"}))
	if resp.Error == nil || resp.Error.Code != mcp.CodeMissingClientCapability {
		t.Fatalf("setup: want the -32021 refusal, got %+v", resp.Error)
	}
	init, _ := c.call(t, "initialize", map[string]any{"protocolVersion": mcp.LatestProtocolVersion})
	if init.Error != nil {
		t.Fatalf("initialize after a refused tasks/get: %+v — the refusal latched the era it was refused for", init.Error)
	}
}

// TestTasks_MethodsAreAbsentWithoutAProvider — a server with no task provider does not implement
// them, so the answer is an ordinary method-not-found rather than a capability complaint about a
// method that does not exist.
// MUTATION: return true unconditionally for the tasks methods in modernMethodImplemented.
func TestTasks_MethodsAreAbsentWithoutAProvider(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	resp, _ := c.call(t, "tasks/get", tasksParams(map[string]any{"taskId": "run-1"}))
	if resp.Error == nil || resp.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("tasks/get on a server with no provider: %+v, want -32601", resp.Error)
	}
}

// --- tasks/update and tasks/cancel ---

// TestTasksUpdate_IsAnEmptyAcknowledgement. We request no mid-flight input, so no task ever reaches
// `input_required` and every `inputResponses` key is unknown — which the extension says to ignore.
// The acknowledgement carries `resultType: "complete"` (a MUST) and nothing else.
// MUTATION: return the task object from updateTask instead of an empty result.
func TestTasksUpdate_IsAnEmptyAcknowledgement(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", workingView())
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "tasks/update", tasksParams(map[string]any{
		"taskId": "run-1", "inputResponses": map[string]any{"nobody-asked": map[string]any{"x": 1}},
	}))
	if resp.Error != nil {
		t.Fatalf("tasks/update: %+v", resp.Error)
	}
	if resp.Result["resultType"] != mcp.ResultTypeComplete {
		t.Errorf("resultType = %v, want %q (a MUST on UpdateTaskResult)", resp.Result["resultType"], mcp.ResultTypeComplete)
	}
	for k := range resp.Result {
		if k != "resultType" && k != "_meta" {
			t.Errorf("tasks/update result carried %q; it is an empty acknowledgement", k)
		}
	}
}

// TestTasksCancel_AcknowledgesAndPromisesOnlyDelivery.
//
// THE ACKNOWLEDGEMENT IS NOT A PROMISE ABOUT THE TERMINAL STATUS. The extension: "Honor them when
// possible, but cancellation is cooperative — the task may still reach a non-`cancelled` terminal
// status." This test therefore asserts the ack and the DELIVERY, and deliberately asserts NOTHING
// about the status the task goes on to reach — a test that did would be pinning a guarantee we do
// not make.
// MUTATION: make cancelTask return the task object, or skip the CancelTask call.
func TestTasksCancel_AcknowledgesAndPromisesOnlyDelivery(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", workingView())
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "tasks/cancel", tasksParams(map[string]any{"taskId": "run-1"}))
	if resp.Error != nil {
		t.Fatalf("tasks/cancel: %+v", resp.Error)
	}
	if resp.Result["resultType"] != mcp.ResultTypeComplete {
		t.Errorf("resultType = %v, want %q (a MUST on CancelTaskResult)", resp.Result["resultType"], mcp.ResultTypeComplete)
	}
	for k := range resp.Result {
		if k != "resultType" && k != "_meta" {
			t.Errorf("tasks/cancel result carried %q; the extension says acknowledge with an EMPTY result", k)
		}
	}
	if got := f.cancels(); len(got) != 1 || got[0] != "run-1" {
		t.Fatalf("cancel delivery = %v, want exactly one delivery to run-1 — the ack means the signal reached the run's context, and it must actually have", got)
	}

	// A TERMINAL task is still a KNOWN id, so cancelling it is a no-op and not an error. -32602 is
	// for an id that names nothing.
	f.put("run-2", mcp.TaskView{Status: mcp.TaskCompleted, CreatedAt: time.Now(), LastUpdatedAt: time.Now(), TTLMs: ttl(1000)})
	resp2, _ := c.call(t, "tasks/cancel", tasksParams(map[string]any{"taskId": "run-2"}))
	if resp2.Error != nil {
		t.Errorf("tasks/cancel on a finished task: %+v, want an acknowledgement", resp2.Error)
	}
}

// TestTasks_NotificationsCancelledCannotCancelATask is the extension's flat prohibition — "The
// `notifications/cancelled` notification MUST NOT be used for task cancellation" — and here it holds
// BY CONSTRUCTION rather than by a special case: once the CreateTaskResult has been returned, the
// originating tools/call is complete and its id has left the in-flight table, so the notification
// finds nothing and reaches nothing.
//
// This CANNOT fail behaviorally against the old tree, because the old tree had no tasks at all for a
// notification to fail to cancel. It is a regression guard for the property, and the mutation that
// proves it is real is named below.
// MUTATION: have the `slow` handler keep its Call registered and route handleCancelled into the
// provider's CancelTask — the assertion on f.cancels() then fails.
func TestTasks_NotificationsCancelledCannotCancelATask(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", workingView())
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "tools/call", tasksParams(map[string]any{"name": "slow", "arguments": map[string]any{}}))
	if resp.Result["taskId"] != "run-1" {
		t.Fatalf("setup: want a task handle, got %v", resp.Result)
	}
	// The id of the tools/call that produced the handle. That call is COMPLETE — cancelling it is
	// meaningless, and the extension forbids using this notification for the task.
	var callID int
	_ = json.Unmarshal(resp.ID, &callID)
	c.notify("notifications/cancelled", map[string]any{"requestId": callID, "reason": "user"})

	// Round-trip a request so the notification has certainly been processed before we look.
	if r, _ := c.call(t, "tasks/get", tasksParams(map[string]any{"taskId": "run-1"})); r.Error != nil {
		t.Fatalf("tasks/get: %+v", r.Error)
	}
	if got := f.cancels(); len(got) != 0 {
		t.Fatalf("notifications/cancelled reached the run's cancellation path (%v) — the extension MUST NOTs exactly this; only tasks/cancel may cancel a task", got)
	}
}

// TestTasks_NonDeclaringTranscriptHasExactlyOneDelta is the SAFETY PROPERTY that lets the extension
// ship additively, and it is written as an ALLOWLIST OF ONE rather than as unqualified byte-identity.
//
// Unqualified byte-identity is impossible and it would be wrong to demand it: advertising the
// extension in `server/discover` is HOW a client learns it may opt in, so that delta is REQUIRED by
// the extension. Everything else must be untouched, and "everything else" is checked by diffing two
// live transcripts — the same request sequence against a server WITH a task provider and against one
// WITHOUT — rather than by re-recording the current bytes, which would pass by construction.
//
// MUTATION: make taskHandOff ignore the client's declaration, or add any second field to the
// discover capabilities under `if s.Tasks != nil`.
func TestTasks_NonDeclaringTranscriptHasExactlyOneDelta(t *testing.T) {
	// The same sequence a non-declaring modern client would drive, including a `tools/call` that
	// outlives nothing and one that a task-capable server COULD have handed back as a task.
	sequence := func(t *testing.T, c *client) []string {
		t.Helper()
		var out []string
		record := func(method string, params map[string]any) {
			resp, _ := c.call(t, method, params)
			b, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal %s: %v", method, err)
			}
			out = append(out, method+" => "+string(b))
		}
		record("server/discover", modernParams(modern, nil))
		record("tools/list", modernParams(modern, nil))
		record("tools/call", modernParams(modern, map[string]any{"name": "slow", "arguments": map[string]any{}}))
		record("tools/call", modernParams(modern, map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}}))
		record("ping", modernParams(modern, nil))
		return out
	}

	f := newFakeTasks()
	f.put("run-1", workingView())
	withTasks, stopA := serve(t, taskServer(f, "run-1"))
	defer stopA()
	// The SAME server minus the provider: same tool, same handler, same everything.
	withoutTasks, stopB := serve(t, newServer(func(s *mcp.Server) {
		s.Register(mcp.Tool{Name: "slow", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
			func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
				if c.CreateTask("run-1") {
					return nil, nil
				}
				return mcp.Result("still running", map[string]any{"runId": "run-1", "state": "running"}), nil
			})
	}))
	defer stopB()

	got := sequence(t, withTasks)
	base := sequence(t, withoutTasks)
	if len(got) != len(base) {
		t.Fatalf("frame counts differ: %d vs %d", len(got), len(base))
	}
	deltas := 0
	for i := range got {
		if got[i] == base[i] {
			continue
		}
		deltas++
		if !strings.HasPrefix(got[i], "server/discover ") {
			t.Fatalf("a NON-declaring client saw a change on %q.\n with tasks: %s\nwithout:    %s",
				strings.SplitN(got[i], " ", 2)[0], got[i], base[i])
		}
		// The one permitted delta, and it must be exactly the extensions advertisement: strip it and
		// the two frames must be identical again.
		stripped := strings.Replace(got[i],
			`"extensions":{"`+mcp.ExtensionTasks+`":{}},`, "", 1)
		if stripped == got[i] {
			stripped = strings.Replace(got[i],
				`,"extensions":{"`+mcp.ExtensionTasks+`":{}}`, "", 1)
		}
		if stripped != base[i] {
			t.Fatalf("server/discover changed by more than the extensions advertisement.\nwith tasks: %s\nwithout:    %s", got[i], base[i])
		}
	}
	if deltas != 1 {
		t.Fatalf("delta count = %d, want exactly 1 (the server/discover advertisement)", deltas)
	}
}

// TestTasks_TheTaskFieldsSurviveTheEraEnvelope guards a real collision the extension creates: `ttlMs` is
// owned by `server/utilities/caching` on the six cacheable operations AND by the tasks extension on a
// task object. The envelope's reserved-field check must not reject every payload carrying `ttlMs`:
// that would make every CreateTaskResult a marshalling error.
// MUTATION: move "ttlMs" back into reservedResultFields unconditionally.
func TestTasks_TheTaskFieldsSurviveTheEraEnvelope(t *testing.T) {
	f := newFakeTasks()
	f.put("run-1", mcp.TaskView{
		Status: mcp.TaskWorking, CreatedAt: time.Now(), LastUpdatedAt: time.Now(),
		TTLMs: ttl(1234), PollIntervalMs: 2000, StatusMessage: "half way",
	})
	c, stop := serve(t, taskServer(f, "run-1"))
	defer stop()

	resp, _ := c.call(t, "tasks/get", tasksParams(map[string]any{"taskId": "run-1"}))
	if resp.Error != nil {
		t.Fatalf("tasks/get: %+v — a task's own ttlMs must not collide with the caching envelope's", resp.Error)
	}
	if v, _ := resp.Result["ttlMs"].(float64); v != 1234 {
		t.Errorf("ttlMs = %v, want the task's own lifetime 1234", resp.Result["ttlMs"])
	}
	if resp.Result["pollIntervalMs"] == nil {
		t.Errorf("pollIntervalMs is absent: %v", resp.Result)
	}
	if resp.Result["statusMessage"] != "half way" {
		t.Errorf("statusMessage = %v", resp.Result["statusMessage"])
	}
	for _, k := range []string{"createdAt", "lastUpdatedAt"} {
		if _, ok := resp.Result[k].(string); !ok {
			t.Errorf("%s = %v, want an RFC 3339 string (the Task interface declares both non-optional)", k, resp.Result[k])
		}
	}
}
