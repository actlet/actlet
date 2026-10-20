package journal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
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

// Flipping any single bit in the file header or in a batch that another
// batch follows must be reported as corruption, never skipped or truncated.
// The same damage in the final batch is a write that never fully reached the
// disk: it is truncated as a torn tail.
func TestBitFlips(t *testing.T) {
	const n = 3
	data := fixture(t, n)
	ends := batchEnds(t, data)
	lastStart := ends[n-2]
	for i := range data {
		for bit := range 8 {
			c := bytes.Clone(data)
			c[i] ^= 1 << bit
			res, err := scanBytes(c, ReaderOptions{})
			if int64(i) < lastStart {
				if !errors.Is(err, ErrCorrupt) {
					t.Fatalf("flip byte %d bit %d: err = %v, want ErrCorrupt", i, bit, err)
				}
				continue
			}
			if err != nil || res.Tail != TailTorn || res.ValidEnd != lastStart || res.LastSeq != n-1 {
				t.Fatalf("flip byte %d bit %d in the final batch: err=%v tail=%v ValidEnd=%d LastSeq=%d, want torn at %d",
					i, bit, err, res.Tail, res.ValidEnd, res.LastSeq, lastStart)
			}
		}
	}
}

// Power loss can persist an unsynced final batch partially or out of order.
// Damage confined to the final batch is truncated on open; the same damage in
// an earlier batch is corruption and the file is left alone.
func TestPowerLossShapes(t *testing.T) {
	const n = 3
	data := fixture(t, n)
	ends := batchEnds(t, data)
	starts := []int64{int64(len(encodeFileHeader(Header{SessionID: "test-session"}))), ends[0], ends[1]}

	type shape struct {
		name   string
		damage func(c []byte, start, end int64)
	}
	shapes := []shape{
		{"payload zeroed, trailer present", func(c []byte, start, end int64) {
			clear(c[start+batchHeaderLen : end-batchChecksumLen-batchTrailerLen])
		}},
		{"payload garbled", func(c []byte, start, end int64) {
			for i := start + batchHeaderLen; i < end-batchChecksumLen-batchTrailerLen; i++ {
				c[i] ^= 0x5a
			}
		}},
		{"header zeroed, rest present", func(c []byte, start, end int64) {
			clear(c[start : start+batchHeaderLen])
		}},
		{"header garbled", func(c []byte, start, end int64) {
			for i := start; i < start+batchHeaderLen; i++ {
				c[i] ^= 0xa5
			}
		}},
		{"checksum zeroed", func(c []byte, start, end int64) {
			clear(c[end-batchChecksumLen-batchTrailerLen : end-batchTrailerLen])
		}},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			// Final batch: truncated, then appending resumes.
			c := bytes.Clone(data)
			sh.damage(c, starts[n-1], ends[n-1])
			path := tempJournal(t)
			if err := os.WriteFile(path, c, 0o600); err != nil {
				t.Fatal(err)
			}
			w := mustOpen(t, path, testOptions())
			rec := w.Recovery()
			if rec.Tail != TailTorn || rec.ValidEnd != starts[n-1] || rec.Truncated() != ends[n-1]-starts[n-1] {
				t.Fatalf("final batch: Recovery = %+v, want %d bytes truncated at %d", rec, ends[n-1]-starts[n-1], starts[n-1])
			}
			if !strings.Contains(rec.TailErr.Error(), "damaged final batch") {
				t.Errorf("TailErr = %v, want it to name the damaged final batch", rec.TailErr)
			}
			appendN(t, w, 1)
			mustClose(t, w)
			checkRecords(t, readAll(t, path), n)

			// Second-to-last batch: corruption, file untouched.
			c = bytes.Clone(data)
			sh.damage(c, starts[n-2], ends[n-2])
			path = tempJournal(t)
			if err := os.WriteFile(path, c, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, testOptions()); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("second-to-last batch: Open err = %v, want ErrCorrupt", err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, c) {
				t.Fatal("Open modified a corrupt journal")
			}
		})
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

func TestGarbageTail(t *testing.T) {
	data := fixture(t, 2)
	ends := batchEnds(t, data)
	garbage := []byte("garbage that is not a batch header..")

	// Garbage where the next batch would start, with nothing valid after it,
	// is a damaged final write: truncated.
	res, err := scanBytes(append(bytes.Clone(data), garbage...), ReaderOptions{})
	if err != nil || res.Tail != TailTorn || res.ValidEnd != ends[1] {
		t.Fatalf("garbage at end: err=%v tail=%v ValidEnd=%d", err, res.Tail, res.ValidEnd)
	}

	// Garbage followed by a valid batch is damage in the middle: corruption.
	mid := append(bytes.Clone(data[:ends[0]]), garbage...)
	mid = append(mid, data[ends[0]:]...)
	if _, err := scanBytes(mid, ReaderOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("garbage before a batch: err = %v, want ErrCorrupt", err)
	}
}

// A final batch with a valid header that is unsupported or over the limits
// may come from a newer or differently configured writer: never truncated.
func TestFinalBatchNotTruncatedWhenUnsupported(t *testing.T) {
	data := fixture(t, 2)
	unknownCodec := append(bytes.Clone(data), craftBatch(batchHeader{codec: 42, firstSeq: 3, count: 1, storedLen: 3, rawLen: 3}, []byte("abc"))...)
	if _, err := scanBytes(unknownCodec, ReaderOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown codec in final batch: err = %v, want ErrUnsupported", err)
	}
	limits := DefaultLimits()
	limits.MaxBatchRecords = 1
	tooMany := append(bytes.Clone(data), craftBatch(batchHeader{firstSeq: 3, count: 2, storedLen: 3, rawLen: 3}, []byte("abc"))...)
	if _, err := scanBytes(tooMany, ReaderOptions{Limits: limits}); !errors.Is(err, ErrLimit) {
		t.Fatalf("over-limit final batch: err = %v, want ErrLimit", err)
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

// Open validates checksums without decoding payloads; a batch that passes
// its checksum but doesn't decode is reported when its records are read.
func TestOpenDoesNotDecodePayloads(t *testing.T) {
	data := fixture(t, 1)
	garbage := []byte("not protobuf \xff\xff\xff")
	data = append(data, craftBatch(batchHeader{codec: CodecNone, firstSeq: 2, count: 1,
		storedLen: uint32(len(garbage)), rawLen: uint32(len(garbage))}, garbage)...)
	path := tempJournal(t)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	w := mustOpen(t, path, testOptions())
	if got := w.Recovery(); got.LastSeq != 2 || got.Tail != TailClean {
		t.Fatalf("Recovery = %+v, want 2 records and a clean tail", got)
	}
	mustClose(t, w)
	if _, err := scanBytes(data, ReaderOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("decoding scan: err = %v, want ErrCorrupt", err)
	}
}
