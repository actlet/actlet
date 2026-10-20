//go:build !linux && !windows

package supervise

import (
	"errors"
	"syscall"
)

// groupAlive reports whether a process group has members left.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
