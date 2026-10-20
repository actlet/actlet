//go:build !windows

package supervise

import (
	"os/exec"
	"syscall"
)

func leadsGroup(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && (cmd.SysProcAttr.Setpgid && cmd.SysProcAttr.Pgid == 0 || cmd.SysProcAttr.Setsid)
}

func killGroup(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }
