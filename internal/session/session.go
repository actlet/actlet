// Package session runs one session in its session folder: it records
// progress and commands in the journal, applies control commands, runs the
// task, and ends the run as completed, paused, stopped or failed.
//
// Pausing and stopping end the process. A paused session resumes when a new
// run starts on the same folder; it recovers from the journal exactly like
// a run after a crash.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/session/sessionpb"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Journal record kinds.
const (
	kindStarted = "session.started"
	kindCommand = "command"
	kindStep    = "task.step"
	kindEnded   = "session.ended"
)

// Exit codes of a run, so the orchestrator knows why the process ended.
const (
	ExitCompleted = 0
	ExitFailed    = 1
	ExitUsage     = 2
	ExitPaused    = 3
	ExitStopped   = 4
)

// MaxNoteLen bounds steer notes.
const MaxNoteLen = 4096

// Outcome is how a run ended.
type Outcome struct {
	State  gatewayv0.SessionState
	Reason string
}

// ExitCode returns the process exit code for the outcome.
func (o Outcome) ExitCode() int {
	switch o.State {
	case gatewayv0.SessionState_SESSION_STATE_COMPLETED:
		return ExitCompleted
	case gatewayv0.SessionState_SESSION_STATE_PAUSED:
		return ExitPaused
	case gatewayv0.SessionState_SESSION_STATE_STOPPED:
		return ExitStopped
	default:
		return ExitFailed
	}
}

func (o Outcome) status() sessionfs.Status {
	switch o.State {
	case gatewayv0.SessionState_SESSION_STATE_COMPLETED:
		return sessionfs.StatusCompleted
	case gatewayv0.SessionState_SESSION_STATE_PAUSED:
		return sessionfs.StatusPaused
	case gatewayv0.SessionState_SESSION_STATE_STOPPED:
		return sessionfs.StatusStopped
	default:
		return sessionfs.StatusFailed
	}
}

// Recovered is what the journal says about a session before this run.
type Recovered struct {
	SessionID string
	// Epochs is the number of runs so far.
	Epochs uint64
	// Commands are the commands received so far, by ID.
	Commands map[string]*sessionpb.Command
	// NextStep is the first task step not completed yet.
	NextStep uint64
	// Ended is how the last run ended, or nil if it crashed.
	Ended *sessionpb.Ended
	// Config is the last run's effective configuration.
	Config *sessionpb.Config
}

// Resumable reports whether a new run may continue the session: it hasn't
// completed or been stopped.
func (r *Recovered) Resumable() bool {
	return r.Ended == nil || r.Ended.GetState() == string(sessionfs.StatusPaused) || r.Ended.GetState() == string(sessionfs.StatusFailed)
}

// EndedOutcome returns the outcome of a session that has ended for good.
func (r *Recovered) EndedOutcome() Outcome {
	switch r.Ended.GetState() {
	case string(sessionfs.StatusCompleted):
		return Outcome{State: gatewayv0.SessionState_SESSION_STATE_COMPLETED, Reason: "session already completed"}
	case string(sessionfs.StatusStopped):
		return Outcome{State: gatewayv0.SessionState_SESSION_STATE_STOPPED, Reason: "session was stopped"}
	}
	return Outcome{State: gatewayv0.SessionState_SESSION_STATE_FAILED, Reason: "unknown end state " + r.Ended.GetState()}
}

// Recover reads the session journal in the folder. It returns nil if there
// is none: a new session.
func Recover(f sessionfs.Folder) (*Recovered, error) {
	file, err := os.Open(f.Journal())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	r, err := journal.NewReader(file, st.Size(), journal.ReaderOptions{})
	if err != nil {
		return nil, fmt.Errorf("session journal: %w", err)
	}
	defer r.Close()
	rec := &Recovered{
		SessionID: r.Header().SessionID,
		Commands:  map[string]*sessionpb.Command{},
		NextStep:  1,
	}
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
			return rec, nil // opening the journal for writing truncates a torn tail
		}
		if err != nil {
			return nil, fmt.Errorf("session journal: %w", err)
		}
		for _, jr := range b.Records {
			if err := rec.apply(jr); err != nil {
				return nil, fmt.Errorf("session journal record %d: %w", jr.Seq, err)
			}
		}
	}
}

func (r *Recovered) apply(jr journal.Record) error {
	switch jr.Kind {
	case kindStarted:
		var m sessionpb.Started
		if err := proto.Unmarshal(jr.Data, &m); err != nil {
			return err
		}
		r.Epochs++
		r.Config = m.GetConfig()
		r.Ended = nil
	case kindCommand:
		var m sessionpb.Command
		if err := proto.Unmarshal(jr.Data, &m); err != nil {
			return err
		}
		r.Commands[m.GetId()] = &m
	case kindStep:
		var m sessionpb.StepDone
		if err := proto.Unmarshal(jr.Data, &m); err != nil {
			return err
		}
		r.NextStep = max(r.NextStep, m.GetStep()+1)
	case kindEnded:
		var m sessionpb.Ended
		if err := proto.Unmarshal(jr.Data, &m); err != nil {
			return err
		}
		r.Ended = &m
	}
	return nil
}

// Params configure a run.
type Params struct {
	Folder    sessionfs.Folder
	SessionID string
	// Epoch is this run's number: the previous runs plus one.
	Epoch     uint64
	Recovered *Recovered // nil for a new session
	Config    *sessionpb.Config
	// MaxStopGrace caps the grace period of stop commands.
	MaxStopGrace time.Duration
	Task         *sessionfs.Task
	WorkerID     string
	Version      string
	// Emit publishes an activity event. It must not block.
	Emit   func(*gatewayv0.ActivityEvent)
	Logger *slog.Logger
}

// Session is a running session.
type Session struct {
	p   Params
	log *slog.Logger
	j   *journal.Writer

	cmdMu    sync.Mutex // serialises commands
	commands map[string]*sessionpb.Command

	mu         sync.Mutex
	state      gatewayv0.SessionState
	target     gatewayv0.SessionState // PAUSED or STOPPED once requested
	reason     string
	noNewWork  bool
	signalled  int
	softCancel context.CancelFunc // asks running work to stop
	hardCancel context.CancelFunc // kills running work
	soft, hard context.Context
	timers     []*time.Timer

	done     chan struct{}
	outcome  Outcome
	closeErr error
}

// Start opens or creates the session journal, records the start of this run
// and starts the task.
func Start(p Params) (*Session, error) {
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	if p.Emit == nil {
		p.Emit = func(*gatewayv0.ActivityEvent) {}
	}
	jopts := journal.DefaultOptions(p.SessionID)
	jopts.Logger = p.Logger
	var j *journal.Writer
	var err error
	if p.Recovered == nil {
		j, err = journal.Create(p.Folder.Journal(), jopts)
	} else {
		j, err = journal.Open(p.Folder.Journal(), jopts)
	}
	if err != nil {
		return nil, err
	}
	if rec := j.Recovery(); rec.Truncated() > 0 {
		p.Logger.Warn("session journal: dropped an incomplete final batch", "bytes", rec.Truncated(), "reason", rec.TailErr)
	}

	s := &Session{
		p:        p,
		log:      p.Logger,
		j:        j,
		commands: map[string]*sessionpb.Command{},
		state:    gatewayv0.SessionState_SESSION_STATE_RUNNING,
		done:     make(chan struct{}),
	}
	nextStep := uint64(1)
	if p.Recovered != nil {
		for id, c := range p.Recovered.Commands {
			s.commands[id] = c
		}
		nextStep = p.Recovered.NextStep
	}
	s.soft, s.softCancel = context.WithCancel(context.Background())
	s.hard, s.hardCancel = context.WithCancel(context.Background())

	started := &sessionpb.Started{
		Epoch: p.Epoch, SessionId: p.SessionID, Config: p.Config,
		Resumed: p.Recovered != nil, WorkerId: p.WorkerID, ActletVersion: p.Version,
	}
	if err := s.record(context.Background(), kindStarted, started); err != nil {
		_ = j.Close()
		return nil, err
	}
	if err := p.Folder.WriteStatus(sessionfs.StatusRunning); err != nil {
		_ = j.Close()
		return nil, err
	}
	p.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Started{Started: &gatewayv0.SessionStarted{
		Epoch: p.Epoch, ProfileId: p.Config.GetProfileId(), ProfileVersion: p.Config.GetProfileVersion(), Resumed: p.Recovered != nil,
	}}})
	s.emitState(gatewayv0.SessionState_SESSION_STATE_UNSPECIFIED, gatewayv0.SessionState_SESSION_STATE_RUNNING, "started")

	go s.run(nextStep)
	return s, nil
}

// ID returns the session ID.
func (s *Session) ID() string { return s.p.SessionID }

// Done is closed when the run has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// Outcome returns how the run ended, once Done is closed.
func (s *Session) Outcome() Outcome {
	<-s.done
	return s.outcome
}

// State returns the session state.
func (s *Session) State() gatewayv0.SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Close waits for the run to end and returns any error from closing the
// journal, which the run closes when it ends.
func (s *Session) Close() error {
	<-s.done
	return s.closeErr
}

func (s *Session) record(ctx context.Context, kind string, m proto.Message) error {
	data, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.j.Append(ctx, kind, data)
	return err
}

func (s *Session) emitText(format string, args ...any) {
	s.p.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: fmt.Sprintf(format, args...)}}})
}

func (s *Session) emitState(from, to gatewayv0.SessionState, reason string) {
	s.p.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_State{State: &gatewayv0.StateChange{From: from, To: to, Reason: reason}}})
}

// Apply handles a control command: it records the command in the journal,
// then applies it, then returns the acknowledgement. A command whose ID was
// seen before, in this run or an earlier one, is acknowledged again but not
// applied twice. An error means the command couldn't be recorded and must
// not be acknowledged.
func (s *Session) Apply(ctx context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error) {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	if prev, ok := s.commands[cmd.GetId()]; ok {
		return s.ack(cmd.GetId(), prev, true), nil
	}
	rec := s.check(cmd)
	select {
	case <-s.done:
		rec.Accepted, rec.Message = false, "the session has ended"
		return s.ack(cmd.GetId(), rec, false), nil
	default:
	}
	if err := s.record(ctx, kindCommand, rec); err != nil {
		return nil, fmt.Errorf("record command: %w", err)
	}
	s.commands[rec.Id] = rec

	if rec.Accepted {
		switch a := cmd.Action.(type) {
		case *gatewayv0.Command_Pause:
			if msg := s.requestPause(a.Pause.GetMode(), "pause command"); msg != "" {
				rec.Message = msg
			}
		case *gatewayv0.Command_Stop:
			if msg := s.requestStop(rec.GetStopGrace().AsDuration(), rec.GetStopKill(), "stop command"); msg != "" {
				rec.Message = msg
			}
		case *gatewayv0.Command_Steer:
			s.p.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Steer{Steer: &gatewayv0.SteerNote{
				Note: rec.Note, Origin: cmd.GetOrigin(), Actor: cmd.GetActor(),
			}}})
		}
	}
	result := gatewayv0.CommandResult_COMMAND_RESULT_REJECTED
	if rec.Accepted {
		result = gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED
	}
	s.p.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Command{Command: &gatewayv0.CommandReceived{
		Command: cmd, Result: result, Message: rec.Message,
	}}})
	return s.ack(cmd.GetId(), rec, false), nil
}

func (s *Session) ack(id string, rec *sessionpb.Command, duplicate bool) *gatewayv0.CommandAck {
	result := gatewayv0.CommandResult_COMMAND_RESULT_REJECTED
	if rec.GetAccepted() {
		result = gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED
	}
	return &gatewayv0.CommandAck{
		CommandId: id, Result: result, Message: rec.GetMessage(), Duplicate: duplicate, State: s.State(),
	}
}

// check validates a command and returns its journal record.
func (s *Session) check(cmd *gatewayv0.Command) *sessionpb.Command {
	rec := &sessionpb.Command{Id: cmd.GetId(), Origin: originName(cmd.GetOrigin()), Actor: cmd.GetActor(), Accepted: true}
	reject := func(msg string) *sessionpb.Command {
		rec.Accepted, rec.Message = false, msg
		return rec
	}
	if cmd.GetId() == "" {
		return reject("command has no ID")
	}
	switch a := cmd.Action.(type) {
	case *gatewayv0.Command_Pause:
		rec.Action = "pause"
		switch a.Pause.GetMode() {
		case gatewayv0.PauseMode_PAUSE_MODE_DRAIN, gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT:
			rec.PauseMode = pauseModeName(a.Pause.GetMode())
		default:
			return reject("pause needs a mode: drain or interrupt")
		}
	case *gatewayv0.Command_Stop:
		rec.Action = "stop"
		rec.StopKill = a.Stop.GetKill()
		grace := s.p.Config.GetStopGrace().AsDuration()
		if g := a.Stop.GetGrace(); g != nil {
			if g.AsDuration() < 0 {
				return reject("negative grace period")
			}
			grace = g.AsDuration()
		}
		if s.p.MaxStopGrace > 0 && grace > s.p.MaxStopGrace {
			grace = s.p.MaxStopGrace
			rec.Message = fmt.Sprintf("grace period capped at %v", grace)
		}
		rec.StopGrace = durationpb.New(grace)
	case *gatewayv0.Command_Steer:
		rec.Action = "steer"
		rec.Note = a.Steer.GetNote()
		switch {
		case rec.Note == "":
			return reject("empty steer note")
		case len(rec.Note) > MaxNoteLen:
			rec.Note = rec.Note[:MaxNoteLen]
			return reject(fmt.Sprintf("steer note longer than %d bytes", MaxNoteLen))
		}
	default:
		rec.Action = "unknown"
		return reject("unknown command")
	}
	return rec
}

func originName(o gatewayv0.Origin) string {
	switch o {
	case gatewayv0.Origin_ORIGIN_ORCHESTRATOR:
		return "orchestrator"
	case gatewayv0.Origin_ORIGIN_LOCAL:
		return "local"
	}
	return "unspecified"
}

func pauseModeName(m gatewayv0.PauseMode) string {
	if m == gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT {
		return "interrupt"
	}
	return "drain"
}

// PauseModeFromName parses "drain" or "interrupt".
func PauseModeFromName(name string) (gatewayv0.PauseMode, bool) {
	switch name {
	case "drain":
		return gatewayv0.PauseMode_PAUSE_MODE_DRAIN, true
	case "interrupt":
		return gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT, true
	}
	return gatewayv0.PauseMode_PAUSE_MODE_UNSPECIFIED, false
}

// requestPause stops new work and, for interrupt, signals running work. It
// returns a note when the request changes nothing.
func (s *Session) requestPause(mode gatewayv0.PauseMode, reason string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case gatewayv0.SessionState_SESSION_STATE_RUNNING:
		s.setStateLocked(gatewayv0.SessionState_SESSION_STATE_PAUSING, reason)
		s.target, s.reason, s.noNewWork = gatewayv0.SessionState_SESSION_STATE_PAUSED, reason, true
		if mode == gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT {
			s.softCancel()
		}
		return ""
	case gatewayv0.SessionState_SESSION_STATE_PAUSING:
		if mode == gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT {
			s.softCancel()
			return "already pausing; running work interrupted"
		}
		return "already pausing"
	default:
		return "already stopping"
	}
}

// requestStop stops new work and signals running work, killing it after
// grace, or at once with kill.
func (s *Session) requestStop(grace time.Duration, kill bool, reason string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := ""
	if s.state == gatewayv0.SessionState_SESSION_STATE_STOPPING {
		msg = "already stopping"
	} else {
		s.setStateLocked(gatewayv0.SessionState_SESSION_STATE_STOPPING, reason)
		s.target, s.reason, s.noNewWork = gatewayv0.SessionState_SESSION_STATE_STOPPED, reason, true
	}
	s.softCancel()
	if kill {
		s.hardCancel()
	} else {
		s.timers = append(s.timers, time.AfterFunc(grace, s.hardCancel))
	}
	return msg
}

// Signal handles an OS termination signal: the first pauses in the
// profile's default mode and gives running work until the signal deadline;
// a second one interrupts and kills running work at once.
func (s *Session) Signal() {
	s.mu.Lock()
	s.signalled++
	first := s.signalled == 1
	s.mu.Unlock()
	if !first {
		s.softCancel()
		s.hardCancel()
		return
	}
	mode, _ := PauseModeFromName(s.p.Config.GetDefaultPauseMode())
	s.requestPause(mode, "signal")
	deadline := s.p.Config.GetSignalDeadline().AsDuration()
	s.mu.Lock()
	s.timers = append(s.timers, time.AfterFunc(deadline, func() {
		s.softCancel()
		s.hardCancel()
	}))
	s.mu.Unlock()
}

func (s *Session) setStateLocked(to gatewayv0.SessionState, reason string) {
	from := s.state
	s.state = to
	s.emitState(from, to, reason)
}

// wantsStop reports whether new work must not start.
func (s *Session) wantsStop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noNewWork
}

func (s *Session) run(nextStep uint64) {
	o := s.work(nextStep)
	s.mu.Lock()
	for _, t := range s.timers {
		t.Stop()
	}
	s.mu.Unlock()
	s.softCancel()
	s.hardCancel()

	// No command may be recorded between the end of the run and closing
	// the journal; later commands see the run as ended.
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	if err := s.record(context.Background(), kindEnded, &sessionpb.Ended{State: string(o.status()), Reason: o.Reason}); err != nil {
		s.log.Error("session: recording the end of the run", "err", err)
		o = Outcome{State: gatewayv0.SessionState_SESSION_STATE_FAILED, Reason: "recording the end of the run: " + err.Error()}
	}
	s.closeErr = s.j.Close()
	if err := s.p.Folder.WriteStatus(o.status()); err != nil {
		s.log.Error("session: writing status", "err", err)
	}
	s.mu.Lock()
	s.setStateLocked(o.State, o.Reason)
	s.mu.Unlock()
	s.outcome = o
	close(s.done)
}

// work runs the task from nextStep until it completes or a pause or stop
// takes effect.
func (s *Session) work(nextStep uint64) Outcome {
	failed := func(format string, args ...any) Outcome {
		reason := fmt.Sprintf(format, args...)
		s.emitText("failed: %s", reason)
		return Outcome{State: gatewayv0.SessionState_SESSION_STATE_FAILED, Reason: reason}
	}
	task, err := newDemoTask(s.p.Task)
	if err != nil {
		return failed("%v", err)
	}
	step := nextStep
	for ; step <= task.steps; step++ {
		if s.wantsStop() {
			break
		}
		s.emitText("step %d of %d started", step, task.steps)
		res := task.runStep(s.soft, s.hard, step, s.emitText)
		if res != stepDone {
			s.emitText("step %d %s", step, res)
			break
		}
		if err := s.record(context.Background(), kindStep, &sessionpb.StepDone{Step: step}); err != nil {
			return failed("recording step %d: %v", step, err)
		}
		s.emitText("step %d of %d done", step, task.steps)
	}
	if step > task.steps {
		path, err := task.deliver(s.p.Folder.Output())
		if err != nil {
			return failed("writing the deliverable: %v", err)
		}
		s.emitText("task completed; deliverable %s", path)
		return Outcome{State: gatewayv0.SessionState_SESSION_STATE_COMPLETED, Reason: "task completed"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.target == gatewayv0.SessionState_SESSION_STATE_UNSPECIFIED {
		return Outcome{State: gatewayv0.SessionState_SESSION_STATE_FAILED, Reason: "work stopped unexpectedly"}
	}
	return Outcome{State: s.target, Reason: s.reason}
}
