//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// newProcessGroup makes the command lead a process group of its own, so
// that it and everything it starts can be signalled together.
func newProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate asks the command's process group to end.
func terminate(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// kill ends the command's process group at once.
func kill(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
