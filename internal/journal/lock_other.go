//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package journal

import (
	"fmt"
	"os"
	"runtime"
)

// lockFile fails on platforms without flock(2) support: the writer refuses
// to run without exclusive ownership.
func lockFile(f *os.File) error {
	return fmt.Errorf("%w: exclusive file locking on %s", ErrUnsupported, runtime.GOOS)
}
