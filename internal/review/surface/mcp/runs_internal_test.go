package mcp

import (
	"sync"
	"testing"
)

// The registry's reservation is where applying an accepted set at most once is decided; these tests
// exercise it directly rather than through goroutine scheduling at the transport.

// Many goroutines reserve one source run; exactly one is admitted and the rest receive the winner.
func TestRegistry_ReserveIsAtomicPerSourceRun(t *testing.T) {
	const callers = 64
	rg := newRegistry()
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	var mu sync.Mutex
	var winners, attached int
	var winnerID string
	for i := range callers {
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

// The same property for the idempotency key.
func TestRegistry_ReserveIsAtomicPerKey(t *testing.T) {
	const callers = 32
	rg := newRegistry()
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	var mu sync.Mutex
	winners := 0
	for i := range callers {
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
