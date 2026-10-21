// Package gwclient is the worker side of the gateway protocol: it connects
// out to the orchestrator's gateway, registers the session, and keeps the
// control and activity streams running across disconnects.
//
// Control and activity use separate streams with separate loops, so a slow
// or stalled activity stream never delays commands.
package gwclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	grpcbackoff "google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/actlet/actlet/internal/activity"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// ProtocolVersion is the gateway protocol version this client speaks.
const ProtocolVersion = "v0"

// Config configures a Client.
type Config struct {
	// Addr is the gateway's address, host:port.
	Addr string
	// Token is sent as a bearer token with every call.
	Token string
	// TokenFile names a file holding the token instead, read again for
	// every call, so that a rotated token is used from the next call or
	// reconnection on. A trailing newline is ignored. An empty or
	// unreadable file fails the call as unavailable, to be retried. Set
	// at most one of Token and TokenFile.
	TokenFile string
	// TLS enables TLS; CAFile optionally names a PEM file of CA
	// certificates to trust instead of the system's.
	TLS    bool
	CAFile string

	// BackoffMin and BackoffMax bound the delay between reconnect
	// attempts. Defaults 200ms and 10s.
	BackoffMin, BackoffMax time.Duration
	// RegisterTimeout bounds one registration attempt. Default 10s.
	RegisterTimeout time.Duration
	// Heartbeat is how often the control stream reports status. Default
	// 10s.
	Heartbeat time.Duration
	// AckTimeout is how long the activity stream may go without an
	// acknowledgement while events are outstanding before it reconnects.
	// Default 30s.
	AckTimeout time.Duration
	// Window bounds unacknowledged activity events in flight. Default 1024.
	Window int
	// InferenceIdle is how long a model call may go without a response
	// message before it is retried. Default 5m.
	InferenceIdle time.Duration

	Logger *slog.Logger
}

func (c *Config) setDefaults() {
	if c.BackoffMin <= 0 {
		c.BackoffMin = 200 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 10 * time.Second
	}
	if c.RegisterTimeout <= 0 {
		c.RegisterTimeout = 10 * time.Second
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = 10 * time.Second
	}
	if c.AckTimeout <= 0 {
		c.AckTimeout = 30 * time.Second
	}
	if c.Window <= 0 {
		c.Window = 1024
	}
	if c.InferenceIdle <= 0 {
		c.InferenceIdle = 5 * time.Minute
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Client is a connection to the gateway.
type Client struct {
	cfg Config
	log *slog.Logger
	cc  *grpc.ClientConn
	gw  gatewayv0.GatewayClient

	connected atomic.Bool // the control stream is up
}

// Connected reports whether the control stream is up.
func (c *Client) Connected() bool { return c.connected.Load() }

// Addr returns the gateway's address.
func (c *Client) Addr() string { return c.cfg.Addr }

// New returns a client for the gateway. It doesn't connect until first
// used, and reconnects as needed.
func New(cfg Config) (*Client, error) {
	cfg.setDefaults()
	if cfg.Addr == "" {
		return nil, errors.New("gateway address is required")
	}
	if cfg.Token != "" && cfg.TokenFile != "" {
		return nil, errors.New("give a token or a token file, not both")
	}
	creds, err := Credentials(cfg.TLS, cfg.CAFile)
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		// gRPC's own reconnect backoff grows to two minutes; keep it within
		// ours so a restarted gateway is found again quickly.
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: grpcbackoff.Config{
				BaseDelay: cfg.BackoffMin, Multiplier: 1.6, Jitter: 0.2, MaxDelay: cfg.BackoffMax,
			},
			MinConnectTimeout: 5 * time.Second,
		}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		// A registration's profile can carry skills of a few MiB.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxReceive)),
	}
	switch {
	case cfg.Token != "":
		opts = append(opts, grpc.WithPerRPCCredentials(bearer{token: cfg.Token, tls: cfg.TLS}))
	case cfg.TokenFile != "":
		opts = append(opts, grpc.WithPerRPCCredentials(BearerFile(cfg.TokenFile, cfg.TLS)))
	}
	cc, err := grpc.NewClient(cfg.Addr, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, log: cfg.Logger, cc: cc, gw: gatewayv0.NewGatewayClient(cc)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.cc.Close() }

// Credentials returns the transport credentials for a connection: TLS,
// trusting the CA certificates in the PEM file caFile if it is given, or
// else plaintext.
func Credentials(useTLS bool, caFile string) (credentials.TransportCredentials, error) {
	if !useTLS {
		return insecure.NewCredentials(), nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s holds no certificates", caFile)
		}
		tc.RootCAs = pool
	}
	return credentials.NewTLS(tc), nil
}

// Bearer returns per-call credentials that send the token with every
// call. Plaintext is allowed unless requireTLS, for development gateways.
func Bearer(token string, requireTLS bool) credentials.PerRPCCredentials {
	return bearer{token: token, tls: requireTLS}
}

// bearer sends the token with every call.
type bearer struct {
	token string
	tls   bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearer) RequireTransportSecurity() bool { return b.tls }

// BearerFile returns per-call credentials that send the token in a file
// with every call, reading the file again each time, so that a rotated
// token is picked up. A trailing newline is ignored. An empty or
// unreadable file fails the call as unavailable, so that it is retried.
func BearerFile(path string, requireTLS bool) credentials.PerRPCCredentials {
	return bearerFile{path: path, tls: requireTLS}
}

type bearerFile struct {
	path string
	tls  bool
}

// maxToken bounds a token file.
const maxToken = 64 << 10

func (b bearerFile) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := ReadToken(b.path)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (b bearerFile) RequireTransportSecurity() bool { return b.tls }

// ReadToken reads a token from a file, without its trailing newline. An
// empty file is an error. Errors name the file, never its contents.
func ReadToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	if len(data) > maxToken {
		return "", fmt.Errorf("token file %s is larger than %d bytes", path, maxToken)
	}
	token := strings.TrimRight(string(data), "\r\n")
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

// backoff is exponential with full jitter.
type backoff struct {
	min, max, cur time.Duration
}

func (b *backoff) next() time.Duration {
	if b.cur < b.min {
		b.cur = b.min
	} else {
		b.cur = min(b.cur*2, b.max)
	}
	return time.Duration(rand.Int64N(int64(b.cur)) + 1)
}

func (b *backoff) reset() { b.cur = 0 }

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// permanent reports errors that retrying won't fix.
func permanent(err error) bool {
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented:
		return true
	}
	return false
}

// Register registers the session, retrying with backoff until it succeeds,
// fails permanently or ctx ends.
func (c *Client) Register(ctx context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
	req.ProtocolVersion = ProtocolVersion
	b := backoff{min: c.cfg.BackoffMin, max: c.cfg.BackoffMax}
	for {
		rctx, cancel := context.WithTimeout(ctx, c.cfg.RegisterTimeout)
		resp, err := c.gw.Register(rctx, req, grpc.WaitForReady(true))
		cancel()
		if err == nil {
			return resp, nil
		}
		if permanent(err) {
			return nil, fmt.Errorf("register: %w", err)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d := b.next()
		c.log.Warn("gateway: registration failed, retrying", "err", err, "in", d.Round(time.Millisecond))
		if err := sleep(ctx, d); err != nil {
			return nil, err
		}
	}
}

// ControlHandler is the session side of the control stream.
type ControlHandler interface {
	// Hello describes the session when a control stream opens.
	Hello() *gatewayv0.ControlHello
	// Apply handles a command. An error means it wasn't recorded and must
	// not be acknowledged.
	Apply(ctx context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error)
	// State is the current session state, for heartbeats.
	State() gatewayv0.SessionState
	// Done is closed when the session has ended: the stream then reports
	// the final state and closes.
	Done() <-chan struct{}
}

// A FileServer is a ControlHandler that serves the session's files to
// viewers attached through the orchestrator.
type FileServer interface {
	// ReadFile reads a file the gateway asks for. It must be safe to call
	// concurrently.
	ReadFile(req *gatewayv0.FileRequest) *gatewayv0.FileResponse
}

// MaxFileReads bounds the file reads served at once; the gateway's
// further requests are refused until one finishes.
const MaxFileReads = 4

// Reregister registers the session again, when the gateway doesn't know it,
// for example after a gateway restart.
type Reregister func(ctx context.Context) error

// RunControl keeps a control stream open until the handler is done or ctx
// ends, reconnecting with backoff.
func (c *Client) RunControl(ctx context.Context, h ControlHandler, reregister Reregister) error {
	b := backoff{min: c.cfg.BackoffMin, max: c.cfg.BackoffMax}
	for {
		established, err := c.controlOnce(ctx, h)
		switch {
		case err == nil:
			return nil // the session ended and its final state was sent
		case ctx.Err() != nil:
			return ctx.Err()
		}
		if established {
			b.reset()
		}
		if status.Code(err) == codes.NotFound && reregister != nil {
			c.log.Info("gateway: session unknown, registering again")
			if rerr := reregister(ctx); rerr != nil {
				c.log.Warn("gateway: registering again failed", "err", rerr)
			}
		}
		d := b.next()
		c.log.Warn("gateway: control stream lost, reconnecting", "err", err, "in", d.Round(time.Millisecond))
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
}

// controlOnce runs one control stream. established reports whether the
// gateway answered at all.
func (c *Client) controlOnce(ctx context.Context, h ControlHandler) (established bool, err error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.gw.Control(sctx, grpc.WaitForReady(true))
	if err != nil {
		return false, err
	}
	var sendMu sync.Mutex
	send := func(m *gatewayv0.ControlUp) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}
	statusMsg := func() *gatewayv0.ControlUp {
		return &gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Status{Status: &gatewayv0.Status{State: h.State(), Time: timestamppb.Now()}}}
	}
	if err := send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Hello{Hello: h.Hello()}}); err != nil {
		return false, err
	}

	// Commands are received and applied on their own goroutine; this one
	// sends heartbeats and the final status. File reads run on goroutines
	// of their own, a few at a time, so they never hold commands up.
	recvErr := make(chan error, 1)
	var gotAny bool
	var gotMu sync.Mutex
	reads := make(chan struct{}, MaxFileReads)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			gotMu.Lock()
			gotAny = true
			gotMu.Unlock()
			if req := msg.GetReadFile(); req != nil {
				c.serveFile(h, req, reads, send)
				continue
			}
			cmd := msg.GetCommand()
			if cmd == nil {
				continue // a heartbeat
			}
			ack, err := h.Apply(sctx, cmd)
			if err != nil {
				c.log.Error("control: command not recorded, not acknowledging", "command", cmd.GetId(), "err", err)
				continue
			}
			if err := send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Ack{Ack: ack}}); err != nil {
				recvErr <- err
				return
			}
		}
	}()
	established = true // the stream opened and the hello was sent
	c.connected.Store(true)
	defer c.connected.Store(false)

	tick := time.NewTicker(c.cfg.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return established, ctx.Err()
		case err := <-recvErr:
			gotMu.Lock()
			defer gotMu.Unlock()
			return established && gotAny, err
		case <-tick.C:
			if err := send(statusMsg()); err != nil {
				return established, err
			}
		case <-h.Done():
			if err := send(statusMsg()); err != nil {
				return established, err
			}
			sendMu.Lock()
			err := stream.CloseSend()
			sendMu.Unlock()
			if err != nil {
				return established, err
			}
			// Let the gateway finish the stream, so the final status is
			// delivered before the stream is torn down.
			select {
			case err := <-recvErr:
				if !errors.Is(err, io.EOF) {
					c.log.Debug("control: stream ended after the final status", "err", err)
				}
			case <-time.After(2 * time.Second):
			}
			return established, nil
		}
	}
}

// serveFile answers a file request, or refuses it when the handler serves
// no files or too many reads are running.
func (c *Client) serveFile(h ControlHandler, req *gatewayv0.FileRequest, reads chan struct{}, send func(*gatewayv0.ControlUp) error) {
	reply := func(resp *gatewayv0.FileResponse) {
		resp.Id = req.GetId()
		if err := send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_File{File: resp}}); err != nil {
			c.log.Debug("control: answering a file request", "err", err)
		}
	}
	fs, ok := h.(FileServer)
	if !ok {
		reply(&gatewayv0.FileResponse{ErrorCode: int32(codes.Unimplemented), Error: "this worker serves no files"})
		return
	}
	select {
	case reads <- struct{}{}:
	default:
		reply(&gatewayv0.FileResponse{ErrorCode: int32(codes.ResourceExhausted), Error: "too many file reads at once"})
		return
	}
	go func() {
		defer func() { <-reads }()
		reply(fs.ReadFile(req))
	}()
}

// ActivitySource is one epoch's activity, as kept by an activity.Publisher.
type ActivitySource interface {
	Epoch() uint64
	Next(ctx context.Context, after uint64, max int) ([]*gatewayv0.ActivityEvent, error)
	Ack(seq uint64)
	Acked() uint64
	Done() bool
}

// RunActivity publishes a source's events until it is drained and fully
// acknowledged, or ctx ends, reconnecting with backoff.
func (c *Client) RunActivity(ctx context.Context, sessionID string, src ActivitySource, reregister Reregister) error {
	b := backoff{min: c.cfg.BackoffMin, max: c.cfg.BackoffMax}
	for {
		progressed, err := c.activityOnce(ctx, sessionID, src)
		switch {
		case err == nil && src.Done():
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			err = errStreamClosed // never give up on a source with events left
		}
		if progressed {
			b.reset()
		}
		if status.Code(err) == codes.NotFound && reregister != nil {
			if rerr := reregister(ctx); rerr != nil {
				c.log.Warn("gateway: registering again failed", "err", rerr)
			}
		}
		d := b.next()
		c.log.Warn("gateway: activity stream lost, reconnecting", "epoch", src.Epoch(), "err", err, "in", d.Round(time.Millisecond))
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
}

var (
	errAckTimeout   = errors.New("no acknowledgement in time")
	errStreamClosed = errors.New("activity stream closed")
)

func (c *Client) activityOnce(ctx context.Context, sessionID string, src ActivitySource) (progressed bool, err error) {
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c.log.Debug("activity: opening stream", "epoch", src.Epoch())
	stream, err := c.gw.PublishActivity(sctx, grpc.WaitForReady(true))
	if err != nil {
		return false, err
	}
	c.log.Debug("activity: stream open, sending hello", "epoch", src.Epoch())
	hello := &gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Hello{Hello: &gatewayv0.ActivityHello{SessionId: sessionID, Epoch: src.Epoch()}}}
	if err := stream.Send(hello); err != nil {
		return false, err
	}
	first, err := stream.Recv()
	if err != nil {
		return false, err
	}
	src.Ack(first.GetAckedSeq())
	sent := first.GetAckedSeq()
	c.log.Debug("activity: resuming", "epoch", src.Epoch(), "gateway_has", sent)

	// Acknowledgements arrive on their own goroutine.
	var mu sync.Mutex
	lastAck := time.Now()
	acked := make(chan struct{}, 1)
	go func() {
		for {
			ack, err := stream.Recv()
			if err != nil {
				c.log.Debug("activity: stream ended", "epoch", src.Epoch(), "err", err)
				cancel(err)
				return
			}
			src.Ack(ack.GetAckedSeq())
			mu.Lock()
			lastAck = time.Now()
			progressed = true
			mu.Unlock()
			select {
			case acked <- struct{}{}:
			default:
			}
		}
	}()
	// Reconnect if acknowledgements stall while events are outstanding.
	go func() {
		t := time.NewTicker(max(c.cfg.AckTimeout/4, 10*time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				mu.Lock()
				stalled := sent > src.Acked() && time.Since(lastAck) > c.cfg.AckTimeout
				mu.Unlock()
				if stalled {
					cancel(errAckTimeout)
					return
				}
			}
		}
	}()

	// fail reports why the stream ended. It never returns a nil error: a
	// Send can fail before the receiving goroutine has recorded the cause.
	fail := func(err error) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case ctx.Err() != nil:
			err = ctx.Err()
		case context.Cause(sctx) != nil:
			err = context.Cause(sctx)
		case err == nil:
			err = errStreamClosed
		}
		return progressed, err
	}
	for {
		// Keep at most Window events unacknowledged.
		for sent-src.Acked() >= uint64(c.cfg.Window) {
			select {
			case <-sctx.Done():
				return fail(nil)
			case <-acked:
			}
		}
		evs, err := src.Next(sctx, sent, min(256, c.cfg.Window))
		if errors.Is(err, activity.ErrDrained) {
			// Wait for the rest to be acknowledged, then finish.
			for !src.Done() {
				select {
				case <-sctx.Done():
					return fail(nil)
				case <-acked:
				}
			}
			_ = stream.CloseSend()
			return true, nil
		}
		if err != nil {
			return fail(err)
		}
		if err := stream.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Batch{Batch: &gatewayv0.ActivityBatch{Events: evs}}}); err != nil {
			return fail(fmt.Errorf("send: %w", err))
		}
		mu.Lock()
		if sent == src.Acked() {
			lastAck = time.Now() // the stall timer starts with the first outstanding event
		}
		sent = evs[len(evs)-1].GetSeq()
		mu.Unlock()
	}
}

// maxReceive bounds a message from the gateway.
const maxReceive = 64 << 20
