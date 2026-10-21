package journal

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockOffset is where the lock is taken: one byte far past the end of any
// journal. Windows' locks are mandatory, so a lock on the data itself
// would keep readers, such as a fork or a viewer, from reading it.
const lockOffset = 1 << 62

// lockFile takes an exclusive, non-blocking LockFileEx lock on f. Like
// flock(2) elsewhere, it belongs to the open handle: it is released when f
// is closed or the process dies, and a second open of the same file, even
// in this process, can't take it.
//
// It returns an error wrapping ErrLocked if another writer holds the lock,
// and one wrapping errLockUnavailable if the filesystem can't lock at all.
func lockFile(f *os.File) error {
	ol := &windows.Overlapped{Offset: uint32(lockOffset & 0xffffffff), OffsetHigh: uint32(lockOffset >> 32)}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		return fmt.Errorf("%w: %s", ErrLocked, f.Name())
	default:
		return fmt.Errorf("%w: LockFileEx: %w", errLockUnavailable, err)
	}
}
