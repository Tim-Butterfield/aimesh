//go:build windows

package shell

import (
	"os"
	"syscall"
)

// createNewProcessGroup (CREATE_NEW_PROCESS_GROUP) puts the child in its own group
// so it can be terminated as a unit on timeout/cancel.
const createNewProcessGroup = 0x00000200

func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

// killProcessGroup terminates the child (and, with the new group, its tree).
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
