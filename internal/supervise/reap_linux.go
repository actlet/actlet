package supervise

import (
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// prSetChildSubreaper is prctl's PR_SET_CHILD_SUBREAPER.
const prSetChildSubreaper = 36

var supervised atomic.Bool

// Supervise makes this process a child subreaper, so that its orphaned
// descendants are adopted by it, and reaps them as they exit. Programs
// that os/exec waits for are left to it: every program this process
// starts must go through Start. stop ends the reaping.
func Supervise() (stop func()) {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		return func() {}
	}
	supervised.Store(true)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second) // in case a signal was missed
		defer t.Stop()
		for {
			select {
			case <-sig:
			case <-t.C:
			case <-done:
				return
			}
			reapOrphans()
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
		// A process whose threads are still exiting shows as a zombie
		// before it can be reaped.
		deadline := time.Now().Add(2 * time.Second)
		for reapOrphans() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// reapOrphans waits for the children that have exited and that os/exec
// doesn't wait for: adopted orphans. It returns how many it couldn't
// reap yet.
func reapOrphans() (left int) {
	startMu.Lock()
	defer startMu.Unlock()
	self := os.Getpid()
	for pid, p := range procs() {
		if p.ppid == self && p.zombie && !waiting(pid) {
			var ws syscall.WaitStatus
			if got, _ := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); got != pid {
				left++
			}
		}
	}
	return left
}

// waiting reports whether os/exec waits for a process.
func waiting(pid int) bool {
	mu.Lock()
	defer mu.Unlock()
	return children[pid]
}

type proc struct {
	ppid, pgid int
	zombie     bool
}

// procs reads every process's parent and state from /proc.
func procs() map[int]proc {
	out := map[int]proc{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...; comm may hold spaces and parentheses.
		s := string(data)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 3 {
			continue
		}
		ppid, err1 := strconv.Atoi(f[1])
		pgid, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil {
			continue
		}
		out[pid] = proc{ppid: ppid, pgid: pgid, zombie: f[0] == "Z" || f[0] == "X"}
	}
	return out
}

// live returns this process's descendants that haven't exited.
func live() []int {
	ps := procs()
	kids := map[int][]int{}
	for pid, p := range ps {
		kids[p.ppid] = append(kids[p.ppid], pid)
	}
	var out []int
	queue := []int{os.Getpid()}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, k := range kids[pid] {
			if !ps[k].zombie {
				out = append(out, k)
			}
			queue = append(queue, k)
		}
	}
	return out
}

func killDescendants() {
	for _, pid := range live() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func descendants() int { return len(live()) }

// groupAlive reports whether a process group has members that haven't
// exited.
func groupAlive(pgid int) bool {
	for _, p := range procs() {
		if p.pgid == pgid && !p.zombie {
			return true
		}
	}
	return false
}
