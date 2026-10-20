package tools

import (
	"context"
	"sync"
)

// Locks serialises tool calls whose write sets conflict: two calls
// conflict when one writes a path the other writes, or a directory
// containing it, or when either writes everything. Calls that write
// nothing never wait. Waiting calls go in the order they asked.
//
// One Locks is shared by every worker of a session, since they share the
// workspace.
type Locks struct {
	mu    sync.Mutex
	held  []*lockEntry
	queue []*lockEntry
}

type lockEntry struct {
	all      bool
	paths    []string
	ready    chan struct{}
	admitted bool
}

// NewLocks returns an empty lock table.
func NewLocks() *Locks { return &Locks{} }

func (a *lockEntry) conflicts(b *lockEntry) bool {
	if a.all || b.all {
		return true
	}
	for _, p := range a.paths {
		for _, q := range b.paths {
			if within(p, q) || within(q, p) {
				return true
			}
		}
	}
	return false
}

// Acquire waits until the call may run, and returns the function that
// releases its lock. It returns ctx's error if ctx ends first.
func (l *Locks) Acquire(ctx context.Context, c *Call) (release func(), err error) {
	return l.Enqueue(c).Wait(ctx)
}

// Ticket is a call's place in the queue.
type Ticket struct {
	l *Locks
	e *lockEntry
}

// Enqueue takes the call's place in the queue, so that calls enqueued in
// order are let through in order, then waits with Wait.
func (l *Locks) Enqueue(c *Call) *Ticket {
	if !c.writesAll && len(c.writes) == 0 {
		return &Ticket{}
	}
	e := &lockEntry{all: c.writesAll, paths: c.writes, ready: make(chan struct{})}
	l.mu.Lock()
	l.queue = append(l.queue, e)
	l.admit()
	l.mu.Unlock()
	return &Ticket{l: l, e: e}
}

// Wait waits until the call may run, and returns the function that
// releases its lock. It returns ctx's error if ctx ends first.
func (t *Ticket) Wait(ctx context.Context) (release func(), err error) {
	if t.e == nil {
		return func() {}, nil
	}
	l, e := t.l, t.e
	release = func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, h := range l.held {
			if h == e {
				l.held = append(l.held[:i], l.held[i+1:]...)
				break
			}
		}
		l.admit()
	}
	select {
	case <-e.ready:
		return release, nil
	case <-ctx.Done():
		l.mu.Lock()
		if e.admitted {
			l.mu.Unlock()
			release()
		} else {
			for i, q := range l.queue {
				if q == e {
					l.queue = append(l.queue[:i], l.queue[i+1:]...)
					break
				}
			}
			l.admit()
			l.mu.Unlock()
		}
		return nil, ctx.Err()
	}
}

// admit lets waiting calls run, in order, that conflict neither with a
// running call nor with a call waiting before them.
func (l *Locks) admit() {
	var waiting []*lockEntry
	for _, e := range l.queue {
		ok := true
		for _, h := range l.held {
			if e.conflicts(h) {
				ok = false
				break
			}
		}
		for _, w := range waiting {
			if ok && e.conflicts(w) {
				ok = false
			}
		}
		if ok {
			e.admitted = true
			l.held = append(l.held, e)
			close(e.ready)
		} else {
			waiting = append(waiting, e)
		}
	}
	l.queue = waiting
}
