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
// It returns an error wrapping ErrLocked if another writer holds the lock,
// and one wrapping errLockUnavailable if the filesystem can't lock at all
// (for example ENOLCK or ENOTSUP on some network filesystems).
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
			return fmt.Errorf("%w: flock: %w", errLockUnavailable, err)
		}
	}
}
