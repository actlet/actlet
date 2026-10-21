//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

// Unix only: the end-to-end tests stop and crash actlet with signals and
// read its wait status, which Windows doesn't have.

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/actlet/actlet/internal/session"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

// leaveProcesses is a shell command that leaves a process running in the
// background and, where setsid is installed, a daemon in a session of
// its own, writing their PIDs to bg.pid and daemon.pid.
const leaveProcesses = `sleep 300 >/dev/null 2>&1 & echo $! > bg.pid; ` +
	`if command -v setsid >/dev/null; then setsid sh -c 'echo $$ > daemon.pid; exec sleep 300' >/dev/null 2>&1 </dev/null & fi; ` +
	`sleep 0.3; echo started`

// processRunning reports whether a process runs; a zombie doesn't.
func processRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// Whichever way a run ends, nothing its tools started is left running:
// not a background process, nor, on Linux, a daemon.
func TestNoProcessesLeft(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	tests := []struct {
		name string
		last string // the script's second turn
		end  func(t *testing.T, g *agentGateway, cmd *exec.Cmd)
		code int
	}{
		{"completed", "  - text: done\n", nil, session.ExitCompleted},
		{"failed", "  - expect:\n      - contains: NOT THERE\n    text: done\n", nil, session.ExitFailed},
		{"stopped", "  - delay: 60s\n    text: done\n", func(t *testing.T, g *agentGateway, _ *exec.Cmd) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			for _, s := range g.srv.Sessions() {
				if _, err := s.Stop(ctx, sdk.StopOptions{}); err != nil {
					t.Error(err)
				}
			}
		}, session.ExitStopped},
		{"paused by SIGTERM", "  - delay: 60s\n    text: done\n", func(_ *testing.T, _ *agentGateway, cmd *exec.Cmd) {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}, session.ExitPaused},
		{"paused by SIGTERM mid-tool", "  - tool_calls:\n      - name: shell\n        input: {command: \"sleep 60\"}\n  - text: done\n",
			func(_ *testing.T, _ *agentGateway, cmd *exec.Cmd) { _ = cmd.Process.Signal(syscall.SIGTERM) }, session.ExitPaused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := "turns:\n  - tool_calls:\n      - name: shell\n        input: {command: " + strconv.Quote(leaveProcesses) + "}\n" + tt.last
			g := newAgentGateway(t, script)
			dir := sessionDir(t, nil)
			cmd := g.worker(dir, "")
			ws := filepath.Join(dir, "workspace")
			deadline := time.Now().Add(20 * time.Second)
			for readFile(t, filepath.Join(ws, "bg.pid")) == "" || g.eventCount(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetToolFinished() != nil }) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the first tool call never finished")
				}
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond) // into the second turn
			if tt.end != nil {
				tt.end(t, g, cmd)
			}
			if code := exitCode(t, cmd); code != tt.code {
				t.Fatalf("exit %d, want %d", code, tt.code)
			}
			for _, name := range []string{"bg.pid", "daemon.pid"} {
				s := strings.TrimSpace(readFile(t, filepath.Join(ws, name)))
				if s == "" {
					continue // no setsid here
				}
				pid, err := strconv.Atoi(s)
				if err != nil {
					t.Fatalf("%s: %q", name, s)
				}
				deadline := time.Now().Add(3 * time.Second)
				for processRunning(pid) {
					if time.Now().After(deadline) {
						_ = syscall.Kill(pid, syscall.SIGKILL)
						t.Fatalf("%s %d outlived the run", name, pid)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			_ = os.Remove(filepath.Join(ws, "bg.pid"))
		})
	}
}
