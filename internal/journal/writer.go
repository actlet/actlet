package journal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// file is the subset of *os.File the writer uses. Tests substitute it to
// inject write and sync failures.
type file interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Truncate(size int64) error
	Close() error
}

// BatchPolicy decides how appended records are grouped into batches. Every
// batch costs one write and one sync.
type BatchPolicy struct {
	// MaxRecords is the most records in one batch.
	MaxRecords int
	// MaxBytes bounds the encoded size of one batch.
	MaxBytes int
	// MaxDelay is how long the first record of a batch may wait for others
	// to join it. Zero writes as soon as the writer is idle, which still
	// groups records that arrive while a previous batch is being synced.
	MaxDelay time.Duration
}

// Options configure a Writer.
type Options struct {
	// SessionID is stored in the header by Create. If set for Open, the
	// journal's session ID must match.
	SessionID string
	// Codec encodes batch payloads. The zero value is CodecNone; use
	// DefaultOptions for zstd.
	Codec CodecID
	// Batch controls batching. Zero fields take defaults.
	Batch BatchPolicy
	// Limits bound records and batches. Zero means DefaultLimits.
	Limits Limits

	// wrap substitutes the file, for fault injection in tests.
	wrap func(file) file
	// noSync skips syncing, for building large test fixtures quickly.
	noSync bool
}

// DefaultOptions returns options with zstd compression and default batching
// for the given session.
func DefaultOptions(sessionID string) Options {
	return Options{SessionID: sessionID, Codec: CodecZstd}
}

func (o Options) withDefaults() (Options, error) {
	if o.Limits == (Limits{}) {
		o.Limits = DefaultLimits()
	}
	if err := o.Limits.validate(); err != nil {
		return o, err
	}
	if o.Codec != CodecNone && o.Codec != CodecZstd {
		return o, fmt.Errorf("%w: codec %v", ErrUnsupported, o.Codec)
	}
	b := &o.Batch
	if b.MaxRecords == 0 {
		b.MaxRecords = min(256, o.Limits.MaxBatchRecords)
	}
	if b.MaxBytes == 0 {
		b.MaxBytes = min(1<<20, o.Limits.MaxBatchRaw)
	}
	switch {
	case b.MaxRecords < 0 || b.MaxRecords > o.Limits.MaxBatchRecords:
		return o, fmt.Errorf("journal: batch MaxRecords %d out of range 1..%d", b.MaxRecords, o.Limits.MaxBatchRecords)
	case b.MaxBytes < 0 || b.MaxBytes > o.Limits.MaxBatchRaw:
		return o, fmt.Errorf("journal: batch MaxBytes %d out of range 1..%d", b.MaxBytes, o.Limits.MaxBatchRaw)
	case b.MaxDelay < 0:
		return o, fmt.Errorf("journal: negative batch MaxDelay")
	}
	return o, nil
}

// Stats are cumulative counters for one Writer.
type Stats struct {
	Batches     uint64
	Records     uint64
	RawBytes    uint64 // encoded record bytes before compression
	StoredBytes uint64 // bytes written, framing included
	SyncTime    time.Duration
}

// Writer appends records to a journal. It holds an exclusive lock on the
// file for its lifetime. Append is safe for concurrent use; a single
// goroutine owns the file and writes batches in order.
//
// After any write or sync failure the writer stops: every later Append fails
// with ErrWriterFailed, and the journal must be reopened, which recovers the
// valid committed prefix. A failed sync is never retried, because after one
// the kernel may already have dropped the unwritten data.
type Writer struct {
	f        file
	opts     Options
	header   Header
	recovery ScanResult
	codecs   *codecs
	codec    codec

	mu     sync.RWMutex // guards closed and sends on reqs
	closed bool
	reqs   chan *appendReq
	done   chan struct{}

	statMu  sync.Mutex
	stats   Stats
	failErr error // sticky, guarded by statMu
	lastSeq uint64

	// Owned by the loop goroutine.
	off     int64
	nextSeq uint64
	recs    []Record
	raw     []byte
	stored  []byte
	frame   []byte
}

type appendReq struct {
	rec  Record
	done chan appendResult
}

type appendResult struct {
	seq uint64
	err error
}

// Create creates a new journal at path, which must not exist, and returns a
// writer holding its lock. The file and its directory entry are synced
// before Create returns.
func Create(path string, opts Options) (w *Writer, err error) {
	opts, err = opts.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := validateSessionID(opts.SessionID); err != nil {
		return nil, err
	}
	osf, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: create: %w", err)
	}
	f := opts.wrapFile(osf)
	if err := lockFile(osf); err != nil {
		// Someone else opened and locked the new file already: leave it.
		_ = f.Close()
		return nil, err
	}
	defer func() {
		if err != nil {
			// Remove the half-created journal so Create can be retried.
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()

	h := Header{Version: FormatVersion, SessionID: opts.SessionID}
	hb := encodeFileHeader(h)
	if n, err := f.WriteAt(hb, 0); err != nil || n != len(hb) {
		return nil, fmt.Errorf("journal: write header: %w", orShortWrite(err))
	}
	if !opts.noSync {
		if err := f.Sync(); err != nil {
			return nil, fmt.Errorf("journal: sync header: %w", err)
		}
		// The new directory entry must be durable too, or the whole file
		// can vanish after a crash.
		if err := syncDir(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("journal: sync directory: %w", err)
		}
	}
	res := ScanResult{Header: h, ValidEnd: int64(len(hb)), Size: int64(len(hb))}
	return newWriter(f, h, res, opts)
}

// Open opens an existing journal for appending. It takes the lock, then
// scans the file: a torn trailing batch is truncated and the truncation
// synced; corruption or an unsupported format is returned as an error and
// the journal is left untouched.
func Open(path string, opts Options) (w *Writer, err error) {
	opts, err = opts.withDefaults()
	if err != nil {
		return nil, err
	}
	osf, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("journal: open: %w", err)
	}
	f := opts.wrapFile(osf)
	defer func() {
		if err != nil {
			_ = f.Close()
		}
	}()
	if err := lockFile(osf); err != nil {
		return nil, err
	}
	st, err := osf.Stat()
	if err != nil {
		return nil, fmt.Errorf("journal: stat: %w", err)
	}

	res, err := Scan(f, st.Size(), ReaderOptions{Limits: opts.Limits})
	if err != nil {
		return nil, err
	}
	if opts.SessionID != "" && opts.SessionID != res.Header.SessionID {
		return nil, fmt.Errorf("journal: session ID is %q, want %q", res.Header.SessionID, opts.SessionID)
	}
	if res.Tail == TailTorn {
		if err := f.Truncate(res.ValidEnd); err != nil {
			return nil, fmt.Errorf("journal: truncate torn tail: %w", err)
		}
		if !opts.noSync {
			if err := f.Sync(); err != nil {
				return nil, fmt.Errorf("journal: sync after truncate: %w", err)
			}
		}
	}
	return newWriter(f, res.Header, res, opts)
}

func newWriter(f file, h Header, res ScanResult, opts Options) (*Writer, error) {
	cs, err := newCodecs(opts.Limits)
	if err != nil {
		return nil, err
	}
	c, _ := cs.get(opts.Codec)
	w := &Writer{
		f:        f,
		opts:     opts,
		header:   h,
		recovery: res,
		codecs:   cs,
		codec:    c,
		reqs:     make(chan *appendReq, opts.Batch.MaxRecords),
		done:     make(chan struct{}),
		off:      res.ValidEnd,
		nextSeq:  res.LastSeq + 1,
		lastSeq:  res.LastSeq,
	}
	go w.loop()
	return w, nil
}

func (o Options) wrapFile(f *os.File) file {
	if o.wrap != nil {
		return o.wrap(f)
	}
	return f
}

// Header returns the journal header.
func (w *Writer) Header() Header { return w.header }

// Recovery returns what Open found when it scanned the journal, before any
// torn tail was truncated.
func (w *Writer) Recovery() ScanResult { return w.recovery }

// LastSeq returns the sequence number of the last durable record.
func (w *Writer) LastSeq() uint64 {
	w.statMu.Lock()
	defer w.statMu.Unlock()
	return w.lastSeq
}

// Stats returns the writer's counters.
func (w *Writer) Stats() Stats {
	w.statMu.Lock()
	defer w.statMu.Unlock()
	return w.stats
}

// Err returns the error that stopped the writer, or nil.
func (w *Writer) Err() error {
	w.statMu.Lock()
	defer w.statMu.Unlock()
	return w.failErr
}

// Append adds a record and returns its sequence number once the batch
// holding it has been written and synced. data is copied.
//
// An error wrapping ErrNotDurable means the record was not acknowledged: it
// may still be present after reopening. This includes ctx ending after the
// record was handed to the writer.
func (w *Writer) Append(ctx context.Context, kind string, data []byte) (uint64, error) {
	l := w.opts.Limits
	switch {
	case kind == "":
		return 0, errors.New("journal: empty record kind")
	case len(kind) > l.MaxKindLen:
		return 0, fmt.Errorf("%w: record kind is %d bytes, max %d", ErrLimit, len(kind), l.MaxKindLen)
	case len(data) > l.MaxRecordData:
		return 0, fmt.Errorf("%w: record data is %d bytes, max %d", ErrLimit, len(data), l.MaxRecordData)
	}
	if err := w.Err(); err != nil {
		return 0, err
	}

	req := &appendReq{
		rec:  Record{Kind: kind, Data: bytes.Clone(data)},
		done: make(chan appendResult, 1),
	}
	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return 0, ErrClosed
	}
	select {
	case w.reqs <- req:
	case <-ctx.Done():
		w.mu.RUnlock()
		return 0, ctx.Err()
	}
	w.mu.RUnlock()

	select {
	case res := <-req.done:
		return res.seq, res.err
	case <-ctx.Done():
		return 0, fmt.Errorf("%w: %w", ErrNotDurable, ctx.Err())
	}
}

// Close waits for pending appends, then closes the file, which releases
// the lock. It returns the error that stopped the writer, if any.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	w.closed = true
	close(w.reqs)
	w.mu.Unlock()

	<-w.done
	w.codecs.close()
	cerr := w.f.Close()
	if err := w.Err(); err != nil {
		return err
	}
	if cerr != nil {
		return fmt.Errorf("journal: close: %w", cerr)
	}
	return nil
}

func (w *Writer) loop() {
	defer close(w.done)
	p := w.opts.Batch
	batch := make([]*appendReq, 0, p.MaxRecords)
	var carry *appendReq
	for {
		first := carry
		carry = nil
		if first == nil {
			r, ok := <-w.reqs
			if !ok {
				return
			}
			first = r
		}
		batch = append(batch[:0], first)
		size := first.rec.encodedSizeBound()

		var timer *time.Timer
		var timeout <-chan time.Time
		if p.MaxDelay > 0 {
			timer = time.NewTimer(p.MaxDelay)
			timeout = timer.C
		}
	gather:
		for len(batch) < p.MaxRecords {
			var r *appendReq
			var ok bool
			if timeout == nil {
				select {
				case r, ok = <-w.reqs:
				default:
					break gather
				}
			} else {
				select {
				case r, ok = <-w.reqs:
				case <-timeout:
					break gather
				}
			}
			if !ok {
				break
			}
			if n := r.rec.encodedSizeBound(); size+n > p.MaxBytes {
				carry = r
				break
			} else {
				size += n
			}
			batch = append(batch, r)
		}
		if timer != nil {
			timer.Stop()
		}
		w.commit(batch)
	}
}

// commit writes and syncs one batch and answers its appenders.
func (w *Writer) commit(batch []*appendReq) {
	reply := func(err error) {
		for _, r := range batch {
			r.done <- appendResult{err: err}
		}
	}
	if err := w.Err(); err != nil {
		reply(err)
		return
	}

	now := time.Now()
	w.recs = w.recs[:0]
	for i, r := range batch {
		r.rec.Seq = w.nextSeq + uint64(i)
		r.rec.Time = now
		w.recs = append(w.recs, r.rec)
	}
	raw, err := marshalBatch(w.raw, w.recs)
	if err != nil {
		reply(fmt.Errorf("journal: encode batch: %w", err))
		return
	}
	w.raw = raw
	l := w.opts.Limits
	if len(raw) > l.MaxBatchRaw {
		reply(fmt.Errorf("%w: batch encodes to %d bytes, max %d", ErrLimit, len(raw), l.MaxBatchRaw))
		return
	}
	codecID, c := w.opts.Codec, w.codec
	w.stored = c.encode(w.stored[:0], raw)
	if codecID != CodecNone && len(w.stored) >= len(raw) {
		// Compression didn't help, typically for small or random batches:
		// store the batch as is. This also keeps it within MaxBatchStored.
		codecID = CodecNone
		w.stored = noneCodec{}.encode(w.stored[:0], raw)
	}

	h := batchHeader{
		codec:     codecID,
		checksum:  ChecksumCRC32C,
		firstSeq:  w.nextSeq,
		count:     uint32(len(batch)),
		storedLen: uint32(len(w.stored)),
		rawLen:    uint32(len(raw)),
	}
	n := int(h.frameLen())
	w.frame = grow(w.frame, n)
	frame := w.frame[:n]
	encodeBatchHeader(frame, h)
	copy(frame[batchHeaderLen:], w.stored)
	sum, _ := batchChecksum(h.checksum, frame[:batchHeaderLen], w.stored)
	p := batchHeaderLen + len(w.stored)
	binary.LittleEndian.PutUint32(frame[p:], sum)
	copy(frame[p+batchChecksumLen:], batchTrailerMagic[:])

	if wn, err := w.f.WriteAt(frame, w.off); err != nil || wn != n {
		reply(w.fail(fmt.Errorf("write batch at offset %d: %w", w.off, orShortWrite(err))))
		return
	}
	var syncTime time.Duration
	if !w.opts.noSync {
		start := time.Now()
		if err := w.f.Sync(); err != nil {
			reply(w.fail(fmt.Errorf("sync batch at offset %d: %w", w.off, err)))
			return
		}
		syncTime = time.Since(start)
	}

	w.off += int64(n)
	w.nextSeq += uint64(len(batch))
	w.statMu.Lock()
	w.lastSeq = h.lastSeq()
	w.stats.Batches++
	w.stats.Records += uint64(len(batch))
	w.stats.RawBytes += uint64(len(raw))
	w.stats.StoredBytes += uint64(n)
	w.stats.SyncTime += syncTime
	w.statMu.Unlock()

	for i, r := range batch {
		r.done <- appendResult{seq: h.firstSeq + uint64(i)}
	}
}

// fail stops the writer and returns the error for the batch that failed.
func (w *Writer) fail(cause error) error {
	w.statMu.Lock()
	if w.failErr == nil {
		w.failErr = fmt.Errorf("%w: %w", ErrWriterFailed, cause)
	}
	w.statMu.Unlock()
	return fmt.Errorf("%w: %w", ErrNotDurable, cause)
}

func orShortWrite(err error) error {
	if err == nil {
		return io.ErrShortWrite
	}
	return err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	return serr
}
