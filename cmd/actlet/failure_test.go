//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

// Unix only: the end-to-end tests stop and crash actlet with signals and
// read its wait status, which Windows doesn't have.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/session"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// journalFaultEnv makes the helper's session journal fail: "write" or
// "sync", the errno, and how many of those operations succeed first, as
// in "write:ENOSPC:12".
const journalFaultEnv = "ACTLET_TEST_JOURNAL_FAULT"

func setJournalFault(spec string) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		panic("bad " + journalFaultEnv + ": " + spec)
	}
	errno := map[string]syscall.Errno{"ENOSPC": syscall.ENOSPC, "EIO": syscall.EIO}[parts[1]]
	after, _ := strconv.Atoi(parts[2])
	var n atomic.Int32
	journal.SetFaultForTest(func(path, op string) error {
		if !strings.HasSuffix(path, "session.actlet") || op != parts[0] {
			return nil
		}
		if int(n.Add(1)) > after {
			return errno
		}
		return nil
	})
}

// faultScript is a task of a few turns, each journaling several records.
func faultScript(turns int) string {
	var sb strings.Builder
	sb.WriteString("turns:\n")
	for i := range turns {
		fmt.Fprintf(&sb, "  - tool_calls:\n      - name: write_file\n        input: {path: f%d.txt, content: \"%d\"}\n", i, i)
	}
	sb.WriteString("  - text: done\n")
	return sb.String()
}

// The session journal fails mid-run, the disk full or a sync failing: the
// run stops with exit code 1, without reporting the task done, and a
// later run, with the disk fine again, recovers and finishes it.
func TestJournalFailsMidRun(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	for _, fault := range []string{"write:ENOSPC:12", "sync:EIO:12"} {
		t.Run(fault, func(t *testing.T) {
			g := newAgentGateway(t, faultScript(8))
			dir := sessionDir(t, nil)
			cmd := g.command(dir, "")
			cmd.Env = append(cmd.Env, journalFaultEnv+"="+fault)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if code := exitCode(t, cmd); code != session.ExitFailed {
				t.Fatalf("exit %d, want 1; stderr:\n%s", code, stderr.String())
			}
			completed := func(ev *gatewayv0.ActivityEvent) bool {
				return ev.GetState().GetTo() == gatewayv0.SessionState_SESSION_STATE_COMPLETED
			}
			cause := map[string]string{"write": "no space left on device", "sync": "input/output error"}[strings.Split(fault, ":")[0]]
			if g.eventCount(func(ev *gatewayv0.ActivityEvent) bool {
				st := ev.GetState()
				return st.GetTo() == gatewayv0.SessionState_SESSION_STATE_FAILED && strings.Contains(st.GetReason(), cause)
			}) != 1 {
				t.Errorf("no failed state with %q", cause)
			}
			if g.eventCount(completed) != 0 {
				t.Fatal("the failed run reported the task done")
			}
			if n := len(filesIn(t, filepath.Join(dir, "workspace"))); n >= 8 {
				t.Fatalf("the run wrote all %d files before the fault", n)
			}

			if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
				t.Fatalf("the run after: exit %d, want 0", code)
			}
			if n := len(filesIn(t, filepath.Join(dir, "workspace"))); n != 8 {
				t.Errorf("%d files written", n)
			}
		})
	}
}

func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// A journal damaged in the middle, not at its end, isn't recovered from:
// actlet run refuses it and says where, and actlet-cli journal inspect
// shows the same offset.
func TestJournalCorruptMidFile(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, strings.Replace(faultScript(6), "  - text: done\n", "  - delay: 60s\n    text: done\n", 1))
	dir := sessionDir(t, nil)
	cmd := g.worker(dir, "")
	waitFor(t, func() bool { return len(filesIn(t, filepath.Join(dir, "workspace"))) == 6 })
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if code := exitCode(t, cmd); code != session.ExitPaused {
		t.Fatalf("exit %d, want paused", code)
	}
	path := filepath.Join(dir, ".actlet", "session.actlet")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(data) / 2
	data[mid] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	run := g.command(dir, "")
	var stderr strings.Builder
	run.Stderr = &stderr
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(t, run); code != session.ExitFailed {
		t.Fatalf("exit %d, want 1", code)
	}
	offset := regexp.MustCompile(`corrupt at offset (\d+)`).FindStringSubmatch(stderr.String())
	if offset == nil || !strings.Contains(stderr.String(), "damaged before its end") || !strings.Contains(stderr.String(), "actlet-cli journal inspect") {
		t.Fatalf("stderr:\n%s", stderr.String())
	}
	if at, _ := strconv.Atoi(offset[1]); at > mid || at < mid-64<<10 {
		t.Errorf("reported offset %d, damaged byte at %d", at, mid)
	}
	inspect := exec.Command("go", "run", "../actlet-cli", "journal", "inspect", path)
	out, _ := inspect.CombinedOutput()
	if !strings.Contains(string(out), "offset "+offset[1]) {
		t.Errorf("journal inspect doesn't show offset %s:\n%s", offset[1], out)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A run that starts while the previous one still holds the session
// journal, as a replacement Pod may, waits for it to let go, then
// recovers from what it wrote last.
func TestRunWaitsForTheJournal(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, strings.Replace(faultScript(2), "  - text: done\n", "  - delay: 60s\n    text: done\n", 1))
	dir := sessionDir(t, nil)
	cmd := g.worker(dir, "")
	waitFor(t, func() bool { return len(filesIn(t, filepath.Join(dir, "workspace"))) == 2 })
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if code := exitCode(t, cmd); code != session.ExitPaused {
		t.Fatalf("exit %d, want paused", code)
	}
	held, err := journal.Open(filepath.Join(dir, ".actlet", "session.actlet"), journal.DefaultOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	if !held.Locked() {
		t.Skip("this filesystem can't lock")
	}
	next := g.command(dir, "")
	var stderr strings.Builder
	next.Stderr = &stderr
	if err := next.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	// It resumes as the session's second run; pause it again.
	waitFor(t, func() bool {
		for _, s := range g.srv.Sessions() {
			if s.Connected() && s.Epoch() == 2 {
				return true
			}
		}
		return false
	})
	_ = next.Process.Signal(syscall.SIGTERM)
	if code := exitCode(t, next); code != session.ExitPaused {
		t.Fatalf("the second run: exit %d, want paused; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "waiting for it to let go") {
		t.Errorf("stderr:\n%s", stderr.String())
	}
}
