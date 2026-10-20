// Package attach lets viewers attach to a running box: the hub fans the
// run's activity out to them, and the server offers it, with the
// commands a viewer may send, over the attach protocol on a Unix socket in
// the session folder.
//
// Execution never depends on a viewer. Publishing to the hub never
// blocks, and everything it holds is bounded: a backlog of recent events,
// and a small buffer per viewer. A viewer that reads too slowly skips
// events and is told how many.
package attach

import (
	"sync"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Defaults of a hub's bounds.
const (
	DefaultBacklog    = 4096
	DefaultPinned     = 1024
	DefaultClientBuf  = 256
	DefaultMaxClients = 8
)

// HubOptions bound a hub.
type HubOptions struct {
	// Backlog is how many recent events the hub keeps for viewers that
	// attach later.
	Backlog int
	// Pinned is how many structural events (the run's start, its state,
	// subagents starting and ending) it keeps beyond the backlog, so a
	// late viewer still sees the run's shape.
	Pinned int
	// ClientBuf is each viewer's buffer, in events.
	ClientBuf int
	// MaxClients bounds the viewers attached at once.
	MaxClients int
}

func (o *HubOptions) setDefaults() {
	if o.Backlog <= 0 {
		o.Backlog = DefaultBacklog
	}
	if o.Pinned <= 0 {
		o.Pinned = DefaultPinned
	}
	if o.ClientBuf <= 0 {
		o.ClientBuf = DefaultClientBuf
	}
	if o.MaxClients <= 0 {
		o.MaxClients = DefaultMaxClients
	}
}

// Hub keeps recent activity and fans new events out to viewers.
type Hub struct {
	opts HubOptions

	mu      sync.Mutex
	ring    []*gatewayv0.ActivityEvent // recent events, oldest first
	pinned  []*gatewayv0.ActivityEvent // structural events older than ring
	clients map[*Subscription]struct{}
	closed  bool
}

// NewHub returns a hub.
func NewHub(opts HubOptions) *Hub {
	opts.setDefaults()
	return &Hub{opts: opts, clients: map[*Subscription]struct{}{}}
}

// pinnedKind reports events kept beyond the backlog.
func pinnedKind(ev *gatewayv0.ActivityEvent) bool {
	switch ev.GetBody().(type) {
	case *gatewayv0.ActivityEvent_Started, *gatewayv0.ActivityEvent_State, *gatewayv0.ActivityEvent_SessionInfo,
		*gatewayv0.ActivityEvent_SubagentSpawned, *gatewayv0.ActivityEvent_SubagentFinished:
		return true
	}
	return false
}

// Publish adds an event: it never blocks. The event must not change
// afterwards.
func (h *Hub) Publish(ev *gatewayv0.ActivityEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.ring = append(h.ring, ev)
	if len(h.ring) > h.opts.Backlog {
		old := h.ring[0]
		h.ring = h.ring[1:]
		if pinnedKind(old) {
			h.pinned = append(h.pinned, old)
			if len(h.pinned) > h.opts.Pinned {
				h.pinned = h.pinned[1:]
			}
		}
	}
	for c := range h.clients {
		c.offer(&attachv0.WatchResponse{Msg: &attachv0.WatchResponse_Event{Event: ev}})
	}
}

// Subscription is a viewer's feed: the backlog, then live events.
type Subscription struct {
	h  *Hub
	ch chan *attachv0.WatchResponse

	mu      sync.Mutex
	skipped uint64 // events dropped since the last skipped marker
}

// C returns the feed. It is closed when the hub closes.
func (s *Subscription) C() <-chan *attachv0.WatchResponse { return s.ch }

// offer delivers a message, or counts it as skipped if the viewer's
// buffer is full. The caller holds h.mu.
func (s *Subscription) offer(m *attachv0.WatchResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.skipped > 0 {
		marker := &attachv0.WatchResponse{Msg: &attachv0.WatchResponse_Skipped{Skipped: &attachv0.Skipped{
			Count: s.skipped, Reason: "the viewer read too slowly",
		}}}
		select {
		case s.ch <- marker:
			s.skipped = 0
		default:
			s.skipped++
			return
		}
	}
	select {
	case s.ch <- m:
	default:
		s.skipped++
	}
}

// Skipped returns the events dropped and not yet reported.
func (s *Subscription) Skipped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skipped
}

// Close detaches the viewer.
func (s *Subscription) Close() {
	h := s.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[s]; ok {
		delete(h.clients, s)
		close(s.ch)
	}
}

// ErrTooManyViewers is returned by Subscribe when the hub is full.
type ErrTooManyViewers struct{ Max int }

func (e ErrTooManyViewers) Error() string {
	return "too many viewers attached"
}

// Subscribe attaches a viewer. Its feed starts with what the hub holds
// after afterSeq (a skipped marker first if events are missing), then
// carries new events. Backlog and live events neither overlap nor leave
// a hole.
func (h *Hub) Subscribe(afterSeq uint64) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= h.opts.MaxClients {
		return nil, ErrTooManyViewers{h.opts.MaxClients}
	}
	var backlog []*gatewayv0.ActivityEvent
	for _, ev := range h.pinned {
		if ev.GetSeq() > afterSeq {
			backlog = append(backlog, ev)
		}
	}
	for _, ev := range h.ring {
		if ev.GetSeq() > afterSeq {
			backlog = append(backlog, ev)
		}
	}
	// Events between afterSeq and the backlog the hub no longer holds.
	var missing uint64
	next := afterSeq + 1
	for _, ev := range backlog {
		if ev.GetSeq() > next {
			missing += ev.GetSeq() - next
		}
		next = ev.GetSeq() + 1
	}
	size := h.opts.ClientBuf + len(backlog)
	if missing > 0 {
		size++
	}
	s := &Subscription{h: h, ch: make(chan *attachv0.WatchResponse, size)}
	if missing > 0 {
		s.ch <- &attachv0.WatchResponse{Msg: &attachv0.WatchResponse_Skipped{Skipped: &attachv0.Skipped{
			Count: missing, Reason: "older than what the box keeps",
		}}}
	}
	for _, ev := range backlog {
		s.ch <- &attachv0.WatchResponse{Msg: &attachv0.WatchResponse_Event{Event: ev}}
	}
	if h.closed {
		close(s.ch)
		return s, nil
	}
	h.clients[s] = struct{}{}
	return s, nil
}

// Close ends every feed.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		close(c.ch)
		delete(h.clients, c)
	}
}

// Held reports how many events the hub holds and how many messages wait
// in viewers' buffers, to check its bounds.
func (h *Hub) Held() (events, queued int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		queued += len(c.ch)
	}
	return len(h.ring) + len(h.pinned), queued
}
