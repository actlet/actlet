package sdk

import (
	"context"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/actlet/actlet/internal/attach"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// AttachSessionHeader is the request metadata in which a viewer names the
// session it attaches to.
const AttachSessionHeader = attach.SessionHeader

// fileTimeout bounds a viewer's file read through the worker.
const fileTimeout = 10 * time.Second

// maxRelayPending bounds the live events a relay holds while it reads a
// session's earlier activity from the store.
const maxRelayPending = attach.DefaultBacklog

// RegisterAttach adds the attach service to a gRPC server: viewers, such
// as `actlet-cli attach`, watch a session's activity, send it the
// commands they may, and read its files, all relayed through this server.
// Execution never depends on a viewer: a viewer that reads too slowly
// skips events.
//
// Attach calls are authenticated with Options.Authenticate, like workers'
// calls; grpc.Method tells them apart. Options.AuthorizeAttach and
// Options.AttachCommands decide what a viewer may do.
//
// A viewer that attaches gets the session's earlier activity, if the
// store is an ActivityReader, then live events. The server keeps a
// bounded backlog per watched session, as a box does.
func (s *Server) RegisterAttach(r grpc.ServiceRegistrar) {
	attachv0.RegisterAttachServer(r, &attachService{srv: s})
}

// An ActivityReader is an ActivityStore that reads a session's events
// back, for viewers that attach to it.
type ActivityReader interface {
	// ReadActivity calls fn with the session's stored events, in order of
	// epoch and sequence number, until fn returns an error. A session
	// with no events is no error.
	ReadActivity(ctx context.Context, sessionID string, fn func(*gatewayv0.ActivityEvent) error) error
}

type attachService struct {
	attachv0.UnimplementedAttachServer
	srv *Server
}

// session authenticates a call and finds the session it names.
func (a *attachService) session(ctx context.Context) (*Session, error) {
	if err := a.srv.authenticate(ctx); err != nil {
		return nil, err
	}
	id := attach.RequestedSession(ctx)
	if id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "name the session in the %q request metadata", AttachSessionHeader)
	}
	sess := a.srv.Session(id)
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "no session %q", id)
	}
	if err := a.authorize(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

func (a *attachService) authorize(ctx context.Context, sess *Session) error {
	if f := a.srv.opts.AuthorizeAttach; f != nil {
		if err := f(ctx, sess); err != nil {
			return status.Errorf(codes.PermissionDenied, "attaching to session %s: %v", sess.ID(), err)
		}
	}
	return nil
}

// commands returns what a viewer may send the session.
func (a *attachService) commands(ctx context.Context, sess *Session) []string {
	if f := a.srv.opts.AttachCommands; f != nil {
		return f(ctx, sess)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return attach.AllowedCommands(sess.localAttach)
}

// Sessions implements the attach Sessions call.
func (a *attachService) Sessions(ctx context.Context, _ *attachv0.SessionsRequest) (*attachv0.SessionsResponse, error) {
	if err := a.srv.authenticate(ctx); err != nil {
		return nil, err
	}
	resp := &attachv0.SessionsResponse{}
	for _, sess := range a.srv.Sessions() {
		if a.authorize(ctx, sess) != nil {
			continue
		}
		st, _ := sess.State()
		resp.Sessions = append(resp.Sessions, &attachv0.SessionSummary{
			SessionId: sess.ID(), WorkerId: sess.WorkerID(), Epoch: sess.Epoch(), State: st, Connected: sess.Connected(),
		})
	}
	return resp, nil
}

// Info implements the attach Info call. The box's own figures, such as
// its memory, aren't known here.
func (a *attachService) Info(ctx context.Context, _ *attachv0.InfoRequest) (*attachv0.InfoResponse, error) {
	sess, err := a.session(ctx)
	if err != nil {
		return nil, err
	}
	st, _ := sess.State()
	sess.mu.Lock()
	version := sess.version
	sess.mu.Unlock()
	return &attachv0.InfoResponse{
		SessionId: sess.ID(), Epoch: sess.Epoch(), WorkerId: sess.WorkerID(), ActletVersion: version, State: st,
		Commands: a.commands(ctx, sess),
	}, nil
}

// Command implements the attach Command call: the command is sent to the
// session with origin remote.
func (a *attachService) Command(ctx context.Context, req *attachv0.CommandRequest) (*gatewayv0.CommandAck, error) {
	sess, err := a.session(ctx)
	if err != nil {
		return nil, err
	}
	cmd, what, err := attach.ViewerCommand(req, gatewayv0.Origin_ORIGIN_REMOTE, "remote viewer")
	if err != nil {
		return nil, err
	}
	if !slices.Contains(a.commands(ctx, sess), what) {
		return nil, status.Errorf(codes.PermissionDenied, "a viewer may not %s this session", what)
	}
	a.srv.log.Info("attach: viewer command", "session", sess.ID(), "command", what, "actor", cmd.GetActor())
	return sess.Send(ctx, cmd)
}

// ReadFile implements the attach ReadFile call, asking the session's
// worker for the file.
func (a *attachService) ReadFile(ctx context.Context, req *attachv0.ReadFileRequest) (*attachv0.ReadFileResponse, error) {
	sess, err := a.session(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "no path")
	}
	ctx, cancel := context.WithTimeout(ctx, fileTimeout)
	defer cancel()
	f, err := sess.ReadFile(ctx, req.GetPath())
	if err != nil {
		return nil, err
	}
	return &attachv0.ReadFileResponse{Path: f.GetPath(), Content: f.GetContent(), Size: f.GetSize(),
		Truncated: f.GetTruncated(), Binary: f.GetBinary()}, nil
}

// Watch implements the attach Watch call.
func (a *attachService) Watch(req *attachv0.WatchRequest, stream attachv0.Attach_WatchServer) error {
	ctx := stream.Context()
	sess, err := a.session(ctx)
	if err != nil {
		return err
	}
	r, release, err := a.srv.watch(ctx, sess.ID())
	if err != nil {
		return err
	}
	defer release()
	sub, err := r.hub.Subscribe(req.GetAfterSeq())
	if err != nil {
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	defer sub.Close()
	for {
		select {
		case m, ok := <-sub.C():
			if !ok {
				return nil // the session ended
			}
			if err := stream.Send(m); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-a.srv.drain:
			return errDraining
		}
	}
}

// relay fans a watched session's activity out to its viewers: what the
// store holds, then what arrives.
type relay struct {
	hub      *attach.Hub
	ready    chan struct{}
	watchers int // guarded by Server.mu

	mu      sync.Mutex
	priming bool
	pending []*gatewayv0.ActivityEvent
	// The highest sequence number read from the store, per epoch.
	primed map[uint64]uint64
}

// watch returns the session's relay, starting it if it is the first
// viewer, once it holds the session's earlier activity. release detaches
// the viewer; the last one stops the relay.
func (s *Server) watch(ctx context.Context, sessionID string) (*relay, func(), error) {
	s.mu.Lock()
	r := s.relays[sessionID]
	if r == nil {
		r = &relay{hub: attach.NewHub(attach.HubOptions{}), ready: make(chan struct{}), priming: true, primed: map[uint64]uint64{}}
		s.relays[sessionID] = r
		go s.prime(r, sessionID)
	}
	r.watchers++
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.watchers--; r.watchers == 0 {
			if s.relays[sessionID] == r {
				delete(s.relays, sessionID)
			}
			r.hub.Close()
		}
	}
	select {
	case <-r.ready:
		return r, release, nil
	case <-ctx.Done():
		release()
		return nil, nil, ctx.Err()
	}
}

// prime reads the session's earlier activity into the relay, then lets
// live events through, skipping those it already read.
func (s *Server) prime(r *relay, sessionID string) {
	defer close(r.ready)
	if reader, ok := s.opts.Store.(ActivityReader); ok {
		err := reader.ReadActivity(context.Background(), sessionID, func(ev *gatewayv0.ActivityEvent) error {
			r.hub.Publish(ev)
			r.primed[ev.GetEpoch()] = max(r.primed[ev.GetEpoch()], ev.GetSeq())
			if ended(ev) {
				r.hub.Close()
			}
			return nil
		})
		if err != nil {
			s.log.Warn("attach: reading the session's activity", "session", sessionID, "err", err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.pending {
		if ev.GetSeq() > r.primed[ev.GetEpoch()] {
			r.push(ev)
		}
	}
	r.pending, r.priming = nil, false
}

// relayActivity passes newly stored events to the session's viewers, if
// it has any. It never blocks on a viewer.
func (s *Server) relayActivity(sessionID string, events []*gatewayv0.ActivityEvent) {
	s.mu.Lock()
	r := s.relays[sessionID]
	s.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.priming {
		r.pending = append(r.pending, events...)
		if n := len(r.pending) - maxRelayPending; n > 0 {
			r.pending = r.pending[n:] // viewers see the gap
		}
		return
	}
	for _, ev := range events {
		r.push(ev)
	}
}

// push publishes a live event; a session that ended has no more.
func (r *relay) push(ev *gatewayv0.ActivityEvent) {
	r.hub.Publish(ev)
	if ended(ev) {
		r.hub.Close()
	}
}

// ended reports whether an event ends the session for good: it completed
// or was stopped. A failed or paused session may run again.
func ended(ev *gatewayv0.ActivityEvent) bool {
	switch ev.GetState().GetTo() {
	case gatewayv0.SessionState_SESSION_STATE_COMPLETED, gatewayv0.SessionState_SESSION_STATE_STOPPED:
		return true
	}
	return false
}
