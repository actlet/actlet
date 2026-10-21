//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

// Unix only: the end-to-end tests stop and crash actlet with signals and
// read its wait status, which Windows doesn't have.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent"
	"github.com/actlet/actlet/internal/agent/agentpb"
	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// toolOutcomes reads the session journal's tool outcomes for a worker.
func toolOutcomes(t *testing.T, dir, worker string) []*agentpb.ToolOutcome {
	t.Helper()
	f, err := os.Open(sessionfs.Folder{Dir: dir}.Journal())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	r, err := journal.NewReader(f, st.Size(), journal.ReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out []*agentpb.ToolOutcome
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range b.Records {
			if rec.Kind != agent.KindToolOutcome {
				continue
			}
			o := &agentpb.ToolOutcome{}
			if err := proto.Unmarshal(rec.Data, o); err != nil {
				t.Fatal(err)
			}
			if o.GetWorkerId() == worker {
				out = append(out, o)
			}
		}
	}
}

const subagentScript = `
turns:
  - text: I'll split the work between two subagents.
    tool_calls:
      - name: spawn_subagent
        input: {task: "Count the lines of ../inputs/a.txt and write the count to counts/a.txt."}
      - name: spawn_subagent
        input: {task: "Count the lines of ../inputs/b.txt and write the count to counts/b.txt."}
  - expect:
      - contains: a.txt has 2 lines
      - contains: b.txt has 3 lines
    tool_calls:
      - name: shell
        input: {command: "awk '{s += $1} END {print s}' counts/a.txt counts/b.txt > ../output/total.txt && cat ../output/total.txt"}
  - expect:
      - contains: "stdout:\n5\n"
    text: The files have 5 lines in all.
subagents:
  - match: inputs/a.txt
    turns:
      - tool_calls:
          - name: shell
            input: {command: "mkdir -p counts && sleep 0.5 && wc -l < ../inputs/a.txt | tr -d ' ' > counts/a.txt && cat counts/a.txt", writes: [counts/a.txt]}
      - expect:
          - contains: "stdout:\n2\n"
        text: a.txt has 2 lines.
  - match: inputs/b.txt
    turns:
      - tool_calls:
          - name: shell
            input: {command: "mkdir -p counts && sleep 0.5 && wc -l < ../inputs/b.txt | tr -d ' ' > counts/b.txt && cat counts/b.txt", writes: [counts/b.txt]}
      - expect:
          - contains: "stdout:\n3\n"
        text: b.txt has 3 lines.
`

// The agent spawns two subagents that work in parallel on different
// files; both results reach it once, and it delivers the total.
func TestSubagentsEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, subagentScript)
	dir := sessionDir(t, map[string]string{"a.txt": "1\n2\n", "b.txt": "1\n2\n3\n"})
	if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
		t.Fatalf("exit code %d, want 0", code)
	}
	if got := readFile(t, filepath.Join(dir, "output", "total.txt")); got != "5\n" {
		t.Errorf("total.txt = %q", got)
	}
	if n := len(toolOutcomes(t, dir, agent.RootWorker)); n != 3 {
		t.Errorf("the root has %d tool outcomes, want 3: two subagent results and the shell", n)
	}
	// Both subagents started before either finished.
	var started, finished []time.Time
	g.mu.Lock()
	for _, ev := range g.events {
		switch {
		case ev.GetSubagentSpawned() != nil:
			started = append(started, ev.GetTime().AsTime())
		case ev.GetSubagentFinished() != nil:
			finished = append(finished, ev.GetTime().AsTime())
		}
	}
	g.mu.Unlock()
	if len(started) != 2 || len(finished) != 2 {
		t.Fatalf("%d subagents started, %d finished", len(started), len(finished))
	}
	if started[1].After(finished[0]) {
		t.Error("the subagents didn't run in parallel")
	}
	for _, w := range []string{"root.1", "root.2"} {
		if g.eventCount(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetWorker() == w && ev.GetModelTurn() != nil }) != 2 {
			t.Errorf("no model turns of %s in the activity", w)
		}
	}
}

// crashSubagentScript has the agent spawn one subagent that appends to a
// file once; the subagent expects the given result from its command.
func crashSubagentScript(childResult string) string {
	return fmt.Sprintf(`
turns:
  - tool_calls:
      - name: spawn_subagent
        input: {task: "Append a line to side.txt."}
  - expect:
      - contains: appended
    text: done
subagents:
  - match: side.txt
    turns:
      - tool_calls:
          - name: shell
            input: {command: "echo once >> side.txt"}
      - expect:
          - contains: %q
        text: appended
`, childResult)
}

// SIGKILL around a subagent: after it is created but before it starts,
// while it runs a command, and after it finished but before its result
// was delivered. Each recovers with the result delivered exactly once and
// the command run once.
func TestSubagentCrashes(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	tests := []struct {
		name, crashAt, childResult string
	}{
		{"after spawn", "spawn@root", "exit code 0"},
		{"child mid-tool", "tool.done:shell@root.1", "outcome_unknown"},
		{"after the child finished", "worker.done@root.1", "exit code 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newAgentGateway(t, crashSubagentScript(tt.childResult))
			dir := sessionDir(t, nil)
			if code := exitCode(t, g.worker(dir, tt.crashAt)); code != -int(syscall.SIGKILL) {
				t.Fatalf("first run: exit %d, want killed", code)
			}
			if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
				t.Fatalf("recovered run: exit %d, want 0", code)
			}
			if got := readFile(t, filepath.Join(dir, "workspace", "side.txt")); got != "once\n" {
				t.Errorf("side.txt = %q", got)
			}
			if n := len(toolOutcomes(t, dir, agent.RootWorker)); n != 1 {
				t.Errorf("the subagent's result was delivered %d times", n)
			}
			if n := len(toolOutcomes(t, dir, "root.1")); n != 1 {
				t.Errorf("the subagent's command has %d outcomes", n)
			}
		})
	}
}

// The gateway goes away while a long command runs: the command finishes
// and is journaled, the next model call waits, and the session carries on
// once the gateway is back.
func TestGatewayDownDuringTool(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, `
turns:
  - tool_calls:
      - name: shell
        input: {command: "echo yes > started; sleep 1; echo yes > finished"}
  - expect:
      - contains: exit code 0
    text: done
`)
	dir := sessionDir(t, nil)
	cmd := g.worker(dir, "")
	waitFile := func(name string) {
		deadline := time.Now().Add(20 * time.Second)
		for readFile(t, filepath.Join(dir, "workspace", name)) == "" {
			if time.Now().After(deadline) {
				t.Fatalf("%s never appeared", name)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFile("started")
	g.stop()
	down := time.Now()
	waitFile("finished")
	// The outcome is journaled while the gateway is down.
	deadline := time.Now().Add(10 * time.Second)
	for len(toolOutcomes(t, dir, agent.RootWorker)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the command's outcome wasn't journaled while the gateway was down")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(time.Second) // the model call is waiting meanwhile
	if n := len(g.model.Calls()); n != 1 {
		t.Fatalf("%d model calls while the gateway was down", n)
	}
	g.start()
	up := time.Now()
	if code := exitCode(t, cmd); code != session.ExitCompleted {
		t.Fatalf("exit %d, want 0", code)
	}
	calls := g.model.Calls()
	if len(calls) != 2 || calls[1].Time.Before(up) {
		t.Errorf("calls %+v; the gateway was down from %v to %v", calls, down, up)
	}
	// The activity of the downtime arrived after reconnecting.
	if g.eventCount(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetToolFinished().GetTool() == "shell" }) != 1 {
		t.Error("the command's result isn't in the activity")
	}
}

// A steer note sent while a command runs, just before a pause, reaches the
// agent's next model call after resuming, exactly once.
func TestSteerAcrossPause(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, `
turns:
  - tool_calls:
      - name: shell
        input: {command: "echo yes > started; sleep 1"}
  - expect:
      - contains: exit code 0
    expect_once: [use metric units]
    tool_calls:
      - name: glob
        input: {pattern: "*"}
  - expect_once: [use metric units]
    text: done
`)
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
	if _, err := sess.Steer(ctx, "use metric units"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Pause(ctx, gatewayv0.PauseMode_PAUSE_MODE_DRAIN); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(t, cmd); code != session.ExitPaused {
		t.Fatalf("paused run: exit %d, want 3", code)
	}
	if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
		t.Fatalf("resumed run: exit %d, want 0", code)
	}
}
