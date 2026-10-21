//go:build !windows

// Not on Windows: it looks for a process with signal 0, which Windows
// doesn't have.

package tools

import "syscall"

func processAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}
