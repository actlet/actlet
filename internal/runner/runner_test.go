package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

const token = "test-token"

// gateway is an in-process orchestrator gateway built on the SDK.
type gateway struct {
	addr  string
	srv   *sdk.Server
	gs    *grpc.Server
	store sdk.ActivityStore
}

func demoTask(definition string) *gatewayv0.Task {
	return &gatewayv0.Task{Schema: "v0", Id: "t1", Kind: session.DemoKind,
		Definition: []byte(definition), Input: []byte(`{"message":"from the gateway"}`)}
}

// startGateway serves on addr ("" for a new port) with the given store and
// task.
func startGateway(t *testing.T, addr string, store sdk.ActivityStore, task *gatewayv0.Task) *gateway {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var lis net.Listener
	var err error
	for range 50 { // the port of a stopped gateway may take a moment to free
		if lis, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sdk.NewServer(sdk.Options{
		Authenticate: sdk.StaticToken(token),
		Store:        store,
		Logger:       slog.New(slog.DiscardHandler),
		Assign: func(_ context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			return &gatewayv0.RegisterResponse{
				SessionId: req.GetSessionId(),
				Profile: &gatewayv0.Profile{Id: "test", Version: "1",
					DefaultPauseMode: gatewayv0.PauseMode_PAUSE_MODE_DRAIN, StopGrace: durationpb.New(time.Second)},
				Task: task,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(sdk.ServerOptions()...)
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	g := &gateway{addr: lis.Addr().String(), srv: srv, gs: gs, store: store}
	t.Cleanup(gs.Stop)
	return g
}

// session waits for a worker's control stream and returns its session.
func (g *gateway) session(t *testing.T) *sdk.Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range g.srv.Sessions() {
			if s.Connected() {
				return s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no worker connected")
	return nil
}

type worker struct {
	exit    chan int
	signals chan os.Signal
}

func testConfig(dir, addr string) Config {
	return Config{
		Gateway: gwclient.Config{
			Addr: addr, Token: token,
			BackoffMin: 10 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
			Heartbeat: 100 * time.Millisecond, AckTimeout: 2 * time.Second,
		},
		SessionDir: dir, WorkerID: "worker-1", Version: "test",
		MaxWorkers: 4, MaxToolConcurrency: 4,
		DefaultStopGrace: time.Second, MaxStopGrace: 10 * time.Second, SignalDeadline: 2 * time.Second,
		ActivityQueue: 1024, ActivitySpillMax: 64 << 20, FlushTimeout: 5 * time.Second,
		Logger: testLogger(),
	}
}

func run(t *testing.T, cfg Config) *worker {
	t.Helper()
	w := &worker{exit: make(chan int, 1), signals: make(chan os.Signal, 2)}
	go func() { w.exit <- Run(context.Background(), cfg, w.signals) }()
	return w
}

func (w *worker) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-w.exit:
		return code
	case <-time.After(30 * time.Second):
		t.Fatal("actlet run didn't exit")
		return -1
	}
}

// accepted checks that a command was acknowledged as accepted:
// accepted(t)(sess.Send(...)).
func accepted(t *testing.T) func(*gatewayv0.CommandAck, error) *gatewayv0.CommandAck {
	return func(ack *gatewayv0.CommandAck, err error) *gatewayv0.CommandAck {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
			t.Fatalf("command not accepted: %v", ack)
		}
		return ack
	}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// events returns the events the gateway stored for a session and checks
// that every epoch is numbered 1, 2, 3... with no duplicates.
func events(t *testing.T, store *sdk.MemoryStore, id string) []*gatewayv0.ActivityEvent {
	t.Helper()
	evs := store.Events(id)
	checkSequence(t, evs)
	return evs
}

func checkSequence(t *testing.T, evs []*gatewayv0.ActivityEvent) {
	t.Helper()
	next := map[uint64]uint64{}
	seen := map[string]bool{}
	for _, ev := range evs {
		if seen[ev.GetId()] {
			t.Fatalf("event %s stored twice", ev.GetId())
		}
		seen[ev.GetId()] = true
		if want := next[ev.GetEpoch()] + 1; ev.GetSeq() != want {
			t.Fatalf("epoch %d: event %d follows %d", ev.GetEpoch(), ev.GetSeq(), want-1)
		}
		next[ev.GetEpoch()] = ev.GetSeq()
	}
}

func count(evs []*gatewayv0.ActivityEvent, match func(*gatewayv0.ActivityEvent) bool) int {
	n := 0
	for _, ev := range evs {
		if match(ev) {
			n++
		}
	}
	return n
}

func folderStatus(t *testing.T, dir string) sessionfs.Status {
	t.Helper()
	s, err := sessionfs.Folder{Dir: dir}.ReadStatus()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunCompletes(t *testing.T) {
	store := sdk.NewMemoryStore()
	g := startGateway(t, "", store, demoTask(`{"steps":2,"step":"30ms","ticks":3}`))
	dir := t.TempDir()
	if code := run(t, testConfig(dir, g.addr)).wait(t); code != session.ExitCompleted {
		t.Fatalf("exit %d, want %d", code, session.ExitCompleted)
	}
	// The task came from the gateway and was written to the folder.
	task, err := sessionfs.Folder{Dir: dir}.ReadTask()
	if err != nil || task.ID != "t1" {
		t.Fatalf("task.json: %+v, %v", task, err)
	}
	out, err := os.ReadFile(filepath.Join(dir, "output", "result.txt"))
	if err != nil || !strings.Contains(string(out), "from the gateway") {
		t.Fatalf("deliverable %q, %v", out, err)
	}
	sess := g.srv.Sessions()[0]
	evs := events(t, store, sess.ID())
	last := evs[len(evs)-1]
	if last.GetState().GetTo() != gatewayv0.SessionState_SESSION_STATE_COMPLETED {
		t.Errorf("last event %v, want the completed state", last)
	}
	// The final status arrives on the control stream as the worker exits.
	deadline := time.Now().Add(5 * time.Second)
	for st, _ := sess.State(); st != gatewayv0.SessionState_SESSION_STATE_COMPLETED; st, _ = sess.State() {
		if time.Now().After(deadline) {
			t.Fatalf("gateway saw final state %v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if folderStatus(t, dir) != sessionfs.StatusCompleted {
		t.Error("folder not marked completed")
	}
}

func TestTaskJSONComesFirst(t *testing.T) {
	g := startGateway(t, "", sdk.NewMemoryStore(), demoTask(`{"steps":1,"step":"1ms"}`))
	dir := t.TempDir()
	f, err := sessionfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = f.WriteTask(&sessionfs.Task{Schema: "v0", ID: "local", Kind: session.DemoKind,
		Definition: json.RawMessage(`{"steps":1,"step":"1ms","output":"local.txt"}`), Input: json.RawMessage(`{"message":"from the folder"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if code := run(t, testConfig(dir, g.addr)).wait(t); code != session.ExitCompleted {
		t.Fatalf("exit %d", code)
	}
	out, err := os.ReadFile(filepath.Join(dir, "output", "local.txt"))
	if err != nil || !strings.Contains(string(out), "from the folder") {
		t.Fatalf("deliverable %q, %v", out, err)
	}
}

func TestPauseResumeStop(t *testing.T) {
	store := sdk.NewMemoryStore()
	g := startGateway(t, "", store, demoTask(`{"steps":6,"step":"150ms"}`))
	dir := t.TempDir()

	// Run 1: pause.
	w := run(t, testConfig(dir, g.addr))
	sess := g.session(t)
	id := sess.ID()
	pause := &gatewayv0.Command{Id: "pause-1", Action: &gatewayv0.Command_Pause{Pause: &gatewayv0.Pause{Mode: gatewayv0.PauseMode_PAUSE_MODE_DRAIN}}}
	accepted(t)(sess.Send(ctxT(t), pause))
	if code := w.wait(t); code != session.ExitPaused {
		t.Fatalf("pause: exit %d, want %d", code, session.ExitPaused)
	}
	if folderStatus(t, dir) != sessionfs.StatusPaused {
		t.Error("folder not marked paused")
	}

	// Run 2: resume on the same folder, same session.
	w = run(t, testConfig(dir, g.addr))
	sess = g.session(t)
	if sess.ID() != id || sess.Epoch() != 2 {
		t.Fatalf("resumed as session %s epoch %d, want %s epoch 2", sess.ID(), sess.Epoch(), id)
	}
	// The pause from run 1, resent, is recognised and not applied again.
	ack := accepted(t)(sess.Send(ctxT(t), pause))
	if !ack.GetDuplicate() {
		t.Fatalf("resent pause not recognised: %v", ack)
	}
	accepted(t)(sess.Steer(ctxT(t), "check the logs"))
	accepted(t)(sess.Stop(ctxT(t), sdk.StopOptions{Kill: true}))
	if code := w.wait(t); code != session.ExitStopped {
		t.Fatalf("stop: exit %d, want %d", code, session.ExitStopped)
	}

	evs := events(t, store, id)
	if n := count(evs, func(ev *gatewayv0.ActivityEvent) bool { return ev.GetSteer().GetNote() == "check the logs" }); n != 1 {
		t.Errorf("steer note in activity %d times", n)
	}
	if n := count(evs, func(ev *gatewayv0.ActivityEvent) bool { return ev.GetStarted() != nil }); n != 2 {
		t.Errorf("%d runs started, want 2", n)
	}
	rec, err := session.Recover(sessionfs.Folder{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if c := rec.Commands["pause-1"]; c == nil || c.GetOrigin() != "orchestrator" {
		t.Errorf("pause not journaled with its origin: %v", c)
	}

	// Run 3: a stopped session doesn't resume.
	if code := run(t, testConfig(dir, g.addr)).wait(t); code != session.ExitStopped {
		t.Fatalf("run after stop: exit %d, want %d", code, session.ExitStopped)
	}
}

func TestPauseWithoutModeRejected(t *testing.T) {
	g := startGateway(t, "", sdk.NewMemoryStore(), demoTask(`{"steps":20,"step":"100ms"}`))
	w := run(t, testConfig(t.TempDir(), g.addr))
	sess := g.session(t)
	ack, err := sess.Send(ctxT(t), &gatewayv0.Command{Action: &gatewayv0.Command_Pause{Pause: &gatewayv0.Pause{}}})
	if err != nil || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_REJECTED {
		t.Fatalf("pause without mode: %v, %v", ack, err)
	}
	if ack.GetState() != gatewayv0.SessionState_SESSION_STATE_RUNNING {
		t.Errorf("state after rejected pause: %v", ack.GetState())
	}
	accepted(t)(sess.Pause(ctxT(t), gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT))
	if code := w.wait(t); code != session.ExitPaused {
		t.Fatalf("exit %d", code)
	}
}

func TestSignalPauses(t *testing.T) {
	g := startGateway(t, "", sdk.NewMemoryStore(), demoTask(`{"steps":20,"step":"100ms"}`))
	dir := t.TempDir()
	w := run(t, testConfig(dir, g.addr))
	g.session(t)
	w.signals <- syscall.SIGTERM
	if code := w.wait(t); code != session.ExitPaused {
		t.Fatalf("exit %d, want %d", code, session.ExitPaused)
	}
	if folderStatus(t, dir) != sessionfs.StatusPaused {
		t.Error("folder not marked paused")
	}
}

// Killing the gateway and starting it again loses nothing and duplicates
// nothing: the worker keeps working, reconnects, re-registers, and resends
// only what the gateway's store doesn't have.
func TestGatewayRestart(t *testing.T) {
	storeDir := t.TempDir()
	store, err := sdk.NewRecordingStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	task := demoTask(`{"steps":8,"step":"150ms","ticks":5}`)
	g := startGateway(t, "", store, task)
	dir := t.TempDir()
	w := run(t, testConfig(dir, g.addr))
	sess := g.session(t)
	id := sess.ID()
	steer := &gatewayv0.Command{Id: "steer-1", Action: &gatewayv0.Command_Steer{Steer: &gatewayv0.Steer{Note: "once"}}}
	accepted(t)(sess.Send(ctxT(t), steer))
	time.Sleep(200 * time.Millisecond)

	g.gs.Stop() // kill the gateway
	_ = store.Close()
	time.Sleep(400 * time.Millisecond) // the worker keeps going meanwhile

	store, err = sdk.NewRecordingStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	g = startGateway(t, g.addr, store, task)
	sess = g.session(t)
	if sess.ID() != id {
		t.Fatalf("re-registered as %s, want %s", sess.ID(), id)
	}
	// The new gateway resends the same command; it isn't applied twice.
	if ack := accepted(t)(sess.Send(ctxT(t), steer)); !ack.GetDuplicate() {
		t.Fatalf("resent command not recognised: %v", ack)
	}
	if code := w.wait(t); code != session.ExitCompleted {
		t.Fatalf("exit %d", code)
	}

	evs := readRecording(t, store.Path(id))
	checkSequence(t, evs)
	if n := count(evs, func(ev *gatewayv0.ActivityEvent) bool { return ev.GetSteer() != nil }); n != 1 {
		t.Errorf("steer note recorded %d times", n)
	}
	if n := count(evs, func(ev *gatewayv0.ActivityEvent) bool { return strings.Contains(ev.GetText().GetText(), "done") }); n != 8 {
		t.Errorf("%d step completions recorded, want 8", n)
	}
	rec, _ := session.Recover(sessionfs.Folder{Dir: dir})
	if len(rec.Commands) != 1 {
		t.Errorf("journal holds %d commands, want 1", len(rec.Commands))
	}
}

// A long disconnect spills activity to the session folder; it is delivered
// after reconnecting.
func TestLongDisconnectSpills(t *testing.T) {
	for _, tc := range []struct {
		name     string
		spillMax int64
		wantGap  bool
	}{
		{"spill holds everything", 64 << 20, false},
		{"spill cap reached", 2 << 10, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := sdk.NewMemoryStore()
			task := demoTask(`{"steps":3,"step":"300ms","ticks":60}`)
			g := startGateway(t, "", store, task)
			dir := t.TempDir()
			cfg := testConfig(dir, g.addr)
			cfg.ActivityQueue, cfg.ActivitySpillMax = 8, tc.spillMax
			w := run(t, cfg)
			id := g.session(t).ID()
			g.gs.Stop()

			spill := filepath.Join(dir, ".actlet", "activity-1.actrec")
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(spill); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no spill file during the disconnect")
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond)

			startGateway(t, g.addr, store, task)
			if code := w.wait(t); code != session.ExitCompleted {
				t.Fatalf("exit %d", code)
			}
			evs := events(t, store, id)
			gaps := count(evs, func(ev *gatewayv0.ActivityEvent) bool { return ev.GetGap() != nil })
			holes := progressHoles(evs)
			if tc.wantGap {
				if gaps == 0 {
					t.Errorf("no gap event")
				}
			} else if gaps != 0 || holes != 0 {
				t.Errorf("%d gap events and %d missing progress events, want none lost", gaps, holes)
			}
			if _, err := os.Stat(spill); err == nil {
				t.Error("spill file kept after delivery")
			}
		})
	}
}

type slowStore struct{ *sdk.MemoryStore }

func (s slowStore) Append(ctx context.Context, id string, evs []*gatewayv0.ActivityEvent) error {
	time.Sleep(300 * time.Millisecond)
	return s.MemoryStore.Append(ctx, id, evs)
}

func TestSlowConsumerDoesNotDelayControl(t *testing.T) {
	g := startGateway(t, "", slowStore{sdk.NewMemoryStore()}, demoTask(`{"steps":10,"step":"100ms","ticks":20}`))
	w := run(t, testConfig(t.TempDir(), g.addr))
	sess := g.session(t)
	time.Sleep(200 * time.Millisecond) // activity is backing up
	for i := range 3 {
		start := time.Now()
		accepted(t)(sess.Steer(ctxT(t), fmt.Sprint("note ", i)))
		if d := time.Since(start); d > 250*time.Millisecond {
			t.Errorf("steer %d acknowledged after %v", i, d)
		}
	}
	accepted(t)(sess.Stop(ctxT(t), sdk.StopOptions{Kill: true}))
	if code := w.wait(t); code != session.ExitStopped {
		t.Fatalf("exit %d", code)
	}
}

// progressHoles counts progress events missing from the demo task's
// numbered "working (k/n)" events: each step's must run 1, 2, 3... A
// ticker may skip ticks under load, so the count per step varies, but a
// hole means an event was lost.
func progressHoles(evs []*gatewayv0.ActivityEvent) int {
	last := map[int]int{}
	holes := 0
	for _, ev := range evs {
		var step, k, n int
		if _, err := fmt.Sscanf(ev.GetText().GetText(), "step %d: working (%d/%d)", &step, &k, &n); err != nil {
			continue
		}
		holes += k - last[step] - 1
		last[step] = k
	}
	return holes
}

// testLogger discards logs unless ACTLET_TEST_LOG is set.
func testLogger() *slog.Logger {
	if os.Getenv("ACTLET_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

func TestMissingFeatures(t *testing.T) {
	if m := missingFeatures([]string{"control.pause", "activity.v0"}); len(m) != 0 {
		t.Errorf("supported features reported missing: %v", m)
	}
	if m := missingFeatures([]string{"control.pause", "budgets.v1"}); len(m) != 1 || m[0] != "budgets.v1" {
		t.Errorf("missing = %v, want [budgets.v1]", m)
	}
}

// Draining a gateway sends the worker to another one, here started on the
// same address with the same store, with nothing lost or duplicated.
func TestGatewayDrain(t *testing.T) {
	storeDir := t.TempDir()
	store, err := sdk.NewRecordingStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	task := demoTask(`{"steps":6,"step":"150ms","ticks":5}`)
	g := startGateway(t, "", store, task)
	w := run(t, testConfig(t.TempDir(), g.addr))
	id := g.session(t).ID()
	accepted(t)(g.session(t).Steer(ctxT(t), "before the drain"))
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := g.srv.Drain(ctx, g.gs); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	_ = store.Close()

	store, err = sdk.NewRecordingStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	g2 := startGateway(t, g.addr, store, task)
	accepted(t)(g2.session(t).Steer(ctxT(t), "after the drain"))
	if code := w.wait(t); code != session.ExitCompleted {
		t.Fatalf("exit %d", code)
	}
	evs := readRecording(t, store.Path(id))
	checkSequence(t, evs)
	if n := count(evs, func(ev *gatewayv0.ActivityEvent) bool { return ev.GetSteer() != nil }); n != 2 {
		t.Errorf("%d steer notes recorded, want 2", n)
	}
	if n := count(evs, func(ev *gatewayv0.ActivityEvent) bool { return strings.Contains(ev.GetText().GetText(), "done") }); n != 6 {
		t.Errorf("%d step completions recorded, want 6", n)
	}
}
