//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows

package journal

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSecondWriterRefused(t *testing.T) {
	path := tempJournal(t)
	w := mustCreate(t, path, testOptions())
	if !w.Locked() {
		t.Fatal("Locked() = false on a local filesystem")
	}
	appendN(t, w, 1)

	// A second open in the same process gets its own open file
	// description, or handle on Windows, so the lock refuses it just as it
	// would another process.
	if _, err := Open(path, testOptions()); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: err = %v, want ErrLocked", err)
	}
	// The refused open must not have damaged the journal, and the lock
	// doesn't keep readers out: on Windows, where locks are mandatory, it
	// covers no data.
	appendN(t, w, 1)
	checkRecords(t, readAll(t, path), 2)
	mustClose(t, w)

	// Closing releases the lock.
	w = mustOpen(t, path, testOptions())
	appendN(t, w, 1)
	mustClose(t, w)
	checkRecords(t, readAll(t, path), 3)
}

// WaitUnlocked waits for a writer to let go, and gives up when told.
func TestWaitUnlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := WaitUnlocked(t.Context(), path); err != nil {
		t.Fatalf("a missing journal: %v", err)
	}
	w, err := Create(path, DefaultOptions("s"))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Locked() {
		t.Skip("this filesystem can't lock")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := WaitUnlocked(ctx, path); !errors.Is(err, ErrLocked) {
		t.Fatalf("while locked: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = w.Close()
	}()
	start := time.Now()
	if err := WaitUnlocked(t.Context(), path); err != nil || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("after %v: %v", time.Since(start), err)
	}
}
