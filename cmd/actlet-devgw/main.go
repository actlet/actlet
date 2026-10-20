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
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
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
  help                       show this help
  quit                       stop the gateway

Run a worker against it:

  actlet-devgw -token dev
  ACTLET_GATEWAY=127.0.0.1:7400 ACTLET_TOKEN=dev actlet run -session ./my-session

Flags:
`

type config struct {
	listen     string
	token      string
	dataDir    string
	taskFile   string
	profileID  string
	pauseMode  string
	stopGrace  time.Duration
	demoDef    string
	demoInput  string
	cmdTimeout time.Duration
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
	fs.StringVar(&cfg.token, "token", envDefault("ACTLET_DEVGW_TOKEN", ""), "bearer token workers must send; empty accepts any [ACTLET_DEVGW_TOKEN]")
	fs.StringVar(&cfg.dataDir, "data", envDefault("ACTLET_DEVGW_DATA", "devgw-data"), "directory for activity recordings [ACTLET_DEVGW_DATA]")
	fs.StringVar(&cfg.taskFile, "task", "", "task file to assign, in the task.json format (default: a demo task)")
	fs.StringVar(&cfg.demoDef, "demo", `{"steps":5,"step":"2s","ticks":2}`, "definition of the default demo task")
	fs.StringVar(&cfg.demoInput, "demo-input", `{"message":"hello from actlet-devgw"}`, "input of the default demo task")
	fs.StringVar(&cfg.profileID, "profile", "dev", "profile ID to assign")
	fs.StringVar(&cfg.pauseMode, "pause-mode", "drain", "profile's default pause mode on SIGTERM: drain or interrupt")
	fs.DurationVar(&cfg.stopGrace, "stop-grace", 10*time.Second, "profile's default stop grace period")
	fs.DurationVar(&cfg.cmdTimeout, "command-timeout", time.Minute, "how long a command waits for the worker's acknowledgement")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	gw, err := newGateway(cfg, stdout, log)
	if err != nil {
		fmt.Fprintf(stderr, "actlet-devgw: %v\n", err)
		return 1
	}
	defer func() { _ = gw.store.Close() }()

	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		fmt.Fprintf(stderr, "actlet-devgw: %v\n", err)
		return 1
	}
	gs := grpc.NewServer(sdk.ServerOptions()...)
	gw.srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	log.Info("gateway listening", "addr", lis.Addr().String(), "data", cfg.dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		gw.admin(ctx, stdin)
		stop()
	}()
	<-ctx.Done()
	log.Info("shutting down")
	gs.GracefulStop()
	gw.wait()
	return 0
}

type gateway struct {
	cfg   config
	out   io.Writer
	log   *slog.Logger
	srv   *sdk.Server
	store *sdk.RecordingStore
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
	mode := gatewayv0.PauseMode_PAUSE_MODE_DRAIN
	switch cfg.pauseMode {
	case "drain":
	case "interrupt":
		mode = gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT
	default:
		return nil, fmt.Errorf("pause mode %q: use drain or interrupt", cfg.pauseMode)
	}
	store, err := sdk.NewRecordingStore(cfg.dataDir)
	if err != nil {
		return nil, err
	}
	g := &gateway{cfg: cfg, out: out, log: log, store: store, task: task}
	opts := sdk.Options{
		Store:  store,
		Logger: log,
		Assign: func(_ context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			id := req.GetSessionId()
			if id == "" {
				id = sdk.NewID()
			}
			g.mu.Lock()
			g.current = id
			g.mu.Unlock()
			return &gatewayv0.RegisterResponse{
				SessionId: id,
				Profile: &gatewayv0.Profile{
					Id: cfg.profileID, Version: "1",
					DefaultPauseMode: mode,
					StopGrace:        durationpb.New(cfg.stopGrace),
				},
				Task: task,
			}, nil
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
	g.srv, err = sdk.NewServer(opts)
	if err != nil {
		return nil, err
	}
	return g, nil
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
	default:
		what = fmt.Sprintf("%T", ev.Body)
	}
	g.printf("[%s %s] %s", short(session), ev.GetId(), what)
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

// admin reads commands from in until it ends or "quit".
func (g *gateway) admin(ctx context.Context, in io.Reader) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "quit" || fields[0] == "exit" {
			return
		}
		g.handle(ctx, fields)
	}
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
	default:
		g.printf("unknown command %q; try help", f[0])
	}
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
		ack, err := do(ctx, s)
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
		g.printf("%s %s: %s%s%s, session %s", short(id), what, result, dup, msg, stateName(ack.GetState()))
	}()
}
