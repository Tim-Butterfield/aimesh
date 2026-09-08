//go:build !windows

package acpagent

import (
	"os"
	"syscall"
)

// sysProcAttr starts the child in its own process group so a timeout/cancel can
// terminate the ACP server and any grandchildren it spawned, not just the direct child.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// killProcessGroup asks the child's whole process group (negative pid) to terminate with
// SIGTERM so the cmd.WaitDelay grace period is honored; os/exec force-kills past WaitDelay.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, syscall.SIGTERM)
}
