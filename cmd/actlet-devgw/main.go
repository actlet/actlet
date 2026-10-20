// Command actlet-devgw is a development orchestrator for Actlet: a gateway
// that assigns one task to the workers that connect, records their activity
// and takes control commands on standard input.
//
// It is built only on the public gateway SDK (package sdk) and is meant for
// local development and testing, not production.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/actlet/actlet/internal/buildinfo"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/scripted"
)

const usage = `Usage: actlet-devgw [flags]

A development gateway for Actlet workers. It assigns one task to every new
session, records their activity under the data directory, and takes control
commands on standard input:

  sessions                   list sessions
  use <session>              choose the session commands go to (default: the latest)
  pause drain|interrupt      pause the session; run actlet again to resume it
  stop [<grace>|kill]        stop the session, e.g. "stop 5s" or "stop kill"
  steer <note>               send an operator note
  profile-update <file>      change the profile mid-session: a YAML or JSON
                             file of the fields to change, such as budgets,
                             and reset_budgets: [money]
  store-delay <duration>     slow down storing activity, to try a slow consumer (0: off)
  help                       show this help
  quit                       stop the gateway

Run a worker against it:

  actlet-devgw -token dev
  ACTLET_GATEWAY=127.0.0.1:7400 ACTLET_TOKEN=dev actlet run -session ./my-session

Viewers attach to its sessions with the same token:

  ACTLET_ATTACH_TOKEN=dev actlet-cli attach -gateway 127.0.0.1:7400

Flags:
`

type config struct {
	listen       string
	token        string
	dataDir      string
	taskFile     string
	profile      string
	profileID    string
	pauseMode    string
	stopGrace    time.Duration
	demoDef      string
	demoInput    string
	cmdTimeout   time.Duration
	drainTimeout time.Duration
	heartbeat    time.Duration
	modelScript  string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func envDefault(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var cfg config
	fs := flag.NewFlagSet("actlet-devgw", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&cfg.listen, "listen", envDefault("ACTLET_DEVGW_LISTEN", "127.0.0.1:7400"), "address to listen on [ACTLET_DEVGW_LISTEN]")
	fs.StringVar(&cfg.token, "token", envDefault("ACTLET_DEVGW_TOKEN", ""), "bearer token workers and viewers must send; empty accepts any [ACTLET_DEVGW_TOKEN]")
	fs.StringVar(&cfg.dataDir, "data", envDefault("ACTLET_DEVGW_DATA", "devgw-data"), "directory for activity recordings [ACTLET_DEVGW_DATA]")
	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "print version information and exit")
	fs.StringVar(&cfg.taskFile, "task", "", "task file to assign, in the task.json format (default: a demo task)")
	fs.StringVar(&cfg.demoDef, "demo", `{"steps":5,"step":"2s","ticks":2}`, "definition of the default demo task")
	fs.StringVar(&cfg.demoInput, "demo-input", `{"message":"hello from actlet-devgw"}`, "input of the default demo task")
	fs.StringVar(&cfg.modelScript, "model", "", "serve model calls from this script of turns, YAML or JSON (default: no model)")
	fs.StringVar(&cfg.profile, "profile", "", "profile file to assign, JSON or YAML (default: built from the flags below)")
	fs.StringVar(&cfg.profileID, "profile-id", "dev", "ID of the default profile")
	fs.StringVar(&cfg.pauseMode, "pause-mode", "drain", "default profile's pause mode on SIGTERM: drain or interrupt")
	fs.DurationVar(&cfg.stopGrace, "stop-grace", 10*time.Second, "default profile's stop grace period")
	fs.DurationVar(&cfg.cmdTimeout, "command-timeout", time.Minute, "how long a command waits for the worker's acknowledgement")
	fs.DurationVar(&cfg.drainTimeout, "drain-timeout", 10*time.Second, "how long draining may take on SIGTERM")
	fs.DurationVar(&cfg.heartbeat, "heartbeat", sdk.DefaultHeartbeat, "how often to send a heartbeat on idle control streams; 0 turns them off")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if showVersion {
		return buildinfo.RunVersion("actlet-devgw", nil, stdout, stderr)
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	gw, err := newGateway(cfg, stdout, log)
	if err != nil {
		fmt.Fprintf(stderr, "actlet-devgw: %v\n", err)
		return 1
	}
	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		fmt.Fprintf(stderr, "actlet-devgw: %v\n", err)
		return 1
	}
	gs := grpc.NewServer(sdk.ServerOptions()...)
	gw.srv.RegisterService(gs)
	gw.srv.RegisterAttach(gs)
	go func() { _ = gs.Serve(lis) }()
	log.Info("gateway listening", "addr", lis.Addr().String(), "data", cfg.dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		if gw.admin(ctx, stdin) {
			stop() // "quit"
			return
		}
		// Without a terminal, as in a container, standard input ends at
		// once: keep serving until a signal arrives.
		log.Info("standard input closed: admin commands unavailable; stop with SIGTERM or Ctrl-C")
	}()
	<-ctx.Done()
	log.Info("draining: workers will reconnect elsewhere")
	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.drainTimeout)
	defer cancel()
	if err := gw.srv.Drain(drainCtx, gs); err != nil {
		log.Warn("drain did not finish in time; stopped", "err", err)
	}
	gw.wait()
	// Release the recordings so that another replica can take them over.
	if err := gw.store.Close(); err != nil {
		log.Warn("closing recordings", "err", err)
	}
	log.Info("stopped")
	return 0
}

type gateway struct {
	cfg   config
	out   io.Writer
	log   *slog.Logger
	srv   *sdk.Server
	store *sdk.RecordingStore
	delay *delayStore
	task  *gatewayv0.Task

	mu      sync.Mutex
	current string
	pending sync.WaitGroup
}

func newGateway(cfg config, out io.Writer, log *slog.Logger) (*gateway, error) {
	task, err := loadTask(cfg)
	if err != nil {
		return nil, err
	}
	profile, err := loadProfile(cfg)
	if err != nil {
		return nil, err
	}
	store, err := sdk.NewRecordingStore(cfg.dataDir)
	if err != nil {
		return nil, err
	}
	g := &gateway{cfg: cfg, out: out, log: log, store: store, delay: &delayStore{RecordingStore: store}, task: task}
	heartbeat := cfg.heartbeat
	if heartbeat == 0 {
		heartbeat = -1 // off
	}
	opts := sdk.Options{
		Store:     g.delay,
		Logger:    log,
		Heartbeat: heartbeat,
		Assign: func(_ context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			id := req.GetSessionId()
			if id == "" {
				id = sdk.NewID()
			}
			g.mu.Lock()
			g.current = id
			g.mu.Unlock()
			return &gatewayv0.RegisterResponse{SessionId: id, Profile: profile, Task: task}, nil
		},
		OnActivity: g.printEvent,
		OnControl: func(s *sdk.Session, connected bool) {
			if connected {
				log.Info("worker connected", "session", s.ID(), "worker", s.WorkerID(), "epoch", s.Epoch())
			} else {
				st, _ := s.State()
				log.Info("worker disconnected", "session", s.ID(), "state", stateName(st))
			}
		},
	}
	if cfg.token != "" {
		opts.Authenticate = sdk.StaticToken(cfg.token)
	}
	if cfg.modelScript != "" {
		model, err := scripted.Load(cfg.modelScript)
		if err != nil {
			return nil, err
		}
		opts.OnInference = func(ctx context.Context, call *sdk.InferenceCall) error {
			req := call.Request
			g.printf("[%s] model call %s (%s, %d messages)", short(req.GetSessionId()), short(req.GetIntentId()), req.GetModel(), len(req.GetMessages()))
			err := model.Serve(ctx, call)
			if err != nil {
				g.printf("[%s] model call %s failed: %v", short(req.GetSessionId()), short(req.GetIntentId()), err)
			}
			return err
		}
		opts.OnDecide = func(ctx context.Context, d *sdk.DecideRequest) (*sdk.Decision, error) {
			g.printf("[%s] %s asks to decide: %s", short(d.Session.ID()), d.Worker, oneLine(d.Question, 120))
			dec, err := model.Decide(ctx, d)
			if err == nil {
				g.printf("[%s] decided: %s", short(d.Session.ID()), dec.Decision)
			}
			return dec, err
		}
	}
	g.srv, err = sdk.NewServer(opts)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// loadProfile reads the profile file, or builds the default profile from
// the flags.
func loadProfile(cfg config) (*gatewayv0.Profile, error) {
	p := &sdk.Profile{
		Schema: sdk.ProfileSchema, ID: cfg.profileID, Version: "1",
		DefaultPauseMode: cfg.pauseMode, StopGrace: sdk.Duration(cfg.stopGrace),
	}
	if cfg.profile != "" {
		var err error
		if p, err = sdk.LoadProfile(cfg.profile); err != nil {
			return nil, err
		}
	}
	return p.Proto()
}

// loadTask reads the task file, or builds the demo task.
func loadTask(cfg config) (*gatewayv0.Task, error) {
	if cfg.taskFile == "" {
		return &gatewayv0.Task{Schema: "v0", Id: "demo-" + time.Now().UTC().Format("20060102-150405"), Kind: "demo",
			Definition: []byte(cfg.demoDef), Input: []byte(cfg.demoInput)}, nil
	}
	data, err := os.ReadFile(cfg.taskFile)
	if err != nil {
		return nil, err
	}
	var t struct {
		Schema     string          `json:"schema"`
		ID         string          `json:"id"`
		Kind       string          `json:"kind"`
		Definition json.RawMessage `json:"definition"`
		Input      json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("task file %s: %w", cfg.taskFile, err)
	}
	return &gatewayv0.Task{Schema: t.Schema, Id: t.ID, Kind: t.Kind, Definition: t.Definition, Input: t.Input}, nil
}

func (g *gateway) wait() { g.pending.Wait() }

func (g *gateway) printf(format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fmt.Fprintf(g.out, format+"\n", args...)
}

// printEvent prints one line per activity event.
func (g *gateway) printEvent(session string, ev *gatewayv0.ActivityEvent) {
	var what string
	switch b := ev.Body.(type) {
	case *gatewayv0.ActivityEvent_Started:
		what = fmt.Sprintf("started (epoch %d, resumed %v)", b.Started.GetEpoch(), b.Started.GetResumed())
	case *gatewayv0.ActivityEvent_State:
		what = fmt.Sprintf("state %s -> %s (%s)", stateName(b.State.GetFrom()), stateName(b.State.GetTo()), b.State.GetReason())
	case *gatewayv0.ActivityEvent_Command:
		what = fmt.Sprintf("command %s: %s", b.Command.GetCommand().GetId(), strings.TrimPrefix(b.Command.GetResult().String(), "COMMAND_RESULT_"))
		if m := b.Command.GetMessage(); m != "" {
			what += " (" + m + ")"
		}
	case *gatewayv0.ActivityEvent_Steer:
		what = "steer: " + b.Steer.GetNote()
	case *gatewayv0.ActivityEvent_Text:
		what = b.Text.GetText()
	case *gatewayv0.ActivityEvent_Gap:
		what = fmt.Sprintf("gap: %d events dropped by the worker", b.Gap.GetDropped())
	case *gatewayv0.ActivityEvent_ModelTurn:
		m := b.ModelTurn
		what = fmt.Sprintf("model turn %d (%s, %s): %s", m.GetTurn(), m.GetModel(),
			strings.ToLower(strings.TrimPrefix(m.GetStopReason().String(), "STOP_REASON_")), oneLine(m.GetText(), 120))
		if len(m.GetToolCalls()) > 0 {
			what += " -> " + strings.Join(m.GetToolCalls(), ", ")
		}
	case *gatewayv0.ActivityEvent_ToolStarted:
		what = fmt.Sprintf("tool %s %s: %s", b.ToolStarted.GetTool(), b.ToolStarted.GetCallId(), oneLine(b.ToolStarted.GetInput(), 120))
	case *gatewayv0.ActivityEvent_ToolFinished:
		f := b.ToolFinished
		result := "ok"
		switch {
		case f.GetOutcomeUnknown():
			result = "OUTCOME UNKNOWN"
		case f.GetIsError():
			result = "error"
		}
		what = fmt.Sprintf("tool %s %s %s in %v: %s", f.GetTool(), f.GetCallId(), result, f.GetTook().AsDuration().Round(time.Millisecond), oneLine(f.GetOutput(), 120))
	case *gatewayv0.ActivityEvent_SubagentSpawned:
		what = fmt.Sprintf("subagent started by %s (%s): %s", b.SubagentSpawned.GetParent(), b.SubagentSpawned.GetModel(), oneLine(b.SubagentSpawned.GetTask(), 120))
	case *gatewayv0.ActivityEvent_SubagentFinished:
		f := b.SubagentFinished
		result := "finished"
		if f.GetIsError() {
			result = "failed"
		}
		what = fmt.Sprintf("subagent %s after %d turns: %s", result, f.GetTurns(), oneLine(f.GetResult(), 120))
	case *gatewayv0.ActivityEvent_FileChange:
		c := b.FileChange
		verb := "changed"
		if c.GetCreated() {
			verb = "created"
		}
		what = fmt.Sprintf("file %s %s\n%s", verb, c.GetPath(), strings.TrimRight(c.GetDiff(), "\n"))
	default:
		what = fmt.Sprintf("%T", ev.Body)
	}
	if w := ev.GetWorker(); w != "" {
		what = w + ": " + what
	}
	g.printf("[%s %s] %s", short(session), ev.GetId(), what)
}

// oneLine shortens s to one line of at most n bytes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func stateName(s gatewayv0.SessionState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "SESSION_STATE_"))
}

// admin reads commands from in until it ends or "quit". It reports
// whether "quit" was given.
func (g *gateway) admin(ctx context.Context, in io.Reader) bool {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "quit" || fields[0] == "exit" {
			return true
		}
		g.handle(ctx, fields)
	}
	return false
}

func (g *gateway) handle(ctx context.Context, f []string) {
	switch f[0] {
	case "help":
		g.printf("%s", strings.SplitN(usage, "\nRun a worker", 2)[0])
	case "sessions":
		sessions := g.srv.Sessions()
		if len(sessions) == 0 {
			g.printf("no sessions")
		}
		for _, s := range sessions {
			st, _ := s.State()
			g.printf("%s  worker %s  epoch %d  %s  connected %v  recording %s",
				s.ID(), s.WorkerID(), s.Epoch(), stateName(st), s.Connected(), g.store.Path(s.ID()))
		}
	case "use":
		if len(f) != 2 || g.srv.Session(f[1]) == nil {
			g.printf("usage: use <session>; see sessions")
			return
		}
		g.mu.Lock()
		g.current = f[1]
		g.mu.Unlock()
	case "pause":
		mode := map[string]gatewayv0.PauseMode{"drain": gatewayv0.PauseMode_PAUSE_MODE_DRAIN, "interrupt": gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT}
		if len(f) != 2 || mode[f[1]] == gatewayv0.PauseMode_PAUSE_MODE_UNSPECIFIED {
			g.printf("usage: pause drain|interrupt")
			return
		}
		g.send(ctx, "pause "+f[1], func(ctx context.Context, s *sdk.Session) (*gatewayv0.CommandAck, error) {
			return s.Pause(ctx, mode[f[1]])
		})
	case "stop":
		opts := sdk.StopOptions{}
		if len(f) == 2 {
			if f[1] == "kill" {
				opts.Kill = true
			} else if d, err := time.ParseDuration(f[1]); err == nil && d > 0 {
				opts.Grace = d
			} else {
				g.printf("usage: stop [<grace>|kill]")
				return
			}
		} else if len(f) > 2 {
			g.printf("usage: stop [<grace>|kill]")
			return
		}
		g.send(ctx, strings.Join(f, " "), func(ctx context.Context, s *sdk.Session) (*gatewayv0.CommandAck, error) {
			return s.Stop(ctx, opts)
		})
	case "steer":
		if len(f) < 2 {
			g.printf("usage: steer <note>")
			return
		}
		note := strings.Join(f[1:], " ")
		g.send(ctx, "steer", func(ctx context.Context, s *sdk.Session) (*gatewayv0.CommandAck, error) {
			return s.Steer(ctx, note)
		})
	case "profile-update":
		if len(f) != 2 {
			g.printf("usage: profile-update <file>")
			return
		}
		u, err := sdk.LoadProfileUpdate(f[1])
		if err != nil {
			g.printf("%v", err)
			return
		}
		g.send(ctx, "profile-update "+f[1], func(ctx context.Context, s *sdk.Session) (*gatewayv0.CommandAck, error) {
			return s.UpdateProfile(ctx, u)
		})
	case "store-delay":
		d, err := time.ParseDuration(strings.Join(f[1:], ""))
		if len(f) != 2 || err != nil || d < 0 {
			g.printf("usage: store-delay <duration>, e.g. store-delay 500ms; 0 turns it off")
			return
		}
		g.delay.set(d)
		g.printf("activity store delay: %v", d)
	default:
		g.printf("unknown command %q; try help", f[0])
	}
}

// delayStore slows down storing activity, to try out a slow consumer.
type delayStore struct {
	*sdk.RecordingStore
	delay atomic.Int64
}

func (s *delayStore) set(d time.Duration) { s.delay.Store(int64(d)) }

func (s *delayStore) Append(ctx context.Context, id string, evs []*gatewayv0.ActivityEvent) error {
	if d := time.Duration(s.delay.Load()); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.RecordingStore.Append(ctx, id, evs)
}

// send runs a command against the current session in the background and
// prints the acknowledgement.
func (g *gateway) send(ctx context.Context, what string, do func(context.Context, *sdk.Session) (*gatewayv0.CommandAck, error)) {
	g.mu.Lock()
	id := g.current
	g.mu.Unlock()
	s := g.srv.Session(id)
	if s == nil {
		g.printf("no session yet: start a worker first")
		return
	}
	if !s.Connected() {
		g.printf("%s: worker not connected; the command waits until it is", short(id))
	}
	g.pending.Add(1)
	go func() {
		defer g.pending.Done()
		ctx, cancel := context.WithTimeout(ctx, g.cfg.cmdTimeout)
		defer cancel()
		start := time.Now()
		ack, err := do(ctx, s)
		took := time.Since(start).Round(time.Millisecond)
		if err != nil {
			g.printf("%s %s: %v", short(id), what, err)
			return
		}
		result := strings.ToLower(strings.TrimPrefix(ack.GetResult().String(), "COMMAND_RESULT_"))
		msg := ""
		if ack.GetMessage() != "" {
			msg = ": " + ack.GetMessage()
		}
		dup := ""
		if ack.GetDuplicate() {
			dup = " (duplicate)"
		}
		g.printf("%s %s: %s%s%s, session %s (in %v)", short(id), what, result, dup, msg, stateName(ack.GetState()), took)
	}()
}
