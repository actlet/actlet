// Package activity buffers a session's activity events until the gateway
// acknowledges them.
//
// Events wait in a bounded in-memory queue. When it is full they spill to a
// local recording file, so a long disconnect loses nothing; when that
// reaches its size cap too, display-only events are dropped and a gap event
// records how many. Emitting never blocks on the network or the disk sync.
package activity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/actlet/actlet/internal/journal"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Event is an activity event.
type Event = gatewayv0.ActivityEvent

// recordKind is the journal record kind of events in a spill file.
const recordKind = "activity"

// Options configure a Publisher.
type Options struct {
	// SessionID is written to the spill file header.
	SessionID string
	// Epoch numbers this run of the session; event sequence numbers start
	// at 1 in each epoch.
	Epoch uint64
	// Dir holds the spill file of each epoch.
	Dir string
	// QueueLen is how many events wait in memory before spilling. Zero
	// means 1024.
	QueueLen int
	// SpillMax caps the spill file in bytes. Zero means 64 MiB. Events that
	// aren't display-only may use a further eighth of it, at least 4 KiB.
	SpillMax int64
	// Logger receives warnings. Nil means slog.Default().
	Logger *slog.Logger
}

// SpillPath returns the spill file of an epoch in dir.
func SpillPath(dir string, epoch uint64) string {
	return filepath.Join(dir, fmt.Sprintf("activity-%d.actrec", epoch))
}

// Publisher numbers and buffers the activity of one epoch until it is
// acknowledged. It is safe for concurrent use.
type Publisher struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	nextSeq uint64
	acked   uint64
	mem     []*Event // waiting in memory, in order; all before the spill
	spill   *spillFile
	gap     *gatewayv0.Gap // events dropped and not yet reported
	closed  bool
	wake    chan struct{} // closed and replaced when events arrive
}

// New returns a Publisher for a new epoch.
func New(opts Options) *Publisher {
	if opts.QueueLen <= 0 {
		opts.QueueLen = 1024
	}
	if opts.SpillMax <= 0 {
		opts.SpillMax = 64 << 20
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Publisher{opts: opts, log: opts.Logger, nextSeq: 1, wake: make(chan struct{})}
}

// Epoch returns the publisher's epoch.
func (p *Publisher) Epoch() uint64 { return p.opts.Epoch }

// Acked returns the highest acknowledged sequence number.
func (p *Publisher) Acked() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acked
}

// LastSeq returns the sequence number of the last stored event.
func (p *Publisher) LastSeq() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nextSeq - 1
}

// Emit numbers and stores an event; Body must be set. Text events are
// display-only and are the first to be dropped when the buffers are full.
// Emit never waits for the network.
func (p *Publisher) Emit(ev *Event) {
	ev.Time = timestamppb.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	displayOnly := isDisplayOnly(ev)
	if !p.roomLocked(ev, displayOnly) {
		p.dropLocked(ev)
		return
	}
	// Report earlier drops before the event that follows them.
	if p.gap != nil && !p.flushGapLocked() {
		p.dropLocked(ev)
		return
	}
	if !p.storeLocked(ev, displayOnly) {
		p.dropLocked(ev)
	}
}

// spillLimit is the spill size up to which an event may be stored. Events
// that aren't display-only, such as state changes and gap reports, may use
// a reserve beyond SpillMax.
func (p *Publisher) spillLimit(displayOnly bool) int64 {
	if displayOnly {
		return p.opts.SpillMax
	}
	return p.opts.SpillMax + max(p.opts.SpillMax/8, 4<<10)
}

// roomLocked reports whether ev would fit in memory or the spill file.
func (p *Publisher) roomLocked(ev *Event, displayOnly bool) bool {
	if p.spill == nil && len(p.mem) < p.opts.QueueLen {
		return true
	}
	var size int64
	if p.spill != nil {
		size = p.spill.size
	}
	// The sequence number and ID aren't set yet; allow for them.
	return size+int64(proto.Size(ev))+32 <= p.spillLimit(displayOnly)
}

func isDisplayOnly(ev *Event) bool {
	_, ok := ev.Body.(*gatewayv0.ActivityEvent_Text)
	return ok
}

func (p *Publisher) dropLocked(ev *Event) {
	if p.gap == nil {
		p.gap = &gatewayv0.Gap{First: ev.Time}
		p.log.Warn("activity: buffers full, dropping display-only events", "epoch", p.opts.Epoch)
	}
	p.gap.Dropped++
	p.gap.Last = ev.Time
}

// flushGapLocked stores the pending gap event, if there is room.
func (p *Publisher) flushGapLocked() bool {
	ev := &Event{Time: timestamppb.Now(), Body: &gatewayv0.ActivityEvent_Gap{Gap: p.gap}}
	if !p.storeLocked(ev, false) {
		return false
	}
	p.gap = nil
	return true
}

// storeLocked numbers ev and keeps it in memory or in the spill file. It
// reports false if there is no room.
func (p *Publisher) storeLocked(ev *Event, displayOnly bool) bool {
	ev.Epoch = p.opts.Epoch
	ev.Seq = p.nextSeq
	ev.Id = fmt.Sprintf("%d.%d", ev.Epoch, ev.Seq)
	if p.spill == nil && len(p.mem) < p.opts.QueueLen {
		p.mem = append(p.mem, ev)
	} else {
		if err := p.spillLocked(ev, p.spillLimit(displayOnly)); err != nil {
			if !errors.Is(err, errSpillFull) {
				p.log.Warn("activity: can't spill", "err", err)
			}
			return false
		}
	}
	p.nextSeq++
	close(p.wake)
	p.wake = make(chan struct{})
	return true
}

var errSpillFull = errors.New("spill file full")

func (p *Publisher) spillLocked(ev *Event, limit int64) error {
	if p.spill == nil {
		s, err := createSpill(SpillPath(p.opts.Dir, p.opts.Epoch), p.opts.SessionID, ev.Seq)
		if err != nil {
			return err
		}
		p.spill = s
	}
	return p.spill.append(ev, limit)
}

// Next returns up to max events after sequence number after, in order. It
// waits until there is at least one, or ctx ends.
func (p *Publisher) Next(ctx context.Context, after uint64, max int) ([]*Event, error) {
	for {
		p.mu.Lock()
		if p.gap != nil && p.nextSeq-1 <= after {
			// Everything stored has been sent: report the drop now.
			p.flushGapLocked()
		}
		evs, err := p.collectLocked(after, max)
		wake := p.wake
		p.mu.Unlock()
		if err != nil || len(evs) > 0 {
			return evs, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		}
	}
}

func (p *Publisher) collectLocked(after uint64, max int) ([]*Event, error) {
	if after < p.acked {
		after = p.acked // acknowledged events are gone
	}
	var out []*Event
	for _, ev := range p.mem {
		if ev.Seq > after && len(out) < max {
			out = append(out, ev)
		}
	}
	if p.spill != nil && len(out) < max {
		from := max64(after+1, p.spill.first)
		if len(out) > 0 {
			from = out[len(out)-1].Seq + 1
		}
		more, err := p.spill.read(from, max-len(out))
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// Ack records that the gateway holds every event up to seq. Those events
// are discarded; a spill file that is fully acknowledged is deleted.
func (p *Publisher) Ack(seq uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if seq <= p.acked {
		return
	}
	p.acked = min(seq, p.nextSeq-1)
	i := sort.Search(len(p.mem), func(i int) bool { return p.mem[i].Seq > p.acked })
	p.mem = append(p.mem[:0], p.mem[i:]...)
	if p.spill != nil && p.acked >= p.spill.last && p.spill.last >= p.spill.first {
		if err := p.spill.remove(); err != nil {
			p.log.Warn("activity: removing spill file", "err", err)
		}
		p.spill = nil
	}
}

// Done reports whether every stored event has been acknowledged.
func (p *Publisher) Done() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acked == p.nextSeq-1 && p.gap == nil
}

// Close waits until every event is acknowledged or ctx ends, then writes
// what is left to the spill file so that a later run can send it. Events
// emitted after Close are discarded.
func (p *Publisher) Close(ctx context.Context) error {
wait:
	for !p.Done() {
		select {
		case <-ctx.Done():
			break wait
		case <-time.After(20 * time.Millisecond): // acknowledgements don't signal
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.gap != nil {
		p.flushGapLocked()
	}
	// Everything still in memory goes to the spill file, in order and
	// before what is spilled already: rewrite the file if there is one.
	pending := p.mem
	if p.spill != nil {
		rest, err := p.spill.read(max64(p.acked+1, p.spill.first), int(p.nextSeq))
		if err != nil {
			return err
		}
		pending = append(pending, rest...)
		if err := p.spill.remove(); err != nil {
			return err
		}
		p.spill = nil
	}
	if len(pending) == 0 {
		return nil
	}
	s, err := createSpill(SpillPath(p.opts.Dir, p.opts.Epoch), p.opts.SessionID, pending[0].Seq)
	if err != nil {
		return err
	}
	for _, ev := range pending {
		if err := s.append(ev, 1<<62); err != nil {
			_ = s.close()
			return err
		}
	}
	return s.close()
}

// OpenBacklogs returns a publisher for each spill file in dir of an epoch
// before current, oldest first: events an earlier run couldn't deliver.
// Each one only resends; once acknowledged, its file is deleted.
func OpenBacklogs(dir string, current uint64, log *slog.Logger) ([]*Publisher, error) {
	if log == nil {
		log = slog.Default()
	}
	matches, err := filepath.Glob(filepath.Join(dir, "activity-*.actrec"))
	if err != nil {
		return nil, err
	}
	var out []*Publisher
	for _, path := range matches {
		var epoch uint64
		if _, err := fmt.Sscanf(filepath.Base(path), "activity-%d.actrec", &epoch); err != nil || epoch >= current {
			continue
		}
		s, first, last, err := openSpill(path)
		if err != nil {
			log.Warn("activity: skipping unreadable spill file", "path", path, "err", err)
			continue
		}
		if s == nil {
			continue // empty
		}
		p := New(Options{Epoch: epoch, Dir: dir, Logger: log})
		p.spill, p.nextSeq, p.acked, p.closed = s, last+1, first-1, true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].opts.Epoch < out[j].opts.Epoch })
	return out, nil
}

// spillFile is an append-only recording of spilled events, read back in
// order when they are sent.
type spillFile struct {
	path        string
	w           *journal.Writer // nil for a backlog being replayed
	first, last uint64          // sequence numbers of the events in the file
	size        int64

	f      *os.File
	r      *journal.Reader
	buf    []*Event // decoded but not yet returned
	cursor uint64   // sequence number of buf[0] or the next event to read
}

func createSpill(path, sessionID string, first uint64) (*spillFile, error) {
	if sessionID == "" {
		sessionID = "activity"
	}
	opts := journal.DefaultOptions(sessionID)
	opts.NoSync = true // display data: losing the tail in a crash is fine
	w, err := journal.Create(path, opts)
	if errors.Is(err, os.ErrExist) {
		if rerr := os.Remove(path); rerr != nil {
			return nil, err
		}
		w, err = journal.Create(path, opts)
	}
	if err != nil {
		return nil, err
	}
	return &spillFile{path: path, w: w, first: first, last: first - 1}, nil
}

// openSpill opens an existing spill file for replay and returns the range of
// sequence numbers in it, or nil if it holds no events.
func openSpill(path string) (*spillFile, uint64, uint64, error) {
	s := &spillFile{path: path}
	var first, last uint64
	evs, err := s.scan()
	if err != nil {
		return nil, 0, 0, err
	}
	if len(evs) == 0 {
		return nil, 0, 0, os.Remove(path)
	}
	first, last = evs[0].Seq, evs[len(evs)-1].Seq
	s.first, s.last = first, last
	return s, first, last, nil
}

// scan reads every event in the file.
func (s *spillFile) scan() ([]*Event, error) {
	var out []*Event
	for {
		more, err := s.read(uint64(len(out))+s.first, 1024)
		if err != nil {
			return nil, err
		}
		if len(more) == 0 {
			return out, nil
		}
		if len(out) == 0 && s.first == 0 {
			s.first = more[0].Seq
		}
		out = append(out, more...)
	}
}

func (s *spillFile) append(ev *Event, limit int64) error {
	data, err := proto.Marshal(ev)
	if err != nil {
		return err
	}
	if s.size+int64(len(data)) > limit {
		return errSpillFull
	}
	if _, err := s.w.Append(context.Background(), recordKind, data); err != nil {
		return err
	}
	s.size += int64(len(data))
	s.last = ev.Seq
	return nil
}

// read returns up to n events from sequence number from on.
func (s *spillFile) read(from uint64, n int) ([]*Event, error) {
	if s.r == nil || from < s.cursor {
		if err := s.reopen(); err != nil {
			return nil, err
		}
	}
	var out []*Event
	for len(out) < n {
		if len(s.buf) == 0 {
			ok, err := s.fill()
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
			continue
		}
		ev := s.buf[0]
		s.buf = s.buf[1:]
		s.cursor = ev.Seq + 1
		if ev.Seq >= from {
			out = append(out, ev)
		}
	}
	return out, nil
}

// fill decodes the next batch into buf; false means no more for now.
func (s *spillFile) fill() (bool, error) {
	b, err := s.r.Next()
	if errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
		st, serr := s.f.Stat()
		if serr != nil {
			return false, serr
		}
		s.r.Grow(st.Size())
		if b, err = s.r.Next(); errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
			return false, nil
		}
	}
	if err != nil {
		return false, err
	}
	for _, rec := range b.Records {
		ev := new(Event)
		if err := proto.Unmarshal(rec.Data, ev); err != nil {
			return false, fmt.Errorf("activity: spill record %d: %w", rec.Seq, err)
		}
		s.buf = append(s.buf, ev)
	}
	return true, nil
}

func (s *spillFile) reopen() error {
	s.closeReader()
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r, err := journal.NewReader(f, st.Size(), journal.ReaderOptions{})
	if err != nil {
		_ = f.Close()
		return err
	}
	s.f, s.r, s.buf, s.cursor = f, r, nil, s.first
	return nil
}

func (s *spillFile) closeReader() {
	if s.r != nil {
		s.r.Close()
		_ = s.f.Close()
		s.r, s.f = nil, nil
	}
}

func (s *spillFile) close() error {
	s.closeReader()
	if s.w != nil {
		return s.w.Close()
	}
	return nil
}

func (s *spillFile) remove() error {
	cerr := s.close()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return cerr
}
