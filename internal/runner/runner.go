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
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/actlet/actlet/internal/activity"
	"github.com/actlet/actlet/internal/agent"
	"github.com/actlet/actlet/internal/attach"
	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/session/sessionpb"
	"github.com/actlet/actlet/internal/sessionfs"
	"github.com/actlet/actlet/internal/tools"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
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

	// ToolsManifest is a file in the image describing what is installed,
	// for the agent's prompt. A missing file is no manifest.
	ToolsManifest string
	// Tools overrides the programs the agent's tools use, such as the
	// shell; see tools.Options.
	Tools tools.Options

	// NoAttach turns the local attach socket off, whatever the profile
	// says.
	NoAttach bool
	// Attached is called once the attach socket is listening, with its
	// path, such as to attach the TUI to it.
	Attached func(socket string)
	// AttachListen, if set, opens the box's attach port on this address,
	// host:port, for remote viewers that send AttachToken. It must be a
	// loopback address unless AttachAllowRemote.
	AttachListen      string
	AttachToken       string
	AttachAllowRemote bool

	Logger *slog.Logger
}

// Capabilities are the features this worker announces when registering.
var Capabilities = []string{"control.pause", "control.stop", "control.steer", "activity.v0", "task.demo", "task.agent", "inference.v0", "tools.local", "attach.files"}

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
	agentSettings, err := agentSettings(cfg, resp.GetProfile(), effective)
	if err != nil {
		return fail("agent settings", "err", err)
	}

	// The attach port opens before the session starts, so a port that
	// can't be opened fails the run cleanly.
	var portLis net.Listener
	if cfg.AttachListen != "" {
		if portLis, err = attach.ListenPort(cfg.AttachListen, cfg.AttachAllowRemote); err != nil {
			return fail("attach port", "err", err)
		}
		defer func() { _ = portLis.Close() }()
	}

	hub := attach.NewHub(attach.HubOptions{})
	defer hub.Close()
	pub := activity.New(activity.Options{
		SessionID: sessionID, Epoch: epoch, Dir: folder.State(),
		QueueLen: cfg.ActivityQueue, SpillMax: cfg.ActivitySpillMax, Logger: log,
		Observe: hub.Publish,
	})
	backlogs, err := activity.OpenBacklogs(folder.State(), epoch, log)
	if err != nil {
		log.Warn("activity backlog", "err", err)
	}

	reregister := func(ctx context.Context) error {
		_, err := client.Register(ctx, registerReq())
		return err
	}
	infer := func(ctx context.Context, req *gatewayv0.InferenceRequest) (*gwclient.Inference, error) {
		return client.Infer(ctx, req, reregister)
	}
	commands := attach.AllowedCommands(resp.GetProfile().GetLocalAttach())
	info := sessionInfo(cfg, folder, task, resp.GetProfile(), effective, agentSettings, commands, log)
	sess, err := session.Start(session.Params{
		Folder: folder, SessionID: sessionID, Epoch: epoch, Recovered: recovered,
		Config: effective, MaxStopGrace: cfg.MaxStopGrace, Task: task, Info: info,
		Agent: agentSettings, Infer: infer,
		WorkerID: cfg.WorkerID, Version: cfg.Version, Emit: pub.Emit, Logger: log,
	})
	if err != nil {
		return fail("starting the session", "err", err)
	}

	// Local viewers attach over a socket in the session folder, and remote
	// ones, if the deployment opens it, to the attach port. Closing the
	// hub ends the viewers' feeds after the last events, so the servers
	// stop gracefully.
	as := attach.NewServer(sessionID, hub, &box{sess: sess, sessionID: sessionID, epoch: epoch, cfg: cfg, folder: folder, client: client, started: time.Now()}, commands, log)
	as.ServeFiles(folder.Dir)
	if !cfg.NoAttach && !resp.GetProfile().GetLocalAttach().GetDisabled() {
		lis, err := attach.Listen(folder.State())
		if err != nil {
			log.Warn("local attach unavailable", "err", err)
		} else {
			srv := attach.Start(lis, as)
			defer func() {
				hub.Close()
				srv.Stop()
			}()
			if cfg.Attached != nil {
				cfg.Attached(lis.Path)
			}
		}
	}
	if portLis != nil {
		srv := attach.StartPort(portLis, as.Remote(), cfg.AttachToken)
		defer func() {
			hub.Close()
			srv.Stop()
		}()
		log.Info("attach port listening", "addr", portLis.Addr().String())
	}

	netCtx, cancelNet := context.WithCancel(context.Background())
	defer cancelNet()
	var wg sync.WaitGroup
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		_ = client.RunControl(netCtx, controlHandler{sess, epoch, folder.Dir}, reregister)
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
	model := p.GetModels().GetRoles()["main"]
	if model == "" {
		model = "main"
	}
	var granted []string
	for _, g := range p.GetTools() {
		granted = append(granted, g.GetName())
	}
	subModel := p.GetModels().GetRoles()["subagent"]
	if subModel == "" {
		subModel = model
	}
	orDefault := func(v uint32, def uint32) uint32 {
		if v == 0 {
			return def
		}
		return v
	}
	return &sessionpb.Config{
		Model:                model,
		SubagentModel:        subModel,
		MaxSubagentDepth:     orDefault(p.GetLimits().GetMaxSubagentDepth(), agent.DefaultMaxSubagentDepth),
		MaxSubagents:         orDefault(p.GetLimits().GetMaxSubagents(), agent.DefaultMaxSubagents),
		CheckpointEveryTurns: max(p.GetCheckpointEveryTurns(), 1),
		Tools:                granted,
		ProfileId:            p.GetId(),
		ProfileVersion:       p.GetVersion(),
		MaxWorkers:           clamp(p.GetLimits().GetMaxWorkers(), cfg.MaxWorkers),
		MaxToolConcurrency:   clamp(p.GetLimits().GetMaxToolConcurrency(), cfg.MaxToolConcurrency),
		StopGrace:            durationpb.New(grace),
		DefaultPauseMode:     mode,
		SignalDeadline:       durationpb.New(cfg.SignalDeadline),
		Budgets:              p.GetBudgets(),
		LocalCommands:        attach.AllowedCommands(p.GetLocalAttach()),
	}
}

type controlHandler struct {
	s     *session.Session
	epoch uint64
	dir   string // the session folder
}

func (h controlHandler) Hello() *gatewayv0.ControlHello {
	return &gatewayv0.ControlHello{SessionId: h.s.ID(), Epoch: h.epoch, State: h.s.State()}
}

func (h controlHandler) Apply(ctx context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error) {
	return h.s.Apply(ctx, cmd)
}

func (h controlHandler) State() gatewayv0.SessionState { return h.s.State() }

func (h controlHandler) Done() <-chan struct{} { return h.s.Done() }

// ReadFile serves a file to a viewer attached through the orchestrator,
// by the rules of local attach.
func (h controlHandler) ReadFile(req *gatewayv0.FileRequest) *gatewayv0.FileResponse {
	resp, err := attach.ReadFile(h.dir, &attachv0.ReadFileRequest{Path: req.GetPath()})
	if err != nil {
		st := status.Convert(err)
		return &gatewayv0.FileResponse{ErrorCode: int32(st.Code()), Error: st.Message()}
	}
	return &gatewayv0.FileResponse{Path: resp.GetPath(), Content: resp.GetContent(), Size: resp.GetSize(),
		Truncated: resp.GetTruncated(), Binary: resp.GetBinary()}
}

// agentSettings configures agent tasks from the profile and the image.
func agentSettings(cfg Config, p *gatewayv0.Profile, effective *sessionpb.Config) (agent.Settings, error) {
	manifest, err := agent.ReadManifest(cfg.ToolsManifest)
	if err != nil {
		return agent.Settings{}, fmt.Errorf("tools manifest: %w", err)
	}
	s := agent.Settings{
		Model:              effective.GetModel(),
		SubagentModel:      effective.GetSubagentModel(),
		Selectable:         p.GetModels().GetSelectable(),
		CheckpointEvery:    uint64(effective.GetCheckpointEveryTurns()),
		MaxWorkers:         int(effective.GetMaxWorkers()),
		MaxToolConcurrency: int(effective.GetMaxToolConcurrency()),
		MaxSubagentDepth:   int(effective.GetMaxSubagentDepth()),
		MaxSubagents:       int(effective.GetMaxSubagents()),
		Prompt: agent.Prompt{
			Base: p.GetBasePrompt(), Instructions: p.GetInstructions(),
			Manifests: []string{manifest, p.GetToolsManifest()},
		},
		Tools: cfg.Tools,
	}
	if len(p.GetTools()) > 0 {
		s.Tools.Grants = []tools.Grant{}
		for _, g := range p.GetTools() {
			grant := tools.Grant{Name: g.GetName(), Settings: g.GetSettings()}
			if agent.IsStandard(g.GetName()) {
				s.Standard = append(s.Standard, grant)
			} else {
				s.Tools.Grants = append(s.Tools.Grants, grant)
			}
		}
	}
	return s, nil
}
