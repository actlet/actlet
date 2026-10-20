//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/actlet/actlet/internal/session"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/scripted"
)

// agentGateway serves an agent task with a scripted model, and collects
// the activity it receives.
type agentGateway struct {
	t     *testing.T
	addr  string
	srv   *sdk.Server
	gs    *grpc.Server
	store *sdk.RecordingStore
	data  string
	model *scripted.Model
	task  *gatewayv0.Task
	// local is the profile's local attach setting.
	local *gatewayv0.LocalAttach
	// budgets are the profile's budgets.
	budgets []*gatewayv0.Budget

	mu     sync.Mutex
	events []*gatewayv0.ActivityEvent
}

func newAgentGateway(t *testing.T, script string, opts ...func(*agentGateway)) *agentGateway {
	t.Helper()
	model, err := scripted.Parse([]byte(script))
	if err != nil {
		t.Fatal(err)
	}
	g := &agentGateway{t: t, model: model, addr: "127.0.0.1:0", data: t.TempDir(),
		task: &gatewayv0.Task{Schema: "v0", Id: "sum", Kind: "agent", Input: []byte(`"Add up the numbers in the input file and write a report."`)}}
	for _, o := range opts {
		o(g)
	}
	g.start()
	t.Cleanup(g.stop)
	return g
}

// stop stops the gateway at once, as a crash would.
func (g *agentGateway) stop() {
	g.gs.Stop()
	_ = g.store.Close()
}

// start starts a gateway on the address, knowing no sessions.
func (g *agentGateway) start() {
	t := g.t
	t.Helper()
	var err error
	if g.store, err = sdk.NewRecordingStore(g.data); err != nil {
		t.Fatal(err)
	}
	g.srv, err = sdk.NewServer(sdk.Options{
		Store:        g.store,
		Authenticate: sdk.StaticToken("tok"),
		Logger:       slog.New(slog.DiscardHandler),
		OnInference:  g.model.Serve,
		OnActivity: func(_ string, ev *gatewayv0.ActivityEvent) {
			g.mu.Lock()
			g.events = append(g.events, ev)
			g.mu.Unlock()
		},
		Assign: func(_ context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			return &gatewayv0.RegisterResponse{
				SessionId: req.GetSessionId(),
				Profile: &gatewayv0.Profile{Id: "agent", Models: &gatewayv0.ModelCatalog{
					Aliases: []*gatewayv0.ModelAlias{{Alias: "scripted"}}, Roles: map[string]string{"main": "scripted"},
				}, Limits: &gatewayv0.Limits{MaxWorkers: 4, MaxToolConcurrency: 4}, LocalAttach: g.local, Budgets: g.budgets},
				Task: g.task,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var lis net.Listener
	for range 50 { // the address may take a moment to free up
		if lis, err = net.Listen("tcp", g.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	g.gs = grpc.NewServer(sdk.ServerOptions()...)
	g.srv.RegisterService(g.gs)
	go func() { _ = g.gs.Serve(lis) }()
	g.addr = lis.Addr().String()
}

// worker starts `actlet run` on the session folder, optionally dying at a
// crash point.
func (g *agentGateway) worker(dir, crashAt string) *exec.Cmd {
	g.t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunHelper$")
	cmd.Env = append(os.Environ(), runHelperEnv+"=1", crashpointEnv+"="+crashAt,
		"ACTLET_SESSION="+dir, "ACTLET_GATEWAY="+g.addr, "ACTLET_TOKEN=tok", "ACTLET_TOOLS_MANIFEST=")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		g.t.Fatal(err)
	}
	return cmd
}

// exitCode waits for the worker and returns its exit code; -9 means it
// was killed.
func exitCode(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return -int(ws.Signal())
			}
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the worker didn't exit")
		return 0
	}
}

func (g *agentGateway) eventCount(match func(*gatewayv0.ActivityEvent) bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, ev := range g.events {
		if match(ev) {
			n++
		}
	}
	return n
}

func sessionDir(t *testing.T, inputs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range inputs {
		p := filepath.Join(dir, "inputs", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

const e2eScript = `
turns:
  - text: Let me read the input first.
    tool_calls:
      - name: read_file
        input: {path: ../inputs/numbers.txt}
  - expect:
      - contains: "     3\t5"
    text: I'll sort the numbers with the shell and add them up in Python.
    tool_calls:
      - name: shell
        input: {command: "sort -n ../inputs/numbers.txt > sorted.txt && wc -l < sorted.txt"}
      - name: python
        input: {code: "nums = [int(line) for line in open('sorted.txt')]\nprint(sum(nums))"}
  - expect:
      - contains: "stdout:\n"
        is_error: false
      - contains: "stdout:\n12\n"
        is_error: false
    tool_calls:
      - name: write_file
        input: {path: report.md, content: "# Report\n\nTotal: TBD\n"}
  - expect:
      - contains: created report.md
    tool_calls:
      - name: edit_file
        input: {path: report.md, old_string: TBD, new_string: "12"}
  - expect:
      - contains: "+Total: 12"
    tool_calls:
      - name: shell
        input: {command: "cp report.md ../output/report.md"}
  - expect:
      - contains: exit code 0
    text: The total is 12. The report is in output/report.md.
`

// A scripted agent task runs end to end through the gateway: it reads its
// input, uses the shell, Python and file edits, delivers a report in
// output/ and exits 0, and the activity shows every tool call and diff.
func TestAgentTaskEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	g := newAgentGateway(t, e2eScript)
	dir := sessionDir(t, map[string]string{"numbers.txt": "4\n3\n5\n"})
	if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
		t.Fatalf("exit code %d, want 0", code)
	}
	if got := readFile(t, filepath.Join(dir, "output", "report.md")); got != "# Report\n\nTotal: 12\n" {
		t.Errorf("report.md = %q", got)
	}
	if g.eventCount(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetModelTurn() != nil }) != 6 {
		t.Error("not 6 model turns in the activity")
	}
	for _, tool := range []string{"read_file", "shell", "python", "write_file", "edit_file"} {
		if g.eventCount(func(ev *gatewayv0.ActivityEvent) bool {
			f := ev.GetToolFinished()
			return f.GetTool() == tool && !f.GetIsError()
		}) == 0 {
			t.Errorf("no successful %s call in the activity", tool)
		}
	}
	if g.eventCount(func(ev *gatewayv0.ActivityEvent) bool {
		return ev.GetFileChange().GetPath() == "report.md" && strings.Contains(ev.GetFileChange().GetDiff(), "-Total: TBD\n+Total: 12\n")
	}) != 1 {
		t.Error("the edit's diff isn't in the activity")
	}
}

// crashScript calls one tool, then expects the given result for it.
func crashScript(tool, input, result string) string {
	return fmt.Sprintf(`
turns:
  - tool_calls:
      - name: %s
        input: %s
  - expect:
      - contains: %q
    text: done
`, tool, input, result)
}

// SIGKILL right after a tool call's intent is journaled, before it runs:
// a call that is safe to repeat runs again; a shell command isn't run and
// the model is told its outcome is unknown.
func TestCrashAfterToolIntent(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	tests := []struct {
		tool, input, result string
		side                string // workspace/side.txt afterwards
	}{
		{"shell", `{command: "echo once >> side.txt"}`, "outcome_unknown", ""},
		{"write_file", `{path: side.txt, content: "once\n"}`, "created side.txt", "once\n"},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			g := newAgentGateway(t, crashScript(tt.tool, tt.input, tt.result))
			dir := sessionDir(t, nil)
			if code := exitCode(t, g.worker(dir, "tool.intent:"+tt.tool)); code != -int(syscall.SIGKILL) {
				t.Fatalf("first run: exit %d, want killed", code)
			}
			if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
				t.Fatalf("recovered run: exit %d, want 0", code)
			}
			if got := readFile(t, filepath.Join(dir, "workspace", "side.txt")); got != tt.side {
				t.Errorf("side.txt = %q, want %q", got, tt.side)
			}
		})
	}
}

// SIGKILL after a shell command's side effect, before its outcome is
// journaled: the command isn't run again, and the model is told its
// outcome is unknown.
func TestCrashAfterShellSideEffect(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, crashScript("shell", `{command: "echo once >> side.txt"}`, "outcome_unknown"))
	dir := sessionDir(t, nil)
	if code := exitCode(t, g.worker(dir, "tool.done:shell")); code != -int(syscall.SIGKILL) {
		t.Fatalf("first run: exit %d, want killed", code)
	}
	if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
		t.Fatalf("recovered run: exit %d, want 0", code)
	}
	if got := readFile(t, filepath.Join(dir, "workspace", "side.txt")); got != "once\n" {
		t.Errorf("side.txt = %q: the command ran %d times", got, strings.Count(got, "once"))
	}
	if g.eventCount(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetToolFinished().GetOutcomeUnknown() }) != 1 {
		t.Error("the unknown outcome isn't in the activity")
	}
}

// SIGKILL during a model call: the next run makes the call again from the
// checkpoint, with the same intent ID.
func TestCrashDuringInference(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, `
turns:
  - tool_calls:
      - name: glob
        input: {pattern: "*"}
  - expect:
      - contains: no files match
    delay: 1s
    text: done
`)
	dir := sessionDir(t, nil)
	cmd := g.worker(dir, "")
	deadline := time.Now().Add(20 * time.Second)
	for len(g.model.Calls()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the second model call never came")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGKILL) // while the model is answering
	if code := exitCode(t, cmd); code != -int(syscall.SIGKILL) {
		t.Fatalf("first run: exit %d", code)
	}
	if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
		t.Fatalf("recovered run: exit %d, want 0", code)
	}
	calls := g.model.Calls()
	if len(calls) != 3 || calls[1].Turn != 2 || calls[2].Turn != 2 || calls[1].Intent != calls[2].Intent || calls[0].Intent == calls[1].Intent {
		t.Fatalf("calls %+v: want turn 2 twice with the same intent ID", calls)
	}
}

// Pausing while a shell command runs: drain lets it finish, interrupt
// stops it; either way the session pauses (exit 3) and the next run
// carries on and completes.
func TestPauseDuringTool(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	for _, mode := range []gatewayv0.PauseMode{gatewayv0.PauseMode_PAUSE_MODE_DRAIN, gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT} {
		t.Run(mode.String(), func(t *testing.T) {
			result := "exit code 0"
			if mode == gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT {
				result = "interrupted"
			}
			g := newAgentGateway(t, crashScript("shell", `{command: "echo yes > started; sleep 2; echo yes > finished"}`, result))
			dir := sessionDir(t, nil)
			cmd := g.worker(dir, "")
			deadline := time.Now().Add(20 * time.Second)
			for readFile(t, filepath.Join(dir, "workspace", "started")) == "" {
				if time.Now().After(deadline) {
					t.Fatal("the command never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
			sess := g.srv.Sessions()[0]
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if _, err := sess.Pause(ctx, mode); err != nil {
				t.Fatal(err)
			}
			if code := exitCode(t, cmd); code != session.ExitPaused {
				t.Fatalf("paused run: exit %d, want 3", code)
			}
			finished := readFile(t, filepath.Join(dir, "workspace", "finished")) != ""
			if finished != (mode == gatewayv0.PauseMode_PAUSE_MODE_DRAIN) {
				t.Errorf("the command finished: %v", finished)
			}
			if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
				t.Fatalf("resumed run: exit %d, want 0", code)
			}
		})
	}
}

// The example in examples/agent runs as its docs say.
func TestAgentExample(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	dir := filepath.Join("..", "..", "examples", "agent")
	script, err := os.ReadFile(filepath.Join(dir, "model.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var task struct {
		Schema, ID, Kind  string
		Definition, Input json.RawMessage
	}
	data, err := os.ReadFile(filepath.Join(dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &task); err != nil {
		t.Fatal(err)
	}
	numbers, err := os.ReadFile(filepath.Join(dir, "numbers.txt"))
	if err != nil {
		t.Fatal(err)
	}
	g := newAgentGateway(t, string(script), func(g *agentGateway) {
		g.task = &gatewayv0.Task{Schema: task.Schema, Id: task.ID, Kind: task.Kind, Definition: task.Definition, Input: task.Input}
	})
	session := sessionDir(t, map[string]string{"numbers.txt": string(numbers)})
	if code := exitCode(t, g.worker(session, "")); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	if got := readFile(t, filepath.Join(session, "output", "report.md")); got != "# Report\n\nTotal: 12\n" {
		t.Errorf("report.md = %q", got)
	}
}
