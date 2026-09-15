package cli

// This file maps an error to the process exit code. Errors carry their class from the shared meshcore
// fault taxonomy (docs/architecture.md, "Halt taxonomy"), so a script can tell a missing binary from a
// malformed profile from an internal bug.

import (
	"errors"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/manager"
)

// codeOf returns the exit code for err. A manager.BlockedError, which is not a *fault.Fault, is a
// configuration error (3); every other error uses fault.CodeOf, so an unclassified error is Internal (8).
func codeOf(err error) int {
	if err == nil {
		return int(fault.OK)
	}
	if _, ok := errors.AsType[*manager.BlockedError](err); ok {
		return int(fault.Config)
	}
	return int(fault.CodeOf(err))
}
