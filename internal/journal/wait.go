package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// WaitUnlocked waits until no writer holds the journal at path, such as a
// previous run of the session that is still shutting down, or ctx ends,
// when it returns an error wrapping ErrLocked. A journal that doesn't
// exist, or whose filesystem can't lock, isn't waited for. Recovery reads
// a journal only after waiting, so that it reads what the previous writer
// wrote last.
func WaitUnlocked(ctx context.Context, path string) error {
	for {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("journal: open: %w", err)
		}
		err = lockFile(f)
		_ = f.Close() // releases the lock, if it was taken
		if !errors.Is(err, ErrLocked) {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(200 * time.Millisecond):
		}
	}
}
