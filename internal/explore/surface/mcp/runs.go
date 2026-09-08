package mcp

import (
	"sync"
	"time"
)

// This file is the JOB REGISTRY.
//
// The registry exists because MCP calls are job-shaped: common client request timeouts are around 60
// seconds, and a multi-minute exploration would otherwise be killed MID-SPEND with no way to reach the
// subprocesses it started. Holding the run here means a client that times out, disconnects or crashes can
// still poll `explore_run_status` and fetch `explore_run_result` — and, just as importantly, that the run has an identity
// (its run id) which the on-disk record is keyed by.
//
// It is no longer a spend governor. See the note on the retention constants for why the admission
// bounds were removed and where the concurrency decision moved to.

// Run states, as they appear on the wire.
const (
	StateRunning   = "running"
	StateComplete  = "complete"
	StateHalted    = "halted"
	StateCancelled = "cancelled"
)

// There is deliberately NO ADMISSION GOVERNOR — neither a lifetime run cap nor an in-flight one.
//
// The lifetime cap was never the spend ceiling its name implied: stopping and starting the server
// cleared the counter, so its guarantee lasted exactly as long as the process did.
//
// The in-flight cap did hold, but it was the wrong shape. What it really bounded was how many
// provider CLI subprocesses this machine hosts at once — a fact about the operator's hardware
// (memory, process budget, whether the model is local and loads weights) and their provider rate
// limits. A launch-time constant chosen by exploremesh is a guess about all of those. That number is
// now stated PER INVOCATION as `maxParallel`, by the caller who knows it, and it bounds the seats of
// their own run rather than admission to the server.
//
// So the registry bounds only RETENTION, which is about this process's memory and nothing else.
const (
	// retainFinished bounds how many FINISHED runs stay fetchable; finishedTTL bounds how long. Both are
	// needed: a long-lived server must not grow without bound, and a short-lived one must not evict a
	// result before the client that started it can read it.
	retainFinished = 50
	finishedTTL    = time.Hour
)

// record is one run's lifetime state.
type record struct {
	ID    string
	Tool  string
	Mode  string
	Key   string // caller idempotency key ("" when none was supplied)
	Start time.Time
	End   time.Time

	done   chan struct{}
	cancel func()

	mu         sync.Mutex
	state      string
	structured map[string]any
	text       string
	isError    bool
	// pick is the panel this run ASKED for, captured at admission. It is held on the record rather than
	// only on the starting call so that a `explore_run_status`/`explore_run_result` issued while the run is still in
	// flight can still answer "which panel is this?" — the requested-vs-executed echo is required on
	// every branch of the output schema, including `running`.
	pick panelPick
}

// snapshot returns the record's terminal payload (nil while it is still running).
func (r *record) snapshot() (state string, structured map[string]any, text string, isError bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.structured, r.text, r.isError
}

func (r *record) finish(state string, structured map[string]any, text string, isError bool) {
	r.mu.Lock()
	r.state, r.structured, r.text, r.isError = state, structured, text, isError
	r.End = time.Now()
	r.mu.Unlock()
	close(r.done)
}

// timestamps returns Start and End UNDER THE LOCK. End is written by finish() from the run's own
// goroutine while a poller may be reading it from another, so it is not a field a reader may take
// directly — and the tasks projection reads it on every `tasks/get`.
func (r *record) timestamps() (start, end time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Start, r.End
}

// attachPick records the requested panel at admission; pickSnapshot reads it back. Both take the mutex
// because a concurrent explore_run_status/explore_run_result reads it from another goroutine.
func (r *record) attachPick(p panelPick) {
	r.mu.Lock()
	r.pick = p
	r.mu.Unlock()
}

func (r *record) pickSnapshot() panelPick {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pick
}

// registry holds this process's runs. It is bounded in two independent ways — concurrency and
// retention — because they fail differently: concurrency bounds the spend RATE, and retention bounds
// memory. `started` is reported telemetry, not a ceiling.
type registry struct {
	mu      sync.Mutex
	byID    map[string]*record
	byKey   map[string]string
	order   []string
	active  int
	started int
	// onEvict is called with the run id of every record the registry drops, so the resources a run
	// published cannot outlive the run itself.
	onEvict func(runID string)
}

func newRegistry() *registry {
	return &registry{byID: map[string]*record{}, byKey: map[string]string{}}
}

// existing returns the run already registered under an idempotency key. A duplicate key is NEVER a second
// run: a client retrying after a dropped connection must get its original run back, not a second panel
// billed to the same person for the same question.
func (rg *registry) existing(key string) *record {
	if key == "" {
		return nil
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if id, ok := rg.byKey[key]; ok {
		return rg.byID[id]
	}
	return nil
}

// admit registers a new run. It no longer refuses: there is no admission governor (see the note on
// retention above), so the error return is kept only because callers treat admission as fallible and
// a future bound would land here.
func (rg *registry) admit(id, tool, modeName, key string, cancel func()) (*record, error) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	rec := &record{ID: id, Tool: tool, Mode: modeName, Key: key, Start: time.Now(), done: make(chan struct{}), cancel: cancel, state: StateRunning}
	rg.byID[id] = rec
	rg.order = append(rg.order, id)
	if key != "" {
		rg.byKey[key] = id
	}
	rg.active++
	rg.started++
	return rec, nil
}

// release marks a run finished and evicts stale finished runs.
func (rg *registry) release(id string) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	rg.active--
	if rg.active < 0 {
		rg.active = 0
	}
	rg.evictLocked()
}

// evictLocked drops finished runs past the TTL, then past the retention count (oldest first). A RUNNING
// run is never evicted — losing the handle would leave a live subprocess tree with nothing able to cancel
// it.
func (rg *registry) evictLocked() {
	now := time.Now()
	keep := rg.order[:0]
	for _, id := range rg.order {
		rec := rg.byID[id]
		if rec == nil {
			continue
		}
		state, _, _, _ := rec.snapshot()
		if state != StateRunning && now.Sub(rec.End) > finishedTTL {
			rg.dropLocked(rec)
			continue
		}
		keep = append(keep, id)
	}
	rg.order = keep
	finished := 0
	for _, id := range rg.order {
		if rec := rg.byID[id]; rec != nil {
			if state, _, _, _ := rec.snapshot(); state != StateRunning {
				finished++
			}
		}
	}
	for i := 0; i < len(rg.order) && finished > retainFinished; i++ {
		rec := rg.byID[rg.order[i]]
		if rec == nil {
			continue
		}
		if state, _, _, _ := rec.snapshot(); state == StateRunning {
			continue
		}
		rg.dropLocked(rec)
		rg.order = append(rg.order[:i], rg.order[i+1:]...)
		i--
		finished--
	}
}

func (rg *registry) dropLocked(rec *record) {
	if rg.onEvict != nil {
		rg.onEvict(rec.ID)
	}
	delete(rg.byID, rec.ID)
	if rec.Key != "" && rg.byKey[rec.Key] == rec.ID {
		delete(rg.byKey, rec.Key)
	}
}

// get looks a run up by id.
func (rg *registry) get(id string) *record {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.byID[id]
}

// cancelAll cancels every in-flight run — used when the server's transport goes away, so a disconnect
// never leaves a panel of model CLIs running with nobody to receive their output.
func (rg *registry) cancelAll() {
	rg.mu.Lock()
	recs := make([]*record, 0, len(rg.byID))
	for _, rec := range rg.byID {
		recs = append(recs, rec)
	}
	rg.mu.Unlock()
	for _, rec := range recs {
		if state, _, _, _ := rec.snapshot(); state == StateRunning && rec.cancel != nil {
			rec.cancel()
		}
	}
}

// counts reports the in-flight and lifetime run counts (for the limits block `explore_list` reports).
func (rg *registry) counts() (active, started int) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.active, rg.started
}
