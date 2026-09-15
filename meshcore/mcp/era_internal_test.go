package mcp

import (
	"sync"
	"testing"
)

// The era latch's atomicity under real concurrency. The stdio read loop admits frames serially, so the
// wire-level tests in era_test.go never race the latch; this test does, so exactly one winner is a
// property of the compare-and-set rather than of the dispatcher.

func TestLatchEra_ExactlyOneWinnerUnderRealConcurrency(t *testing.T) {
	for run := range 200 {
		s := &Server{}
		const n = 8
		var start sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		results := make([]Era, n)
		for i := range n {
			done.Add(1)
			want := EraModern
			if i%2 == 0 {
				want = EraLegacy
			}
			go func(i int, want Era) {
				defer done.Done()
				start.Wait()
				results[i] = s.latchEra(want)
			}(i, want)
		}
		start.Done()
		done.Wait()

		// EVERY caller must agree on the era in force afterwards — the winner because it set it, and
		// each loser because it RE-READ it rather than erroring on the race.
		final := s.era()
		if final != EraLegacy && final != EraModern {
			t.Fatalf("run %d: the latch settled on %q", run, final)
		}
		for i, got := range results {
			if got != final {
				t.Fatalf("run %d: caller %d was told the era is %q while the cell holds %q — two writers of one cell is how they come to disagree", run, i, got, final)
			}
		}
		// And the cell never moves again.
		if again := s.latchEra(EraLegacy); again != final {
			t.Fatalf("run %d: a later opener re-latched %q over %q", run, again, final)
		}
	}
}

// The `--protocol legacy` mode is a launch posture, not a per-request one: it must be unaffected by
// anything a client sends.
func TestMode_DefaultsToDualAndIsNotDerivedFromTheLatch(t *testing.T) {
	s := &Server{}
	if s.Mode() != ProtocolDual {
		t.Fatalf("Mode() = %q on a zero server, want %q — an unset posture must be the permissive-to-both default, not an accidental pin", s.Mode(), ProtocolDual)
	}
	s.latchEra(EraModern)
	if s.Mode() != ProtocolDual {
		t.Fatalf("Mode() = %q after a modern latch — the mode is what the OPERATOR chose and no client may change it", s.Mode())
	}
	if (&Server{Protocol: ProtocolLegacy}).Mode() != ProtocolLegacy {
		t.Fatal("an explicit legacy posture was not honoured")
	}
}

func TestValidProtocolMode(t *testing.T) {
	for _, ok := range []string{"", "dual", "legacy", "LEGACY", " dual "} {
		if !ValidProtocolMode(ok) {
			t.Errorf("ValidProtocolMode(%q) = false", ok)
		}
	}
	// There is deliberately no `modern`: a modern-only pin would strand legacy hosts, which have no
	// fall-forward mechanism, and buys nothing an operator cannot get by not sending `initialize`. An
	// operator who types it must be told, not quietly given `dual`.
	for _, bad := range []string{"modern", "2026-07-28", "both", "off"} {
		if ValidProtocolMode(bad) {
			t.Errorf("ValidProtocolMode(%q) = true", bad)
		}
	}
}
