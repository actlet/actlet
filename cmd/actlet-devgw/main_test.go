package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/runner"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/adapters/router"
	"github.com/actlet/actlet/sdk/scripted"
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
		stopGrace: time.Second, cmdTimeout: 10 * time.Second, heartbeat: 20 * time.Millisecond,
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
	out.waitFor(t, "(in ")

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
	g.handle(ctx, []string{"store-delay", "25ms"})
	out.waitFor(t, "activity store delay: 25ms")
	g.handle(ctx, []string{"store-delay", "soon"})
	out.waitFor(t, "usage: store-delay")
}

// The dev gateway is built on the SDK's public API only, like any other
// orchestrator. The one exception is the shared version information.
func TestUsesOnlyTheSDK(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range strings.Fields(string(out)) {
		if strings.HasPrefix(imp, "github.com/actlet/actlet/internal/") && imp != "github.com/actlet/actlet/internal/buildinfo" {
			t.Errorf("actlet-devgw imports %s", imp)
		}
	}
}

func TestProfileFlag(t *testing.T) {
	dir := t.TempDir()
	good := dir + "/p.yaml"
	if err := os.WriteFile(good, []byte("schema: v0\nid: from-file\nstop_grace: 3s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := loadProfile(config{profile: good})
	if err != nil || p.GetId() != "from-file" || p.GetStopGrace().AsDuration() != 3*time.Second {
		t.Fatalf("profile %v, %v", p, err)
	}
	bad := dir + "/bad.json"
	if err := os.WriteFile(bad, []byte(`{"schema":"v0","id":"x","surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProfile(config{profile: bad}); err == nil {
		t.Error("accepted a profile with an unknown field")
	}
	if _, err := loadProfile(config{profileID: "dev", pauseMode: "sleep"}); err == nil {
		t.Error("accepted pause mode sleep")
	}
}

func TestStdinEndKeepsServing(t *testing.T) {
	g, err := newGateway(config{dataDir: t.TempDir(), profileID: "dev", pauseMode: "drain", demoDef: "{}", demoInput: "{}"},
		&syncBuffer{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.store.Close() }()
	if g.admin(t.Context(), strings.NewReader("sessions\n")) {
		t.Error("end of input treated as quit")
	}
	if !g.admin(t.Context(), strings.NewReader("sessions\nquit\nsessions\n")) {
		t.Error("quit not recognised")
	}
}

// The example scripts, profiles and tasks load.
func TestExamplesLoad(t *testing.T) {
	for _, dir := range []string{"agent", "demo"} {
		root := filepath.Join("..", "..", "examples", dir)
		if _, err := scripted.Load(filepath.Join(root, "model.yaml")); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
		if _, err := loadTask(config{taskFile: filepath.Join(root, "task.json")}); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
		if p := filepath.Join(root, "profile.yaml"); fileExists(p) {
			if _, err := loadProfile(config{profile: p}); err != nil {
				t.Errorf("%s: %v", dir, err)
			}
		}
	}
	r, err := router.Load(filepath.Join("..", "..", "examples", "providers.yaml"), func(string) string { return "a-key" })
	if err != nil || !r.Decides() || len(r.Aliases()) != 4 {
		t.Errorf("providers.yaml: %v", err)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
