package mcp

import (
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file is the JOB REGISTRY — the same mechanism exploremesh's MCP surface uses, with one
// addition this server needs and that one does not: a registry entry also HOLDS the decision set a
// later `review_remediate --fromRun` writes from.
//
// That is what makes two-phase remediation possible at all. The accepted findings, the files the
// reviewers were actually shown, and the base hashes of every targeted file are captured when the
// report run completes and are never recomputed: a remediation that re-derived its own accepted set
// would be a second adjudication wearing the first one's name.
//
// It is the FAST path, not the only one. The registry is in-memory and bounded, so an entry is
// evicted by retention and lost outright on a restart; the same set is ALSO on disk, in the run's
// own directory, and `review_remediate {fromRun}` falls back to reading it there (mcp.go's
// remediateFromDisk). What lives only here — and therefore only for the life of the process — is
// the source-run guard below, which returns the ORIGINAL receipt to a retry. After a restart the
// durable base-hash pins refuse the second write instead, as a halt.
//
// It is no longer a spend governor — see the note on the retention constants for why the admission
// bounds were removed and where the concurrency decision moved to. For the write primitive, the
// idempotency guard is not a convenience. A retried remediation that applied a second time would be
// an unrequested write.

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
// limits. A launch-time constant chosen by reviewmesh is a guess about all of those. That number is
// now stated PER INVOCATION as `maxParallel`, by the caller who knows it, and it bounds the seats of
// their own run rather than admission to the server.
//
// So the registry bounds only RETENTION, which is about this process's memory and nothing else.
const (
	retainFinished = 50
	finishedTTL    = time.Hour
)

// decisionSet is a completed report run's inspectable, already-adjudicated output — the exact input
// a `fromRun` remediation writes from.
type decisionSet struct {
	// Workspace is the LIVE directory the findings were judged against ("" for an inline
	// workspace, which is why an inline run is not remediable).
	Workspace string
	// WorkspaceIdentity is that directory's CANONICAL IDENTITY (device+inode plus resolved path)
	// as it was when the review ran. The path string alone is not the reviewed tree: a symlink
	// re-pointed, a checkout swapped, or a bind mount changed between the report and the apply
	// would leave the string valid and the tree different. Captured once, here, and re-verified
	// by the remediation.
	WorkspaceIdentity run.WorkspaceIdentity
	Inline            bool
	Profile           string
	Panel             []review.SeatSpec
	Overrides         map[review.Role]review.SeatSpec
	Findings          []review.Finding
	Decisions         []review.Decision
	Shown             map[string]bool
	// BaseHashes is path → digest AS REVIEWED. Re-verifying it is what makes a stale decision set
	// a halt rather than a silent write against a file nobody judged.
	BaseHashes map[string]string
	// Accepted is how many findings are in the accepted set (host-computed once, here).
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

	// SourceRunID is set on a REMEDIATION record: the report run whose set it applied. It is what
	// makes "never apply the same decision set twice" enforceable without an idempotency key.
	SourceRunID string

	done   chan struct{}
	cancel func()

	mu         sync.Mutex
	state      string
	structured map[string]any
	text       string
	isError    bool
	set        *decisionSet
	// pick is the panel this run ASKED for, captured at admission. It is held on the record rather
	// than only on the starting call so that a `review_run_status`/`review_run_result` issued while the run is
	// still in flight can still answer "which panel is this?" — the requested-vs-executed echo is
	// required on every branch of the review output schema, including `running`.
	pick panelPick
	// links are the extra content blocks a terminal result carries beyond its text rendering —
	// today, the `resource_link` to a remediation's patch. They live on the RECORD rather than only
	// on the starting call's response so that a `review_run_result` fetched after the inline budget expired
	// hands back the same link: the call that paid for the write is often not the call that collects
	// it.
	links []proto.Content
}

func (r *record) snapshot() (state string, structured map[string]any, text string, isError bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.structured, r.text, r.isError
}

// timestamps returns Start and End UNDER THE LOCK. End is written by finish() from the run's own
// goroutine while a poller may be reading it from another, so it is not a field a reader may take
// directly — and the tasks projection reads it on every `tasks/get`, which makes the unsynchronized
// read a real one rather than a theoretical one.
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

// attachPick records the requested panel at admission; pickSnapshot reads it back. Both take the
// mutex because a concurrent review_run_status/review_run_result reads it from another goroutine.
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

// registry holds this process's runs, bounded in two independent ways — concurrency (spend RATE)
// and retention (memory). `started` is kept as reported telemetry, not as a ceiling.
type registry struct {
	mu       sync.Mutex
	byID     map[string]*record
	byKey    map[string]string
	bySource map[string]string // report runId → the remediation record that applied it
	order    []string
	active   int
	started  int
	// onEvict is called with the run id of every record the registry drops. It exists so that the
	// resources a run published cannot outlive the run itself: a `resource_link` the server can no
	// longer explain is worse than no link, because a client would retry it.
	onEvict func(runID string)
}

func newRegistry() *registry {
	return &registry{
		byID: map[string]*record{}, byKey: map[string]string{}, bySource: map[string]string{},
	}
}

// existing returns the run already registered under an idempotency key. A duplicate key is NEVER a
// second run: a client retrying after a dropped connection must get its original run back, not a
// second panel billed to the same person — and, on the remediation path, not a second write.
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

// existingForSource returns the remediation that has already applied a given report run.
//
// This is the guard an idempotency key cannot provide: a client that retries WITHOUT a key, or with
// a fresh one, is still asking to apply a decision set that has already been applied. Returning the
// prior receipt is the only answer that cannot double-write.
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

// reserve is the ATOMIC form of "is this already running/run, and if not, admit it".
//
// existing/existingForSource followed by admit is check-then-act, and on the WRITE primitive that
// is not a style point: two calls naming the same source run can both find no prior remediation and
// both proceed, and the second one applies a decision set that has already been applied. The gap is
// wide — several map lookups plus the whole validation path sit inside it — so it is not a
// theoretical race either.
//
// One lock covers both halves. The first caller to arrive is admitted and becomes the winner;
// every later caller for the same key or the same source run is handed the WINNER's record and
// attaches to it rather than starting anything. `prior` and `rec` are never both non-nil.
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

// admit registers a new run. It no longer refuses: there is no admission governor (see the note on
// retention above), so the error return is kept only because callers treat admission as fallible and
// a future bound would land here.
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

// evictLocked drops finished runs past the TTL, then past the retention count (oldest first). A
// RUNNING run is never evicted — losing the handle would leave a live subprocess tree with nothing
// able to cancel it.
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

// cancelAll cancels every in-flight run — used when the transport goes away, so a disconnect never
// leaves a panel of model CLIs running with nobody to receive their output.
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

// counts reports the in-flight and lifetime run counts (for the limits block `review_list` reports).
func (rg *registry) counts() (active, started int) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.active, rg.started
}
