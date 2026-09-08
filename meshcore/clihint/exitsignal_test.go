package clihint

import "testing"

// Tests for the MEASURED exit-status channel — the one that survives a provider rewording its error.
//
// The measurements behind them (2026-08-12; claude 2.1.227, codex-cli 0.147.0, gemini 0.54.4,
// darwin/arm64) are recorded on exitSignals. These tests pin the two things that matter about the
// table: that its one row works when the prose is gone, and that it stays one row.

// TestExitSignal_ClassifiesWhenTheWordingIsGone is the point of the channel. Gemini's trust refusal is
// matched by prose today; when Google rewrites that sentence, the status still names the cause.
func TestExitSignal_ClassifiesWhenTheWordingIsGone(t *testing.T) {
	// A future gemini that refuses the same way in words no matcher knows.
	unmatched := "workspace verification did not complete; see https://example.invalid/docs\n"
	if got := First(unmatched, ""); got != "" {
		t.Fatalf("precondition: this text was supposed to match nothing, got %q", got)
	}
	got := ForFailure(Failure{Stderr: unmatched, ExitCode: 55, Adapter: "gemini-cli"})
	if got != FolderTrust {
		t.Errorf("ForFailure = %q, want %q — the measured exit status is what is left when the prose changes", got, FolderTrust)
	}
	// The same status from a DIFFERENT CLI says nothing: 55 is a fact about gemini, not about exit codes.
	if got := ForFailure(Failure{Stderr: unmatched, ExitCode: 55, Adapter: "codex-cli"}); got != "" {
		t.Errorf("codex exit 55 classified as %q; the table is per adapter and nothing was measured for codex", got)
	}
	// And with no adapter named, only the text channels are consulted — never a guessed table.
	if got := ForFailure(Failure{Stderr: unmatched, ExitCode: 55}); got != "" {
		t.Errorf("an unattributed exit 55 classified as %q, want no signal", got)
	}
}

// TestExitSignal_TextOutranksTheTable pins the channel ORDER. The two agree wherever both speak today,
// so the order only decides a disagreement — and there the provider's account of THIS call beats a
// status measured once, months earlier, against one version.
func TestExitSignal_TextOutranksTheTable(t *testing.T) {
	// gemini exits 55 (the trust code) while telling us plainly that it is out of quota.
	stderr := "Error: you have exceeded your current quota for this model; try again later\n"
	got := ForFailure(Failure{Stderr: stderr, ExitCode: 55, Adapter: "gemini-cli"})
	if got != QuotaExhausted {
		t.Errorf("ForFailure = %q, want %q — a stale table row must not overrule the provider saying what went wrong", got, QuotaExhausted)
	}
}

// TestExitSignal_TheTableCarriesOnlyMeasuredRows guards the table against being "completed" with
// plausible values. Every entry named here was forced against the installed CLI and observed; the
// measurement also established that every OTHER blocker exits 1, which each CLI shares with a mistyped
// flag — so an added row is almost certainly a guess, and a guess here re-creates #49.
func TestExitSignal_TheTableCarriesOnlyMeasuredRows(t *testing.T) {
	measured := map[string]map[int]Signal{
		"gemini-cli": {55: FolderTrust},
	}
	for adapter, rows := range exitSignals {
		want, known := measured[adapter]
		if !known {
			t.Errorf("exitSignals has an entry for %q that no recorded measurement covers — add the measurement (date + CLI version) or remove the row", adapter)
			continue
		}
		for code, sig := range rows {
			if want[code] != sig {
				t.Errorf("exitSignals[%q][%d] = %q, but the recorded measurement says %q", adapter, code, sig, want[code])
			}
		}
	}
	// Specifically: 1 is every measured CLI's generic failure status (it is also what a mistyped flag
	// produces on claude and gemini), so it can never identify a cause.
	for adapter := range exitSignals {
		if s := ExitSignal(adapter, 1); s != "" {
			t.Errorf("exitSignals[%q][1] = %q — exit 1 means \"something went wrong\" on every CLI measured; mapping it manufactures confidence", adapter, s)
		}
	}
}
