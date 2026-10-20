package gwclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/actlet/actlet/internal/activity"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// fakeGateway is a scriptable gateway: each test sets the handlers it needs.
type fakeGateway struct {
	gatewayv0.UnimplementedGatewayServer
	register func(context.Context, *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error)
	control  func(gatewayv0.Gateway_ControlServer) error
	publish  func(gatewayv0.Gateway_PublishActivityServer) error
	infer    func(*gatewayv0.InferenceRequest, gatewayv0.Gateway_InferServer) error
	callTool func(context.Context, *gatewayv0.ToolCallRequest) (*gatewayv0.ToolCallResponse, error)
}

func (f *fakeGateway) CallTool(ctx context.Context, req *gatewayv0.ToolCallRequest) (*gatewayv0.ToolCallResponse, error) {
	return f.callTool(ctx, req)
}

func (f *fakeGateway) Infer(req *gatewayv0.InferenceRequest, s gatewayv0.Gateway_InferServer) error {
	return f.infer(req, s)
}

func (f *fakeGateway) Register(ctx context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
	return f.register(ctx, req)
}

func (f *fakeGateway) Control(s gatewayv0.Gateway_ControlServer) error { return f.control(s) }

func (f *fakeGateway) PublishActivity(s gatewayv0.Gateway_PublishActivityServer) error {
	return f.publish(s)
}

// serve starts the fake gateway and returns a client for it.
func serve(t *testing.T, f *fakeGateway, cfg Config) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	gatewayv0.RegisterGatewayServer(gs, f)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cfg.Addr = lis.Addr().String()
	if cfg.BackoffMin == 0 {
		cfg.BackoffMin, cfg.BackoffMax = 5*time.Millisecond, 20*time.Millisecond
	}
	cfg.Logger = slog.New(slog.DiscardHandler)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestRegisterRetriesUntilSuccess(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, &fakeGateway{register: func(ctx context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer tok" {
			return nil, status.Error(codes.Unauthenticated, "bad token")
		}
		if req.GetProtocolVersion() != ProtocolVersion {
			return nil, status.Error(codes.FailedPrecondition, "bad version")
		}
		if calls.Add(1) < 3 {
			return nil, status.Error(codes.Unavailable, "not yet")
		}
		return &gatewayv0.RegisterResponse{SessionId: "s1"}, nil
	}}, Config{Token: "tok"})

	resp, err := c.Register(ctxT(t, 5*time.Second), &gatewayv0.RegisterRequest{WorkerId: "w"})
	if err != nil || resp.GetSessionId() != "s1" {
		t.Fatalf("Register = %v, %v", resp, err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("%d attempts, want 3", n)
	}
}

func TestRegisterPermanentErrorStops(t *testing.T) {
	for _, code := range []codes.Code{codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument, codes.FailedPrecondition} {
		var calls atomic.Int32
		c := serve(t, &fakeGateway{register: func(context.Context, *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			calls.Add(1)
			return nil, status.Error(code, "no")
		}}, Config{})
		_, err := c.Register(ctxT(t, 5*time.Second), &gatewayv0.RegisterRequest{})
		if status.Code(errors.Unwrap(err)) != code || calls.Load() != 1 {
			t.Errorf("%v: err %v after %d attempts, want one attempt", code, err, calls.Load())
		}
	}
}

func TestRegisterGivesUpWhenCancelled(t *testing.T) {
	c := serve(t, &fakeGateway{register: func(context.Context, *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}}, Config{})
	_, err := c.Register(ctxT(t, 100*time.Millisecond), &gatewayv0.RegisterRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
}

func TestBackoffBounds(t *testing.T) {
	b := backoff{min: 10 * time.Millisecond, max: 80 * time.Millisecond}
	for i := range 20 {
		d := b.next()
		if d <= 0 || d > b.cur || b.cur > b.max {
			t.Fatalf("step %d: delay %v with ceiling %v", i, d, b.cur)
		}
	}
	if b.cur != b.max {
		t.Errorf("ceiling %v, want it to reach %v", b.cur, b.max)
	}
	b.reset()
	if d := b.next(); d > b.min {
		t.Errorf("after reset: delay %v above minimum %v", d, b.min)
	}
}

// handler is a fake session for the control stream.
type handler struct {
	mu      sync.Mutex
	applied []string
	fail    map[string]bool // commands whose Apply fails
	state   gatewayv0.SessionState
	done    chan struct{}
}

func newHandler() *handler {
	return &handler{fail: map[string]bool{}, state: gatewayv0.SessionState_SESSION_STATE_RUNNING, done: make(chan struct{})}
}

func (h *handler) Hello() *gatewayv0.ControlHello {
	return &gatewayv0.ControlHello{SessionId: "s1", Epoch: 2, State: h.State()}
}

func (h *handler) Apply(_ context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail[cmd.GetId()] {
		return nil, errors.New("journal full")
	}
	h.applied = append(h.applied, cmd.GetId())
	return &gatewayv0.CommandAck{CommandId: cmd.GetId(), Result: gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED}, nil
}

func (h *handler) State() gatewayv0.SessionState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

func (h *handler) Done() <-chan struct{} { return h.done }

func (h *handler) finish(st gatewayv0.SessionState) {
	h.mu.Lock()
	h.state = st
	h.mu.Unlock()
	close(h.done)
}

func command(id string) *gatewayv0.ControlDown {
	return &gatewayv0.ControlDown{Msg: &gatewayv0.ControlDown_Command{Command: &gatewayv0.Command{Id: id}}}
}

func TestControlStream(t *testing.T) {
	h := newHandler()
	h.fail["c2"] = true
	type seen struct {
		hello  *gatewayv0.ControlHello
		acks   []string
		status []gatewayv0.SessionState
		closed bool
	}
	result := make(chan seen, 1)
	c := serve(t, &fakeGateway{control: func(s gatewayv0.Gateway_ControlServer) error {
		var got seen
		first, err := s.Recv()
		if err != nil {
			return err
		}
		got.hello = first.GetHello()
		for _, id := range []string{"c1", "c2", "c3"} {
			if err := s.Send(command(id)); err != nil {
				return err
			}
		}
		for {
			msg, err := s.Recv()
			if errors.Is(err, io.EOF) {
				got.closed = true
				result <- got
				return nil
			}
			if err != nil {
				result <- got
				return err
			}
			if ack := msg.GetAck(); ack != nil {
				got.acks = append(got.acks, ack.GetCommandId())
			}
			if st := msg.GetStatus(); st != nil {
				got.status = append(got.status, st.GetState())
				if len(got.acks) == 2 && st.GetState() == gatewayv0.SessionState_SESSION_STATE_RUNNING {
					h.finish(gatewayv0.SessionState_SESSION_STATE_PAUSED)
				}
			}
		}
	}}, Config{Heartbeat: 20 * time.Millisecond})

	if err := c.RunControl(ctxT(t, 5*time.Second), h, nil); err != nil {
		t.Fatalf("RunControl: %v", err)
	}
	got := <-result
	if got.hello.GetSessionId() != "s1" || got.hello.GetEpoch() != 2 {
		t.Errorf("hello %v", got.hello)
	}
	// c2 couldn't be recorded, so it must not be acknowledged.
	if len(got.acks) != 2 || got.acks[0] != "c1" || got.acks[1] != "c3" {
		t.Errorf("acks %v, want c1 and c3", got.acks)
	}
	if len(got.status) < 2 || got.status[len(got.status)-1] != gatewayv0.SessionState_SESSION_STATE_PAUSED || !got.closed {
		t.Errorf("status %v, closed %v: want heartbeats, then the final state, then the stream closed", got.status, got.closed)
	}
}

func TestControlReregistersWhenSessionUnknown(t *testing.T) {
	h := newHandler()
	var streams, reregistered atomic.Int32
	c := serve(t, &fakeGateway{control: func(s gatewayv0.Gateway_ControlServer) error {
		if _, err := s.Recv(); err != nil {
			return err
		}
		if streams.Add(1) == 1 {
			return status.Error(codes.NotFound, "unknown session")
		}
		h.finish(gatewayv0.SessionState_SESSION_STATE_COMPLETED)
		for {
			if _, err := s.Recv(); err != nil {
				return nil
			}
		}
	}}, Config{})
	err := c.RunControl(ctxT(t, 5*time.Second), h, func(context.Context) error {
		reregistered.Add(1)
		return nil
	})
	if err != nil || streams.Load() != 2 || reregistered.Load() != 1 {
		t.Fatalf("err %v after %d streams and %d re-registrations, want 2 and 1", err, streams.Load(), reregistered.Load())
	}
}

// source is a fake ActivitySource holding events 1..n, closed when all are
// emitted.
type source struct {
	mu     sync.Mutex
	evs    []*gatewayv0.ActivityEvent
	acked  uint64
	wake   chan struct{}
	closed bool
}

func newSource(n int) *source {
	s := &source{wake: make(chan struct{}, 1), closed: true}
	for i := 1; i <= n; i++ {
		s.evs = append(s.evs, &gatewayv0.ActivityEvent{Id: "1." + string(rune('0'+i)), Epoch: 1, Seq: uint64(i)})
	}
	return s
}

func (s *source) Epoch() uint64 { return 1 }

func (s *source) Next(ctx context.Context, after uint64, max int) ([]*gatewayv0.ActivityEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after >= uint64(len(s.evs)) {
		if s.closed {
			return nil, activity.ErrDrained
		}
		return nil, ctx.Err()
	}
	end := min(int(after)+max, len(s.evs))
	return s.evs[after:end], nil
}

func (s *source) Ack(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acked = max(s.acked, seq)
}

func (s *source) Acked() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked
}

func (s *source) Done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed && s.acked == uint64(len(s.evs))
}

// ackingPublisher acknowledges every batch; it starts by claiming to hold
// events up to have.
func ackingPublisher(have uint64, received chan<- uint64) func(gatewayv0.Gateway_PublishActivityServer) error {
	return func(s gatewayv0.Gateway_PublishActivityServer) error {
		first, err := s.Recv()
		if err != nil {
			return err
		}
		if err := s.Send(&gatewayv0.ActivityAck{Epoch: first.GetHello().GetEpoch(), AckedSeq: have}); err != nil {
			return err
		}
		for {
			msg, err := s.Recv()
			if err != nil {
				return nil
			}
			var last uint64
			for _, ev := range msg.GetBatch().GetEvents() {
				received <- ev.GetSeq()
				last = ev.GetSeq()
			}
			if err := s.Send(&gatewayv0.ActivityAck{Epoch: 1, AckedSeq: last}); err != nil {
				return err
			}
		}
	}
}

func TestActivityResumesFromGatewayPosition(t *testing.T) {
	received := make(chan uint64, 100)
	c := serve(t, &fakeGateway{publish: ackingPublisher(2, received)}, Config{})
	src := newSource(5)
	if err := c.RunActivity(ctxT(t, 5*time.Second), "s1", src, nil); err != nil {
		t.Fatalf("RunActivity: %v", err)
	}
	close(received)
	var got []uint64
	for seq := range received {
		got = append(got, seq)
	}
	if len(got) != 3 || got[0] != 3 || got[2] != 5 || !src.Done() {
		t.Fatalf("gateway received %v, want 3..5 and everything acknowledged", got)
	}
}

func TestActivityWindow(t *testing.T) {
	const window = 2
	var maxOutstanding atomic.Int64
	c := serve(t, &fakeGateway{publish: func(s gatewayv0.Gateway_PublishActivityServer) error {
		if _, err := s.Recv(); err != nil {
			return err
		}
		if err := s.Send(&gatewayv0.ActivityAck{Epoch: 1}); err != nil {
			return err
		}
		var acked, last uint64
		for {
			msg, err := s.Recv()
			if err != nil {
				return nil
			}
			for _, ev := range msg.GetBatch().GetEvents() {
				last = ev.GetSeq()
			}
			if out := int64(last - acked); out > maxOutstanding.Load() {
				maxOutstanding.Store(out)
			}
			// Acknowledge slowly, so the worker runs into its window.
			time.Sleep(5 * time.Millisecond)
			acked = last
			if err := s.Send(&gatewayv0.ActivityAck{Epoch: 1, AckedSeq: acked}); err != nil {
				return err
			}
		}
	}}, Config{Window: window})
	src := newSource(9)
	if err := c.RunActivity(ctxT(t, 5*time.Second), "s1", src, nil); err != nil {
		t.Fatalf("RunActivity: %v", err)
	}
	if m := maxOutstanding.Load(); m > window {
		t.Fatalf("%d events outstanding, window %d", m, window)
	}
}

func TestActivityReconnectsWhenAcksStall(t *testing.T) {
	var streams atomic.Int32
	received := make(chan uint64, 100)
	acking := ackingPublisher(0, received)
	c := serve(t, &fakeGateway{publish: func(s gatewayv0.Gateway_PublishActivityServer) error {
		if streams.Add(1) == 1 {
			// Take the events but never acknowledge them.
			if _, err := s.Recv(); err != nil {
				return err
			}
			if err := s.Send(&gatewayv0.ActivityAck{Epoch: 1}); err != nil {
				return err
			}
			for {
				if _, err := s.Recv(); err != nil {
					return nil
				}
			}
		}
		return acking(s)
	}}, Config{AckTimeout: 50 * time.Millisecond})
	src := newSource(3)
	if err := c.RunActivity(ctxT(t, 5*time.Second), "s1", src, nil); err != nil {
		t.Fatalf("RunActivity: %v", err)
	}
	if streams.Load() != 2 || !src.Done() {
		t.Fatalf("%d streams, done %v: want a reconnect and everything acknowledged", streams.Load(), src.Done())
	}
}

func TestActivityReregistersWhenSessionUnknown(t *testing.T) {
	var streams, reregistered atomic.Int32
	received := make(chan uint64, 100)
	acking := ackingPublisher(0, received)
	c := serve(t, &fakeGateway{publish: func(s gatewayv0.Gateway_PublishActivityServer) error {
		if streams.Add(1) == 1 {
			return status.Error(codes.NotFound, "unknown session")
		}
		return acking(s)
	}}, Config{})
	src := newSource(2)
	err := c.RunActivity(ctxT(t, 5*time.Second), "s1", src, func(context.Context) error {
		reregistered.Add(1)
		return nil
	})
	if err != nil || reregistered.Load() != 1 || !src.Done() {
		t.Fatalf("err %v, %d re-registrations, done %v", err, reregistered.Load(), src.Done())
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New without an address succeeded")
	}
	if _, err := New(Config{Addr: "x:1", TLS: true, CAFile: "/does/not/exist"}); err == nil {
		t.Error("New with a missing CA file succeeded")
	}
}

// A gateway that ends the stream while the worker is sending must not stop
// delivery: the worker reconnects and finishes.
func TestActivityReconnectsWhenGatewayEndsStream(t *testing.T) {
	var streams atomic.Int32
	received := make(chan uint64, 100)
	acking := ackingPublisher(0, received)
	c := serve(t, &fakeGateway{publish: func(s gatewayv0.Gateway_PublishActivityServer) error {
		if streams.Add(1) <= 3 {
			// Answer the hello, then end the stream at once.
			if _, err := s.Recv(); err != nil {
				return err
			}
			return s.Send(&gatewayv0.ActivityAck{Epoch: 1})
		}
		return acking(s)
	}}, Config{})
	src := newSource(50)
	if err := c.RunActivity(ctxT(t, 5*time.Second), "s1", src, nil); err != nil {
		t.Fatalf("RunActivity: %v", err)
	}
	if !src.Done() || streams.Load() != 4 {
		t.Fatalf("done %v after %d streams, want everything delivered on the fourth", src.Done(), streams.Load())
	}
}

// fileHandler serves files slowly, until released.
type fileHandler struct {
	*handler
	release chan struct{}
}

func (h fileHandler) ReadFile(req *gatewayv0.FileRequest) *gatewayv0.FileResponse {
	<-h.release
	return &gatewayv0.FileResponse{Path: "workspace/" + req.GetPath(), Content: []byte("hi"), Size: 2}
}

// File reads are answered on the control stream, a few at a time, and
// never hold commands up; a handler that serves no files refuses them.
func TestControlFileReads(t *testing.T) {
	for _, serves := range []bool{true, false} {
		h := newHandler()
		release := make(chan struct{})
		var ctl ControlHandler = h
		if serves {
			ctl = fileHandler{h, release}
		}
		answers := make(chan *gatewayv0.FileResponse, 16)
		c := serve(t, &fakeGateway{control: func(s gatewayv0.Gateway_ControlServer) error {
			if _, err := s.Recv(); err != nil {
				return err
			}
			for i := range MaxFileReads + 2 {
				if err := s.Send(&gatewayv0.ControlDown{Msg: &gatewayv0.ControlDown_ReadFile{ReadFile: &gatewayv0.FileRequest{Id: fmt.Sprint(i), Path: "a.txt"}}}); err != nil {
					return err
				}
			}
			if err := s.Send(command("c1")); err != nil {
				return err
			}
			for {
				msg, err := s.Recv()
				if err != nil {
					return nil
				}
				if f := msg.GetFile(); f != nil {
					answers <- f
				}
				if msg.GetAck() != nil {
					close(release) // the command got through while reads were waiting
				}
			}
		}}, Config{Heartbeat: 20 * time.Millisecond})
		go func() { _ = c.RunControl(ctxT(t, 5*time.Second), ctl, nil) }()

		var got []string
		for range MaxFileReads + 2 {
			select {
			case f := <-answers:
				got = append(got, fmt.Sprintf("%s:%d:%s", f.GetId(), f.GetErrorCode(), f.GetContent()))
			case <-time.After(5 * time.Second):
				t.Fatalf("serves %v: answers %v", serves, got)
			}
		}
		h.finish(gatewayv0.SessionState_SESSION_STATE_COMPLETED)
		slices.Sort(got)
		var refused, served int
		for _, g := range got {
			switch {
			case strings.HasSuffix(g, fmt.Sprintf(":%d:", codes.ResourceExhausted)):
				refused++
			case strings.HasSuffix(g, fmt.Sprintf(":%d:", codes.Unimplemented)):
				refused++
			case strings.HasSuffix(g, ":0:hi"):
				served++
			}
		}
		if serves && (served != MaxFileReads || refused != 2) || !serves && refused != MaxFileReads+2 {
			t.Errorf("serves %v: answers %v", serves, got)
		}
	}
}
