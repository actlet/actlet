package sdk

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Session is one Actlet session as the gateway sees it: the worker serving
// it, its last reported state, and the commands on their way to it.
//
// Commands are delivered in the order they were issued and stay queued
// until the worker acknowledges them. If the control stream drops, they are
// sent again on the next one; the worker recognises a repeated command ID
// and acknowledges it without applying it twice.
type Session struct {
	id      string
	srv     *Server
	created time.Time

	mu        sync.Mutex
	workerID  string
	epoch     uint64
	state     gatewayv0.SessionState
	stateTime time.Time
	conn      *controlConn
	pending   []*pendingCommand
	byID      map[string]*pendingCommand
	notify    chan struct{}
}

type pendingCommand struct {
	cmd    *gatewayv0.Command
	sentOn *controlConn
	done   chan struct{}
	ack    *gatewayv0.CommandAck
}

type controlConn struct {
	cancel context.CancelFunc
}

func newSession(srv *Server, id string) *Session {
	return &Session{
		id:      id,
		srv:     srv,
		created: time.Now(),
		byID:    map[string]*pendingCommand{},
		notify:  make(chan struct{}, 1),
	}
}

// ID returns the session ID.
func (s *Session) ID() string { return s.id }

// WorkerID returns the ID of the worker that last registered the session.
func (s *Session) WorkerID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workerID
}

// Epoch returns the epoch the worker last registered or connected with.
func (s *Session) Epoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// Connected reports whether a worker's control stream is attached.
func (s *Session) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// State returns the last state the worker reported and when.
func (s *Session) State() (gatewayv0.SessionState, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.stateTime
}

// Pending returns the number of commands waiting for an acknowledgement.
func (s *Session) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// Send delivers a command and waits for the worker's acknowledgement. An
// empty ID is replaced by a new one, and an unspecified origin means the
// orchestrator. Sending an ID that is still pending waits for the same
// acknowledgement.
//
// If ctx ends first, Send returns its error but the command stays queued:
// it was issued and will be delivered when a worker connects.
func (s *Session) Send(ctx context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error) {
	if cmd.Id == "" {
		cmd.Id = NewID()
	}
	if cmd.Origin == gatewayv0.Origin_ORIGIN_UNSPECIFIED {
		cmd.Origin = gatewayv0.Origin_ORIGIN_ORCHESTRATOR
	}
	s.mu.Lock()
	p, ok := s.byID[cmd.Id]
	if !ok {
		p = &pendingCommand{cmd: cmd, done: make(chan struct{})}
		s.byID[cmd.Id] = p
		s.pending = append(s.pending, p)
	}
	s.mu.Unlock()
	s.wake()

	select {
	case <-p.done:
		return p.ack, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ErrNoPauseMode is returned by Pause when no mode is given.
var ErrNoPauseMode = errors.New("sdk: pause needs a mode: drain or interrupt")

// Pause asks the worker to pause: stop starting new work in the given mode,
// persist, and exit. The session resumes when a worker is started again on
// its session folder.
func (s *Session) Pause(ctx context.Context, mode gatewayv0.PauseMode) (*gatewayv0.CommandAck, error) {
	if mode == gatewayv0.PauseMode_PAUSE_MODE_UNSPECIFIED {
		return nil, ErrNoPauseMode
	}
	return s.Send(ctx, &gatewayv0.Command{Action: &gatewayv0.Command_Pause{Pause: &gatewayv0.Pause{Mode: mode}}})
}

// StopOptions control how a session stops.
type StopOptions struct {
	// Grace is how long running work gets after being signalled before it
	// is killed. Zero means the profile's default.
	Grace time.Duration
	// Kill stops running work immediately.
	Kill bool
}

// Stop asks the worker to cancel the task, persist, and exit. A stopped
// session doesn't resume.
func (s *Session) Stop(ctx context.Context, opts StopOptions) (*gatewayv0.CommandAck, error) {
	stop := &gatewayv0.Stop{Kill: opts.Kill}
	if opts.Grace > 0 {
		stop.Grace = durationpb.New(opts.Grace)
	}
	return s.Send(ctx, &gatewayv0.Command{Action: &gatewayv0.Command_Stop{Stop: stop}})
}

// Steer passes an operator note to the session.
func (s *Session) Steer(ctx context.Context, note string) (*gatewayv0.CommandAck, error) {
	return s.Send(ctx, &gatewayv0.Command{Action: &gatewayv0.Command_Steer{Steer: &gatewayv0.Steer{Note: note}}})
}

func (s *Session) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Session) registered(req *gatewayv0.RegisterRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workerID = req.GetWorkerId()
	s.epoch = req.GetEpoch()
}

// attach makes conn the session's control stream, cancelling any previous
// one: a reconnecting worker replaces its stale stream.
func (s *Session) attach(hello *gatewayv0.ControlHello, cancel context.CancelFunc) *controlConn {
	conn := &controlConn{cancel: cancel}
	s.mu.Lock()
	old := s.conn
	s.conn = conn
	s.epoch = hello.GetEpoch()
	s.setStateLocked(hello.GetState())
	s.mu.Unlock()
	if old != nil {
		old.cancel()
	}
	if cb := s.srv.opts.OnControl; cb != nil {
		cb(s, true)
	}
	s.wake()
	return conn
}

func (s *Session) detach(conn *controlConn) {
	s.mu.Lock()
	current := s.conn == conn
	if current {
		s.conn = nil
	}
	s.mu.Unlock()
	if current {
		if cb := s.srv.opts.OnControl; cb != nil {
			cb(s, false)
		}
	}
}

func (s *Session) setStateLocked(st gatewayv0.SessionState) {
	if st != gatewayv0.SessionState_SESSION_STATE_UNSPECIFIED {
		s.state, s.stateTime = st, time.Now()
	}
}

// sendCommands sends every pending command not yet sent on this stream, in
// order, until ctx ends.
func (s *Session) sendCommands(ctx context.Context, conn *controlConn, stream interface {
	Send(*gatewayv0.ControlDown) error
}) error {
	for {
		s.mu.Lock()
		var batch []*gatewayv0.Command
		for _, p := range s.pending {
			if p.sentOn != conn {
				p.sentOn = conn
				batch = append(batch, p.cmd)
			}
		}
		s.mu.Unlock()
		for _, cmd := range batch {
			if err := stream.Send(&gatewayv0.ControlDown{Msg: &gatewayv0.ControlDown_Command{Command: cmd}}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.notify:
		}
	}
}

// receiveControl handles acknowledgements and status from the worker.
func (s *Session) receiveControl(stream interface {
	Recv() (*gatewayv0.ControlUp, error)
}) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		switch m := msg.Msg.(type) {
		case *gatewayv0.ControlUp_Ack:
			s.acknowledged(m.Ack)
		case *gatewayv0.ControlUp_Status:
			s.mu.Lock()
			s.setStateLocked(m.Status.GetState())
			s.mu.Unlock()
		}
	}
}

func (s *Session) acknowledged(ack *gatewayv0.CommandAck) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStateLocked(ack.GetState())
	p, ok := s.byID[ack.GetCommandId()]
	if !ok {
		return // a late duplicate acknowledgement
	}
	delete(s.byID, ack.GetCommandId())
	for i, q := range s.pending {
		if q == p {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	p.ack = ack
	close(p.done)
}
