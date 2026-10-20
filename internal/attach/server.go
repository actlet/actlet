package attach

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/actlet/actlet/internal/gwclient"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Local commands a viewer may be allowed to send.
const (
	CommandSteer = "steer"
	CommandPause = "pause"
	CommandStop  = "stop"
)

// AllowedCommands returns the commands a profile's local attach settings
// allow: steer notes only unless it says otherwise.
func AllowedCommands(la *gatewayv0.LocalAttach) []string {
	if la.GetDisabled() {
		return nil
	}
	cmds := la.GetCommands()
	if len(cmds) == 0 {
		return []string{CommandSteer}
	}
	var out []string
	for _, c := range cmds {
		if (c == CommandSteer || c == CommandPause || c == CommandStop) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// Box is the running session, as the server needs it.
type Box interface {
	// Info describes the session and the box's figures now.
	Info() *attachv0.InfoResponse
	// Apply handles a command like one from the gateway.
	Apply(ctx context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error)
}

// SessionHeader is the request metadata that names the session a call is
// about, for an orchestrator serving many.
const SessionHeader = "actlet-session"

// RequestedSession returns the session a call names in its metadata, or
// "".
func RequestedSession(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(SessionHeader); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// Server serves the attach protocol.
type Server struct {
	attachv0.UnimplementedAttachServer
	hub       *Hub
	box       Box
	commands  *commandSet
	log       *slog.Logger
	dir       string // the session folder, for ReadFile
	sessionID string
	origin    gatewayv0.Origin
}

// NewServer returns a server for the box's session, its hub and box,
// allowing the commands. Commands are recorded with origin local.
func NewServer(sessionID string, hub *Hub, box Box, commands []string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{hub: hub, box: box, commands: &commandSet{list: commands}, log: log, sessionID: sessionID, origin: gatewayv0.Origin_ORIGIN_LOCAL}
}

// commandSet is the commands viewers may send, which a profile update may
// change.
type commandSet struct {
	mu   sync.Mutex
	list []string
}

func (c *commandSet) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.list
}

// SetCommands changes the commands viewers may send, for this server and
// those Remote made from it.
func (s *Server) SetCommands(commands []string) {
	s.commands.mu.Lock()
	defer s.commands.mu.Unlock()
	s.commands.list = commands
}

// Remote returns a server like s for viewers elsewhere: their commands
// are recorded with origin remote.
func (s *Server) Remote() *Server {
	r := *s
	r.origin = gatewayv0.Origin_ORIGIN_REMOTE
	return &r
}

// Register adds the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) { attachv0.RegisterAttachServer(r, s) }

// checkSession refuses a call about another session.
func (s *Server) checkSession(ctx context.Context) error {
	if id := RequestedSession(ctx); id != "" && id != s.sessionID {
		return status.Errorf(codes.NotFound, "this box runs session %s, not %s", s.sessionID, id)
	}
	return nil
}

// Info implements the Info call.
func (s *Server) Info(ctx context.Context, _ *attachv0.InfoRequest) (*attachv0.InfoResponse, error) {
	if err := s.checkSession(ctx); err != nil {
		return nil, err
	}
	resp := s.box.Info()
	resp.Commands = s.commands.get()
	return resp, nil
}

// Sessions implements the Sessions call: the box's own session.
func (s *Server) Sessions(context.Context, *attachv0.SessionsRequest) (*attachv0.SessionsResponse, error) {
	info := s.box.Info()
	return &attachv0.SessionsResponse{Sessions: []*attachv0.SessionSummary{{
		SessionId: s.sessionID, WorkerId: info.GetWorkerId(), Epoch: info.GetEpoch(), State: info.GetState(), Connected: true,
	}}}, nil
}

// Watch implements the Watch call.
func (s *Server) Watch(req *attachv0.WatchRequest, stream attachv0.Attach_WatchServer) error {
	if err := s.checkSession(stream.Context()); err != nil {
		return err
	}
	sub, err := s.hub.Subscribe(req.GetAfterSeq())
	if err != nil {
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	defer sub.Close()
	for {
		select {
		case m, ok := <-sub.C():
			if !ok {
				return nil // the run ended
			}
			if err := stream.Send(m); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// Command implements the Command call.
func (s *Server) Command(ctx context.Context, req *attachv0.CommandRequest) (*gatewayv0.CommandAck, error) {
	if err := s.checkSession(ctx); err != nil {
		return nil, err
	}
	who := "local"
	if s.origin == gatewayv0.Origin_ORIGIN_REMOTE {
		who = "remote"
	}
	cmd, what, err := ViewerCommand(req, s.origin, who+" viewer")
	if err != nil {
		return nil, err
	}
	cmd.Id = who + "-" + newID()
	if !slices.Contains(s.commands.get(), what) {
		return nil, status.Errorf(codes.PermissionDenied, "the profile doesn't let a %s viewer %s the session", who, what)
	}
	s.log.Info("attach: viewer command", "command", what, "origin", who, "actor", cmd.Actor)
	ack, err := s.box.Apply(ctx, cmd)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return ack, nil
}

// ViewerCommand turns a viewer's request into a command with the origin,
// naming the actor def when the viewer gives no name, and says which
// command it is: CommandSteer, CommandPause or CommandStop. The command
// has no ID yet.
func ViewerCommand(req *attachv0.CommandRequest, origin gatewayv0.Origin, def string) (*gatewayv0.Command, string, error) {
	cmd := &gatewayv0.Command{Origin: origin, Actor: req.GetActor()}
	if cmd.Actor == "" {
		cmd.Actor = def
	}
	var what string
	switch a := req.GetAction().(type) {
	case *attachv0.CommandRequest_Steer:
		what = CommandSteer
		if strings.TrimSpace(a.Steer.GetNote()) == "" {
			return nil, "", status.Error(codes.InvalidArgument, "the note is empty")
		}
		cmd.Action = &gatewayv0.Command_Steer{Steer: a.Steer}
	case *attachv0.CommandRequest_Pause:
		what = CommandPause
		cmd.Action = &gatewayv0.Command_Pause{Pause: a.Pause}
	case *attachv0.CommandRequest_Stop:
		what = CommandStop
		cmd.Action = &gatewayv0.Command_Stop{Stop: a.Stop}
	default:
		return nil, "", status.Error(codes.InvalidArgument, "no command")
	}
	return cmd, what, nil
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Files in the session's state directory.
const (
	SocketFile = "attach.sock"
	// AddrFile names the socket a box listens on, which is elsewhere when
	// the state directory's path is too long for a socket.
	AddrFile = "attach.addr"
)

// maxSocketPath is a safe socket path length on every platform (macOS
// allows 103 bytes).
const maxSocketPath = 100

// Listener is the attach socket of a box.
type Listener struct {
	net.Listener
	Path     string
	stateDir string
	tmpDir   string
}

// Listen opens the attach socket for a box whose state directory is
// stateDir, replacing a stale one left by a crashed run. It fails if
// another box is listening there.
func Listen(stateDir string) (*Listener, error) {
	path := filepath.Join(stateDir, SocketFile)
	tmpDir := ""
	if len(path) > maxSocketPath {
		var err error
		if tmpDir, err = os.MkdirTemp("", "actlet-"); err != nil {
			return nil, err
		}
		path = filepath.Join(tmpDir, SocketFile)
	}
	if prev, err := Endpoint(stateDir); err == nil {
		if c, err := net.DialTimeout("unix", prev, time.Second); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("attach: a box is already listening on %s", prev)
		}
		_ = os.Remove(prev)
	}
	_ = os.Remove(path)
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	// Only the box's own user may attach.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stateDir, AddrFile), []byte(path+"\n"), 0o600); err != nil {
		_ = lis.Close()
		return nil, err
	}
	return &Listener{Listener: lis, Path: path, stateDir: stateDir, tmpDir: tmpDir}, nil
}

// Close closes the socket and removes its files.
func (l *Listener) Close() error {
	err := l.Listener.Close()
	_ = os.Remove(l.Path)
	_ = os.Remove(filepath.Join(l.stateDir, AddrFile))
	if l.tmpDir != "" {
		_ = os.RemoveAll(l.tmpDir)
	}
	return err
}

// Endpoint returns the attach socket of the box whose state directory is
// stateDir.
func Endpoint(stateDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, AddrFile))
	if errors.Is(err, os.ErrNotExist) {
		path := filepath.Join(stateDir, SocketFile)
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("no box is running in this session folder (no %s)", path)
		}
		return path, nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Serve serves the attach protocol on a listener until Stop.
type Serve struct {
	gs    *grpc.Server
	close func() error
}

// Start serves srv on the session folder's socket.
func Start(lis *Listener, srv *Server) *Serve {
	gs := grpc.NewServer()
	srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	return &Serve{gs: gs, close: lis.Close}
}

// Stop ends the viewers' calls and closes the listener.
func (s *Serve) Stop() {
	done := make(chan struct{})
	go func() {
		s.gs.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s.gs.Stop()
	}
	_ = s.close()
}

// ListenPort opens a box's attach port on addr, host:port. It must be a
// loopback address, reached for example through `kubectl port-forward`,
// unless allowRemote.
func ListenPort(addr string, allowRemote bool) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("attach port: %w", err)
	}
	if !allowRemote && !loopback(host) {
		return nil, fmt.Errorf("attach port: %s isn't a loopback address; allow other addresses explicitly", addr)
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("attach port: %w", err)
	}
	return lis, nil
}

// loopback reports whether host is a loopback name or address.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// StartPort serves srv on a box's attach port to viewers that send the
// token.
func StartPort(lis net.Listener, srv *Server, token string) *Serve {
	auth := Auth(func(_ context.Context, got string) error {
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			return errors.New("invalid token")
		}
		return nil
	})
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(auth.Unary), grpc.ChainStreamInterceptor(auth.Stream))
	srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	return &Serve{gs: gs, close: func() error { return nil }} // the server closes lis
}

// Auth checks a call's bearer token; its interceptors refuse calls it
// fails.
type Auth func(ctx context.Context, token string) error

func (a Auth) check(ctx context.Context) error {
	if err := a(ctx, BearerToken(ctx)); err != nil {
		return status.Errorf(codes.Unauthenticated, "authentication failed: %v", err)
	}
	return nil
}

// Unary checks unary calls.
func (a Auth) Unary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	if err := a.check(ctx); err != nil {
		return nil, err
	}
	return next(ctx, req)
}

// Stream checks streaming calls.
func (a Auth) Stream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
	if err := a.check(ss.Context()); err != nil {
		return err
	}
	return next(srv, ss)
}

// BearerToken returns the bearer token a call sends, or "".
func BearerToken(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			token, _ := strings.CutPrefix(v[0], "Bearer ")
			return token
		}
	}
	return ""
}

// Dial connects to the box whose state directory is stateDir.
func Dial(stateDir string) (attachv0.AttachClient, func() error, error) {
	path, err := Endpoint(stateDir)
	if err != nil {
		return nil, nil, err
	}
	cc, err := grpc.NewClient("passthrough:///attach",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return attachv0.NewAttachClient(cc), cc.Close, nil
}

// RemoteOptions configure a connection to an orchestrator's attach
// service or a box's attach port.
type RemoteOptions struct {
	// Token is sent as a bearer token with every call.
	Token string
	// TLS enables TLS; CAFile optionally names a PEM file of CA
	// certificates to trust instead of the system's.
	TLS    bool
	CAFile string
	// Session names the session every call is about, for an orchestrator.
	Session string
}

// DialRemote connects to an attach service at addr, host:port.
func DialRemote(addr string, o RemoteOptions) (attachv0.AttachClient, func() error, error) {
	creds, err := gwclient.Credentials(o.TLS, o.CAFile)
	if err != nil {
		return nil, nil, err
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if o.Token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(gwclient.Bearer(o.Token, o.TLS)))
	}
	if o.Session != "" {
		opts = append(opts,
			grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				return invoker(metadata.AppendToOutgoingContext(ctx, SessionHeader, o.Session), method, req, reply, cc, opts...)
			}),
			grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
				return streamer(metadata.AppendToOutgoingContext(ctx, SessionHeader, o.Session), desc, cc, method, opts...)
			}))
	}
	cc, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, nil, err
	}
	return attachv0.NewAttachClient(cc), cc.Close, nil
}
