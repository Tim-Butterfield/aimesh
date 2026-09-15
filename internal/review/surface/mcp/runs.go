package mcp

import (
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file implements the run registry, shared in shape with exploremesh's. An entry also holds the
// decision set a later review_remediate {fromRun} applies: the accepted findings, the files reviewers
// were shown, and base hashes of targeted files, captured once when the report completes.
//
// The registry is the fast path. It is in-memory and bounded, so the same set is also stored in the
// run's directory and remediateFromDisk reads it there. The source-run guard, which returns the
// original receipt to a retry, lives only here; after a restart the stored base-hash pins refuse a
// second write instead.
//
// Idempotency matters for the write tool: a retried remediation applied twice would be an unrequested
// write.

// Run states, as they appear on the wire.
const (
	StateRunning   = "running"
	StateComplete  = "complete"
	StateHalted    = "halted"
	StateCancelled = "cancelled"
)

// The registry bounds only retention, which is about this process's memory. There is no admission
// limit: how many CLIs a machine can run at once depends on its hardware and the caller's provider rate
// limits, so callers state it per run with maxParallel.
const (
	retainFinished = 50
	finishedTTL    = time.Hour
)

// decisionSet is a completed report run's adjudicated output, which a fromRun remediation applies.
type decisionSet struct {
	// Workspace is the directory the findings were judged against; "" for an inline workspace, which is
	// not remediable.
	Workspace string
	// WorkspaceIdentity is that directory's canonical identity (device and inode plus resolved path) when
	// the review ran. A re-pointed symlink or swapped checkout would keep the path valid but change the
	// tree, so the remediation re-verifies it.
	WorkspaceIdentity run.WorkspaceIdentity
	Inline            bool
	Profile           string
	Panel             []review.SeatSpec
	Overrides         map[review.Role]review.SeatSpec
	Findings          []review.Finding
	Decisions         []review.Decision
	Shown             map[string]bool
	// BaseHashes maps path to digest as reviewed; re-verifying them makes a stale decision set halt.
	BaseHashes map[string]string
	// Accepted is the number of findings in the accepted set.
	Accepted int
}

// record is one run's lifetime state.
type record struct {
	ID    string
	Tool  string
	Mode  string
	Key   string // caller idempotency key ("" when none)
	Start time.Time
	End   time.Time

	// SourceRunID, on a remediation record, is the report run whose set it applied. It prevents applying a
	// decision set twice without an idempotency key.
	SourceRunID string

	done   chan struct{}
	cancel func()

	mu         sync.Mutex
	state      string
	structured map[string]any
	text       string
	isError    bool
	set        *decisionSet
	// pick is the panel this run requested, captured at admission so status and result calls can report
	// it while the run is in flight.
	pick panelPick
	// links are the extra content blocks a terminal result carries, such as a patch's resource_link. They
	// live on the record so a later review_run_result returns them too.
	links []proto.Content
}

func (r *record) snapshot() (state string, structured map[string]any, text string, isError bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.structured, r.text, r.isError
}

// timestamps returns Start and End under the lock, since finish writes End concurrently with readers
// such as tasks/get.
func (r *record) timestamps() (start, end time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Start, r.End
}

func (r *record) finish(state string, structured map[string]any, text string, isError bool) {
	r.mu.Lock()
	r.state, r.structured, r.text, r.isError = state, structured, text, isError
	r.End = time.Now()
	r.mu.Unlock()
	close(r.done)
}

// attachPick records the requested panel at admission; pickSnapshot reads it back. Both lock, because
// status and result calls read concurrently.
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

// attachLinks records the resource links a terminal result carries; linksSnapshot reads them back.
func (r *record) attachLinks(cs []proto.Content) {
	r.mu.Lock()
	r.links = cs
	r.mu.Unlock()
}

func (r *record) linksSnapshot() []proto.Content {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]proto.Content(nil), r.links...)
}

// attach records the decision set of a completed report run.
func (r *record) attach(set *decisionSet) {
	r.mu.Lock()
	r.set = set
	r.mu.Unlock()
}

func (r *record) decisions() *decisionSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set
}

// registry holds this process's runs, bounded by retention. started is reported telemetry, not a limit.
type registry struct {
	mu       sync.Mutex
	byID     map[string]*record
	byKey    map[string]string
	bySource map[string]string // report runId → the remediation record that applied it
	order    []string
	active   int
	started  int
	// onEvict is called with the id of every evicted record, so a run's published resources do not
	// outlive it.
	onEvict func(runID string)
}

func newRegistry() *registry {
	return &registry{
		byID: map[string]*record{}, byKey: map[string]string{}, bySource: map[string]string{},
	}
}

// existing returns the run registered under an idempotency key. A duplicate key never starts a second
// run or, for a remediation, a second write.
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

// existingForSource returns the remediation that already applied sourceRunID. It catches retries
// without a key or with a new one, which an idempotency key cannot.
func (rg *registry) existingForSource(sourceRunID string) *record {
	if sourceRunID == "" {
		return nil
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if id, ok := rg.bySource[sourceRunID]; ok {
		return rg.byID[id]
	}
	return nil
}

// reserve atomically checks for an existing run and admits a new one. Separate existing and admit
// calls would let two calls for the same source run both proceed and apply the set twice.
//
// The first caller is admitted; later callers with the same key or source run receive the winner's
// record as prior. rec and prior are never both non-nil.
func (rg *registry) reserve(id, tool, mode, key, sourceRunID string, cancel func()) (rec, prior *record, err error) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if key != "" {
		if pid, ok := rg.byKey[key]; ok {
			if p := rg.byID[pid]; p != nil {
				return nil, p, nil
			}
		}
	}
	if sourceRunID != "" {
		if pid, ok := rg.bySource[sourceRunID]; ok {
			if p := rg.byID[pid]; p != nil {
				return nil, p, nil
			}
		}
	}
	r, aerr := rg.admitLocked(id, tool, mode, key, sourceRunID, cancel)
	if aerr != nil {
		return nil, nil, aerr
	}
	return r, nil, nil
}

// admit registers a new run. There is no admission limit, so it does not currently fail.
func (rg *registry) admit(id, tool, mode, key, sourceRunID string, cancel func()) (*record, error) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.admitLocked(id, tool, mode, key, sourceRunID, cancel)
}

func (rg *registry) admitLocked(id, tool, mode, key, sourceRunID string, cancel func()) (*record, error) {
	rec := &record{
		ID: id, Tool: tool, Mode: mode, Key: key, SourceRunID: sourceRunID,
		Start: time.Now(), done: make(chan struct{}), cancel: cancel, state: StateRunning,
	}
	rg.byID[id] = rec
	rg.order = append(rg.order, id)
	if key != "" {
		rg.byKey[key] = id
	}
	if sourceRunID != "" {
		rg.bySource[sourceRunID] = id
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

// evictLocked drops finished runs past the TTL, then past the retention count, oldest first. A running
// run is never evicted, since that would leave nothing able to cancel it.
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
	if rec.SourceRunID != "" && rg.bySource[rec.SourceRunID] == rec.ID {
		delete(rg.bySource, rec.SourceRunID)
	}
}

func (rg *registry) get(id string) *record {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.byID[id]
}

// cancelAll cancels every in-flight run, so a disconnect leaves no model CLIs running.
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

// counts returns the in-flight and lifetime run counts.
func (rg *registry) counts() (active, started int) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.active, rg.started
}
