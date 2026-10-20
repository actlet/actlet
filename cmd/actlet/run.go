package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/actlet/actlet/internal/attach"
	"github.com/actlet/actlet/internal/buildinfo"
	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/runner"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	"github.com/actlet/actlet/internal/tui"
)

const runUsage = `Usage: actlet run [flags]

Run the session in a session folder: connect to the gateway, register,
and run the task until it completes, or until it is paused or stopped.
Starting it again on the same folder resumes a paused session.

Every flag can also be set with the environment variable in brackets.

Exit codes:
  0  the task completed
  1  the run failed
  2  usage error
  3  the session paused (SIGTERM, or a pause command); run again to resume
  4  the session was stopped; it won't resume

Flags:
`

// envDefault returns the environment variable's value, or def.
func envDefault(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func runRun(args []string, stdout, stderr io.Writer, signals <-chan os.Signal) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, runUsage)
		fs.PrintDefaults()
	}

	hostname, _ := os.Hostname()
	var (
		dir       = fs.String("session", envDefault("ACTLET_SESSION", "."), "session folder [ACTLET_SESSION]")
		addr      = fs.String("gateway", envDefault("ACTLET_GATEWAY", ""), "gateway address, host:port (required) [ACTLET_GATEWAY]")
		token     = fs.String("token", envDefault("ACTLET_TOKEN", ""), "bearer token for the gateway [ACTLET_TOKEN]")
		useTLS    = fs.Bool("tls", envDefault("ACTLET_TLS", "") == "true", "connect with TLS [ACTLET_TLS=true]")
		caFile    = fs.String("tls-ca", envDefault("ACTLET_TLS_CA", ""), "PEM file of CA certificates to trust for TLS [ACTLET_TLS_CA]")
		workerID  = fs.String("worker-id", envDefault("ACTLET_WORKER_ID", hostname), "worker identity [ACTLET_WORKER_ID]")
		logLevel  = fs.String("log-level", envDefault("ACTLET_LOG_LEVEL", "info"), "log level: debug, info, warn or error [ACTLET_LOG_LEVEL]")
		logFormat = fs.String("log-format", envDefault("ACTLET_LOG_FORMAT", "text"), "log format: text or json [ACTLET_LOG_FORMAT]")
		withTUI   = fs.Bool("tui", false, "show the session in a terminal viewer while it runs; logs go to .actlet/run.log")
		noAttach  = fs.Bool("no-attach", envDefault("ACTLET_NO_ATTACH", "") == "true", "don't let local viewers attach [ACTLET_NO_ATTACH=true]")
		attachOn  = fs.String("attach-listen", envDefault("ACTLET_ATTACH_LISTEN", ""),
			"open an attach port for remote viewers on this loopback address, host:port; they must send ACTLET_ATTACH_TOKEN (default: off) [ACTLET_ATTACH_LISTEN]")
		attachAny = fs.Bool("attach-allow-remote", envDefault("ACTLET_ATTACH_ALLOW_REMOTE", "") == "true",
			"let the attach port listen on an address other than loopback [ACTLET_ATTACH_ALLOW_REMOTE=true]")
		manifest = fs.String("tools-manifest", envDefault("ACTLET_TOOLS_MANIFEST", "/etc/actlet/tools.md"),
			"file describing what is installed, for the agent's prompt; ignored if missing [ACTLET_TOOLS_MANIFEST]")
		skillsDir = fs.String("skills", envDefault("ACTLET_SKILLS", "/etc/actlet/skills"),
			"directory of skill folders, each with a SKILL.md, for the agent; ignored if missing [ACTLET_SKILLS]")
	)
	durationFlag := func(name, env string, def time.Duration, usage string) *time.Duration {
		d := def
		if v, ok := os.LookupEnv(env); ok {
			if p, err := time.ParseDuration(v); err == nil {
				d = p
			}
		}
		fs.DurationVar(&d, name, d, usage+" ["+env+"]")
		return &d
	}
	intFlag := func(name, env string, def int64, usage string) *int64 {
		n := def
		if v, ok := os.LookupEnv(env); ok {
			if p, err := strconv.ParseInt(v, 10, 64); err == nil {
				n = p
			}
		}
		fs.Int64Var(&n, name, n, usage+" ["+env+"]")
		return &n
	}
	stopGrace := durationFlag("stop-grace", "ACTLET_STOP_GRACE", 30*time.Second, "grace period of a stop when the profile sets none")
	maxStopGrace := durationFlag("max-stop-grace", "ACTLET_MAX_STOP_GRACE", 5*time.Minute, "longest grace period a stop may ask for")
	signalDeadline := durationFlag("signal-deadline", "ACTLET_SIGNAL_DEADLINE", 8*time.Second,
		"how long pausing on SIGTERM may take; keep it within the container stop timeout")
	flushTimeout := durationFlag("flush-timeout", "ACTLET_FLUSH_TIMEOUT", 2*time.Second, "how long to wait at exit for activity to be delivered")
	maxWorkers := intFlag("max-workers", "ACTLET_MAX_WORKERS", 4, "most workers a profile may use")
	maxTools := intFlag("max-tool-concurrency", "ACTLET_MAX_TOOL_CONCURRENCY", 4, "most concurrent tool calls a profile may use")
	queue := intFlag("activity-queue", "ACTLET_ACTIVITY_QUEUE", 1024, "activity events held in memory before spilling to disk")
	spillMax := intFlag("activity-spill-max", "ACTLET_ACTIVITY_SPILL_MAX", 64<<20, "largest activity spill file, in bytes")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return session.ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "actlet run: unexpected argument %q\n", fs.Arg(0))
		return session.ExitUsage
	}
	if *addr == "" {
		fmt.Fprintln(stderr, "actlet run: a gateway address is required: -gateway or ACTLET_GATEWAY")
		return session.ExitUsage
	}
	attachToken := os.Getenv("ACTLET_ATTACH_TOKEN")
	if *attachOn != "" && attachToken == "" {
		fmt.Fprintln(stderr, "actlet run: -attach-listen needs a token in ACTLET_ATTACH_TOKEN")
		return session.ExitUsage
	}
	logOut := stderr
	if *withTUI {
		if *noAttach {
			fmt.Fprintln(stderr, "actlet run: -tui needs local attach; drop -no-attach")
			return session.ExitUsage
		}
		if err := tui.CheckTerminal(); err != nil {
			fmt.Fprintf(stderr, "actlet run: -tui: %v\n", err)
			return session.ExitUsage
		}
		folder, err := sessionfs.Open(*dir)
		if err != nil {
			fmt.Fprintf(stderr, "actlet run: %v\n", err)
			return session.ExitFailed
		}
		f, err := os.OpenFile(filepath.Join(folder.State(), "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "actlet run: %v\n", err)
			return session.ExitFailed
		}
		defer func() { _ = f.Close() }()
		logOut = f
	}
	log, err := newLogger(logOut, *logLevel, *logFormat)
	if err != nil {
		fmt.Fprintf(stderr, "actlet run: %v\n", err)
		return session.ExitUsage
	}

	cfg := runner.Config{
		Gateway:            gwclient.Config{Addr: *addr, Token: *token, TLS: *useTLS, CAFile: *caFile},
		SessionDir:         *dir,
		WorkerID:           *workerID,
		Version:            buildinfo.Version,
		MaxWorkers:         uint32(max(*maxWorkers, 0)),
		MaxToolConcurrency: uint32(max(*maxTools, 0)),
		DefaultStopGrace:   *stopGrace,
		MaxStopGrace:       *maxStopGrace,
		SignalDeadline:     *signalDeadline,
		ActivityQueue:      int(*queue),
		ActivitySpillMax:   *spillMax,
		FlushTimeout:       *flushTimeout,
		ToolsManifest:      *manifest,
		SkillsDir:          *skillsDir,
		NoAttach:           *noAttach,
		AttachListen:       *attachOn,
		AttachToken:        attachToken,
		AttachAllowRemote:  *attachAny,
		Logger:             log,
	}
	if *attachOn != "" && *attachAny {
		log.Warn("the attach port may listen beyond loopback: anyone who can reach it and has the token can watch the session", "addr", *attachOn)
	}
	// With -tui, the viewer attaches to this run's own socket, like any
	// other. Quitting it detaches and the run carries on; when the run
	// ends first, the viewer stays until it is quit.
	viewerDone := make(chan struct{})
	var viewerStarted, runEnded atomic.Bool
	if *withTUI {
		cfg.Attached = func(string) {
			viewerStarted.Store(true)
			go func() {
				defer close(viewerDone)
				client, closeClient, err := attach.Dial(sessionfs.Folder{Dir: *dir}.State())
				if err != nil {
					log.Error("tui", "err", err)
					return
				}
				defer func() { _ = closeClient() }()
				conn := tui.Connection{Kind: "this process", Target: absPath(*dir), Dir: absPath(*dir)}
				if err := tui.RunLive(context.Background(), client, conn, viewerName()); err != nil {
					log.Error("tui", "err", err)
				}
				if !runEnded.Load() {
					fmt.Fprintln(stderr, "actlet run: the viewer detached; the session carries on (Ctrl-C pauses it; logs: .actlet/run.log)")
				}
			}()
		}
	}
	if signals == nil {
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		defer signal.Stop(ch)
		signals = ch
	}
	code := runner.Run(context.Background(), cfg, signals)
	runEnded.Store(true)
	if viewerStarted.Load() {
		<-viewerDone // the viewer says the run ended; q quits it
	}
	return code
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: use debug, info, warn or error", level)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("log format %q: use text or json", format)
}
