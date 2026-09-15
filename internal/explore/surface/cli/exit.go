package cli

// This file is exploremesh's PRINT-BOUNDARY exit-code mapping. A script must be able to tell a missing
// binary from a malformed profile from an internal bug, so BOTH apps map an error to the SAME shared
// meshcore fault taxonomy (0/1/2/3/4/5/6/7/8; docs/architecture.md "Halt taxonomy"), and they do it the
// same way — the error carries its class (meshcore/fault) and the surface merely reads it here.

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
