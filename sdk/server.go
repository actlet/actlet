// Package sdk is the orchestrator side of the Actlet gateway protocol: a
// gRPC service that Actlet workers connect to, for embedding in an
// orchestrator.
//
// A Server registers workers into sessions, hands them a profile and a task
// (decided by Options.Assign), sends them control commands and takes in the
// activity they publish, deduplicated and acknowledged so that workers can
// resend safely after a reconnect.
//
// The wire contract is defined in proto/actlet/gateway/v0. It is
// experimental (v0) and so is this package's API.
package sdk

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// ProtocolVersion is the gateway protocol version this package serves.
const ProtocolVersion = "v0"

// Options configure a Server.
type Options struct {
	// Assign decides the session, profile and task for a registering
	// worker. Required. req.SessionId is set when a worker resumes an
	// existing session. If the response leaves SessionId empty, the
	// requested ID is kept, or a new one is generated.
	Assign func(ctx context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error)

	// Authenticate checks the bearer token sent with every call. Nil
	// accepts every call. See StaticToken.
	Authenticate func(ctx context.Context, token string) error

	// Store keeps the activity workers publish and tells the server what
	// it already has. Nil means a MemoryStore.
	Store ActivityStore

	// OnActivity is called with every newly stored event, in order for
	// each session and epoch. It runs on the stream that received the
	// event, so a slow callback slows that stream's acknowledgements, but
	// never control.
	OnActivity func(sessionID string, ev *gatewayv0.ActivityEvent)

	// OnControl is called when a worker's control stream connects or
	// disconnects.
	OnControl func(s *Session, connected bool)

	// Logger receives diagnostics. Nil means slog.Default().
	Logger *slog.Logger
}

// StaticToken returns an Authenticate function that accepts one token.
func StaticToken(token string) func(context.Context, string) error {
	return func(_ context.Context, got string) error {
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			return errors.New("invalid token")
		}
		return nil
	}
}

// ServerOptions returns gRPC server options that match the keepalive
// settings Actlet workers use. Pass them to grpc.NewServer.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	}
}

// Server implements the gateway service. Create it with NewServer and add
// it to a grpc.Server with RegisterService.
type Server struct {
	gatewayv0.UnimplementedGatewayServer

	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session
	intake   map[intakeKey]*sync.Mutex
}

type intakeKey struct {
	session string
	epoch   uint64
}

// NewServer returns a Server.
func NewServer(opts Options) (*Server, error) {
	if opts.Assign == nil {
		return nil, errors.New("sdk: Options.Assign is required")
	}
	if opts.Store == nil {
		opts.Store = NewMemoryStore()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Server{
		opts:     opts,
		log:      opts.Logger,
		sessions: map[string]*Session{},
		intake:   map[intakeKey]*sync.Mutex{},
	}, nil
}

// RegisterService adds the gateway service to a gRPC server.
func (s *Server) RegisterService(r grpc.ServiceRegistrar) {
	gatewayv0.RegisterGatewayServer(r, s)
}

// Session returns the session with the given ID, or nil if no worker has
// registered it with this server.
func (s *Server) Session(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// Sessions returns all known sessions, oldest registration first.
func (s *Server) Sessions() []*Session {
	s.mu.Lock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].created.Before(out[j].created) })
	return out
}

func (s *Server) sessionFor(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		sess = newSession(s, id)
		s.sessions[id] = sess
	}
	return sess
}

func (s *Server) intakeLock(session string, epoch uint64) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := intakeKey{session, epoch}
	l, ok := s.intake[k]
	if !ok {
		l = &sync.Mutex{}
		s.intake[k] = l
	}
	return l
}

func (s *Server) authenticate(ctx context.Context) error {
	if s.opts.Authenticate == nil {
		return nil
	}
	var token string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			token, _ = strings.CutPrefix(v[0], "Bearer ")
		}
	}
	if err := s.opts.Authenticate(ctx, token); err != nil {
		return status.Errorf(codes.Unauthenticated, "authentication failed: %v", err)
	}
	return nil
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidSessionID reports whether id can be used as a session ID: 1 to 128
// letters, digits, dots, underscores or hyphens, not starting with a dot or
// symbol. IDs are safe to use as file names.
func ValidSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

// NewID returns a random 128-bit ID in hex, usable as a session or command
// ID.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Register implements the gateway's Register call.
func (s *Server) Register(ctx context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if req.GetProtocolVersion() != ProtocolVersion {
		return nil, status.Errorf(codes.FailedPrecondition, "unsupported protocol version %q, this gateway speaks %q", req.GetProtocolVersion(), ProtocolVersion)
	}
	if req.GetSessionId() != "" && !ValidSessionID(req.GetSessionId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid session ID %q", req.GetSessionId())
	}
	resp, err := s.opts.Assign(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, status.Error(codes.Internal, "no assignment")
	}
	if resp.SessionId == "" {
		resp.SessionId = req.GetSessionId()
		if resp.SessionId == "" {
			resp.SessionId = NewID()
		}
	}
	if !ValidSessionID(resp.SessionId) {
		return nil, status.Errorf(codes.Internal, "assigned invalid session ID %q", resp.SessionId)
	}
	sess := s.sessionFor(resp.SessionId)
	sess.registered(req)
	s.log.Info("worker registered", "session", resp.SessionId, "worker", req.GetWorkerId(), "epoch", req.GetEpoch())
	return resp, nil
}

// Control implements the gateway's Control stream.
func (s *Server) Control(stream gatewayv0.Gateway_ControlServer) error {
	if err := s.authenticate(stream.Context()); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first control message must be a hello")
	}
	sess := s.Session(hello.GetSessionId())
	if sess == nil {
		return status.Errorf(codes.NotFound, "unknown session %q: register first", hello.GetSessionId())
	}

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	conn := sess.attach(hello, cancel)
	defer sess.detach(conn)

	errc := make(chan error, 2)
	go func() { errc <- sess.sendCommands(ctx, conn, stream) }()
	go func() { errc <- sess.receiveControl(stream) }()
	err = <-errc
	cancel()
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// PublishActivity implements the gateway's activity stream: it stores new
// events exactly once, in order, and acknowledges the highest sequence
// number up to which it holds everything.
func (s *Server) PublishActivity(stream gatewayv0.Gateway_PublishActivityServer) error {
	ctx := stream.Context()
	if err := s.authenticate(ctx); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first activity message must be a hello")
	}
	sid, epoch := hello.GetSessionId(), hello.GetEpoch()
	if s.Session(sid) == nil {
		return status.Errorf(codes.NotFound, "unknown session %q: register first", sid)
	}
	if epoch == 0 {
		return status.Error(codes.InvalidArgument, "epoch must be at least 1")
	}
	lock := s.intakeLock(sid, epoch)

	lock.Lock()
	acked, err := s.opts.Store.Acked(ctx, sid, epoch)
	lock.Unlock()
	if err != nil {
		return status.Errorf(codes.Unavailable, "activity store: %v", err)
	}
	if err := stream.Send(&gatewayv0.ActivityAck{Epoch: epoch, AckedSeq: acked}); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		batch := msg.GetBatch()
		if batch == nil {
			continue
		}
		fresh, acked, err := s.intakeBatch(ctx, lock, sid, epoch, batch.GetEvents())
		if err != nil {
			return err
		}
		if s.opts.OnActivity != nil {
			for _, ev := range fresh {
				s.opts.OnActivity(sid, ev)
			}
		}
		if err := stream.Send(&gatewayv0.ActivityAck{Epoch: epoch, AckedSeq: acked}); err != nil {
			return err
		}
	}
}

// intakeBatch stores the events that directly follow what the store has,
// skipping duplicates. It stops at a gap; the worker resends from the ack.
func (s *Server) intakeBatch(ctx context.Context, lock *sync.Mutex, sid string, epoch uint64, events []*gatewayv0.ActivityEvent) ([]*gatewayv0.ActivityEvent, uint64, error) {
	lock.Lock()
	defer lock.Unlock()
	acked, err := s.opts.Store.Acked(ctx, sid, epoch)
	if err != nil {
		return nil, 0, status.Errorf(codes.Unavailable, "activity store: %v", err)
	}
	var fresh []*gatewayv0.ActivityEvent
	next := acked + 1
	for _, ev := range events {
		if ev.GetEpoch() != epoch {
			return nil, 0, status.Errorf(codes.InvalidArgument, "event %q has epoch %d on a stream for epoch %d", ev.GetId(), ev.GetEpoch(), epoch)
		}
		if ev.GetSeq() < next {
			continue // already stored
		}
		if ev.GetSeq() > next {
			break // a gap: wait for the worker to resend from the ack
		}
		if want := EventID(epoch, ev.GetSeq()); ev.GetId() != want {
			return nil, 0, status.Errorf(codes.InvalidArgument, "event ID %q, want %q", ev.GetId(), want)
		}
		fresh = append(fresh, ev)
		next++
	}
	if len(fresh) == 0 {
		return nil, acked, nil
	}
	if err := s.opts.Store.Append(ctx, sid, fresh); err != nil {
		return nil, 0, status.Errorf(codes.Unavailable, "activity store: %v", err)
	}
	return fresh, next - 1, nil
}

// EventID returns the ID of the activity event with the given epoch and
// sequence number.
func EventID(epoch, seq uint64) string { return fmt.Sprintf("%d.%d", epoch, seq) }
