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

	// Authenticate checks the bearer token sent with every call, workers'
	// and viewers' (see RegisterAttach); grpc.Method(ctx) tells them
	// apart. Nil accepts every call. See StaticToken.
	Authenticate func(ctx context.Context, token string) error

	// AuthorizeAttach decides whether an authenticated viewer may attach
	// to a session; an error refuses it. Nil lets every authenticated
	// viewer attach to every session.
	AuthorizeAttach func(ctx context.Context, s *Session) error
	// AttachCommands returns the commands a viewer may send a session:
	// "steer", "pause" and "stop". Nil allows what the session's profile
	// lets local viewers send: steer notes, unless it says otherwise.
	AttachCommands func(ctx context.Context, s *Session) []string
	// AttachViewerLabel names a viewer to the session's other viewers,
	// such as by the user its token authenticates. Without it, or when it
	// returns "", viewers are listed by the name they give themselves.
	AttachViewerLabel func(ctx context.Context) string

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

	// OnInference answers the model calls of workers' agents; see
	// InferenceCall. An error is returned to the worker as the call's
	// status: the worker retries codes.Unavailable, ResourceExhausted,
	// Aborted and DeadlineExceeded with the same intent ID, and fails the
	// session on others. Nil makes the gateway serve no models.
	OnInference func(ctx context.Context, call *InferenceCall) error
	// InferenceCache is how many answered calls the server keeps to answer
	// repeated intent IDs, of model calls and of bridged tool calls each.
	// Zero means DefaultInferenceCache. Calls are kept in memory: a call
	// repeated to another replica, or after a restart, runs again unless
	// the hook recognises its intent ID itself.
	InferenceCache int

	// OnDecide answers the agents' decide calls: route the question to a
	// person, a model or a decision system of your choosing. The call may
	// take as long as deciding does; the worker waits, also across pauses
	// and restarts, asking again with the same intent ID. An error is
	// returned to the worker as the call's status, as for OnInference.
	// Nil leaves decide calls to OnTool.
	OnDecide func(ctx context.Context, d *DecideRequest) (*Decision, error)
	// OnTool answers the agents' calls of other bridged tools. Nil makes
	// the gateway provide none.
	OnTool func(ctx context.Context, call *ToolCall) (*gatewayv0.ToolCallResponse, error)

	// Heartbeat is how often the server sends a heartbeat on each control
	// stream, so that proxies don't cut a stream that is idle in the
	// gateway-to-worker direction. Zero means DefaultHeartbeat; a negative
	// value turns heartbeats off.
	Heartbeat time.Duration

	// Logger receives diagnostics. Nil means slog.Default().
	Logger *slog.Logger
}

// DefaultHeartbeat is the default control stream heartbeat interval: well
// within common proxy read timeouts, such as NGINX's default of 60s.
const DefaultHeartbeat = 15 * time.Second

// StaticToken returns an Authenticate function that accepts one token.
func StaticToken(token string) func(context.Context, string) error {
	return func(_ context.Context, got string) error {
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			return errors.New("invalid token")
		}
		return nil
	}
}

// MaxMessageSize is the largest message a gateway must accept from a
// worker: a model call carries the whole conversation, which can be large
// before the worker compacts it. gRPC's default is 4 MiB.
const MaxMessageSize = 64 << 20

// ServerOptions returns gRPC server options that match what Actlet
// workers send: the keepalive settings they use, and messages up to
// MaxMessageSize. Pass them to grpc.NewServer.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(MaxMessageSize),
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
	// publishing counts the open activity streams per session, for
	// ActivityReleaser stores.
	publishing map[string]*publishers
	// Model calls by intent ID, and the answered ones, oldest first.
	inferences map[inferenceKey]*inference
	answered   []inferenceKey
	// Bridged tool calls by intent ID, and the answered ones, oldest
	// first.
	toolCalls     map[toolKey]*toolResult
	toolsAnswered []toolKey
	// The sessions viewers watch.
	relays map[string]*relay

	// Draining: set once by Shutdown; streams watch drain and end.
	drainOnce sync.Once
	drain     chan struct{}
	streams   sync.WaitGroup
}

type publishers struct {
	mu sync.Mutex
	n  int
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
	if opts.Heartbeat == 0 {
		opts.Heartbeat = DefaultHeartbeat
	}
	if opts.InferenceCache <= 0 {
		opts.InferenceCache = DefaultInferenceCache
	}
	return &Server{
		opts:       opts,
		log:        opts.Logger,
		sessions:   map[string]*Session{},
		intake:     map[intakeKey]*sync.Mutex{},
		publishing: map[string]*publishers{},
		inferences: map[inferenceKey]*inference{},
		toolCalls:  map[toolKey]*toolResult{},
		relays:     map[string]*relay{},
		drain:      make(chan struct{}),
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

// publish records an activity stream of the session opening, and
// returns the function to call when it ends. When the session's last
// stream ends, an ActivityReleaser store releases the session. A stream
// opening meanwhile waits, so it never sees a store that is releasing.
func (s *Server) publish(session string) (end func()) {
	s.mu.Lock()
	p, ok := s.publishing[session]
	if !ok {
		p = &publishers{}
		s.publishing[session] = p
	}
	s.mu.Unlock()
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.n--
		if r, ok := s.opts.Store.(ActivityReleaser); ok && p.n == 0 {
			if err := r.Release(session); err != nil {
				s.log.Warn("activity store: releasing the session", "session", session, "err", err)
			}
		}
	}
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

// errDraining tells a worker to go elsewhere: it reconnects, and a new
// connection reaches another replica.
var errDraining = status.Error(codes.Unavailable, "gateway draining: reconnect")

func (s *Server) draining() bool {
	select {
	case <-s.drain:
		return true
	default:
		return false
	}
}

// Shutdown drains the gateway. It refuses new registrations and streams,
// ends every control stream, and lets each activity stream finish the batch
// it is storing and acknowledge it before ending. Workers see their streams
// end as unavailable and reconnect; behind a load balancer they reach
// another replica. Shutdown returns when all streams have ended, or with
// ctx's error.
//
// Commands still waiting for an acknowledgement stay queued in this
// server: an orchestrator running several replicas must hand them over
// itself, or resend them; workers recognise resent command IDs.
//
// Use Drain to drain and stop a grpc.Server in the right order.
func (s *Server) Shutdown(ctx context.Context) error {
	s.drainOnce.Do(func() { close(s.drain) })
	done := make(chan struct{})
	go func() {
		s.streams.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Drain stops gs gracefully. It first stops gs from accepting new streams
// on existing connections (a GOAWAY), so that reconnecting workers open new
// connections, then drains the gateway's streams with Shutdown. If ctx ends
// first, gs is stopped forcibly.
func (s *Server) Drain(ctx context.Context, gs *grpc.Server) error {
	stopped := make(chan struct{})
	go func() {
		gs.GracefulStop()
		close(stopped)
	}()
	err := s.Shutdown(ctx)
	select {
	case <-stopped:
	case <-ctx.Done():
		gs.Stop()
		<-stopped
		if err == nil {
			err = ctx.Err()
		}
	}
	return err
}

// Register implements the gateway's Register call.
func (s *Server) Register(ctx context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if s.draining() {
		return nil, errDraining
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
	if missing := MissingFeatures(resp.GetProfile().GetRequiredFeatures(), req.GetCapabilities()); len(missing) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "the profile requires features this worker doesn't support: %s", strings.Join(missing, ", "))
	}
	sess := s.sessionFor(resp.SessionId)
	resp.Profile = sess.withUpdates(resp.GetProfile())
	sess.registered(req, resp.GetProfile())
	s.log.Info("worker registered", "session", resp.SessionId, "worker", req.GetWorkerId(), "epoch", req.GetEpoch())
	return resp, nil
}

// Control implements the gateway's Control stream.
func (s *Server) Control(stream gatewayv0.Gateway_ControlServer) error {
	if err := s.authenticate(stream.Context()); err != nil {
		return err
	}
	if s.draining() {
		return errDraining
	}
	s.streams.Add(1)
	defer s.streams.Done()
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
	select {
	case err = <-errc:
	case <-s.drain:
		cancel()
		return errDraining
	}
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
	if s.draining() {
		return errDraining
	}
	s.streams.Add(1)
	defer s.streams.Done()
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
	defer s.publish(sid)()
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

	// Receive on a separate goroutine, so that draining can end the stream
	// between batches: a batch being stored is finished and acknowledged
	// first.
	type received struct {
		msg *gatewayv0.ActivityUp
		err error
	}
	recv := make(chan received)
	go func() {
		for {
			msg, err := stream.Recv()
			select {
			case recv <- received{msg, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		var r received
		select {
		case r = <-recv:
		case <-ctx.Done():
			// The worker went away; the receiving goroutine may have
			// ended without handing over its error.
			return ctx.Err()
		case <-s.drain:
			return errDraining
		}
		msg, err := r.msg, r.err
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
		s.relayActivity(sid, fresh)
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

// MissingFeatures returns the required features that are not among the
// supported ones.
func MissingFeatures(required, supported []string) []string {
	have := make(map[string]bool, len(supported))
	for _, f := range supported {
		have[f] = true
	}
	var missing []string
	for _, f := range required {
		if !have[f] {
			missing = append(missing, f)
		}
	}
	return missing
}
