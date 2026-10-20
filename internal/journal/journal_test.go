package journal

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	for _, codec := range []CodecID{CodecNone, CodecZstd} {
		t.Run(codec.String(), func(t *testing.T) {
			path := tempJournal(t)
			opts := testOptions()
			opts.Codec = codec
			w := mustCreate(t, path, opts)
			if w.LastSeq() != 0 {
				t.Fatalf("LastSeq of new journal = %d", w.LastSeq())
			}
			appendN(t, w, 20)
			mustClose(t, w)

			recs := readAll(t, path)
			checkRecords(t, recs, 20)

			// Reopening continues the sequence.
			w = mustOpen(t, path, opts)
			if got := w.Recovery(); got.LastSeq != 20 || got.Batches != 20 || got.Tail != TailClean {
				t.Fatalf("Recovery = %+v", got)
			}
			appendN(t, w, 5)
			mustClose(t, w)
			checkRecords(t, readAll(t, path), 25)
		})
	}
}

func TestCreateWithSync(t *testing.T) {
	// The other tests skip syncing for speed; this one exercises the real
	// file and directory sync paths.
	path := tempJournal(t)
	w := mustCreate(t, path, DefaultOptions("synced"))
	appendN(t, w, 3)
	if s := w.Stats(); s.Batches != 3 || s.SyncTime <= 0 {
		t.Errorf("Stats = %+v", s)
	}
	mustClose(t, w)
	w = mustOpen(t, path, DefaultOptions("synced"))
	appendN(t, w, 1)
	mustClose(t, w)
	checkRecords(t, readAll(t, path), 4)
}

func TestCreateRefusesExistingFile(t *testing.T) {
	path := tempJournal(t)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(path, testOptions()); err == nil {
		t.Fatal("Create over an existing file succeeded")
	}
	if b, _ := os.ReadFile(path); string(b) != "x" {
		t.Fatalf("existing file changed to %q", b)
	}
}

func TestOpenChecksSessionID(t *testing.T) {
	path := tempJournal(t)
	mustClose(t, mustCreate(t, path, testOptions()))

	opts := testOptions()
	opts.SessionID = "other"
	if _, err := Open(path, opts); err == nil {
		t.Fatal("Open with a different session ID succeeded")
	}
	opts.SessionID = ""
	w := mustOpen(t, path, opts)
	if got := w.Header().SessionID; got != "test-session" {
		t.Errorf("SessionID = %q", got)
	}
	mustClose(t, w)
}

func TestSessionIDValidation(t *testing.T) {
	for _, id := range []string{"", string(make([]byte, MaxSessionIDLen+1)), "\xff"} {
		opts := testOptions()
		opts.SessionID = id
		if _, err := Create(tempJournal(t), opts); err == nil {
			t.Errorf("Create with session ID %q succeeded", id)
		}
	}
}

func TestBatchingGroupsConcurrentAppends(t *testing.T) {
	path := tempJournal(t)
	opts := testOptions()
	opts.Batch = BatchPolicy{MaxRecords: 8, MaxDelay: time.Second}
	w := mustCreate(t, path, opts)

	// With a long MaxDelay, 32 concurrent appends fill exactly 4 batches.
	var wg sync.WaitGroup
	seqs := make(chan uint64, 32)
	for range 32 {
		wg.Go(func() {
			seq, err := w.Append(context.Background(), "test.concurrent", []byte("x"))
			if err != nil {
				t.Error(err)
			}
			seqs <- seq
		})
	}
	wg.Wait()
	close(seqs)
	seen := map[uint64]bool{}
	for s := range seqs {
		seen[s] = true
	}
	for s := uint64(1); s <= 32; s++ {
		if !seen[s] {
			t.Errorf("seq %d missing", s)
		}
	}
	if s := w.Stats(); s.Batches != 4 || s.Records != 32 {
		t.Errorf("Stats = %+v, want 4 batches of 8", s)
	}
	mustClose(t, w)
}

func TestBatchingRespectsMaxBytes(t *testing.T) {
	path := tempJournal(t)
	opts := testOptions()
	opts.Batch = BatchPolicy{MaxRecords: 100, MaxBytes: 4 << 10, MaxDelay: 200 * time.Millisecond}
	w := mustCreate(t, path, opts)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := w.Append(context.Background(), "test.big", make([]byte, 1500)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	mustClose(t, w)

	f, size := openRead(t, path)
	r, err := NewReader(f, size, ReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	total := 0
	for {
		b, err := r.Next()
		if err != nil {
			break
		}
		if b.RawLen > 4<<10 {
			t.Errorf("batch raw size %d exceeds MaxBytes", b.RawLen)
		}
		total += int(b.Count)
	}
	if total != 10 {
		t.Errorf("read %d records, want 10", total)
	}
}

func TestIncompressibleBatchStoredRaw(t *testing.T) {
	path := tempJournal(t)
	opts := testOptions()
	w := mustCreate(t, path, opts)
	data := make([]byte, 64<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(context.Background(), "random", data); err != nil {
		t.Fatal(err)
	}
	mustClose(t, w)

	f, size := openRead(t, path)
	r, err := NewReader(f, size, ReaderOptions{Limits: opts.Limits})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.Codec != CodecNone {
		t.Errorf("codec = %v, want none for an incompressible batch", b.Codec)
	}
	if len(b.Records) != 1 || !bytes.Equal(b.Records[0].Data, data) {
		t.Fatal("random record didn't round-trip")
	}
}

func TestAppendValidation(t *testing.T) {
	w := mustCreate(t, tempJournal(t), testOptions())
	defer mustClose(t, w)
	ctx := context.Background()
	l := DefaultLimits()
	if _, err := w.Append(ctx, "", nil); err == nil {
		t.Error("empty kind accepted")
	}
	if _, err := w.Append(ctx, string(make([]byte, l.MaxKindLen+1)), nil); !errors.Is(err, ErrLimit) {
		t.Errorf("long kind: err = %v, want ErrLimit", err)
	}
	if _, err := w.Append(ctx, "k", make([]byte, l.MaxRecordData+1)); !errors.Is(err, ErrLimit) {
		t.Errorf("oversized record: err = %v, want ErrLimit", err)
	}
	// A rejected record doesn't consume a sequence number.
	if seq, err := w.Append(ctx, "k", nil); err != nil || seq != 1 {
		t.Errorf("Append = %d, %v; want 1, nil", seq, err)
	}
}

func TestAppendAfterClose(t *testing.T) {
	w := mustCreate(t, tempJournal(t), testOptions())
	mustClose(t, w)
	if _, err := w.Append(context.Background(), "k", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Append after Close: err = %v, want ErrClosed", err)
	}
	if err := w.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close: err = %v, want ErrClosed", err)
	}
}

func TestAppendCanceledContext(t *testing.T) {
	w := mustCreate(t, tempJournal(t), testOptions())
	defer mustClose(t, w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.Append(ctx, "k", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestOptionsValidation(t *testing.T) {
	bad := []func(*Options){
		func(o *Options) { o.Codec = 9 },
		func(o *Options) { o.Batch.MaxRecords = -1 },
		func(o *Options) { o.Batch.MaxRecords = 1 << 20 },
		func(o *Options) { o.Batch.MaxBytes = 1 << 30 },
		func(o *Options) { o.Batch.MaxDelay = -time.Second },
		func(o *Options) {
			o.Limits = Limits{MaxBatchRaw: 10, MaxBatchStored: 10, MaxBatchRecords: 1, MaxRecordData: 10, MaxKindLen: 1}
		},
	}
	for i, f := range bad {
		o := testOptions()
		f(&o)
		if _, err := Create(tempJournal(t), o); err == nil {
			t.Errorf("case %d: Create succeeded with invalid options", i)
		}
	}
}

func TestLockUnavailableWarnsAndContinues(t *testing.T) {
	var logs bytes.Buffer
	opts := testOptions()
	opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	opts.lock = func(*os.File) error {
		return fmt.Errorf("%w: flock: %w", errLockUnavailable, syscall.ENOTSUP)
	}

	path := tempJournal(t)
	w := mustCreate(t, path, opts)
	if w.Locked() {
		t.Error("Locked() = true without a lock")
	}
	appendN(t, w, 2)
	mustClose(t, w)
	w = mustOpen(t, path, opts)
	appendN(t, w, 1)
	mustClose(t, w)
	checkRecords(t, readAll(t, path), 3)

	if got := strings.Count(logs.String(), "level=WARN"); got != 2 {
		t.Errorf("logged %d warnings, want one per open:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "continuing without it") {
		t.Errorf("warning doesn't say the writer continues:\n%s", logs.String())
	}
}

func TestLockHeldByAnotherWriterIsRefused(t *testing.T) {
	path := tempJournal(t)
	mustClose(t, mustCreate(t, path, testOptions()))
	opts := testOptions()
	opts.lock = func(f *os.File) error { return fmt.Errorf("%w: %s", ErrLocked, f.Name()) }
	if _, err := Open(path, opts); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open: err = %v, want ErrLocked", err)
	}
}

func TestNoSyncSkipsBatchSyncs(t *testing.T) {
	path := tempJournal(t)
	opts := DefaultOptions("nosync")
	opts.NoSync = true
	w := mustCreate(t, path, opts)
	appendN(t, w, 3)
	if s := w.Stats(); s.Batches != 3 || s.SyncTime != 0 {
		t.Errorf("Stats = %+v, want 3 unsynced batches", s)
	}
	mustClose(t, w) // syncs once
	checkRecords(t, readAll(t, path), 3)
}

func TestAppendBatch(t *testing.T) {
	path := tempJournal(t)
	opts := testOptions()
	opts.Batch = BatchPolicy{MaxRecords: 4}
	w := mustCreate(t, path, opts)
	appendN(t, w, 1)

	recs := make([]Record, 10)
	for i := range recs {
		recs[i] = Record{Kind: "test.record", Data: recordData(uint64(i + 2))}
	}
	first, err := w.AppendBatch(context.Background(), recs)
	if err != nil || first != 2 {
		t.Fatalf("AppendBatch = %d, %v; want 2, nil", first, err)
	}
	// 10 records with at most 4 per batch: 3 batches, plus the first append.
	if s := w.Stats(); s.Batches != 4 || s.Records != 11 {
		t.Errorf("Stats = %+v, want 4 batches of 11 records", s)
	}
	mustClose(t, w)
	checkRecords(t, readAll(t, path), 11)

	if _, err := w.AppendBatch(context.Background(), nil); err == nil {
		t.Error("empty AppendBatch succeeded")
	}
}
