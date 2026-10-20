//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package journal

import (
	"errors"
	"testing"
)

func TestSecondWriterRefused(t *testing.T) {
	path := tempJournal(t)
	w := mustCreate(t, path, testOptions())
	if !w.Locked() {
		t.Fatal("Locked() = false on a local filesystem")
	}
	appendN(t, w, 1)

	// A second open in the same process gets its own open file
	// description, so flock refuses it just as it would another process.
	if _, err := Open(path, testOptions()); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: err = %v, want ErrLocked", err)
	}
	// The refused open must not have damaged the journal.
	appendN(t, w, 1)
	mustClose(t, w)

	// Closing releases the lock.
	w = mustOpen(t, path, testOptions())
	appendN(t, w, 1)
	mustClose(t, w)
	checkRecords(t, readAll(t, path), 3)
}
