package mcp

import (
	"sync"
	"testing"
)

// The registry's reservation is the ONE place "never apply the same accepted set twice" is decided.
// A transport-level race depends on goroutine scheduling to reach the window; this test hits the
// primitive directly, which is where the guarantee either exists or does not.

// TestRegistry_ReserveIsAtomicPerSourceRun hammers one source run from many goroutines. Exactly one
// may be admitted; every other caller must be handed the winner. A check-then-act guard
// (existingForSource, then admit) cannot promise this — between the two, every caller sees nothing.
func TestRegistry_ReserveIsAtomicPerSourceRun(t *testing.T) {
	const callers = 64
	rg := newRegistry()
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	var mu sync.Mutex
	var winners, attached int
	var winnerID string
	for i := 0; i < callers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			rec, prior, err := rg.reserve(runIDFor(i), toolRemediate, "apply", "", "source-run", func() {})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("caller %d refused: %v", i, err)
			case rec != nil && prior != nil:
				t.Errorf("caller %d got BOTH a new run and a prior one", i)
			case rec != nil:
				winners++
				winnerID = rec.ID
			case prior != nil:
				attached++
			default:
				t.Errorf("caller %d got neither a run nor a prior", i)
			}
		}(i)
	}
	start.Done()
	done.Wait()
	if winners != 1 {
		t.Fatalf("%d write windows opened for one source run, want exactly 1", winners)
	}
	if attached != callers-1 {
		t.Fatalf("%d callers attached, want %d", attached, callers-1)
	}
	if got := rg.existingForSource("source-run"); got == nil || got.ID != winnerID {
		t.Fatalf("the source run does not resolve to the winner (%v)", got)
	}
}

// TestRegistry_ReserveIsAtomicPerKey is the same property for the idempotency key, which covers the
// retry that names no source run (the full-cycle form).
func TestRegistry_ReserveIsAtomicPerKey(t *testing.T) {
	const callers = 32
	rg := newRegistry()
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	var mu sync.Mutex
	winners := 0
	for i := 0; i < callers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			rec, _, err := rg.reserve(runIDFor(i), toolRemediate, "apply", "same-key", "", func() {})
			if err != nil {
				t.Errorf("caller %d refused: %v", i, err)
				return
			}
			if rec != nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	start.Done()
	done.Wait()
	if winners != 1 {
		t.Fatalf("%d runs started for one idempotency key, want exactly 1", winners)
	}
}

func runIDFor(i int) string { return "run-" + string(rune('a'+i%26)) + string(rune('a'+i/26)) }
