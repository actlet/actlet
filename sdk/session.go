package sdk

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

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

	mu           sync.Mutex
	workerID     string
	version      string
	capabilities []string
	localAttach  *gatewayv0.LocalAttach
	epoch        uint64
	state        gatewayv0.SessionState
	stateTime    time.Time
	conn         *controlConn
	pending      []*pendingCommand
	byID         map[string]*pendingCommand
	notify       chan struct{}
	// File reads for viewers: waiting to be sent, and waiting for the
	// worker's answer.
	fileQueue []*gatewayv0.FileRequest
	files     map[string]chan *gatewayv0.FileResponse
	// Profile updates not refused, by command ID, in order: applied to the
	// profile again when the worker registers again.
	updates []*profileUpdate
}

type profileUpdate struct {
	id string
	u  *gatewayv0.ProfileUpdate
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
		files:   map[string]chan *gatewayv0.FileResponse{},
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

// UpdateProfile changes some of the session's profile mid-session: the
// fields u names, which must be ones the worker lets change (see
// gatewayv0.ProfileUpdate), and budgets to reset. Like any command, it is
// queued until the worker acknowledges it; the worker records the new
// configuration before it takes effect, and refuses an update naming
// other fields as a whole.
//
// The server also applies the update to the profile Assign returns when
// the worker registers again, so a box that resumes, such as after
// pausing for a budget, starts with it. Budget resets aren't applied
// again.
func (s *Session) UpdateProfile(ctx context.Context, u *gatewayv0.ProfileUpdate) (*gatewayv0.CommandAck, error) {
	cmd := &gatewayv0.Command{Id: NewID(), Action: &gatewayv0.Command_ProfileUpdate{ProfileUpdate: u}}
	keep := proto.Clone(u).(*gatewayv0.ProfileUpdate)
	keep.ResetBudgets = nil
	s.mu.Lock()
	s.updates = append(s.updates, &profileUpdate{id: cmd.Id, u: keep})
	s.mu.Unlock()
	ack, err := s.Send(ctx, cmd)
	if err == nil && ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
		s.mu.Lock()
		s.updates = slices.DeleteFunc(s.updates, func(p *profileUpdate) bool { return p.id == cmd.Id })
		s.mu.Unlock()
	}
	return ack, err
}

// withUpdates returns the profile with the session's profile updates
// applied, leaving p as it is.
func (s *Session) withUpdates(p *gatewayv0.Profile) *gatewayv0.Profile {
	s.mu.Lock()
	updates := slices.Clone(s.updates)
	s.mu.Unlock()
	if len(updates) == 0 || p == nil {
		return p
	}
	p = proto.Clone(p).(*gatewayv0.Profile)
	for _, pu := range updates {
		ApplyProfileUpdate(p, pu.u)
	}
	return p
}

// ApplyProfileUpdate sets the fields an update names in a profile, as a
// worker does. Fields a worker doesn't let change are left alone.
func ApplyProfileUpdate(p *gatewayv0.Profile, u *gatewayv0.ProfileUpdate) {
	from := u.GetProfile()
	limits := func() *gatewayv0.Limits {
		if p.Limits == nil {
			p.Limits = &gatewayv0.Limits{}
		}
		return p.Limits
	}
	for _, f := range u.GetFields() {
		switch f {
		case "limits.max_workers":
			limits().MaxWorkers = from.GetLimits().GetMaxWorkers()
		case "limits.max_tool_concurrency":
			limits().MaxToolConcurrency = from.GetLimits().GetMaxToolConcurrency()
		case "limits.max_subagents":
			limits().MaxSubagents = from.GetLimits().GetMaxSubagents()
		case "budgets":
			p.Budgets = from.GetBudgets()
		case "local_attach":
			p.LocalAttach = from.GetLocalAttach()
		}
	}
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

func (s *Session) registered(req *gatewayv0.RegisterRequest, profile *gatewayv0.Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workerID = req.GetWorkerId()
	s.version = req.GetActletVersion()
	s.capabilities = req.GetCapabilities()
	s.localAttach = profile.GetLocalAttach()
	s.epoch = req.GetEpoch()
}

// MaxFileReads bounds a session's file reads waiting for its worker; more
// are refused until one is answered.
const MaxFileReads = 16

// ReadFile asks the session's worker for a file of its session folder,
// for a viewer: one in inputs/, workspace/ or output/, named as tools
// name it, relative to workspace/. The worker cuts large files short. An
// error is a gRPC status: Unavailable when no worker is connected.
//
// A file read isn't a command: it isn't queued for a worker that isn't
// connected, and it isn't sent again after a reconnect. Give ctx a
// deadline.
func (s *Session) ReadFile(ctx context.Context, path string) (*gatewayv0.FileResponse, error) {
	s.mu.Lock()
	switch {
	case s.conn == nil:
		s.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "the box isn't connected")
	case !slices.Contains(s.capabilities, "attach.files"):
		s.mu.Unlock()
		return nil, status.Error(codes.Unimplemented, "the box doesn't serve files")
	case len(s.files) >= MaxFileReads:
		s.mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "too many file reads waiting")
	}
	req := &gatewayv0.FileRequest{Id: NewID(), Path: path}
	answer := make(chan *gatewayv0.FileResponse, 1)
	s.files[req.Id] = answer
	s.fileQueue = append(s.fileQueue, req)
	s.mu.Unlock()
	s.wake()
	defer func() {
		s.mu.Lock()
		delete(s.files, req.Id)
		s.fileQueue = slices.DeleteFunc(s.fileQueue, func(q *gatewayv0.FileRequest) bool { return q == req })
		s.mu.Unlock()
	}()
	select {
	case resp := <-answer:
		if c := codes.Code(resp.GetErrorCode()); c != codes.OK {
			return nil, status.Error(c, resp.GetError())
		}
		return resp, nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

// answered hands a worker's file to the read waiting for it.
func (s *Session) answered(resp *gatewayv0.FileResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.files[resp.GetId()]; ok {
		select {
		case ch <- resp:
		default:
		}
	}
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
// order, and heartbeats, until ctx ends.
func (s *Session) sendCommands(ctx context.Context, conn *controlConn, stream interface {
	Send(*gatewayv0.ControlDown) error
}) error {
	var heartbeat <-chan time.Time
	if d := s.srv.opts.Heartbeat; d > 0 {
		t := time.NewTicker(d)
		defer t.Stop()
		heartbeat = t.C
	}
	for {
		s.mu.Lock()
		var batch []*gatewayv0.Command
		for _, p := range s.pending {
			if p.sentOn != conn {
				p.sentOn = conn
				batch = append(batch, p.cmd)
			}
		}
		reads := s.fileQueue
		s.fileQueue = nil
		s.mu.Unlock()
		for _, cmd := range batch {
			if err := stream.Send(&gatewayv0.ControlDown{Msg: &gatewayv0.ControlDown_Command{Command: cmd}}); err != nil {
				return err
			}
		}
		for _, req := range reads {
			if err := stream.Send(&gatewayv0.ControlDown{Msg: &gatewayv0.ControlDown_ReadFile{ReadFile: req}}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.notify:
		case <-heartbeat:
			hb := &gatewayv0.ControlDown{Msg: &gatewayv0.ControlDown_Heartbeat{Heartbeat: &gatewayv0.Heartbeat{Time: timestamppb.Now()}}}
			if err := stream.Send(hb); err != nil {
				return err
			}
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
		case *gatewayv0.ControlUp_File:
			s.answered(m.File)
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
