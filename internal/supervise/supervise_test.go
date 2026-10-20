//go:build !windows

package supervise

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// alive reports whether a process runs; a zombie doesn't.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A program that leaves a child running in the background, in its
// process group, ends; KillAll kills the child.
func TestKillAllGroups(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	cmd.Stdout = &out
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	Done(cmd)
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil || !alive(pid) {
		t.Fatalf("background child %q: %v", out.String(), err)
	}
	if g := Groups(); len(g) != 1 || g[0] != cmd.Process.Pid {
		t.Errorf("groups %v", g)
	}
	KillAll()
	waitGone(t, pid)
	if g := Groups(); len(g) != 0 {
		t.Errorf("groups left: %v", g)
	}
}
