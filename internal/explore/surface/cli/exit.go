package cli

// This file is exploremesh's PRINT-BOUNDARY exit-code mapping. Before it, every exploremesh failure
// collapsed to exit 1 regardless of what went wrong, so a script could not tell a missing binary from a
// malformed profile from an internal bug — while `reviewmesh` had exposed the shared meshcore fault
// taxonomy (0/1/2/3/4/5/6/7/8) since its first release. That asymmetry is now gone: BOTH binaries map an
// error to the SAME table (docs/architecture.md "Halt taxonomy"), and they do it the same way — the error
// carries its class (meshcore/fault) and the surface merely reads it here.
//
// This is a DELIBERATE breaking change to exploremesh's exit codes. Anything scripting `exploremesh` on
// "non-zero means failure" is unaffected; anything scripting it on the literal 1 must move to the table.

import (
	"errors"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/manager"
)

// codeOf maps a returned error to its process exit code. It is `fault.CodeOf` plus the ONE app-side
// error type that is deliberately not a *fault.Fault: manager.BlockedError, the "cannot remove — the
// roster still uses it" refusal, which a web surface renders as a 409 and a terminal renders as a
// configuration error (exit 3). Every other error answers for itself: a *fault.Fault yields its class,
// and an unclassified error yields Internal (8) — the same catch-all reviewmesh has always had, and the
// signal that a code path still needs classifying.
func codeOf(err error) int {
	if err == nil {
		return int(fault.OK)
	}
	if _, ok := errors.AsType[*manager.BlockedError](err); ok {
		return int(fault.Config)
	}
	return int(fault.CodeOf(err))
}
