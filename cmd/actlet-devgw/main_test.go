package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/runner"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/sdk"
)

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) waitFor(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(s.String(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q:\n%s", substr, s.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDevGateway(t *testing.T) {
	out := &syncBuffer{}
	discard := slog.New(slog.DiscardHandler)
	g, err := newGateway(config{
		token: "dev", dataDir: t.TempDir(), profileID: "dev", pauseMode: "drain",
		stopGrace: time.Second, cmdTimeout: 10 * time.Second,
		demoDef: `{"steps":50,"step":"50ms"}`, demoInput: `{"message":"hi"}`,
	}, out, discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.store.Close() }()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(sdk.ServerOptions()...)
	g.srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	dir := t.TempDir()
	runWorker := func() chan int {
		exit := make(chan int, 1)
		go func() {
			exit <- runner.Run(context.Background(), runner.Config{
				Gateway:    gwclient.Config{Addr: lis.Addr().String(), Token: "dev", BackoffMin: 10 * time.Millisecond, BackoffMax: 100 * time.Millisecond},
				SessionDir: dir, WorkerID: "w", DefaultStopGrace: time.Second, MaxStopGrace: time.Minute,
				SignalDeadline: time.Second, FlushTimeout: 2 * time.Second, Logger: discard,
			}, nil)
		}()
		return exit
	}
	waitExit := func(exit chan int) int {
		select {
		case code := <-exit:
			return code
		case <-time.After(20 * time.Second):
			t.Fatal("worker didn't exit")
			return -1
		}
	}
	ctx := t.Context()

	exit := runWorker()
	out.waitFor(t, "step 1 of 50 started")
	g.handle(ctx, []string{"steer", "look", "left"})
	out.waitFor(t, "steer: look left")
	g.handle(ctx, []string{"pause"}) // no mode
	out.waitFor(t, "usage: pause drain|interrupt")
	g.handle(ctx, []string{"pause", "drain"})
	if code := waitExit(exit); code != session.ExitPaused {
		t.Fatalf("pause: exit %d", code)
	}
	out.waitFor(t, "pause drain: accepted")

	// Resume, then stop.
	exit = runWorker()
	out.waitFor(t, "resumed true")
	g.handle(ctx, []string{"stop", "kill"})
	if code := waitExit(exit); code != session.ExitStopped {
		t.Fatalf("stop: exit %d", code)
	}
	g.wait()

	g.handle(ctx, []string{"sessions"})
	out.waitFor(t, "epoch 2")
	id := g.srv.Sessions()[0].ID()
	if _, err := os.Stat(g.store.Path(id)); err != nil {
		t.Errorf("no recording: %v", err)
	}
	g.handle(ctx, []string{"bogus"})
	out.waitFor(t, `unknown command "bogus"`)
}

// The dev gateway is built on the SDK's public API only, like any other
// orchestrator.
func TestUsesOnlyTheSDK(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range strings.Fields(string(out)) {
		if strings.HasPrefix(imp, "github.com/actlet/actlet/internal/") {
			t.Errorf("actlet-devgw imports %s", imp)
		}
	}
}
