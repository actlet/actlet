// Package crashpoint lets crash tests kill the process at a chosen point
// in the code, to test recovery from exactly there. Points do nothing
// unless a test sets one.
package crashpoint

import (
	"os"
	"sync/atomic"
)

var point atomic.Pointer[string]

// Set makes Hit kill the process at the named point. For tests only.
func Set(name string) { point.Store(&name) }

// Hit kills the process, as SIGKILL does, if the named point was set.
func Hit(name string) {
	if p := point.Load(); p == nil || *p != name {
		return
	}
	if proc, err := os.FindProcess(os.Getpid()); err == nil {
		_ = proc.Kill()
	}
	select {} // until the kill lands
}
