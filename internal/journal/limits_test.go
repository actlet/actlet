package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// craftBatch frames payload as a batch with a valid header CRC, checksum and
// trailer, so that only the claimed lengths or the payload are malicious.
func craftBatch(h batchHeader, payload []byte) []byte {
	if h.checksum == 0 {
		h.checksum = ChecksumCRC32C
	}
	b := make([]byte, batchHeaderLen+len(payload)+batchChecksumLen+batchTrailerLen)
	encodeBatchHeader(b, h)
	copy(b[batchHeaderLen:], payload)
	sum, _ := batchChecksum(h.checksum, b[:batchHeaderLen], payload)
	binary.LittleEndian.PutUint32(b[batchHeaderLen+len(payload):], sum)
	copy(b[batchHeaderLen+len(payload)+batchChecksumLen:], batchTrailerMagic[:])
	return b
}

func journalWith(batches ...[]byte) []byte {
	data := encodeFileHeader(Header{SessionID: "s"})
	for _, b := range batches {
		data = append(data, b...)
	}
	return data
}

// allocDuring returns the bytes allocated while f runs.
func allocDuring(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestMalformedLengthsRejectedWithoutAllocating(t *testing.T) {
	cases := map[string]batchHeader{
		"huge stored length": {firstSeq: 1, count: 1, storedLen: 0xFFFFFFF0, rawLen: 10},
		"huge raw length":    {firstSeq: 1, count: 1, storedLen: 10, rawLen: 0xFFFFFFF0},
		"huge record count":  {firstSeq: 1, count: 0xFFFFFFF0, storedLen: 10, rawLen: 10},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			// Only the header is present: a length that wasn't checked
			// against the limits would make the reader allocate or read it.
			b := craftBatch(h, nil)[:batchHeaderLen]
			data := journalWith(b, make([]byte, 64))
			var err error
			alloc := allocDuring(func() { _, err = scanBytes(data, ReaderOptions{}) })
			if !errors.Is(err, ErrLimit) {
				t.Fatalf("err = %v, want ErrLimit", err)
			}
			if alloc > 1<<20 {
				t.Fatalf("allocated %d bytes rejecting a malformed length", alloc)
			}
		})
	}
}

func TestZeroRecordBatchIsCorrupt(t *testing.T) {
	data := journalWith(craftBatch(batchHeader{firstSeq: 1, count: 0}, nil))
	if _, err := scanBytes(data, ReaderOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestSequenceGapIsCorrupt(t *testing.T) {
	p, _ := marshalBatch(nil, []Record{{Seq: 5, Kind: "k"}})
	data := journalWith(craftBatch(batchHeader{codec: CodecNone, firstSeq: 5, count: 1, storedLen: uint32(len(p)), rawLen: uint32(len(p))}, p))
	if _, err := scanBytes(data, ReaderOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestUnknownBatchFlagsAreUnsupported(t *testing.T) {
	data := journalWith(craftBatch(batchHeader{flags: 1, firstSeq: 1, count: 1, storedLen: 3, rawLen: 3}, []byte("abc")))
	if _, err := scanBytes(data, ReaderOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestUnknownCodecIsUnsupported(t *testing.T) {
	data := journalWith(craftBatch(batchHeader{codec: 42, firstSeq: 1, count: 1, storedLen: 3, rawLen: 3}, []byte("abc")))
	if _, err := scanBytes(data, ReaderOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// A small, validly checksummed zstd frame that expands far beyond the batch
// limit must be rejected while allocating only about the limit.
func TestDecompressionBombRejected(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxBatchRaw = 1 << 20
	limits.MaxBatchStored = 1 << 20
	limits.MaxRecordData = 512 << 10

	const bombSize = 64 << 20 // 64 MiB of zeros, 64 times the batch limit

	// One frame with the content size declared, and one streamed without it,
	// so the decoder can't reject it from the frame header alone.
	declared := func() []byte {
		enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
		defer func() { _ = enc.Close() }()
		return enc.EncodeAll(make([]byte, bombSize), nil)
	}()
	streamed := func() []byte {
		var buf bytes.Buffer
		enc, _ := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<20))
		zeros := make([]byte, 1<<20)
		for range bombSize / len(zeros) {
			_, _ = enc.Write(zeros)
		}
		_ = enc.Close()
		return buf.Bytes()
	}()

	for name, payload := range map[string][]byte{"declared size": declared, "streamed": streamed} {
		t.Run(name, func(t *testing.T) {
			t.Logf("bomb payload: %d bytes", len(payload))
			for _, rawLen := range []uint32{100, uint32(limits.MaxBatchRaw)} {
				data := journalWith(craftBatch(batchHeader{
					codec: CodecZstd, firstSeq: 1, count: 1,
					storedLen: uint32(len(payload)), rawLen: rawLen,
				}, payload))
				var err error
				alloc := allocDuring(func() { _, err = scanBytes(data, ReaderOptions{Limits: limits}) })
				if !errors.Is(err, ErrCorrupt) {
					t.Fatalf("rawLen %d: err = %v, want ErrCorrupt", rawLen, err)
				}
				// Reader buffers plus decoder state: a small multiple of the
				// batch limit, nowhere near the bomb's size.
				if limit := uint64(8 * limits.MaxBatchRaw); alloc > limit {
					t.Fatalf("rawLen %d: allocated %d bytes, limit %d", rawLen, alloc, limit)
				}
				t.Logf("rawLen %d: rejected after allocating %d bytes: %v", rawLen, alloc, err)
			}
		})
	}
}
