//go:build !windows

package runner

import (
	"syscall"
	"time"
)

// cpuTime returns the CPU time the process has used.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
