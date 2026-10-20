package tools

import "os/exec"

// Windows has no process groups to signal: the command's own process is
// ended, and with it usually its console children.

func newProcessGroup(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) { _ = cmd.Process.Kill() }

func kill(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
