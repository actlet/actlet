package sdk

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

const testToken = "secret"

// harness runs a Server over an in-memory connection.
type harness struct {
	srv    *Server
	gs     *grpc.Server
	client gatewayv0.GatewayClient
	attach attachv0.AttachClient
}

func newHarness(t testing.TB, opts Options) *harness {
	t.Helper()
	if opts.Assign == nil {
		opts.Assign = func(context.Context, *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			return &gatewayv0.RegisterResponse{Profile: &gatewayv0.Profile{Id: "test"}}, nil
		}
	}
	if opts.Authenticate == nil {
		opts.Authenticate = StaticToken(testToken)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(ServerOptions()...)
	srv.RegisterService(gs)
	srv.RegisterAttach(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return &harness{srv: srv, gs: gs, client: gatewayv0.NewGatewayClient(cc), attach: attachv0.NewAttachClient(cc)}
}

func authed(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+testToken)
}

func (h *harness) register(t *testing.T, sessionID string) string {
	t.Helper()
	resp, err := h.client.Register(authed(t.Context()), &gatewayv0.RegisterRequest{
		ProtocolVersion: ProtocolVersion, WorkerId: "w1", SessionId: sessionID, Epoch: 1,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return resp.GetSessionId()
}

// worker is a fake worker's control stream.
type worker struct {
	stream gatewayv0.Gateway_ControlClient
	cancel context.CancelFunc
}

func (h *harness) connectControl(t *testing.T, sessionID string) *worker {
	t.Helper()
	ctx, cancel := context.WithCancel(authed(t.Context()))
	stream, err := h.client.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Hello{Hello: &gatewayv0.ControlHello{
		SessionId: sessionID, Epoch: 1, State: gatewayv0.SessionState_SESSION_STATE_RUNNING,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return &worker{stream: stream, cancel: cancel}
}

func (w *worker) nextCommand(t *testing.T) *gatewayv0.Command {
	t.Helper()
	msg, err := w.stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	return msg.GetCommand()
}

func (w *worker) ack(t *testing.T, cmd *gatewayv0.Command, duplicate bool) {
	t.Helper()
	err := w.stream.Send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Ack{Ack: &gatewayv0.CommandAck{
		CommandId: cmd.GetId(), Result: gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED, Duplicate: duplicate,
		State: gatewayv0.SessionState_SESSION_STATE_RUNNING,
	}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegister(t *testing.T) {
	h := newHarness(t, Options{})
	id := h.register(t, "")
	if !ValidSessionID(id) {
		t.Fatalf("assigned session ID %q is invalid", id)
	}
	if h.srv.Session(id) == nil {
		t.Fatal("registered session unknown to the server")
	}
	// Resuming keeps the requested ID.
	if got := h.register(t, "resumed-1"); got != "resumed-1" {
		t.Errorf("resumed session ID = %q", got)
	}

	_, err := h.client.Register(t.Context(), &gatewayv0.RegisterRequest{ProtocolVersion: ProtocolVersion})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("no token: err = %v, want Unauthenticated", err)
	}
	_, err = h.client.Register(authed(t.Context()), &gatewayv0.RegisterRequest{ProtocolVersion: "v9"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("wrong version: err = %v, want FailedPrecondition", err)
	}
	_, err = h.client.Register(authed(t.Context()), &gatewayv0.RegisterRequest{ProtocolVersion: ProtocolVersion, SessionId: "../x"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad session ID: err = %v, want InvalidArgument", err)
	}
}

func TestControlUnknownSession(t *testing.T) {
	h := newHarness(t, Options{})
	w := h.connectControl(t, "nobody")
	if _, err := w.stream.Recv(); status.Code(err) != codes.NotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestCommandsDeliveredAndAcknowledged(t *testing.T) {
	var events []bool
	var mu sync.Mutex
	h := newHarness(t, Options{OnControl: func(_ *Session, connected bool) {
		mu.Lock()
		events = append(events, connected)
		mu.Unlock()
	}})
	id := h.register(t, "")
	sess := h.srv.Session(id)

	if _, err := sess.Pause(t.Context(), gatewayv0.PauseMode_PAUSE_MODE_UNSPECIFIED); !errors.Is(err, ErrNoPauseMode) {
		t.Fatalf("Pause without mode: err = %v", err)
	}

	// Commands issued before the worker connects are queued.
	type result struct {
		ack *gatewayv0.CommandAck
		err error
	}
	steer := make(chan result, 1)
	go func() {
		ack, err := sess.Steer(t.Context(), "look left")
		steer <- result{ack, err}
	}()
	waitFor(t, func() bool { return sess.Pending() == 1 })
	stop := make(chan result, 1)
	go func() {
		ack, err := sess.Stop(t.Context(), StopOptions{Grace: 3 * time.Second})
		stop <- result{ack, err}
	}()
	waitFor(t, func() bool { return sess.Pending() == 2 })

	w := h.connectControl(t, id)
	c1, c2 := w.nextCommand(t), w.nextCommand(t)
	if c1.GetSteer().GetNote() != "look left" || c1.GetOrigin() != gatewayv0.Origin_ORIGIN_ORCHESTRATOR {
		t.Fatalf("first command = %v", c1)
	}
	if c2.GetStop().GetGrace().AsDuration() != 3*time.Second {
		t.Fatalf("second command = %v", c2)
	}
	w.ack(t, c1, false)
	w.ack(t, c2, false)
	for _, ch := range []chan result{steer, stop} {
		r := <-ch
		if r.err != nil || r.ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
			t.Fatalf("ack = %v, %v", r.ack, r.err)
		}
	}
	if sess.Pending() != 0 || !sess.Connected() {
		t.Errorf("Pending = %d, Connected = %v", sess.Pending(), sess.Connected())
	}
	w.cancel()
	waitFor(t, func() bool { return !sess.Connected() })
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || !events[0] || events[1] {
		t.Errorf("OnControl events = %v, want connect then disconnect", events)
	}
}

// A command sent on a stream that drops before the worker acknowledges it
// is sent again on the next stream.
func TestUnacknowledgedCommandIsResent(t *testing.T) {
	h := newHarness(t, Options{})
	id := h.register(t, "")
	sess := h.srv.Session(id)

	w := h.connectControl(t, id)
	done := make(chan *gatewayv0.CommandAck, 1)
	go func() {
		ack, _ := sess.Steer(t.Context(), "note")
		done <- ack
	}()
	first := w.nextCommand(t)
	w.cancel() // drop the stream without acknowledging

	w = h.connectControl(t, id)
	again := w.nextCommand(t)
	if again.GetId() != first.GetId() {
		t.Fatalf("resent command ID %q, want %q", again.GetId(), first.GetId())
	}
	w.ack(t, again, true)
	if ack := <-done; !ack.GetDuplicate() {
		t.Errorf("ack = %v", ack)
	}

	// Sending a completed ID again delivers it again: the worker decides
	// that it is a duplicate.
	go func() {
		ack, _ := sess.Send(t.Context(), &gatewayv0.Command{Id: first.GetId(), Action: &gatewayv0.Command_Steer{Steer: &gatewayv0.Steer{Note: "note"}}})
		done <- ack
	}()
	if c := w.nextCommand(t); c.GetId() != first.GetId() {
		t.Fatalf("command %q, want %q", c.GetId(), first.GetId())
	}
}

func event(epoch, seq uint64) *gatewayv0.ActivityEvent {
	return &gatewayv0.ActivityEvent{
		Id: EventID(epoch, seq), Epoch: epoch, Seq: seq, Time: timestamppb.Now(),
		Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: "hello"}},
	}
}

func events(epoch, from, to uint64) []*gatewayv0.ActivityEvent {
	var out []*gatewayv0.ActivityEvent
	for s := from; s <= to; s++ {
		out = append(out, event(epoch, s))
	}
	return out
}

// publisher is a fake worker's activity stream.
type publisher struct {
	stream gatewayv0.Gateway_PublishActivityClient
	cancel context.CancelFunc
	acked  uint64
}

func (h *harness) connectActivity(t *testing.T, sessionID string, epoch uint64) *publisher {
	t.Helper()
	ctx, cancel := context.WithCancel(authed(t.Context()))
	stream, err := h.client.PublishActivity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Hello{Hello: &gatewayv0.ActivityHello{SessionId: sessionID, Epoch: epoch}}}); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("initial ack: %v", err)
	}
	return &publisher{stream: stream, cancel: cancel, acked: ack.GetAckedSeq()}
}

func (p *publisher) send(t *testing.T, evs []*gatewayv0.ActivityEvent) uint64 {
	t.Helper()
	if err := p.stream.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Batch{Batch: &gatewayv0.ActivityBatch{Events: evs}}}); err != nil {
		t.Fatal(err)
	}
	ack, err := p.stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	p.acked = ack.GetAckedSeq()
	return p.acked
}

func TestActivityDeduplicated(t *testing.T) {
	store := NewMemoryStore()
	var seen []string
	h := newHarness(t, Options{Store: store, OnActivity: func(_ string, ev *gatewayv0.ActivityEvent) {
		seen = append(seen, ev.GetId())
	}})
	id := h.register(t, "")

	p := h.connectActivity(t, id, 1)
	if p.acked != 0 {
		t.Fatalf("initial ack = %d", p.acked)
	}
	if got := p.send(t, events(1, 1, 5)); got != 5 {
		t.Fatalf("ack = %d, want 5", got)
	}
	p.cancel()

	// A reconnecting worker learns what the gateway has, and resent events
	// are not stored twice.
	p = h.connectActivity(t, id, 1)
	if p.acked != 5 {
		t.Fatalf("ack after reconnect = %d, want 5", p.acked)
	}
	if got := p.send(t, events(1, 3, 7)); got != 7 {
		t.Fatalf("ack = %d, want 7", got)
	}
	// A gap stops intake at the last contiguous event.
	if got := p.send(t, events(1, 9, 9)); got != 7 {
		t.Fatalf("ack after a gap = %d, want 7", got)
	}
	// Epochs are independent.
	p2 := h.connectActivity(t, id, 2)
	if got := p2.send(t, events(2, 1, 2)); got != 2 {
		t.Fatalf("epoch 2 ack = %d", got)
	}

	stored := store.Events(id)
	if len(stored) != 9 || len(seen) != 9 {
		t.Fatalf("stored %d events, OnActivity saw %d; want 9", len(stored), len(seen))
	}
	for i, ev := range stored[:7] {
		if ev.GetSeq() != uint64(i+1) || ev.GetEpoch() != 1 {
			t.Fatalf("stored[%d] = %s", i, ev.GetId())
		}
	}
}

func TestActivityRejectsBadEvents(t *testing.T) {
	h := newHarness(t, Options{})
	id := h.register(t, "")
	p := h.connectActivity(t, id, 1)
	bad := event(1, 1)
	bad.Id = "nope"
	if err := p.stream.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Batch{Batch: &gatewayv0.ActivityBatch{Events: []*gatewayv0.ActivityEvent{bad}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

// A RecordingStore remembers what it holds across gateway restarts.
func TestRecordingStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewRecordingStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{Store: store})
	id := h.register(t, "")
	p := h.connectActivity(t, id, 1)
	p.send(t, events(1, 1, 4))
	p.cancel()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewRecordingStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	h = newHarness(t, Options{Store: store})
	h.register(t, id)
	p = h.connectActivity(t, id, 1)
	if p.acked != 4 {
		t.Fatalf("ack after restart = %d, want 4", p.acked)
	}
	if got := p.send(t, events(1, 2, 6)); got != 6 {
		t.Fatalf("ack = %d, want 6", got)
	}
	acked, err := readRecording(store.Path(id))
	if err != nil || acked[1] != 6 {
		t.Fatalf("recording holds up to %d, %v; want 6", acked[1], err)
	}
}

// Two replicas share a recording directory. Once the worker's activity
// streams to one replica end, the other can take the session over, while
// the first keeps running.
func TestRecordingHandover(t *testing.T) {
	dir := t.TempDir()
	stores := make([]*RecordingStore, 2)
	replicas := make([]*harness, 2)
	for i := range stores {
		var err error
		if stores[i], err = NewRecordingStore(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stores[i].Close() })
		replicas[i] = newHarness(t, Options{Store: stores[i]})
	}
	id := replicas[0].register(t, "")
	p := replicas[0].connectActivity(t, id, 1)
	p.send(t, events(1, 1, 4))
	p.cancel() // the worker leaves replica 0

	replicas[1].register(t, id)
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithCancel(authed(t.Context()))
		stream, err := replicas[1].client.PublishActivity(ctx)
		if err == nil {
			err = stream.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Hello{Hello: &gatewayv0.ActivityHello{SessionId: id, Epoch: 1}}})
		}
		var ack *gatewayv0.ActivityAck
		if err == nil {
			ack, err = stream.Recv()
		}
		cancel()
		if err == nil {
			if ack.GetAckedSeq() != 4 {
				t.Fatalf("replica 1 acknowledges %d, want 4", ack.GetAckedSeq())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica 1 can't take the session over: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type slowStore struct {
	*MemoryStore
	delay time.Duration
}

func (s slowStore) Append(ctx context.Context, id string, evs []*gatewayv0.ActivityEvent) error {
	time.Sleep(s.delay)
	return s.MemoryStore.Append(ctx, id, evs)
}

// A slow activity store holds up activity acknowledgements, not commands.
func TestSlowActivityDoesNotDelayControl(t *testing.T) {
	h := newHarness(t, Options{Store: slowStore{NewMemoryStore(), time.Second}})
	id := h.register(t, "")
	sess := h.srv.Session(id)
	w := h.connectControl(t, id)
	p := h.connectActivity(t, id, 1)
	// Publish in the background; the store is then busy for a second.
	go func() {
		_ = p.stream.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Batch{Batch: &gatewayv0.ActivityBatch{Events: events(1, 1, 10)}}})
	}()
	time.Sleep(50 * time.Millisecond)

	go func() {
		msg, err := w.stream.Recv()
		if err != nil {
			return
		}
		_ = w.stream.Send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Ack{Ack: &gatewayv0.CommandAck{
			CommandId: msg.GetCommand().GetId(), Result: gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED,
		}}})
	}()
	start := time.Now()
	if _, err := sess.Steer(t.Context(), "quick"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("command round trip took %v while activity was slow", d)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRegisterRefusesUnsupportedProfile(t *testing.T) {
	h := newHarness(t, Options{Assign: func(context.Context, *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
		return &gatewayv0.RegisterResponse{Profile: &gatewayv0.Profile{RequiredFeatures: []string{"control.pause", "budgets.v1"}}}, nil
	}})
	req := &gatewayv0.RegisterRequest{ProtocolVersion: ProtocolVersion, Capabilities: []string{"control.pause"}}
	_, err := h.client.Register(authed(t.Context()), req)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "budgets.v1") {
		t.Fatalf("err = %v, want FailedPrecondition naming budgets.v1", err)
	}
	req.Capabilities = append(req.Capabilities, "budgets.v1")
	if _, err := h.client.Register(authed(t.Context()), req); err != nil {
		t.Fatalf("with the feature: %v", err)
	}
}

// Draining ends every stream as unavailable, so workers reconnect elsewhere,
// but only after the batch being stored has been acknowledged.
func TestDrain(t *testing.T) {
	store := slowStore{NewMemoryStore(), 300 * time.Millisecond}
	h := newHarness(t, Options{Store: store})
	id := h.register(t, "")
	w := h.connectControl(t, id)
	p := h.connectActivity(t, id, 1)
	waitFor(t, func() bool { return h.srv.Session(id).Connected() })

	// A batch is being stored (slowly) when draining starts.
	if err := p.stream.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Batch{Batch: &gatewayv0.ActivityBatch{Events: events(1, 1, 3)}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drained <- h.srv.Drain(ctx, h.gs)
	}()

	ack, err := p.stream.Recv()
	if err != nil || ack.GetAckedSeq() != 3 {
		t.Fatalf("in-flight batch: ack %v, %v; want it acknowledged", ack, err)
	}
	if _, err := p.stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("activity stream after drain: %v, want Unavailable", err)
	}
	if _, err := w.stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("control stream after drain: %v, want Unavailable", err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n := len(store.Events(id)); n != 3 {
		t.Fatalf("stored %d events, want 3", n)
	}
	_, err = h.client.Register(authed(t.Context()), &gatewayv0.RegisterRequest{ProtocolVersion: ProtocolVersion})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("Register after drain: %v, want Unavailable", err)
	}
}

func TestShutdownTimesOut(t *testing.T) {
	h := newHarness(t, Options{})
	id := h.register(t, "")
	h.connectControl(t, id)
	waitFor(t, func() bool { return h.srv.Session(id).Connected() })
	// The control stream ends promptly on its own, so Shutdown succeeds.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// A second call is harmless.
	if err := h.srv.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// The server sends heartbeats on an idle control stream, and none when
// they are turned off.
func TestControlHeartbeat(t *testing.T) {
	for _, tt := range []struct {
		interval time.Duration
		want     bool
	}{{30 * time.Millisecond, true}, {-1, false}} {
		h := newHarness(t, Options{Heartbeat: tt.interval})
		id := h.register(t, "")
		w := h.connectControl(t, id)
		got := make(chan *gatewayv0.ControlDown, 1)
		go func() {
			if msg, err := w.stream.Recv(); err == nil {
				got <- msg
			}
		}()
		select {
		case msg := <-got:
			if !tt.want || msg.GetHeartbeat().GetTime() == nil {
				t.Errorf("interval %v: got %v", tt.interval, msg)
			}
		case <-time.After(300 * time.Millisecond):
			if tt.want {
				t.Errorf("interval %v: no heartbeat", tt.interval)
			}
		}
		w.cancel()
	}
}
