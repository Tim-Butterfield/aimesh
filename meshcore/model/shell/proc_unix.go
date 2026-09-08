//go:build !windows

package shell

import (
	"os"
	"syscall"
)

// sysProcAttr starts the child in its own process group so a timeout/cancel can
// terminate the CLI and any grandchildren it spawned, not just the direct child.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// killProcessGroup asks the child's whole process group (negative pid) to
// terminate. It sends SIGTERM (not SIGKILL) so the grace period from cmd.WaitDelay
// is honored — the CLI can clean up; os/exec force-kills the process if it ignores
// the signal past WaitDelay.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, syscall.SIGTERM)
}
