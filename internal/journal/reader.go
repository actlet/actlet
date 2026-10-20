package journal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Batch is one batch as read from a journal file.
type Batch struct {
	// Offset of the batch header in the file.
	Offset int64
	// FrameLen is the on-disk size of the batch including framing.
	FrameLen int64
	FirstSeq uint64
	Count    uint32
	Codec    CodecID
	Checksum ChecksumID
	// StoredLen and RawLen are the payload sizes on disk and decoded.
	StoredLen uint32
	RawLen    uint32
	// Records are the decoded records, nil if the reader skips decoding.
	Records []Record
}

// LastSeq returns the sequence number of the batch's last record.
func (b *Batch) LastSeq() uint64 { return b.FirstSeq + uint64(b.Count) - 1 }

// ReaderOptions configure a Reader.
type ReaderOptions struct {
	// Limits bound what the reader accepts. Zero means DefaultLimits.
	Limits Limits
	// SkipRecords skips decompressing and decoding payloads. Checksums are
	// still verified.
	SkipRecords bool
}

// Reader reads a journal batch by batch, validating as it goes. It never
// holds more than one batch in memory.
//
// A Reader doesn't take the writer lock, so it can read a journal that is
// being written; it then sees the batches appended so far, possibly ending in
// a torn tail.
type Reader struct {
	r      io.ReaderAt
	size   int64
	opts   ReaderOptions
	codecs *codecs
	header Header

	off     int64
	nextSeq uint64
	err     error // sticky

	stored []byte // reused payload buffer
	raw    []byte // reused decode buffer
}

// NewReader reads and validates the header of the journal in r, which is
// size bytes long.
func NewReader(r io.ReaderAt, size int64, opts ReaderOptions) (*Reader, error) {
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits()
	}
	if err := opts.Limits.validate(); err != nil {
		return nil, err
	}
	h, n, err := readFileHeader(r, size)
	if err != nil {
		return nil, err
	}
	c, err := newCodecs(opts.Limits)
	if err != nil {
		return nil, err
	}
	return &Reader{r: r, size: size, opts: opts, codecs: c, header: h, off: n, nextSeq: 1}, nil
}

// Header returns the journal file header.
func (r *Reader) Header() Header { return r.header }

// Offset returns the offset just past the last valid batch read so far.
func (r *Reader) Offset() int64 { return r.off }

// NextSeq returns the sequence number the next record is expected to have.
func (r *Reader) NextSeq() uint64 { return r.nextSeq }

// Close releases the reader's decoders. It doesn't close the underlying
// io.ReaderAt.
func (r *Reader) Close() { r.codecs.close() }

// Next returns the next valid batch. It returns io.EOF at a clean end of
// file, an error wrapping ErrTornTail if the file ends in an incomplete
// batch, and an error wrapping ErrCorrupt, ErrLimit or ErrUnsupported for
// invalid data. Errors are sticky.
func (r *Reader) Next() (*Batch, error) {
	if r.err != nil {
		return nil, r.err
	}
	b, err := r.next()
	if err != nil {
		r.err = err
		return nil, err
	}
	r.off += b.FrameLen
	r.nextSeq = b.LastSeq() + 1
	return b, nil
}

func (r *Reader) next() (*Batch, error) {
	off := r.off
	remaining := r.size - off
	if remaining == 0 {
		return nil, io.EOF
	}
	if remaining < batchHeaderLen {
		// A prefix of a batch header: the write was cut short.
		return nil, formatErr(off, ErrTornTail, "%d bytes of a %d-byte batch header", remaining, batchHeaderLen)
	}

	var hb [batchHeaderLen]byte
	if _, err := r.r.ReadAt(hb[:], off); err != nil {
		return nil, fmt.Errorf("journal: read batch header at %d: %w", off, err)
	}
	h, ok := decodeBatchHeader(hb[:])
	if !ok {
		// Some filesystems extend a file with zeros when a crash interrupts
		// an append: zeros up to EOF are a torn tail. Otherwise the header
		// is damaged. If no valid batch follows, this is the final batch,
		// which was never synced: also a torn tail. If a batch follows, the
		// damage is in synced data: corruption.
		if zero, err := r.zeroUntilEOF(off); err != nil {
			return nil, err
		} else if zero {
			return nil, formatErr(off, ErrTornTail, "zero-filled tail of %d bytes", remaining)
		}
		if later, err := r.batchHeaderAfter(off + 1); err != nil {
			return nil, err
		} else if later >= 0 {
			return nil, formatErr(off, ErrCorrupt, "invalid batch header (a valid batch follows at offset %d)", later)
		}
		return nil, formatErr(off, ErrTornTail, "damaged final batch: invalid batch header, %d bytes to end of file", remaining)
	}

	l := r.opts.Limits
	switch {
	case h.flags != 0:
		return nil, formatErr(off, ErrUnsupported, "batch flags %#x", h.flags)
	case int64(h.storedLen) > int64(l.MaxBatchStored):
		return nil, formatErr(off, ErrLimit, "stored payload length %d exceeds limit %d", h.storedLen, l.MaxBatchStored)
	case int64(h.rawLen) > int64(l.MaxBatchRaw):
		return nil, formatErr(off, ErrLimit, "raw payload length %d exceeds limit %d", h.rawLen, l.MaxBatchRaw)
	case h.count == 0:
		return nil, formatErr(off, ErrCorrupt, "batch with no records")
	case int64(h.count) > int64(l.MaxBatchRecords):
		return nil, formatErr(off, ErrLimit, "record count %d exceeds limit %d", h.count, l.MaxBatchRecords)
	case h.firstSeq != r.nextSeq:
		return nil, formatErr(off, ErrCorrupt, "batch starts at seq %d, want %d", h.firstSeq, r.nextSeq)
	case h.firstSeq+uint64(h.count) < h.firstSeq:
		return nil, formatErr(off, ErrCorrupt, "sequence range overflows")
	}

	frameLen := h.frameLen()
	if frameLen > remaining {
		return nil, formatErr(off, ErrTornTail, "%d of %d batch bytes present", remaining, frameLen)
	}

	// Read payload, checksum and trailer in one go.
	n := int(h.storedLen) + batchChecksumLen + batchTrailerLen
	r.stored = grow(r.stored, n)
	buf := r.stored[:n]
	if _, err := r.r.ReadAt(buf, off+batchHeaderLen); err != nil {
		return nil, fmt.Errorf("journal: read batch at %d: %w", off, err)
	}
	payload := buf[:h.storedLen]
	sum := binary.LittleEndian.Uint32(buf[h.storedLen:])
	trailer := [4]byte(buf[int(h.storedLen)+batchChecksumLen:])

	// A bad trailer or checksum in the final batch, with nothing but zeros
	// after it, is the last write never fully reaching the disk: a torn
	// tail. Followed by more data, the batch was synced before the next one
	// was written, so the damage is corruption.
	damaged := func(reason string) error {
		if zero, err := r.zeroUntilEOF(off + frameLen); err != nil {
			return err
		} else if zero {
			return formatErr(off, ErrTornTail, "damaged final batch: %s", reason)
		}
		return formatErr(off, ErrCorrupt, "%s", reason)
	}
	if trailer != batchTrailerMagic {
		return nil, damaged("invalid commit trailer")
	}
	want, ok := batchChecksum(h.checksum, hb[:], payload)
	if !ok {
		return nil, formatErr(off, ErrUnsupported, "checksum algorithm %v", h.checksum)
	}
	if sum != want {
		return nil, damaged("checksum mismatch")
	}

	c, ok := r.codecs.get(h.codec)
	if !ok {
		return nil, formatErr(off, ErrUnsupported, "codec %v", h.codec)
	}
	b := &Batch{
		Offset:    off,
		FrameLen:  frameLen,
		FirstSeq:  h.firstSeq,
		Count:     h.count,
		Codec:     h.codec,
		Checksum:  h.checksum,
		StoredLen: h.storedLen,
		RawLen:    h.rawLen,
	}
	if r.opts.SkipRecords {
		return b, nil
	}
	r.raw = grow(r.raw, int(h.rawLen))
	raw, err := c.decode(r.raw[:0], payload, int(h.rawLen))
	if err != nil {
		return nil, formatErr(off, ErrCorrupt, "decode payload: %v", err)
	}
	recs, err := unmarshalBatch(raw, h, l)
	if err != nil {
		return nil, formatErr(off, ErrCorrupt, "%v", err)
	}
	b.Records = recs
	return b, nil
}

// zeroUntilEOF reports whether every byte from off to the end of the file is
// zero. It reads in fixed-size chunks.
func (r *Reader) zeroUntilEOF(off int64) (bool, error) {
	var chunk [32 << 10]byte
	for off < r.size {
		n := min(int64(len(chunk)), r.size-off)
		if _, err := r.r.ReadAt(chunk[:n], off); err != nil {
			return false, fmt.Errorf("journal: read at %d: %w", off, err)
		}
		if !allZero(chunk[:n]) {
			return false, nil
		}
		off += n
	}
	return true, nil
}

// batchHeaderAfter returns the offset of the first valid batch header at or
// after off, or -1 if there is none. It reads in fixed-size chunks.
func (r *Reader) batchHeaderAfter(off int64) (int64, error) {
	const chunkLen = 32 << 10
	var chunk [chunkLen + batchHeaderLen - 1]byte
	for off+batchHeaderLen <= r.size {
		n := min(int64(len(chunk)), r.size-off)
		buf := chunk[:n]
		if _, err := r.r.ReadAt(buf, off); err != nil {
			return 0, fmt.Errorf("journal: read at %d: %w", off, err)
		}
		for i := 0; i+batchHeaderLen <= len(buf); i++ {
			if buf[i] == batchMagic[0] {
				if _, ok := decodeBatchHeader(buf[i : i+batchHeaderLen]); ok {
					return off + int64(i), nil
				}
			}
		}
		// Overlap chunks so a header straddling the boundary is seen.
		off += max(n-(batchHeaderLen-1), 1)
	}
	return -1, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// grow returns b with capacity for at least n bytes.
func grow(b []byte, n int) []byte {
	if cap(b) >= n {
		return b
	}
	return make([]byte, n)
}

// readFileHeader reads and validates the file header, returning it and its
// length.
func readFileHeader(r io.ReaderAt, size int64) (Header, int64, error) {
	var fixed [fileHeaderFixedLen]byte
	if size < fileHeaderFixedLen+fileHeaderCRCLen {
		return Header{}, 0, formatErr(0, ErrCorrupt, "file too short for a header (%d bytes)", size)
	}
	if _, err := r.ReadAt(fixed[:], 0); err != nil {
		return Header{}, 0, fmt.Errorf("journal: read header: %w", err)
	}
	if [8]byte(fixed[:8]) != fileMagic {
		return Header{}, 0, formatErr(0, ErrCorrupt, "not a journal file (bad magic)")
	}
	idLen := int64(binary.LittleEndian.Uint16(fixed[12:]))
	if idLen == 0 || idLen > MaxSessionIDLen {
		return Header{}, 0, formatErr(12, ErrCorrupt, "invalid session ID length %d", idLen)
	}
	n := fileHeaderFixedLen + idLen
	if size < n+fileHeaderCRCLen {
		return Header{}, 0, formatErr(0, ErrCorrupt, "file too short for its header")
	}
	b := make([]byte, n+fileHeaderCRCLen)
	if _, err := r.ReadAt(b, 0); err != nil {
		return Header{}, 0, fmt.Errorf("journal: read header: %w", err)
	}
	if binary.LittleEndian.Uint32(b[n:]) != crc32.Checksum(b[:n], crc32c) {
		return Header{}, 0, formatErr(0, ErrCorrupt, "header checksum mismatch")
	}
	h := Header{
		Version:   binary.LittleEndian.Uint16(b[8:]),
		SessionID: string(b[14:n]),
	}
	if h.Version != FormatVersion {
		return Header{}, 0, formatErr(8, ErrUnsupported, "format version %d (this build reads %d)", h.Version, FormatVersion)
	}
	if flags := binary.LittleEndian.Uint16(b[10:]); flags != 0 {
		return Header{}, 0, formatErr(10, ErrUnsupported, "header flags %#x", flags)
	}
	if err := validateSessionID(h.SessionID); err != nil {
		return Header{}, 0, formatErr(14, ErrCorrupt, "%v", err)
	}
	return h, n + fileHeaderCRCLen, nil
}

// TailState describes how a scanned journal ends.
type TailState int

const (
	// TailClean means the file ends exactly after the last valid batch.
	TailClean TailState = iota
	// TailTorn means the file ends in an incomplete batch.
	TailTorn
)

func (t TailState) String() string {
	if t == TailTorn {
		return "torn"
	}
	return "clean"
}

// ScanResult summarises a journal scan.
type ScanResult struct {
	Header  Header
	Batches int
	Records uint64
	// LastSeq is the sequence number of the last valid record, 0 if none.
	LastSeq uint64
	// ValidEnd is the offset just past the last valid batch: the committed
	// prefix.
	ValidEnd int64
	// Size is the file size at the time of the scan.
	Size int64
	Tail TailState
	// TailErr describes the torn tail, if any.
	TailErr error
}

// Truncated returns how many bytes past the valid prefix a writer drops when
// it opens the journal: the torn tail, or 0.
func (r ScanResult) Truncated() int64 {
	if r.Tail != TailTorn {
		return 0
	}
	return r.Size - r.ValidEnd
}

// Scan reads the whole journal and reports its valid committed prefix. A torn
// tail is not an error: it is reported in the result. Corruption is returned
// as an error together with the result for the prefix before it.
func Scan(r io.ReaderAt, size int64, opts ReaderOptions) (ScanResult, error) {
	jr, err := NewReader(r, size, opts)
	if err != nil {
		return ScanResult{Size: size}, err
	}
	defer jr.Close()
	res := ScanResult{Header: jr.Header(), Size: size}
	for {
		b, err := jr.Next()
		res.ValidEnd = jr.Offset()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if errors.Is(err, ErrTornTail) {
			res.Tail, res.TailErr = TailTorn, err
			return res, nil
		}
		if err != nil {
			return res, err
		}
		res.Batches++
		res.Records += uint64(b.Count)
		res.LastSeq = b.LastSeq()
	}
}
