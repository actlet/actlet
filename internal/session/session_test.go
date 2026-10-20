package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/actlet/actlet/internal/session/sessionpb"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// recorder collects emitted activity.
type recorder struct {
	mu  sync.Mutex
	evs []*gatewayv0.ActivityEvent
}

func (r *recorder) emit(ev *gatewayv0.ActivityEvent) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
}

func (r *recorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, ev := range r.evs {
		if t := ev.GetText(); t != nil {
			out = append(out, t.GetText())
		}
	}
	return out
}

func (r *recorder) count(match func(*gatewayv0.ActivityEvent) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.evs {
		if match(ev) {
			n++
		}
	}
	return n
}

func (r *recorder) waitText(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range r.texts() {
			if strings.Contains(s, substr) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no activity containing %q; got %q", substr, r.texts())
}

type run struct {
	s   *Session
	rec *recorder
}

func testConfig() *sessionpb.Config {
	return &sessionpb.Config{
		ProfileId: "p", ProfileVersion: "1",
		StopGrace:        durationpb.New(time.Second),
		DefaultPauseMode: "drain",
		SignalDeadline:   durationpb.New(time.Second),
	}
}

// start runs the session in f with a demo task definition. The epoch and
// recovered state come from the journal, as in a real run.
func start(t *testing.T, f sessionfs.Folder, definition string, cfg *sessionpb.Config) *run {
	t.Helper()
	recovered, err := Recover(f)
	if err != nil {
		t.Fatal(err)
	}
	epoch, id := uint64(1), "s1"
	if recovered != nil {
		epoch, id = recovered.Epochs+1, recovered.SessionID
	}
	rec := &recorder{}
	s, err := Start(Params{
		Folder: f, SessionID: id, Epoch: epoch, Recovered: recovered, Config: cfg,
		MaxStopGrace: 10 * time.Second,
		Task: &sessionfs.Task{Schema: sessionfs.TaskSchema, ID: "t1", Kind: DemoKind,
			Definition: json.RawMessage(definition), Input: json.RawMessage(`{"message":"hello"}`)},
		Emit:   rec.emit,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		<-s.Done()
		_ = s.Close()
	})
	return &run{s: s, rec: rec}
}

func folder(t *testing.T) sessionfs.Folder {
	t.Helper()
	f, err := sessionfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func outcome(t *testing.T, r *run) Outcome {
	t.Helper()
	select {
	case <-r.s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run didn't end")
	}
	return r.s.Outcome()
}

func (r *run) apply(t *testing.T, cmd *gatewayv0.Command) *gatewayv0.CommandAck {
	t.Helper()
	ack, err := r.s.Apply(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func pauseCmd(id string, mode gatewayv0.PauseMode) *gatewayv0.Command {
	return &gatewayv0.Command{Id: id, Origin: gatewayv0.Origin_ORIGIN_ORCHESTRATOR,
		Action: &gatewayv0.Command_Pause{Pause: &gatewayv0.Pause{Mode: mode}}}
}

func stopCmd(id string, grace time.Duration, kill bool) *gatewayv0.Command {
	stop := &gatewayv0.Stop{Kill: kill}
	if grace > 0 {
		stop.Grace = durationpb.New(grace)
	}
	return &gatewayv0.Command{Id: id, Action: &gatewayv0.Command_Stop{Stop: stop}}
}

func steerCmd(id, note string) *gatewayv0.Command {
	return &gatewayv0.Command{Id: id, Actor: "op", Origin: gatewayv0.Origin_ORIGIN_ORCHESTRATOR,
		Action: &gatewayv0.Command_Steer{Steer: &gatewayv0.Steer{Note: note}}}
}

func status(t *testing.T, f sessionfs.Folder) sessionfs.Status {
	t.Helper()
	s, err := f.ReadStatus()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCompletes(t *testing.T) {
	f := folder(t)
	r := start(t, f, `{"steps":2,"step":"5ms","ticks":2}`, testConfig())
	o := outcome(t, r)
	if o.State != gatewayv0.SessionState_SESSION_STATE_COMPLETED || o.ExitCode() != ExitCompleted {
		t.Fatalf("outcome %+v", o)
	}
	out, err := os.ReadFile(f.Output() + "/result.txt")
	if err != nil || !strings.Contains(string(out), "message: hello") {
		t.Fatalf("deliverable %q, %v", out, err)
	}
	if s := status(t, f); s != sessionfs.StatusCompleted {
		t.Errorf("status %q", s)
	}
	rec, err := Recover(f)
	if err != nil || rec.Epochs != 1 || rec.NextStep != 3 || rec.Resumable() {
		t.Fatalf("recovered %+v, %v", rec, err)
	}
}

func TestPauseDrainThenResume(t *testing.T) {
	f := folder(t)
	const def = `{"steps":4,"step":"100ms"}`
	r := start(t, f, def, testConfig())
	r.rec.waitText(t, "step 1 of 4 started")
	ack := r.apply(t, pauseCmd("p1", gatewayv0.PauseMode_PAUSE_MODE_DRAIN))
	if ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED || ack.GetState() != gatewayv0.SessionState_SESSION_STATE_PAUSING {
		t.Fatalf("ack %v", ack)
	}
	o := outcome(t, r)
	if o.State != gatewayv0.SessionState_SESSION_STATE_PAUSED || o.ExitCode() != ExitPaused {
		t.Fatalf("outcome %+v", o)
	}
	// Drain let the running step finish.
	rec, err := Recover(f)
	if err != nil || rec.NextStep != 2 || !rec.Resumable() || status(t, f) != sessionfs.StatusPaused {
		t.Fatalf("after pause: %+v, %v, status %q", rec, err, status(t, f))
	}

	// Resume: a new run on the same folder continues where it stopped.
	r2 := start(t, f, def, testConfig())
	if o := outcome(t, r2); o.State != gatewayv0.SessionState_SESSION_STATE_COMPLETED {
		t.Fatalf("resumed outcome %+v", o)
	}
	if got := r2.rec.texts(); !strings.Contains(strings.Join(got, "\n"), "step 2 of 4 started") || strings.Contains(strings.Join(got, "\n"), "step 1 of 4 started") {
		t.Errorf("resumed run activity: %q", got)
	}
	started := r2.rec.count(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetStarted().GetResumed() })
	if started != 1 {
		t.Errorf("resumed run didn't report resuming")
	}
	if rec, _ := Recover(f); rec.Epochs != 2 {
		t.Errorf("epochs %d, want 2", rec.Epochs)
	}
}

func TestPauseInterrupt(t *testing.T) {
	f := folder(t)
	r := start(t, f, `{"steps":3,"step":"10s","term_delay":"20ms"}`, testConfig())
	r.rec.waitText(t, "step 1 of 3 started")
	begin := time.Now()
	r.apply(t, pauseCmd("p1", gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT))
	o := outcome(t, r)
	if o.State != gatewayv0.SessionState_SESSION_STATE_PAUSED || time.Since(begin) > 2*time.Second {
		t.Fatalf("outcome %+v after %v", o, time.Since(begin))
	}
	r.rec.waitText(t, "step 1 interrupted")
	if rec, _ := Recover(f); rec.NextStep != 1 {
		t.Errorf("interrupted step recorded as done: next step %d", rec.NextStep)
	}
}

func TestPauseWithoutModeRejected(t *testing.T) {
	f := folder(t)
	r := start(t, f, `{"steps":1,"step":"200ms"}`, testConfig())
	ack := r.apply(t, pauseCmd("p0", gatewayv0.PauseMode_PAUSE_MODE_UNSPECIFIED))
	if ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_REJECTED || !strings.Contains(ack.GetMessage(), "mode") {
		t.Fatalf("ack %v", ack)
	}
	if o := outcome(t, r); o.State != gatewayv0.SessionState_SESSION_STATE_COMPLETED {
		t.Fatalf("a rejected pause stopped the run: %+v", o)
	}
	rec, _ := Recover(f)
	if c := rec.Commands["p0"]; c == nil || c.GetAccepted() {
		t.Errorf("rejected command not journaled as rejected: %v", c)
	}
}

func TestStop(t *testing.T) {
	cases := []struct {
		name     string
		def      string
		grace    time.Duration
		kill     bool
		wantText string
		maxWait  time.Duration
		minWait  time.Duration
	}{
		{"grace long enough", `{"steps":3,"step":"10s","term_delay":"20ms"}`, 2 * time.Second, false, "step 1 interrupted", time.Second, 0},
		{"grace runs out", `{"steps":3,"step":"10s","term_delay":"10s"}`, 100 * time.Millisecond, false, "step 1 killed", time.Second, 90 * time.Millisecond},
		{"kill", `{"steps":3,"step":"10s","term_delay":"10s"}`, 5 * time.Second, true, "step 1 killed", 500 * time.Millisecond, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := folder(t)
			r := start(t, f, tc.def, testConfig())
			r.rec.waitText(t, "step 1 of 3 started")
			begin := time.Now()
			r.apply(t, stopCmd("s1", tc.grace, tc.kill))
			o := outcome(t, r)
			took := time.Since(begin)
			if o.State != gatewayv0.SessionState_SESSION_STATE_STOPPED || o.ExitCode() != ExitStopped {
				t.Fatalf("outcome %+v", o)
			}
			if took > tc.maxWait || took < tc.minWait {
				t.Errorf("stop took %v, want between %v and %v", took, tc.minWait, tc.maxWait)
			}
			r.rec.waitText(t, tc.wantText)
			rec, _ := Recover(f)
			if rec.Resumable() || rec.EndedOutcome().ExitCode() != ExitStopped || status(t, f) != sessionfs.StatusStopped {
				t.Errorf("stopped session looks resumable: %+v", rec.Ended)
			}
		})
	}
}

func TestStopGraceCapped(t *testing.T) {
	r := start(t, folder(t), `{"steps":1,"step":"10s","term_delay":"1ms"}`, testConfig())
	ack := r.apply(t, stopCmd("s1", time.Hour, false))
	if !strings.Contains(ack.GetMessage(), "capped") {
		t.Errorf("ack %v", ack)
	}
	outcome(t, r)
}

func TestSignalPauses(t *testing.T) {
	cfg := testConfig()
	cfg.SignalDeadline = durationpb.New(100 * time.Millisecond)
	f := folder(t)
	r := start(t, f, `{"steps":3,"step":"10s","term_delay":"10s"}`, cfg)
	r.rec.waitText(t, "step 1 of 3 started")
	begin := time.Now()
	r.s.Signal()
	o := outcome(t, r)
	took := time.Since(begin)
	// Drain doesn't finish a 10s step, so the deadline kills it.
	if o.State != gatewayv0.SessionState_SESSION_STATE_PAUSED || took < 90*time.Millisecond || took > time.Second {
		t.Fatalf("outcome %+v after %v", o, took)
	}
	r.rec.waitText(t, "step 1 killed")

	// A second signal doesn't wait for the deadline.
	cfg.SignalDeadline = durationpb.New(time.Hour)
	r = start(t, f, `{"steps":3,"step":"10s","term_delay":"10s"}`, cfg)
	r.rec.waitText(t, "started")
	r.s.Signal()
	r.s.Signal()
	if o := outcome(t, r); o.State != gatewayv0.SessionState_SESSION_STATE_PAUSED {
		t.Fatalf("outcome %+v", o)
	}
}

func TestDuplicateCommands(t *testing.T) {
	f := folder(t)
	const def = `{"steps":2,"step":"150ms"}`
	r := start(t, f, def, testConfig())
	steers := func(r *run) int {
		return r.rec.count(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetSteer() != nil })
	}
	if ack := r.apply(t, steerCmd("n1", "look left")); ack.GetDuplicate() || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
		t.Fatalf("first ack %v", ack)
	}
	if ack := r.apply(t, steerCmd("n1", "look left")); !ack.GetDuplicate() || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
		t.Fatalf("second ack %v", ack)
	}
	if n := steers(r); n != 1 {
		t.Fatalf("steer note emitted %d times", n)
	}
	r.apply(t, pauseCmd("p1", gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT))
	outcome(t, r)

	// After a resume, commands from earlier runs are still recognised.
	r2 := start(t, f, def, testConfig())
	if ack := r2.apply(t, pauseCmd("p1", gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT)); !ack.GetDuplicate() {
		t.Fatalf("resent pause not recognised: %v", ack)
	}
	if ack := r2.apply(t, steerCmd("n1", "look left")); !ack.GetDuplicate() || steers(r2) != 0 {
		t.Fatalf("resent steer re-applied: %v", ack)
	}
	if o := outcome(t, r2); o.State != gatewayv0.SessionState_SESSION_STATE_COMPLETED {
		t.Fatalf("a duplicate pause paused the resumed run: %+v", o)
	}
}

// By the time a command is acknowledged, it is in the journal.
func TestCommandJournaledBeforeAck(t *testing.T) {
	f := folder(t)
	r := start(t, f, `{"steps":1,"step":"300ms"}`, testConfig())
	for i := range 5 {
		id := fmt.Sprint("n", i)
		r.apply(t, steerCmd(id, "note"))
		rec, err := Recover(f)
		if err != nil {
			t.Fatal(err)
		}
		if c := rec.Commands[id]; c == nil || c.GetNote() != "note" || c.GetOrigin() != "orchestrator" || c.GetActor() != "op" {
			t.Fatalf("acknowledged command %s not in the journal: %v", id, c)
		}
	}
	outcome(t, r)
}

func TestCommandValidation(t *testing.T) {
	r := start(t, folder(t), `{"steps":1,"step":"200ms"}`, testConfig())
	for _, cmd := range []*gatewayv0.Command{
		steerCmd("", "no id"),
		steerCmd("e", ""),
		steerCmd("l", strings.Repeat("x", MaxNoteLen+1)),
		{Id: "u"},
		{Id: "g", Action: &gatewayv0.Command_Stop{Stop: &gatewayv0.Stop{Grace: durationpb.New(-time.Second)}}},
	} {
		if ack := r.apply(t, cmd); ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_REJECTED {
			t.Errorf("command %v accepted: %v", cmd, ack)
		}
	}
	outcome(t, r)
}

func TestUnknownTaskFails(t *testing.T) {
	f := folder(t)
	rec := &recorder{}
	s, err := Start(Params{
		Folder: f, SessionID: "s1", Epoch: 1, Config: testConfig(),
		Task: &sessionfs.Task{Schema: sessionfs.TaskSchema, ID: "t", Kind: "teleport"},
		Emit: rec.emit, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	o := s.Outcome()
	if o.ExitCode() != ExitFailed || !strings.Contains(o.Reason, "teleport") {
		t.Fatalf("outcome %+v", o)
	}
	if status(t, f) != sessionfs.StatusFailed {
		t.Errorf("status %q", status(t, f))
	}
	if r, _ := Recover(f); !r.Resumable() {
		t.Error("a failed run should be resumable after fixing the cause")
	}
}

func TestApplyAfterEnd(t *testing.T) {
	r := start(t, folder(t), `{"steps":1,"step":"1ms"}`, testConfig())
	outcome(t, r)
	ack, err := r.s.Apply(context.Background(), steerCmd("late", "x"))
	if err != nil || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_REJECTED {
		t.Fatalf("ack %v, %v", ack, err)
	}
}
