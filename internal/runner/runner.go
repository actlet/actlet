// Package runner runs one session for `actlet run`: it recovers the session
// folder, registers with the gateway, runs the session, and keeps control
// and activity flowing until the run ends.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/actlet/actlet/internal/activity"
	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/session/sessionpb"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Config is the deployment configuration of a run: where things are and the
// ceilings the orchestrator's profile must stay within.
type Config struct {
	Gateway    gwclient.Config
	SessionDir string
	WorkerID   string
	Version    string

	// Ceilings and defaults for the profile.
	MaxWorkers         uint32
	MaxToolConcurrency uint32
	DefaultStopGrace   time.Duration
	MaxStopGrace       time.Duration
	// SignalDeadline bounds how long a pause on an OS signal may take. It
	// must fit within the container runtime's stop timeout.
	SignalDeadline time.Duration

	ActivityQueue    int
	ActivitySpillMax int64
	// FlushTimeout bounds how long the run waits at the end for activity to
	// be acknowledged; what's left is kept for the next run.
	FlushTimeout time.Duration

	Logger *slog.Logger
}

// Capabilities are the features this worker announces when registering.
var Capabilities = []string{"control.pause", "control.stop", "control.steer", "activity.v0", "task.demo"}

// Run runs the session and returns the process exit code. Each value
// received on signals asks the session to pause, as an OS termination
// signal does.
func Run(ctx context.Context, cfg Config, signals <-chan os.Signal) int {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
		cfg.Logger = log
	}
	cfg.Gateway.Logger = log
	fail := func(msg string, args ...any) int {
		log.Error(msg, args...)
		return session.ExitFailed
	}

	folder, err := sessionfs.Open(cfg.SessionDir)
	if err != nil {
		return fail("session folder", "err", err)
	}
	recovered, err := session.Recover(folder)
	if err != nil {
		return fail("reading the session journal", "err", err)
	}
	epoch, sessionID := uint64(1), ""
	if recovered != nil {
		if !recovered.Resumable() {
			o := recovered.EndedOutcome()
			log.Info("nothing to resume", "session", recovered.SessionID, "reason", o.Reason)
			return o.ExitCode()
		}
		epoch, sessionID = recovered.Epochs+1, recovered.SessionID
	}

	client, err := gwclient.New(cfg.Gateway)
	if err != nil {
		return fail("gateway", "err", err)
	}
	defer func() { _ = client.Close() }()

	// A signal before the session starts just ends the process: there is
	// nothing running to pause, and the next run starts where this one would
	// have.
	regCtx, cancelReg := context.WithCancel(ctx)
	go func() {
		select {
		case <-signals:
			cancelReg()
		case <-regCtx.Done():
		}
	}()
	registerReq := func() *gatewayv0.RegisterRequest {
		return &gatewayv0.RegisterRequest{
			WorkerId: cfg.WorkerID, SessionId: sessionID, Epoch: epoch,
			Capabilities: Capabilities, ActletVersion: cfg.Version,
		}
	}
	resp, err := client.Register(regCtx, registerReq())
	cancelReg()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Info("stopped before the session started")
			return session.ExitPaused
		}
		return fail("registering with the gateway", "err", err)
	}
	if sessionID != "" && resp.GetSessionId() != sessionID {
		return fail("the gateway assigned a different session to this folder", "folder", sessionID, "gateway", resp.GetSessionId())
	}
	sessionID = resp.GetSessionId()
	if missing := missingFeatures(resp.GetProfile().GetRequiredFeatures()); len(missing) > 0 {
		return fail("the profile requires features this worker doesn't support", "missing", missing)
	}
	log.Info("registered", "session", sessionID, "epoch", epoch, "profile", resp.GetProfile().GetId())

	task, err := loadTask(folder, resp.GetTask())
	if err != nil {
		return fail("task", "err", err)
	}
	effective := effectiveConfig(cfg, resp.GetProfile())

	pub := activity.New(activity.Options{
		SessionID: sessionID, Epoch: epoch, Dir: folder.State(),
		QueueLen: cfg.ActivityQueue, SpillMax: cfg.ActivitySpillMax, Logger: log,
	})
	backlogs, err := activity.OpenBacklogs(folder.State(), epoch, log)
	if err != nil {
		log.Warn("activity backlog", "err", err)
	}

	sess, err := session.Start(session.Params{
		Folder: folder, SessionID: sessionID, Epoch: epoch, Recovered: recovered,
		Config: effective, MaxStopGrace: cfg.MaxStopGrace, Task: task,
		WorkerID: cfg.WorkerID, Version: cfg.Version, Emit: pub.Emit, Logger: log,
	})
	if err != nil {
		return fail("starting the session", "err", err)
	}

	reregister := func(ctx context.Context) error {
		_, err := client.Register(ctx, registerReq())
		return err
	}
	netCtx, cancelNet := context.WithCancel(context.Background())
	defer cancelNet()
	var wg sync.WaitGroup
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		_ = client.RunControl(netCtx, controlHandler{sess, epoch}, reregister)
	}()
	for _, src := range append(backlogs, pub) {
		wg.Go(func() { _ = client.RunActivity(netCtx, sessionID, src, reregister) })
	}

	// Run until the session ends; signals pause it.
	for done := false; !done; {
		select {
		case <-sess.Done():
			done = true
		case sig := <-signals:
			log.Info("signal: pausing", "signal", sig)
			sess.Signal()
		case <-ctx.Done():
			log.Info("shutting down: pausing")
			sess.Signal()
			ctx = context.Background() // signal once
		}
	}
	outcome := sess.Outcome()
	log.Info("session ended", "state", outcome.State, "reason", outcome.Reason, "exit", outcome.ExitCode())

	// Deliver what we can, keep the rest for the next run.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), cfg.FlushTimeout)
	if err := pub.Close(flushCtx); err != nil {
		log.Warn("saving unsent activity", "err", err)
	}
	for _, b := range backlogs {
		for !b.Done() && flushCtx.Err() == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	select {
	case <-controlDone:
	case <-flushCtx.Done():
	}
	cancelFlush()
	cancelNet()
	wg.Wait()
	<-controlDone
	if err := sess.Close(); err != nil {
		log.Error("closing the session journal", "err", err)
	}
	return outcome.ExitCode()
}

// missingFeatures returns the required features this worker lacks.
func missingFeatures(required []string) []string {
	var missing []string
	for _, f := range required {
		if !slices.Contains(Capabilities, f) {
			missing = append(missing, f)
		}
	}
	return missing
}

// loadTask uses task.json if the folder has one, or else the gateway's task,
// which it writes to task.json.
func loadTask(folder sessionfs.Folder, fromGateway *gatewayv0.Task) (*sessionfs.Task, error) {
	task, err := folder.ReadTask()
	if err == nil {
		return task, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if fromGateway == nil {
		return nil, errors.New("no task.json in the session folder and no task from the gateway")
	}
	task = &sessionfs.Task{
		Schema: fromGateway.GetSchema(), ID: fromGateway.GetId(), Kind: fromGateway.GetKind(),
		Definition: json.RawMessage(fromGateway.GetDefinition()), Input: json.RawMessage(fromGateway.GetInput()),
	}
	if task.Schema == "" {
		task.Schema = sessionfs.TaskSchema
	}
	if err := folder.WriteTask(task); err != nil {
		return nil, fmt.Errorf("task from the gateway: %w", err)
	}
	return task, nil
}

// effectiveConfig applies the profile within the deployment's ceilings.
func effectiveConfig(cfg Config, p *gatewayv0.Profile) *sessionpb.Config {
	clamp := func(v, ceiling uint32) uint32 {
		if v == 0 || (ceiling > 0 && v > ceiling) {
			return ceiling
		}
		return v
	}
	grace := cfg.DefaultStopGrace
	if g := p.GetStopGrace(); g != nil && g.AsDuration() > 0 {
		grace = g.AsDuration()
	}
	if cfg.MaxStopGrace > 0 && grace > cfg.MaxStopGrace {
		grace = cfg.MaxStopGrace
	}
	mode := "drain"
	if p.GetDefaultPauseMode() == gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT {
		mode = "interrupt"
	}
	return &sessionpb.Config{
		ProfileId:          p.GetId(),
		ProfileVersion:     p.GetVersion(),
		MaxWorkers:         clamp(p.GetLimits().GetMaxWorkers(), cfg.MaxWorkers),
		MaxToolConcurrency: clamp(p.GetLimits().GetMaxToolConcurrency(), cfg.MaxToolConcurrency),
		StopGrace:          durationpb.New(grace),
		DefaultPauseMode:   mode,
		SignalDeadline:     durationpb.New(cfg.SignalDeadline),
	}
}

type controlHandler struct {
	s     *session.Session
	epoch uint64
}

func (h controlHandler) Hello() *gatewayv0.ControlHello {
	return &gatewayv0.ControlHello{SessionId: h.s.ID(), Epoch: h.epoch, State: h.s.State()}
}

func (h controlHandler) Apply(ctx context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error) {
	return h.s.Apply(ctx, cmd)
}

func (h controlHandler) State() gatewayv0.SessionState { return h.s.State() }

func (h controlHandler) Done() <-chan struct{} { return h.s.Done() }
