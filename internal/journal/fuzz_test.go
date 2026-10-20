package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fuzzLimits are small so that the fuzzer reaches the limit checks and the
// allocation bound stays tight.
func fuzzLimits() Limits {
	return Limits{
		MaxBatchRaw:     64 << 10,
		MaxBatchStored:  64 << 10,
		MaxBatchRecords: 64,
		MaxRecordData:   16 << 10,
		MaxKindLen:      32,
	}
}

// fuzzSeeds returns journals in several states: valid, torn, damaged and
// crafted to stress the limits.
func fuzzSeeds(f *testing.F) [][]byte {
	path := filepath.Join(f.TempDir(), "seed.actlet")
	opts := testOptions()
	opts.Limits = fuzzLimits()
	w, err := Create(path, opts)
	if err != nil {
		f.Fatal(err)
	}
	for i := range 4 {
		if _, err := w.Append(f.Context(), "seed", bytes.Repeat([]byte{byte('a' + i)}, 20*(i+1))); err != nil {
			f.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		f.Fatal(err)
	}
	valid, err := os.ReadFile(path)
	if err != nil {
		f.Fatal(err)
	}
	flipped := bytes.Clone(valid)
	flipped[len(flipped)/2] ^= 0x10
	return [][]byte{
		valid,
		valid[:len(valid)-7],
		flipped,
		append(bytes.Clone(valid), make([]byte, 100)...),
		journalWith(craftBatch(batchHeader{codec: CodecZstd, firstSeq: 1, count: 1, storedLen: 9, rawLen: 1 << 30}, []byte("123456789"))),
		journalWith(),
		{},
	}
}

func isClassified(err error) bool {
	return errors.Is(err, ErrCorrupt) || errors.Is(err, ErrLimit) || errors.Is(err, ErrUnsupported)
}

// FuzzScan feeds arbitrary bytes to the reader. It must never panic, must
// classify every error, must stay within a fixed allocation bound, and the
// prefix it reports as valid must scan cleanly on its own.
func FuzzScan(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s)
	}
	limits := fuzzLimits()
	f.Fuzz(func(t *testing.T, data []byte) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := scanBytes(data, ReaderOptions{Limits: limits})
		runtime.ReadMemStats(&after)

		// Decoder state and buffers: a small multiple of the batch limit
		// plus the input, whatever the input claims.
		if alloc, bound := after.TotalAlloc-before.TotalAlloc, uint64(16*limits.MaxBatchRaw+4*len(data)+(4<<20)); alloc > bound {
			t.Fatalf("Scan allocated %d bytes, bound %d", alloc, bound)
		}
		if err != nil && !isClassified(err) {
			t.Fatalf("unclassified error: %v", err)
		}
		if res.ValidEnd < 0 || res.ValidEnd > int64(len(data)) {
			t.Fatalf("ValidEnd %d outside [0, %d]", res.ValidEnd, len(data))
		}
		if err != nil {
			return
		}

		// The valid prefix is a clean journal with the same records.
		clean, err := scanBytes(data[:res.ValidEnd], ReaderOptions{Limits: limits})
		if err != nil || clean.Tail != TailClean || clean.LastSeq != res.LastSeq || clean.Batches != res.Batches {
			t.Fatalf("valid prefix rescans as %+v, %v; first scan %+v", clean, err, res)
		}
		// Skipping payload decoding finds the same prefix.
		skip, err := scanBytes(data, ReaderOptions{Limits: limits, SkipRecords: true})
		if err != nil || skip.ValidEnd != res.ValidEnd || skip.LastSeq != res.LastSeq {
			t.Fatalf("checksum-only scan %+v, %v; decoding scan %+v", skip, err, res)
		}
	})
}

// FuzzOpen opens arbitrary bytes as a journal for writing. Open either
// refuses with a classified error and leaves the file untouched, or recovers
// a journal that accepts an append and then scans cleanly.
func FuzzOpen(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "fuzz.actlet")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		opts := testOptions()
		opts.SessionID = ""
		opts.Limits = fuzzLimits()
		w, err := Open(path, opts)
		if err != nil {
			if !isClassified(err) {
				t.Fatalf("unclassified error: %v", err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, data) {
				t.Fatal("Open changed a journal it refused")
			}
			return
		}
		last := w.Recovery().LastSeq
		seq, err := w.Append(t.Context(), "fuzz", []byte("after recovery"))
		if err != nil {
			t.Fatalf("Append after recovery: %v", err)
		}
		if seq != last+1 {
			t.Fatalf("Append seq = %d, want %d", seq, last+1)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		res, err := scanBytes(after, ReaderOptions{Limits: opts.Limits, SkipRecords: true})
		if err != nil || res.Tail != TailClean || res.LastSeq != last+1 {
			t.Fatalf("journal after recovery and append: %+v, %v", res, err)
		}
	})
}
