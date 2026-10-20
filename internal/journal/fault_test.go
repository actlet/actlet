package journal

import (
	"context"
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
)

// faultyFile injects failures into the writes and syncs of a real file.
type faultyFile struct {
	file
	writes, syncs atomic.Int32
	// failWrite and failSync, if set, decide the outcome of the n-th call
	// (1-based) instead of the real file.
	failWrite func(n int32, f file, p []byte, off int64) (int, error)
	failSync  func(n int32) error
}

func (f *faultyFile) WriteAt(p []byte, off int64) (int, error) {
	n := f.writes.Add(1)
	if f.failWrite != nil {
		return f.failWrite(n, f.file, p, off)
	}
	return f.file.WriteAt(p, off)
}

func (f *faultyFile) Sync() error {
	n := f.syncs.Add(1)
	if f.failSync != nil {
		if err := f.failSync(n); err != nil {
			return err
		}
	}
	return f.file.Sync()
}

// The first three batches succeed; the fourth hits the fault.
const faultAt = 4

func TestWriteAndSyncFailures(t *testing.T) {
	cases := []struct {
		name string
		ff   *faultyFile
		// wantCause is matched with errors.Is against the append error.
		wantCause error
		// tail is the expected state on reopen.
		tail TailState
		// fourthPresent says whether the unacknowledged record survives.
		fourthPresent bool
	}{
		{
			name: "disk full",
			ff: &faultyFile{failWrite: func(n int32, f file, p []byte, off int64) (int, error) {
				if n == faultAt {
					return 0, syscall.ENOSPC
				}
				return f.WriteAt(p, off)
			}},
			wantCause: syscall.ENOSPC,
			tail:      TailClean,
		},
		{
			name: "short write",
			ff: &faultyFile{failWrite: func(n int32, f file, p []byte, off int64) (int, error) {
				if n == faultAt {
					// Half the batch reaches the file and no error is
					// returned: the writer must notice the short count.
					return f.WriteAt(p[:len(p)/2], off)
				}
				return f.WriteAt(p, off)
			}},
			tail: TailTorn,
		},
		{
			name: "short write with ENOSPC",
			ff: &faultyFile{failWrite: func(n int32, f file, p []byte, off int64) (int, error) {
				if n == faultAt {
					m, _ := f.WriteAt(p[:len(p)-3], off)
					return m, syscall.ENOSPC
				}
				return f.WriteAt(p, off)
			}},
			wantCause: syscall.ENOSPC,
			tail:      TailTorn,
		},
		{
			name: "sync failure",
			ff: &faultyFile{failSync: func(n int32) error {
				if n == faultAt {
					return syscall.EIO
				}
				return nil
			}},
			wantCause: syscall.EIO,
			tail:      TailClean,
			// The batch was written but never acknowledged: it is present
			// on reopen here. Callers must treat it as uncertain.
			fourthPresent: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tempJournal(t)
			mustClose(t, mustCreate(t, path, testOptions()))

			opts := DefaultOptions("test-session") // real syncs, so failSync runs
			opts.wrap = func(f file) file { tc.ff.file = f; return tc.ff }
			w := mustOpen(t, path, opts)
			appendN(t, w, faultAt-1)

			_, err := w.Append(context.Background(), "test.record", recordData(faultAt))
			if !errors.Is(err, ErrNotDurable) {
				t.Fatalf("faulty append: err = %v, want ErrNotDurable", err)
			}
			if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
				t.Fatalf("faulty append: err = %v, want cause %v", err, tc.wantCause)
			}
			if w.LastSeq() != faultAt-1 {
				t.Fatalf("LastSeq = %d after failure, want %d", w.LastSeq(), faultAt-1)
			}

			// The writer refuses to continue, without touching the file.
			writes := tc.ff.writes.Load()
			if _, err := w.Append(context.Background(), "test.record", recordData(faultAt)); !errors.Is(err, ErrWriterFailed) {
				t.Fatalf("append after failure: err = %v, want ErrWriterFailed", err)
			}
			if tc.ff.writes.Load() != writes {
				t.Fatal("writer wrote to the file after a failure")
			}
			if err := w.Close(); !errors.Is(err, ErrWriterFailed) {
				t.Fatalf("Close: err = %v, want ErrWriterFailed", err)
			}

			// Reopening recovers every acknowledged record.
			w = mustOpen(t, path, testOptions())
			rec := w.Recovery()
			if rec.Tail != tc.tail {
				t.Errorf("Recovery tail = %v, want %v", rec.Tail, tc.tail)
			}
			want := uint64(faultAt - 1)
			if tc.fourthPresent {
				want = faultAt
			}
			if rec.LastSeq != want {
				t.Fatalf("recovered LastSeq = %d, want %d", rec.LastSeq, want)
			}
			appendN(t, w, 1)
			mustClose(t, w)
			checkRecords(t, readAll(t, path), int(want)+1)
		})
	}
}

func TestCreateFailsCleanly(t *testing.T) {
	path := tempJournal(t)
	opts := DefaultOptions("s")
	opts.wrap = func(f file) file {
		return &faultyFile{file: f, failSync: func(int32) error { return syscall.EIO }}
	}
	if _, err := Create(path, opts); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Create: err = %v, want EIO", err)
	}
	// The half-created file is removed so that Create can be retried.
	if w, err := Create(path, testOptions()); err != nil {
		t.Fatalf("retry Create: %v", err)
	} else {
		mustClose(t, w)
	}
}
