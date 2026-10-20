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
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/actlet/actlet/internal/agent"
	"github.com/actlet/actlet/internal/budget"
	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/session/sessionpb"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Journal record kinds.
const (
	kindStarted  = "session.started"
	kindCommand  = "command"
	kindStep     = "task.step"
	kindEnded    = "session.ended"
	kindBudget   = "budget"
	kindRevision = "config.revision"
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
	// Agent are the agent's records from its latest checkpoint on.
	Agent []journal.Record
	// Steers are the accepted steer notes and the budget notices, in the
	// order they arrived.
	Steers []agent.Steer
	// Budget is what earlier runs recorded about the budgets.
	Budget budget.Restored

	// The current run's first and latest record times, for its active
	// time.
	runStart, runLast time.Time
}

func steerOf(c *sessionpb.Command) agent.Steer {
	from := c.GetActor()
	if from == "" {
		from = c.GetOrigin()
	}
	return agent.Steer{CommandID: c.GetId(), Note: c.GetNote(), From: from}
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
			rec.endRun()
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

// endRun adds a run's active time: from its start to its last record.
func (r *Recovered) endRun() {
	if !r.runStart.IsZero() && r.runLast.After(r.runStart) {
		r.Budget.ActiveTime += r.runLast.Sub(r.runStart)
	}
	r.runStart, r.runLast = time.Time{}, time.Time{}
}

func (r *Recovered) apply(jr journal.Record) error {
	if jr.Kind == kindStarted {
		r.endRun()
		r.runStart = jr.Time
	}
	if jr.Time.After(r.runLast) {
		r.runLast = jr.Time
	}
	if agent.IsRecord(jr.Kind) {
		if jr.Kind == agent.KindCheckpoint {
			// Keep the parts of a packed checkpoint, which come just
			// before it.
			n := len(r.Agent)
			for n > 0 && r.Agent[n-1].Kind == agent.KindCheckpointPart {
				n--
			}
			r.Agent = append(r.Agent[:0], r.Agent[n:]...)
		}
		jr.Data = append([]byte(nil), jr.Data...)
		r.Agent = append(r.Agent, jr)
		return nil
	}
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
		if m.GetAction() == "steer" && m.GetAccepted() {
			r.Steers = append(r.Steers, steerOf(&m))
		}
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
	case kindBudget:
		var m sessionpb.BudgetEvent
		if err := proto.Unmarshal(jr.Data, &m); err != nil {
			return err
		}
		switch m.GetEvent() {
		case "alert":
			if r.Budget.Alerted == nil {
				r.Budget.Alerted = map[budget.Kind]float64{}
			}
			r.Budget.Alerted[m.GetKind()] = m.GetLimit()
			r.Steers = append(r.Steers, agent.Steer{CommandID: budget.NoticeID(&m), Note: budget.Notice(&m, currencyOf(r.Config, m.GetKind())), System: true})
		case "reset":
			if r.Budget.Offsets == nil {
				r.Budget.Offsets = map[budget.Kind]float64{}
			}
			r.Budget.Offsets[m.GetKind()] = m.GetOffset()
			delete(r.Budget.Alerted, m.GetKind())
		}
	case kindRevision:
		var m sessionpb.ConfigRevision
		if err := proto.Unmarshal(jr.Data, &m); err != nil {
			return err
		}
		r.Config = m.GetConfig()
	}
	return nil
}

// currencyOf returns the currency of the configuration's budget of a
// kind.
func currencyOf(c *sessionpb.Config, k budget.Kind) string {
	for _, b := range c.GetBudgets() {
		if b.GetKind() == k {
			return b.GetCurrency()
		}
	}
	return ""
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
	// MaxWorkers and MaxToolConcurrency cap what a profile update may set;
	// 0 means no cap.
	MaxWorkers, MaxToolConcurrency uint32
	// OnConfig, if set, is told the configuration after a profile update,
	// once it is recorded.
	OnConfig func(*sessionpb.Config)
	Task     *sessionfs.Task
	// Info describes the run for viewers; it is published at the start.
	Info *gatewayv0.SessionInfo
	// Agent configures agent tasks, and Infer makes their model calls.
	Agent    agent.Settings
	Infer    agent.InferFunc
	CallTool agent.CallToolFunc
	WorkerID string
	Version  string
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
	steerMu  sync.Mutex
	steers   []agent.Steer     // accepted steer notes and budget notices, in order
	config   *sessionpb.Config // the effective configuration, after updates

	budget       *budget.Tracker
	stopTracking context.CancelFunc
	limits       *agent.Limits

	mu         sync.Mutex
	state      gatewayv0.SessionState
	target     gatewayv0.SessionState // PAUSED or STOPPED once requested
	reason     string
	noNewWork  bool
	stopping   chan struct{} // closed when noNewWork is set
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
		config:   p.Config,
		limits:   agent.NewLimits(int(p.Config.GetMaxWorkers()), int(p.Config.GetMaxToolConcurrency()), int(p.Config.GetMaxSubagents())),
		state:    gatewayv0.SessionState_SESSION_STATE_RUNNING,
		done:     make(chan struct{}),
		stopping: make(chan struct{}),
	}
	nextStep := uint64(1)
	if p.Recovered != nil {
		for id, c := range p.Recovered.Commands {
			s.commands[id] = c
		}
		s.steers = p.Recovered.Steers
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
	if p.Info != nil {
		p.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_SessionInfo{SessionInfo: p.Info}})
	}
	s.emitState(gatewayv0.SessionState_SESSION_STATE_UNSPECIFIED, gatewayv0.SessionState_SESSION_STATE_RUNNING, "started")

	s.startBudgets()
	go s.run(nextStep)
	return s, nil
}

// startBudgets starts tracking the budgets, as earlier runs left them.
func (s *Session) startBudgets() {
	var restored budget.Restored
	if s.p.Recovered != nil {
		restored = s.p.Recovered.Budget
	}
	s.budget = budget.New(s.p.Config.GetBudgets(), restored, budget.Hooks{
		Record: func(ev *sessionpb.BudgetEvent) error { return s.record(context.Background(), kindBudget, ev) },
		Emit: func(st *gatewayv0.BudgetStatus) {
			s.p.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_BudgetStatus{BudgetStatus: st}})
		},
		Notify: func(id, text string) {
			s.steerMu.Lock()
			s.steers = append(s.steers, agent.Steer{CommandID: id, Note: text, System: true})
			s.steerMu.Unlock()
		},
		Stop: func(reason string) {
			s.emitText("%s: stopping the session", reason)
			s.requestStop(s.p.Config.GetStopGrace().AsDuration(), false, reason)
		},
		Pause: func(reason string) {
			s.emitText("%s", reason)
			mode, _ := PauseModeFromName(s.p.Config.GetDefaultPauseMode())
			s.requestPause(mode, reason)
		},
		Idle:   func(reason string) { s.emitText("%s", reason) },
		Disk:   func() int64 { return sessionfs.DirSize(s.p.Folder.Dir) },
		Logger: s.log,
	})
	ctx, cancel := context.WithCancel(context.Background())
	s.stopTracking = cancel
	go s.budget.Run(ctx)
}

// Budget returns the session's budget tracker.
func (s *Session) Budget() *budget.Tracker { return s.budget }

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
			s.steerMu.Lock()
			s.steers = append(s.steers, steerOf(rec))
			s.steerMu.Unlock()
			s.p.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Steer{Steer: &gatewayv0.SteerNote{
				Note: rec.Note, Origin: cmd.GetOrigin(), Actor: cmd.GetActor(),
			}}})
		case *gatewayv0.Command_ProfileUpdate:
			if err := s.updateProfile(ctx, rec.GetId(), a.ProfileUpdate); err != nil {
				return nil, err
			}
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
	case *gatewayv0.Command_ProfileUpdate:
		rec.Action = "profile_update"
		if _, err := s.planUpdate(a.ProfileUpdate); err != nil {
			return reject(err.Error())
		}
		rec.Message = "changed " + strings.Join(changed(a.ProfileUpdate), ", ")
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
	case gatewayv0.Origin_ORIGIN_REMOTE:
		return "remote"
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
		s.target, s.reason = gatewayv0.SessionState_SESSION_STATE_PAUSED, reason
		s.stopNewWorkLocked()
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
		s.target, s.reason = gatewayv0.SessionState_SESSION_STATE_STOPPED, reason
		s.stopNewWorkLocked()
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

// steerNotes returns the accepted steer notes so far.
func (s *Session) steerNotes() []agent.Steer {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	return slices.Clone(s.steers)
}

// stopNewWorkLocked stops new work from starting. The caller holds s.mu.
func (s *Session) stopNewWorkLocked() {
	if !s.noNewWork {
		s.noNewWork = true
		close(s.stopping)
	}
}

// wantsStop reports whether new work must not start.
func (s *Session) wantsStop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noNewWork
}

func (s *Session) run(nextStep uint64) {
	o := s.work(nextStep)
	s.stopTracking()
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

// work runs the task until it completes or a pause or stop takes effect.
func (s *Session) work(nextStep uint64) Outcome {
	if s.p.Task.Kind == agent.Kind {
		return s.runAgent()
	}
	return s.runDemo(nextStep)
}

func (s *Session) failed(format string, args ...any) Outcome {
	reason := fmt.Sprintf(format, args...)
	s.emitText("failed: %s", reason)
	return Outcome{State: gatewayv0.SessionState_SESSION_STATE_FAILED, Reason: reason}
}

// stoppedOutcome is the outcome of work that ended for a pause or stop.
func (s *Session) stoppedOutcome() Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.target == gatewayv0.SessionState_SESSION_STATE_UNSPECIFIED {
		return Outcome{State: gatewayv0.SessionState_SESSION_STATE_FAILED, Reason: "work stopped unexpectedly"}
	}
	return Outcome{State: s.target, Reason: s.reason}
}

func (s *Session) runAgent() Outcome {
	if s.p.Infer == nil {
		return s.failed("agent tasks need a gateway that serves models")
	}
	var records []journal.Record
	if s.p.Recovered != nil {
		records = s.p.Recovered.Agent
	}
	done, err := agent.Run(agent.Env{
		Settings: s.p.Agent, Folder: s.p.Folder, Task: s.p.Task, SessionID: s.p.SessionID, Epoch: s.p.Epoch,
		Records: records, Record: s.record, Emit: s.p.Emit, Infer: s.p.Infer, CallTool: s.p.CallTool,
		Soft: s.soft, Hard: s.hard, Stopping: s.stopping, Steers: s.steerNotes, Budget: s.budget, Limits: s.limits, Logger: s.log,
	})
	switch {
	case err != nil:
		return s.failed("%v", err)
	case done:
		return Outcome{State: gatewayv0.SessionState_SESSION_STATE_COMPLETED, Reason: "task completed"}
	}
	return s.stoppedOutcome()
}

// runDemo runs the demo task from nextStep.
func (s *Session) runDemo(nextStep uint64) Outcome {
	failed := s.failed
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
	return s.stoppedOutcome()
}
