//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/actlet/actlet/internal/crashpoint"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	"github.com/actlet/actlet/internal/tui"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

const runHelperEnv = "ACTLET_RUN_HELPER"

// tuiReportEnv names a file the helper writes the number of viewers it
// started to.
const tuiReportEnv = "ACTLET_TEST_TUI_REPORT"

// crashpointEnv names the crash point the helper process dies at.
const crashpointEnv = "ACTLET_TEST_CRASHPOINT"

// TestRunHelper is `actlet run` in a separate process for the crash tests.
func TestRunHelper(t *testing.T) {
	if os.Getenv(runHelperEnv) == "" {
		t.Skip("helper process only")
	}
	if p := os.Getenv(crashpointEnv); p != "" {
		crashpoint.Set(p)
	}
	code := run([]string{"run", "-log-level", "warn"}, os.Stdout, os.Stderr)
	if f := os.Getenv(tuiReportEnv); f != "" {
		_ = os.WriteFile(f, []byte(strconv.Itoa(tui.Programs())), 0o600)
	}
	os.Exit(code)
}

// A command is journaled before it is acknowledged: killing the process
// right after the acknowledgement loses nothing, and the resumed session
// recognises the command when it is sent again.
func TestCrashAfterAck(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gw, err := sdk.NewServer(sdk.Options{
		Authenticate: sdk.StaticToken("tok"),
		Logger:       slog.New(slog.DiscardHandler),
		Assign: func(_ context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			return &gatewayv0.RegisterResponse{
				SessionId: req.GetSessionId(),
				Profile:   &gatewayv0.Profile{Id: "p"},
				Task: &gatewayv0.Task{Schema: "v0", Id: "t", Kind: session.DemoKind,
					Definition: []byte(`{"steps":100,"step":"100ms"}`)},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(sdk.ServerOptions()...)
	gw.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	dir := t.TempDir()
	start := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRunHelper$")
		cmd.Env = append(os.Environ(), runHelperEnv+"=1",
			"ACTLET_SESSION="+dir, "ACTLET_GATEWAY="+lis.Addr().String(), "ACTLET_TOKEN=tok")
		cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	connected := func(epoch uint64) *sdk.Session {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			for _, s := range gw.Sessions() {
				if s.Connected() && s.Epoch() == epoch {
					return s
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("no worker connected with epoch %d", epoch)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := start()
	sess := connected(1)
	steer := &gatewayv0.Command{Id: "steer-1", Action: &gatewayv0.Command_Steer{Steer: &gatewayv0.Steer{Note: "survive"}}}
	ack, err := sess.Send(ctx, steer)
	if err != nil || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
		t.Fatalf("ack %v, %v", ack, err)
	}
	_ = cmd.Process.Signal(syscall.SIGKILL) // right after the acknowledgement
	_ = cmd.Wait()

	rec, err := session.Recover(sessionfs.Folder{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if c := rec.Commands["steer-1"]; c == nil || c.GetNote() != "survive" {
		t.Fatalf("acknowledged command missing from the journal after a crash: %v", c)
	}

	// The next run recovers and knows the command.
	cmd = start()
	sess = connected(2)
	ack, err = sess.Send(ctx, steer)
	if err != nil || !ack.GetDuplicate() {
		t.Fatalf("resent command after recovery: %v, %v", ack, err)
	}
	if _, err := sess.Stop(ctx, sdk.StopOptions{Kill: true}); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != session.ExitStopped {
		t.Fatalf("stopped run exited with %v, want code %d", err, session.ExitStopped)
	}
}
