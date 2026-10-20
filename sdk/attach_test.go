package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// viewing is a viewer's context for a session.
func viewing(ctx context.Context, sessionID string) context.Context {
	return metadata.AppendToOutgoingContext(authed(ctx), AttachSessionHeader, sessionID)
}

// registerWith registers a session whose worker has the capabilities.
func (h *harness) registerWith(t *testing.T, sessionID string, caps ...string) {
	t.Helper()
	_, err := h.client.Register(authed(t.Context()), &gatewayv0.RegisterRequest{
		ProtocolVersion: ProtocolVersion, WorkerId: "w1", SessionId: sessionID, Epoch: 1, Capabilities: caps, ActletVersion: "v9",
	})
	if err != nil {
		t.Fatal(err)
	}
}

// watcher reads a viewer's feed in the background.
type watcher struct {
	got  chan string // "epoch.seq", "skipped N" or "end"
	stop context.CancelFunc
}

func (h *harness) watch(t *testing.T, sessionID string, after uint64) *watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(viewing(t.Context(), sessionID))
	stream, err := h.attach.Watch(ctx, &attachv0.WatchRequest{AfterSeq: after})
	if err != nil {
		t.Fatal(err)
	}
	w := &watcher{got: make(chan string, 1<<16), stop: cancel}
	go func() {
		for {
			m, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				w.got <- "end"
				return
			}
			if err != nil {
				w.got <- "error " + status.Code(err).String()
				return
			}
			if sk := m.GetSkipped(); sk != nil {
				w.got <- fmt.Sprintf("skipped %d", sk.GetCount())
			} else {
				w.got <- fmt.Sprintf("%d.%d", m.GetEvent().GetEpoch(), m.GetEvent().GetSeq())
			}
		}
	}()
	t.Cleanup(cancel)
	return w
}

func (w *watcher) next(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for len(out) < n {
		select {
		case s := <-w.got:
			out = append(out, s)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %v, waiting for %d", out, n)
		}
	}
	return out
}

func seqs(epoch, from, to uint64) []string {
	var out []string
	for s := from; s <= to; s++ {
		out = append(out, fmt.Sprintf("%d.%d", epoch, s))
	}
	return out
}

// A viewer gets what the store holds, then live events, across runs, and
// one that attaches while events arrive misses and repeats none.
func TestAttachWatch(t *testing.T) {
	h := newHarness(t, Options{Store: NewMemoryStore()})
	h.registerWith(t, "s1")
	pub := h.connectActivity(t, "s1", 1)
	pub.send(t, events(1, 1, 3))

	w := h.watch(t, "s1", 0)
	if got := w.next(t, 3); !slices.Equal(got, seqs(1, 1, 3)) {
		t.Fatalf("backlog %v", got)
	}
	pub.send(t, events(1, 4, 5))
	if got := w.next(t, 2); !slices.Equal(got, seqs(1, 4, 5)) {
		t.Fatalf("live %v", got)
	}
	pub2 := h.connectActivity(t, "s1", 2)
	pub2.send(t, events(2, 1, 2))
	if got := w.next(t, 2); !slices.Equal(got, seqs(2, 1, 2)) {
		t.Fatalf("second run %v", got)
	}

	// Viewers attach while a burst arrives.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for s := uint64(3); s <= 400; s += 10 {
			pub2.send(t, events(2, s, s+9))
		}
	}()
	late := h.watch(t, "s1", 0)
	<-done
	want := append(append(seqs(1, 1, 5), seqs(2, 1, 2)...), seqs(2, 3, 402)...)
	if got := late.next(t, len(want)); !slices.Equal(got, want) {
		t.Fatalf("a viewer attaching during a burst got %d events, out of order or repeated", len(got))
	}
	if got := w.next(t, 400); !slices.Equal(got, seqs(2, 3, 402)) {
		t.Fatalf("the first viewer got %v", got[:5])
	}
	// After a sequence number: the latest run's events after it.
	if got := h.watch(t, "s1", 400).next(t, 2); !slices.Equal(got, seqs(2, 401, 402)) {
		t.Errorf("after 400: %v", got)
	}
}

// A session that completed ends its viewers' feeds, and a viewer that
// attaches later gets its activity and the end.
func TestAttachSessionEnds(t *testing.T) {
	h := newHarness(t, Options{Store: NewMemoryStore()})
	h.registerWith(t, "s1")
	pub := h.connectActivity(t, "s1", 1)
	pub.send(t, events(1, 1, 2))
	w := h.watch(t, "s1", 0)
	w.next(t, 2)
	end := event(1, 3)
	end.Body = &gatewayv0.ActivityEvent_State{State: &gatewayv0.StateChange{To: gatewayv0.SessionState_SESSION_STATE_COMPLETED}}
	pub.send(t, []*gatewayv0.ActivityEvent{end})
	if got := w.next(t, 2); !slices.Equal(got, []string{"1.3", "end"}) {
		t.Errorf("live viewer: %v", got)
	}
	if got := h.watch(t, "s1", 0).next(t, 4); !slices.Equal(got, []string{"1.1", "1.2", "1.3", "end"}) {
		t.Errorf("later viewer: %v", got)
	}
}

// Only authenticated, authorised viewers attach, to sessions they name.
func TestAttachRefused(t *testing.T) {
	h := newHarness(t, Options{AuthorizeAttach: func(_ context.Context, s *Session) error {
		if s.ID() == "secret" {
			return errors.New("not yours")
		}
		return nil
	}})
	h.registerWith(t, "s1")
	h.registerWith(t, "secret")
	ctx := t.Context()

	noToken := metadata.AppendToOutgoingContext(ctx, AttachSessionHeader, "s1")
	if _, err := h.attach.Info(noToken, &attachv0.InfoRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("info without a token: %v", err)
	}
	if _, err := h.attach.Sessions(ctx, &attachv0.SessionsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("sessions without a token: %v", err)
	}
	stream, err := h.attach.Watch(noToken, &attachv0.WatchRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("watch without a token: %v", err)
	}
	if _, err := h.attach.Info(authed(ctx), &attachv0.InfoRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no session named: %v", err)
	}
	if _, err := h.attach.Info(viewing(ctx, "nope"), &attachv0.InfoRequest{}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown session: %v", err)
	}
	if _, err := h.attach.Info(viewing(ctx, "secret"), &attachv0.InfoRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("unauthorised session: %v", err)
	}
	list, err := h.attach.Sessions(authed(ctx), &attachv0.SessionsRequest{})
	if err != nil || len(list.GetSessions()) != 1 || list.GetSessions()[0].GetSessionId() != "s1" {
		t.Errorf("sessions %v, %v", list, err)
	}
	info, err := h.attach.Info(viewing(ctx, "s1"), &attachv0.InfoRequest{})
	if err != nil || info.GetWorkerId() != "w1" || info.GetActletVersion() != "v9" || fmt.Sprint(info.GetCommands()) != "[steer]" {
		t.Errorf("info %v, %v", info, err)
	}
}

// A viewer's commands reach the worker with origin remote, if allowed.
func TestAttachCommands(t *testing.T) {
	h := newHarness(t, Options{Assign: func(context.Context, *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
		return &gatewayv0.RegisterResponse{Profile: &gatewayv0.Profile{Id: "p", LocalAttach: &gatewayv0.LocalAttach{Commands: []string{"steer", "pause"}}}}, nil
	}})
	h.registerWith(t, "s1")
	w := h.connectControl(t, "s1")
	ctx := viewing(t.Context(), "s1")

	acks := make(chan error, 1)
	go func() {
		ack, err := h.attach.Command(ctx, &attachv0.CommandRequest{Actor: "bob", Action: &attachv0.CommandRequest_Steer{Steer: &gatewayv0.Steer{Note: "hurry"}}})
		if err == nil && ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
			err = fmt.Errorf("ack %v", ack)
		}
		acks <- err
	}()
	cmd := w.nextCommand(t)
	if cmd.GetOrigin() != gatewayv0.Origin_ORIGIN_REMOTE || cmd.GetActor() != "bob" || cmd.GetSteer().GetNote() != "hurry" {
		t.Fatalf("command %v", cmd)
	}
	w.ack(t, cmd, false)
	if err := <-acks; err != nil {
		t.Fatal(err)
	}
	// The profile lets viewers pause, not stop.
	if _, err := h.attach.Command(ctx, &attachv0.CommandRequest{Action: &attachv0.CommandRequest_Stop{Stop: &gatewayv0.Stop{}}}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("stop: %v", err)
	}
	info, _ := h.attach.Info(ctx, &attachv0.InfoRequest{})
	if fmt.Sprint(info.GetCommands()) != "[steer pause]" {
		t.Errorf("commands %v", info.GetCommands())
	}
}

// File reads go to the worker over its control stream and back.
func TestAttachReadFile(t *testing.T) {
	h := newHarness(t, Options{})
	ctx := viewing(t.Context(), "s1")
	read := func(path string) (*attachv0.ReadFileResponse, error) {
		return h.attach.ReadFile(ctx, &attachv0.ReadFileRequest{Path: path})
	}

	h.registerWith(t, "s1")
	if _, err := read("a.txt"); status.Code(err) != codes.Unavailable {
		t.Errorf("no worker connected: %v", err)
	}
	w := h.connectControl(t, "s1")
	waitFor(t, func() bool { return h.srv.Session("s1").Connected() })
	if _, err := read("a.txt"); status.Code(err) != codes.Unimplemented {
		t.Errorf("a worker that serves no files: %v", err)
	}
	w.cancel()

	h.registerWith(t, "s1", "attach.files")
	w = h.connectControl(t, "s1")
	waitFor(t, func() bool { return h.srv.Session("s1").Connected() })
	go func() {
		for {
			msg, err := w.stream.Recv()
			if err != nil {
				return
			}
			req := msg.GetReadFile()
			if req == nil {
				continue
			}
			resp := &gatewayv0.FileResponse{Id: req.GetId(), Path: "workspace/" + req.GetPath(), Content: []byte("hi"), Size: 2}
			if strings.HasPrefix(req.GetPath(), "..") {
				resp = &gatewayv0.FileResponse{Id: req.GetId(), ErrorCode: int32(codes.PermissionDenied), Error: "outside"}
			}
			_ = w.stream.Send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_File{File: resp}})
		}
	}()
	if f, err := read("a.txt"); err != nil || string(f.GetContent()) != "hi" || f.GetPath() != "workspace/a.txt" {
		t.Errorf("read %v, %v", f, err)
	}
	if _, err := read("../.actlet/x"); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "outside") {
		t.Errorf("refused read: %v", err)
	}
}

// A viewer that stops reading never holds up the worker's activity, and
// what the server keeps for it stays bounded.
func TestAttachStalledViewer(t *testing.T) {
	h := newHarness(t, Options{Store: NewMemoryStore()})
	h.registerWith(t, "s1")
	pub := h.connectActivity(t, "s1", 1)
	// The viewer opens its feed and never reads it.
	if _, err := h.attach.Watch(viewing(t.Context(), "s1"), &attachv0.WatchRequest{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		return h.srv.relays["s1"] != nil
	})
	start := time.Now()
	const n = 50000
	for s := uint64(1); s <= n; s += 500 {
		if acked := pub.send(t, events(1, s, s+499)); acked != s+499 {
			t.Fatalf("acked %d", acked)
		}
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("storing took %v with a stalled viewer", took)
	}
	h.srv.mu.Lock()
	held, queued := h.srv.relays["s1"].hub.Held()
	h.srv.mu.Unlock()
	if held > 4096+1024 || queued > 256+4096 {
		t.Errorf("held %d events, %d queued for the viewer", held, queued)
	}
}

// A recording store reads back what it holds, as far as it is written.
func TestRecordingStoreReadActivity(t *testing.T) {
	store, err := NewRecordingStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := t.Context()
	var got []string
	collect := func(ev *gatewayv0.ActivityEvent) error {
		got = append(got, fmt.Sprintf("%d.%d", ev.GetEpoch(), ev.GetSeq()))
		return nil
	}
	if err := store.ReadActivity(ctx, "s1", collect); err != nil || got != nil {
		t.Fatalf("no recording: %v, %v", got, err)
	}
	for _, evs := range [][]*gatewayv0.ActivityEvent{events(1, 1, 3), events(2, 1, 2)} {
		if err := store.Append(ctx, "s1", evs); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReadActivity(ctx, "s1", collect); err != nil || fmt.Sprint(got) != "[1.1 1.2 1.3 2.1 2.2]" {
		t.Errorf("read %v, %v", got, err)
	}
	if err := store.ReadActivity(ctx, "../x", collect); err == nil {
		t.Error("an invalid session ID was read")
	}
}
