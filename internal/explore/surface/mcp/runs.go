package mcp

import (
	"sync"
	"time"
)

// This file holds the run registry. Runs outlive the calls that start them, so a client that times out or
// disconnects can still poll explore_run_status and fetch explore_run_result by run id.
//
// The registry does not limit how many runs start or run at once; callers bound their own runs with
// `maxParallel`. It bounds only how many finished runs it retains, and for how long.

// Run states.
const (
	StateRunning   = "running"
	StateComplete  = "complete"
	StateHalted    = "halted"
	StateCancelled = "cancelled"
)

// Retention limits for finished runs.
const (
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
	// pick is the requested panel, kept so status and result calls can echo it while the run is in flight.
	pick panelPick
}

// snapshot returns the record's state and terminal payload; the payload is nil while running.
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

// timestamps returns Start and End under the lock, since finish writes End concurrently.
func (r *record) timestamps() (start, end time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Start, r.End
}

// attachPick records the requested panel.
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

// registry holds this process's runs. active and started are reported counts, not limits.
type registry struct {
	mu      sync.Mutex
	byID    map[string]*record
	byKey   map[string]string
	order   []string
	active  int
	started int
	// onEvict is called with the id of each dropped run, so its published resources are removed too.
	onEvict func(runID string)
}

func newRegistry() *registry {
	return &registry{byID: map[string]*record{}, byKey: map[string]string{}}
}

// existing returns the run registered under an idempotency key, or nil.
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

// admit registers a new run. It currently always succeeds.
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

// evictLocked drops finished runs older than finishedTTL, then the oldest finished runs beyond
// retainFinished. Running runs are never evicted, so they stay cancellable.
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

// cancelAll cancels every running run. The server calls it when its transport closes.
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

// counts returns the number of running runs and of runs started, as reported by explore_list.
func (rg *registry) counts() (active, started int) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.active, rg.started
}
