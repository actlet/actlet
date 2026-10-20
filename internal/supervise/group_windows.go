package supervise

import "os/exec"

// Windows has no process groups to signal; tools end their own process.

func leadsGroup(*exec.Cmd) bool { return false }

func groupAlive(int) bool { return false }

func killGroup(int) {}
