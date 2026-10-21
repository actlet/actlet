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

// Programs start as they are, in the process group they asked for.

func prepare(*exec.Cmd) {}

func started(*exec.Cmd) {}

func releaseGroup(int) {}
