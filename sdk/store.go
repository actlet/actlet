package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/journal"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// ActivityStore keeps the activity a gateway receives. The server calls it
// with events already deduplicated and in order, so a store only needs to
// remember them and report how far it got.
type ActivityStore interface {
	// Acked returns the highest sequence number up to which the store holds
	// every event of the session's epoch, or 0.
	Acked(ctx context.Context, sessionID string, epoch uint64) (uint64, error)
	// Append stores events of one epoch, numbered consecutively from
	// Acked+1. Once it returns, they are acknowledged to the worker, which
	// may then discard them.
	Append(ctx context.Context, sessionID string, events []*gatewayv0.ActivityEvent) error
}

// An ActivityReleaser is an ActivityStore that holds resources per session,
// such as an open file or a lock. The server calls Release when no activity
// stream of the session is open on it any more, for example because the
// worker paused or moved to another gateway replica. The next stream
// acquires them again.
type ActivityReleaser interface {
	Release(sessionID string) error
}

// MemoryStore is an ActivityStore that keeps events in memory.
type MemoryStore struct {
	mu     sync.Mutex
	events map[string]map[uint64][]*gatewayv0.ActivityEvent
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{events: map[string]map[uint64][]*gatewayv0.ActivityEvent{}}
}

// Acked implements ActivityStore.
func (m *MemoryStore) Acked(_ context.Context, sessionID string, epoch uint64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return uint64(len(m.events[sessionID][epoch])), nil
}

// Append implements ActivityStore.
func (m *MemoryStore) Append(_ context.Context, sessionID string, events []*gatewayv0.ActivityEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	byEpoch := m.events[sessionID]
	if byEpoch == nil {
		byEpoch = map[uint64][]*gatewayv0.ActivityEvent{}
		m.events[sessionID] = byEpoch
	}
	for _, ev := range events {
		have := byEpoch[ev.GetEpoch()]
		if ev.GetSeq() != uint64(len(have))+1 {
			return fmt.Errorf("event %s out of order: have %d events", ev.GetId(), len(have))
		}
		byEpoch[ev.GetEpoch()] = append(have, ev)
	}
	return nil
}

// Events returns the stored events of a session, by epoch and sequence.
func (m *MemoryStore) Events(sessionID string) []*gatewayv0.ActivityEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	epochs := make([]uint64, 0, len(m.events[sessionID]))
	for e := range m.events[sessionID] {
		epochs = append(epochs, e)
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })
	var out []*gatewayv0.ActivityEvent
	for _, e := range epochs {
		out = append(out, m.events[sessionID][e]...)
	}
	return out
}

// ReadActivity implements ActivityReader.
func (m *MemoryStore) ReadActivity(ctx context.Context, sessionID string, fn func(*gatewayv0.ActivityEvent) error) error {
	for _, ev := range m.Events(sessionID) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

// RecordingKind is the journal record kind of activity events in a
// recording.
const RecordingKind = "activity"

// RecordingStore is an ActivityStore that appends each session's events to
// a recording file, <dir>/<session ID>.actrec, in Actlet's journal format.
// It reads existing recordings back when a session's events are first
// needed, so a restarted gateway still deduplicates correctly.
//
// Every Append is synced before it returns: an acknowledged event is on
// disk.
//
// A recording is open, and locked, while the session has activity streams
// on the server; gateway replicas may share the directory on one host.
type RecordingStore struct {
	dir string

	mu       sync.Mutex
	sessions map[string]*recording
}

type recording struct {
	w     *journal.Writer
	acked map[uint64]uint64
}

// NewRecordingStore returns a RecordingStore writing to dir, which is
// created if needed.
func NewRecordingStore(dir string) (*RecordingStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("sdk: recording dir: %w", err)
	}
	return &RecordingStore{dir: dir, sessions: map[string]*recording{}}, nil
}

// Path returns the recording file of a session.
func (r *RecordingStore) Path(sessionID string) string {
	return filepath.Join(r.dir, sessionID+".actrec")
}

func (r *RecordingStore) open(sessionID string) (*recording, error) {
	if rec, ok := r.sessions[sessionID]; ok {
		return rec, nil
	}
	if !ValidSessionID(sessionID) {
		return nil, fmt.Errorf("sdk: invalid session ID %q", sessionID)
	}
	path := r.Path(sessionID)
	acked, err := readRecording(path)
	if err != nil {
		return nil, err
	}
	opts := journal.DefaultOptions(sessionID)
	var w *journal.Writer
	if acked == nil {
		w, err = journal.Create(path, opts)
		acked = map[uint64]uint64{}
	} else {
		w, err = journal.Open(path, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("sdk: recording: %w", err)
	}
	rec := &recording{w: w, acked: acked}
	r.sessions[sessionID] = rec
	return rec, nil
}

// readRecording returns the highest sequence number per epoch in an
// existing recording, or nil if there is none.
func readRecording(path string) (map[uint64]uint64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sdk: recording: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("sdk: recording: %w", err)
	}
	jr, err := journal.NewReader(f, st.Size(), journal.ReaderOptions{})
	if err != nil {
		return nil, fmt.Errorf("sdk: recording %s: %w", path, err)
	}
	defer jr.Close()
	acked := map[uint64]uint64{}
	for {
		b, err := jr.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
			return acked, nil // Open truncates a torn tail
		}
		if err != nil {
			return nil, fmt.Errorf("sdk: recording %s: %w", path, err)
		}
		for _, rec := range b.Records {
			var ev gatewayv0.ActivityEvent
			if err := proto.Unmarshal(rec.Data, &ev); err != nil {
				return nil, fmt.Errorf("sdk: recording %s: record %d: %w", path, rec.Seq, err)
			}
			acked[ev.GetEpoch()] = max(acked[ev.GetEpoch()], ev.GetSeq())
		}
	}
}

// Acked implements ActivityStore.
func (r *RecordingStore) Acked(_ context.Context, sessionID string, epoch uint64) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.open(sessionID)
	if err != nil {
		return 0, err
	}
	return rec.acked[epoch], nil
}

// Append implements ActivityStore. The events are written as one batch
// where they fit and synced before Append returns.
func (r *RecordingStore) Append(ctx context.Context, sessionID string, events []*gatewayv0.ActivityEvent) error {
	r.mu.Lock()
	rec, err := r.open(sessionID)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	recs := make([]journal.Record, len(events))
	for i, ev := range events {
		data, err := proto.Marshal(ev)
		if err != nil {
			return err
		}
		recs[i] = journal.Record{Kind: RecordingKind, Data: data}
	}
	if _, err := rec.w.AppendBatch(ctx, recs); err != nil {
		return err
	}
	r.mu.Lock()
	for _, ev := range events {
		rec.acked[ev.GetEpoch()] = max(rec.acked[ev.GetEpoch()], ev.GetSeq())
	}
	r.mu.Unlock()
	return nil
}

// ReadActivity implements ActivityReader: it reads the session's
// recording as far as it is written.
func (r *RecordingStore) ReadActivity(ctx context.Context, sessionID string, fn func(*gatewayv0.ActivityEvent) error) error {
	if !ValidSessionID(sessionID) {
		return fmt.Errorf("sdk: invalid session ID %q", sessionID)
	}
	f, err := os.Open(r.Path(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sdk: recording: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("sdk: recording: %w", err)
	}
	jr, err := journal.NewReader(f, st.Size(), journal.ReaderOptions{})
	if err != nil {
		return fmt.Errorf("sdk: recording %s: %w", r.Path(sessionID), err)
	}
	defer jr.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := jr.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
			return nil // a batch being written is read next time
		}
		if err != nil {
			return fmt.Errorf("sdk: recording %s: %w", r.Path(sessionID), err)
		}
		for _, rec := range b.Records {
			ev := &gatewayv0.ActivityEvent{}
			if err := proto.Unmarshal(rec.Data, ev); err != nil {
				return fmt.Errorf("sdk: recording %s: record %d: %w", r.Path(sessionID), rec.Seq, err)
			}
			if err := fn(ev); err != nil {
				return err
			}
		}
	}
}

// Release implements ActivityReleaser: it closes the session's recording,
// releasing its lock, so that another gateway replica sharing the directory
// can take the session over.
func (r *RecordingStore) Release(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.sessions[sessionID]
	if !ok {
		return nil
	}
	delete(r.sessions, sessionID)
	return rec.w.Close()
}

// Close closes all open recordings.
func (r *RecordingStore) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for id, rec := range r.sessions {
		errs = append(errs, rec.w.Close())
		delete(r.sessions, id)
	}
	return errors.Join(errs...)
}
