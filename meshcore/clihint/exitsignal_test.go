package clihint

import "testing"

// Tests for the measured exit-status channel, which survives a provider rewording its error. The
// observations behind it are recorded on exitSignals.

// An exit status still names the cause when the provider's wording no longer matches.
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
	// The same status from a different CLI says nothing: 55 is a fact about gemini, not about exit codes.
	if got := ForFailure(Failure{Stderr: unmatched, ExitCode: 55, Adapter: "codex-cli"}); got != "" {
		t.Errorf("codex exit 55 classified as %q; the table is per adapter and nothing was measured for codex", got)
	}
	// And with no adapter named, only the text channels are consulted — never a guessed table.
	if got := ForFailure(Failure{Stderr: unmatched, ExitCode: 55}); got != "" {
		t.Errorf("an unattributed exit 55 classified as %q, want no signal", got)
	}
}

// When text and the exit-status table disagree, the provider's text about this call wins over a status
// observed against one CLI version.
func TestExitSignal_TextOutranksTheTable(t *testing.T) {
	// gemini exits 55 (the trust code) while telling us plainly that it is out of quota.
	stderr := "Error: you have exceeded your current quota for this model; try again later\n"
	got := ForFailure(Failure{Stderr: stderr, ExitCode: 55, Adapter: "gemini-cli"})
	if got != QuotaExhausted {
		t.Errorf("ForFailure = %q, want %q — a stale table row must not overrule the provider saying what went wrong", got, QuotaExhausted)
	}
}

// The table holds only observed rows. Every other blocker exits 1, which each CLI shares with a mistyped
// flag, so an unobserved row would be a guess that misclassifies failures.
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
