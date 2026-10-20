//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package journal

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking flock(2) on f. The lock belongs
// to the open file description: it is released when f is closed or the
// process dies, and a second open of the same file, even in this process,
// can't take it.
//
// Any failure other than "already locked" (for example ENOLCK or ENOTSUP on
// a filesystem without lock support) is returned as an error: the writer
// fails closed rather than run unprotected.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return fmt.Errorf("%w: %s", ErrLocked, f.Name())
		default:
			return fmt.Errorf("journal: lock %s: %w", f.Name(), err)
		}
	}
}
