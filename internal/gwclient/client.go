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
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
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
}

// New returns a client for the gateway. It doesn't connect until first
// used, and reconnects as needed.
func New(cfg Config) (*Client, error) {
	cfg.setDefaults()
	if cfg.Addr == "" {
		return nil, errors.New("gateway address is required")
	}
	creds := insecure.NewCredentials()
	if cfg.TLS {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("gateway CA file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("gateway CA file %s holds no certificates", cfg.CAFile)
			}
			tc.RootCAs = pool
		}
		creds = credentials.NewTLS(tc)
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
	}
	if cfg.Token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer{token: cfg.Token, tls: cfg.TLS}))
	}
	cc, err := grpc.NewClient(cfg.Addr, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, log: cfg.Logger, cc: cc, gw: gatewayv0.NewGatewayClient(cc)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.cc.Close() }

// bearer sends the token with every call. Plaintext is allowed for
// development gateways.
type bearer struct {
	token string
	tls   bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearer) RequireTransportSecurity() bool { return b.tls }

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
	// sends heartbeats and the final status.
	recvErr := make(chan error, 1)
	var gotAny bool
	var gotMu sync.Mutex
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
			cmd := msg.GetCommand()
			if cmd == nil {
				continue
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
			return established, err
		}
	}
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
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
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

var errAckTimeout = errors.New("no acknowledgement in time")

func (c *Client) activityOnce(ctx context.Context, sessionID string, src ActivitySource) (progressed bool, err error) {
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stream, err := c.gw.PublishActivity(sctx, grpc.WaitForReady(true))
	if err != nil {
		return false, err
	}
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

	// Acknowledgements arrive on their own goroutine.
	var mu sync.Mutex
	lastAck := time.Now()
	acked := make(chan struct{}, 1)
	go func() {
		for {
			ack, err := stream.Recv()
			if err != nil {
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

	fail := func() (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if cause := context.Cause(sctx); cause != nil && ctx.Err() == nil {
			return progressed, cause
		}
		return progressed, ctx.Err()
	}
	for {
		// Keep at most Window events unacknowledged.
		for sent-src.Acked() >= uint64(c.cfg.Window) {
			select {
			case <-sctx.Done():
				return fail()
			case <-acked:
			}
		}
		evs, err := src.Next(sctx, sent, min(256, c.cfg.Window))
		if errors.Is(err, activity.ErrDrained) {
			// Wait for the rest to be acknowledged, then finish.
			for !src.Done() {
				select {
				case <-sctx.Done():
					return fail()
				case <-acked:
				}
			}
			_ = stream.CloseSend()
			return true, nil
		}
		if err != nil {
			return fail()
		}
		if err := stream.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Batch{Batch: &gatewayv0.ActivityBatch{Events: evs}}}); err != nil {
			return fail()
		}
		mu.Lock()
		if sent == src.Acked() {
			lastAck = time.Now() // the stall timer starts with the first outstanding event
		}
		sent = evs[len(evs)-1].GetSeq()
		mu.Unlock()
	}
}
