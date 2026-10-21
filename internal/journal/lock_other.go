//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package journal

import (
	"fmt"
	"os"
	"runtime"
)

// lockFile can't lock on platforms without flock(2) or LockFileEx. The
// writer warns and runs without an exclusive lock.
func lockFile(*os.File) error {
	return fmt.Errorf("%w: no file locking on %s", errLockUnavailable, runtime.GOOS)
}
