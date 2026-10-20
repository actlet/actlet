package attach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

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

// Server serves the attach protocol.
type Server struct {
	attachv0.UnimplementedAttachServer
	hub      *Hub
	box      Box
	commands []string
	log      *slog.Logger
	dir      string // the session folder, for ReadFile
}

// NewServer returns a server for the hub and box, allowing the commands.
func NewServer(hub *Hub, box Box, commands []string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{hub: hub, box: box, commands: commands, log: log}
}

// Register adds the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) { attachv0.RegisterAttachServer(r, s) }

// Info implements the Info call.
func (s *Server) Info(context.Context, *attachv0.InfoRequest) (*attachv0.InfoResponse, error) {
	resp := s.box.Info()
	resp.Commands = s.commands
	return resp, nil
}

// Watch implements the Watch call.
func (s *Server) Watch(req *attachv0.WatchRequest, stream attachv0.Attach_WatchServer) error {
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
	cmd := &gatewayv0.Command{Id: "local-" + newID(), Origin: gatewayv0.Origin_ORIGIN_LOCAL, Actor: req.GetActor()}
	if cmd.Actor == "" {
		cmd.Actor = "local viewer"
	}
	var what string
	switch a := req.GetAction().(type) {
	case *attachv0.CommandRequest_Steer:
		what = CommandSteer
		if strings.TrimSpace(a.Steer.GetNote()) == "" {
			return nil, status.Error(codes.InvalidArgument, "the note is empty")
		}
		cmd.Action = &gatewayv0.Command_Steer{Steer: a.Steer}
	case *attachv0.CommandRequest_Pause:
		what = CommandPause
		cmd.Action = &gatewayv0.Command_Pause{Pause: a.Pause}
	case *attachv0.CommandRequest_Stop:
		what = CommandStop
		cmd.Action = &gatewayv0.Command_Stop{Stop: a.Stop}
	default:
		return nil, status.Error(codes.InvalidArgument, "no command")
	}
	if !slices.Contains(s.commands, what) {
		return nil, status.Errorf(codes.PermissionDenied, "the profile doesn't let a local viewer %s the session", what)
	}
	s.log.Info("attach: local command", "command", what, "actor", cmd.Actor)
	ack, err := s.box.Apply(ctx, cmd)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return ack, nil
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

// Serve serves the attach protocol on the listener until Stop.
type Serve struct {
	gs  *grpc.Server
	lis *Listener
}

// Start serves srv on lis.
func Start(lis *Listener, srv *Server) *Serve {
	gs := grpc.NewServer()
	srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	return &Serve{gs: gs, lis: lis}
}

// Stop ends the viewers' calls and closes the socket.
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
	_ = s.lis.Close()
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
