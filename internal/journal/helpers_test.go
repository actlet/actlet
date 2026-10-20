package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func tempJournal(t testing.TB) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "session.actlet")
}

// testOptions returns fast options for tests that don't exercise syncing.
func testOptions() Options {
	o := DefaultOptions("test-session")
	o.noSync = true
	return o
}

func mustCreate(t testing.TB, path string, opts Options) *Writer {
	t.Helper()
	w, err := Create(path, opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return w
}

func mustOpen(t testing.TB, path string, opts Options) *Writer {
	t.Helper()
	w, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return w
}

func mustClose(t testing.TB, w *Writer) {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// recordData returns deterministic data for the record with the given seq.
func recordData(seq uint64) []byte {
	return fmt.Appendf(nil, "record %d: the quick brown fox jumps over the lazy dog", seq)
}

// appendN appends n records one at a time, so each lands in its own batch,
// and checks that they get consecutive sequence numbers.
func appendN(t testing.TB, w *Writer, n int) {
	t.Helper()
	for range n {
		want := w.LastSeq() + 1
		seq, err := w.Append(context.Background(), "test.record", recordData(want))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if seq != want {
			t.Fatalf("Append seq = %d, want %d", seq, want)
		}
	}
}

// readAll reads every record of the journal at path and fails the test on
// any error other than a torn tail.
func readAll(t testing.TB, path string) []Record {
	t.Helper()
	f, size := openRead(t, path)
	r, err := NewReader(f, size, ReaderOptions{})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	var recs []Record
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, ErrTornTail) {
			return recs
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		recs = append(recs, b.Records...)
	}
}

// openRead opens path for reading until the test ends and returns its size.
func openRead(t testing.TB, path string) (*os.File, int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return f, st.Size()
}

// checkRecords verifies that recs are seqs 1..n written by appendN.
func checkRecords(t testing.TB, recs []Record, n int) {
	t.Helper()
	if len(recs) != n {
		t.Fatalf("got %d records, want %d", len(recs), n)
	}
	for i, r := range recs {
		seq := uint64(i + 1)
		if r.Seq != seq || r.Kind != "test.record" || string(r.Data) != string(recordData(seq)) {
			t.Fatalf("record %d = {%d %q %q}, want seq %d", i, r.Seq, r.Kind, r.Data, seq)
		}
	}
}

// batchEnds returns the end offset of every valid batch in the journal.
func batchEnds(t testing.TB, data []byte) []int64 {
	t.Helper()
	r, err := NewReader(bytesReaderAt(data), int64(len(data)), ReaderOptions{SkipRecords: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var ends []int64
	for {
		_, err := r.Next()
		if errors.Is(err, io.EOF) {
			return ends
		}
		if err != nil {
			t.Fatal(err)
		}
		ends = append(ends, r.Offset())
	}
}

type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func scanBytes(data []byte, opts ReaderOptions) (ScanResult, error) {
	return Scan(bytesReaderAt(data), int64(len(data)), opts)
}
