package journal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

// fixture writes a journal of n single-record batches and returns its bytes.
func fixture(t *testing.T, n int) []byte {
	t.Helper()
	path := tempJournal(t)
	w := mustCreate(t, path, testOptions())
	appendN(t, w, n)
	mustClose(t, w)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Cutting the file at any offset after the header must recover exactly the
// batches that are complete before the cut, report a torn tail when the cut
// falls inside a batch, and let appending resume after truncation.
func TestTruncationAtEveryOffset(t *testing.T) {
	const n = 4
	data := fixture(t, n)
	ends := batchEnds(t, data)
	headerLen := int64(len(encodeFileHeader(Header{SessionID: "test-session"})))

	for cut := headerLen; cut <= int64(len(data)); cut++ {
		// Expected committed prefix: the batches that end at or before cut.
		wantEnd, wantSeq := headerLen, uint64(0)
		for i, e := range ends {
			if e <= cut {
				wantEnd, wantSeq = e, uint64(i+1)
			}
		}
		res, err := scanBytes(data[:cut], ReaderOptions{})
		if err != nil {
			t.Fatalf("cut %d: Scan: %v", cut, err)
		}
		if res.ValidEnd != wantEnd || res.LastSeq != wantSeq {
			t.Fatalf("cut %d: ValidEnd=%d LastSeq=%d, want %d %d", cut, res.ValidEnd, res.LastSeq, wantEnd, wantSeq)
		}
		if torn := cut != wantEnd; (res.Tail == TailTorn) != torn {
			t.Fatalf("cut %d: Tail=%v, want torn=%v", cut, res.Tail, torn)
		}

		// Reopen for writing: the torn tail is truncated and the next
		// append continues the sequence.
		path := tempJournal(t)
		if err := os.WriteFile(path, data[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		w := mustOpen(t, path, testOptions())
		appendN(t, w, 1)
		mustClose(t, w)
		checkRecords(t, readAll(t, path), int(wantSeq)+1)
	}
}

func TestTruncatedHeaderIsCorrupt(t *testing.T) {
	data := fixture(t, 1)
	headerLen := len(encodeFileHeader(Header{SessionID: "test-session"}))
	for cut := range headerLen {
		if _, err := scanBytes(data[:cut], ReaderOptions{}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("cut %d: err = %v, want ErrCorrupt", cut, err)
		}
	}
}

// Flipping any single bit anywhere in a journal of complete batches must be
// reported as corruption: never silently skipped, never mistaken for a torn
// tail and truncated.
func TestBitFlipsAreCorruption(t *testing.T) {
	data := fixture(t, 3)
	for i := range data {
		for bit := range 8 {
			c := bytes.Clone(data)
			c[i] ^= 1 << bit
			_, err := scanBytes(c, ReaderOptions{})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("flip byte %d bit %d: err = %v, want ErrCorrupt", i, bit, err)
			}
		}
	}
}

func TestOpenRefusesCorruptJournal(t *testing.T) {
	data := fixture(t, 3)
	ends := batchEnds(t, data)
	data[ends[0]+batchHeaderLen+1] ^= 0xff // payload of the second batch

	path := tempJournal(t)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path, testOptions())
	var fe *FormatError
	if !errors.Is(err, ErrCorrupt) || !errors.As(err, &fe) || fe.Offset != ends[0] {
		t.Fatalf("Open: err = %v, want ErrCorrupt at offset %d", err, ends[0])
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, data) {
		t.Fatal("Open modified a corrupt journal")
	}
}

func TestZeroFilledTailIsTorn(t *testing.T) {
	data := fixture(t, 2)
	ends := batchEnds(t, data)

	cases := map[string][]byte{
		// The filesystem extended the file but no data reached it.
		"zeros after last batch": append(bytes.Clone(data), make([]byte, 4096)...),
		// A batch whose header and part of its payload arrived.
		"partial batch then zeros": append(bytes.Clone(data[:ends[0]+batchHeaderLen+3]), make([]byte, 4096)...),
		// A full-length batch whose trailer never arrived.
		"batch without trailer": func() []byte {
			c := bytes.Clone(data)
			copy(c[len(c)-batchTrailerLen:], []byte{0, 0, 0, 0})
			return c
		}(),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := scanBytes(c, ReaderOptions{})
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if res.Tail != TailTorn {
				t.Fatalf("Tail = %v, want torn", res.Tail)
			}
			if res.ValidEnd != ends[0] && res.ValidEnd != ends[1] {
				t.Fatalf("ValidEnd = %d, want a batch boundary", res.ValidEnd)
			}
		})
	}
}

func TestGarbageAfterLastBatchIsCorrupt(t *testing.T) {
	data := append(fixture(t, 2), []byte("garbage that is not a batch header..")...)
	if _, err := scanBytes(data, ReaderOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestUnsupportedVersion(t *testing.T) {
	h := encodeFileHeader(Header{Version: 7, SessionID: "s"})
	if _, err := scanBytes(h, ReaderOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestReaderSeesLiveJournal(t *testing.T) {
	path := tempJournal(t)
	w := mustCreate(t, path, testOptions())
	defer mustClose(t, w)
	appendN(t, w, 2)
	// No lock is needed to read while the writer is open.
	if got := readAll(t, path); len(got) != 2 {
		t.Fatalf("read %d records from a live journal, want 2", len(got))
	}
	appendN(t, w, 1)
	if _, err := w.Append(context.Background(), "test.record", recordData(4)); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, path); len(got) != 4 {
		t.Fatalf("read %d records, want 4", len(got))
	}
}
