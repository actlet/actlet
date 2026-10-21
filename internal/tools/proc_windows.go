package tools

import (
	"os/exec"

	"github.com/actlet/actlet/internal/supervise"
)

// Windows has no process groups to signal. supervise.Start puts the
// command in a job object of its own, with everything it starts, and
// ending it ends the job; there is no gentler way to ask a console
// program to stop.

func newProcessGroup(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) { kill(cmd) }

func kill(cmd *exec.Cmd) {
	supervise.KillGroup(cmd)
	_ = cmd.Process.Kill() // in case it has no job
}
