//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/actlet/actlet/internal/attach"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	"github.com/actlet/actlet/internal/tui"
	"github.com/actlet/actlet/internal/view"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// slowScript keeps the box busy long enough to attach to it, with a
// subagent.
const slowScript = `
turns:
  - tool_calls:
      - name: shell
        input: {command: "echo yes > started; sleep 1"}
      - name: spawn_subagent
        input: {task: "Write the note file."}
  - expect:
      - contains: exit code 0
      - contains: wrote it
    tool_calls:
      - name: shell
        input: {command: "sleep 1; cp note.txt ../output/note.txt"}
  - text: done
subagents:
  - match: the note file
    turns:
      - tool_calls:
          - name: write_file
            input: {path: note.txt, content: "hello\n"}
      - text: wrote it
`

// attachTo waits for the box in dir to listen, and connects to it.
func attachTo(t *testing.T, dir string) attachv0.AttachClient {
	t.Helper()
	state := sessionfs.Folder{Dir: dir}.State()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := attach.Endpoint(state); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the box never listened for viewers")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client, closeClient, err := attach.Dial(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeClient() })
	return client
}

// collect watches from a position until the run ends or n events
// arrive (n 0: until the end).
func collect(t *testing.T, ctx context.Context, client attachv0.AttachClient, after uint64, n int) []*gatewayv0.ActivityEvent {
	t.Helper()
	stream, err := client.Watch(ctx, &attachv0.WatchRequest{AfterSeq: after})
	if err != nil {
		t.Fatal(err)
	}
	var out []*gatewayv0.ActivityEvent
	for n == 0 || len(out) < n {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		if s := m.GetSkipped(); s != nil {
			t.Errorf("skipped %d events: %s", s.GetCount(), s.GetReason())
		}
		if ev := m.GetEvent(); ev != nil {
			out = append(out, ev)
		}
	}
	return out
}

// Viewers attach, detach and reattach while the box runs; the run is
// unaffected, every viewer sees every event once, and folding what a
// viewer saw gives the same view as replaying the gateway's recording.
func TestAttachDuringRun(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, slowScript)
	dir := sessionDir(t, nil)
	cmd := g.worker(dir, "")
	client := attachTo(t, dir)
	ctx := t.Context()

	full := make(chan []*gatewayv0.ActivityEvent, 1)
	go func() { full <- collect(t, ctx, client, 0, 0) }()

	// Another viewer comes and goes, and picks up where it left off.
	cctx, cancel := context.WithCancel(ctx)
	first := collect(t, cctx, client, 0, 3)
	cancel()
	rest := collect(t, ctx, client, first[len(first)-1].GetSeq(), 0)

	if code := exitCode(t, cmd); code != session.ExitCompleted {
		t.Fatalf("exit %d", code)
	}
	if got := readFile(t, filepath.Join(dir, "output", "note.txt")); got != "hello\n" {
		t.Errorf("note.txt = %q", got)
	}
	all := <-full
	again := append(first, rest...)
	if len(all) == 0 || len(again) != len(all) {
		t.Fatalf("the viewer that stayed saw %d events, the one that came back %d", len(all), len(again))
	}
	for i, ev := range all {
		if ev.GetSeq() != uint64(i+1) || again[i].GetSeq() != ev.GetSeq() {
			t.Fatalf("event %d: seq %d and %d", i, ev.GetSeq(), again[i].GetSeq())
		}
	}

	// Live and replayed views agree.
	live := view.New()
	for _, ev := range all {
		live.Apply(ev)
	}
	recs, _ := filepath.Glob(filepath.Join(g.data, "*.actrec"))
	if len(recs) != 1 {
		t.Fatalf("recordings %v", recs)
	}
	p, err := tui.OpenPlayer(recs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Seek(p.Len())
	replayed := p.View()
	live.ID = replayed.ID
	if !reflect.DeepEqual(live, replayed) {
		t.Errorf("the live view (%d events) and the replay (%d events) differ", live.Events, replayed.Events)
	}
	if w := live.Worker("root.1"); w == nil || w.Status != view.WorkerFinished || live.Info == nil || live.Info.TaskID != "sum" {
		t.Errorf("view: subagent %+v, info %+v", w, live.Info)
	}
}

const viewerHelperEnv = "ACTLET_VIEWER_HELPER"

// TestViewerHelper is a viewer in a separate process: it attaches and
// reads slowly, until killed.
func TestViewerHelper(t *testing.T) {
	dir := os.Getenv(viewerHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	client, closeClient, err := attach.Dial(sessionfs.Folder{Dir: dir}.State())
	if err != nil {
		os.Exit(1)
	}
	defer func() { _ = closeClient() }()
	_ = tui.Feed(context.Background(), client, func(tea.Msg) { time.Sleep(time.Second) })
	os.Exit(0)
}

// A viewer killed in the middle of a run doesn't affect it.
func TestViewerKilled(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, slowScript)
	dir := sessionDir(t, nil)
	cmd := g.worker(dir, "")
	attachTo(t, dir)
	viewer := exec.Command(os.Args[0], "-test.run=^TestViewerHelper$")
	viewer.Env = append(os.Environ(), viewerHelperEnv+"="+dir)
	if err := viewer.Start(); err != nil {
		t.Fatal(err)
	}
	for readFile(t, filepath.Join(dir, "workspace", "started")) == "" {
		time.Sleep(5 * time.Millisecond)
	}
	_ = viewer.Process.Signal(syscall.SIGKILL)
	_ = viewer.Wait()
	if code := exitCode(t, cmd); code != session.ExitCompleted {
		t.Fatalf("exit %d after the viewer was killed", code)
	}
}

// A local viewer may send notes; lifecycle commands need the profile's
// grant.
func TestLocalCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	for _, granted := range []bool{false, true} {
		name := "refused"
		if granted {
			name = "granted"
		}
		t.Run(name, func(t *testing.T) {
			g := newAgentGateway(t, slowScript, func(g *agentGateway) {
				if granted {
					g.local = &gatewayv0.LocalAttach{Commands: []string{"steer", "pause"}}
				}
			})
			dir := sessionDir(t, nil)
			cmd := g.worker(dir, "")
			client := attachTo(t, dir)
			ctx := t.Context()
			ack, err := client.Command(ctx, &attachv0.CommandRequest{Actor: "alice", Action: &attachv0.CommandRequest_Steer{Steer: &gatewayv0.Steer{Note: "hello from the terminal"}}})
			if err != nil || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
				t.Fatalf("steer: %v, %v", ack, err)
			}
			_, err = client.Command(ctx, &attachv0.CommandRequest{Action: &attachv0.CommandRequest_Pause{Pause: &gatewayv0.Pause{Mode: gatewayv0.PauseMode_PAUSE_MODE_DRAIN}}})
			want := session.ExitPaused
			if !granted {
				if status.Code(err) != codes.PermissionDenied {
					t.Fatalf("pause without a grant: %v", err)
				}
				want = session.ExitCompleted
			} else if err != nil {
				t.Fatalf("granted pause: %v", err)
			}
			if code := exitCode(t, cmd); code != want {
				t.Fatalf("exit %d, want %d", code, want)
			}
			// The note was recorded as the local operator's.
			rec, err := session.Recover(sessionfs.Folder{Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, c := range rec.Commands {
				if c.GetNote() == "hello from the terminal" && c.GetOrigin() == "local" && c.GetActor() == "alice" {
					found = true
				}
			}
			if !found {
				t.Error("the note isn't journaled with origin local")
			}
		})
	}
}

// A headless run starts no viewer and writes no terminal control
// sequences.
func TestHeadlessRunStartsNoViewer(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, `
turns:
  - text: done
`)
	report := filepath.Join(t.TempDir(), "tui")
	dir := sessionDir(t, nil)
	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunHelper$")
	cmd.Env = append(os.Environ(), runHelperEnv+"=1", "ACTLET_SESSION="+dir, "ACTLET_GATEWAY="+g.addr,
		"ACTLET_TOKEN=tok", "ACTLET_TOOLS_MANIFEST=", tuiReportEnv+"="+report)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := readFile(t, report); got != "0" {
		t.Errorf("the headless run started %s viewers", got)
	}
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Errorf("the headless run wrote control sequences: %q", out.String())
	}
}
