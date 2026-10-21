// Package supervise keeps the processes Actlet's tools start from
// outliving a run, and lets Actlet run as a container's init process.
//
// Tools start programs with Start, each in a process group of its own,
// and the run ends with KillAll, which kills every group started and
// still there: programs left running in the background included. On
// Linux, Supervise also makes the process a child subreaper, so that
// descendants that leave their group and session, as daemons do, are
// adopted by it rather than by the system's init; KillAll then kills
// every descendant, and a reaper collects the adopted ones as they exit.
// On Windows, which has no process groups, each program runs in a job
// object of its own, which its descendants can't leave. On macOS daemons
// can escape.
package supervise

import (
	"os/exec"
	"sync"
	"time"
)

var (
	// startMu is held for reading while a program starts and is recorded,
	// and for writing while the reaper looks for orphans, so that it
	// never takes a program os/exec is about to wait for.
	startMu sync.RWMutex

	mu       sync.Mutex
	groups   = map[int]bool{} // process groups started, by ID
	children = map[int]bool{} // programs os/exec waits for, by PID
)

// Start starts cmd, which leads a process group of its own, and records
// it. Call Done once cmd.Wait has returned.
func Start(cmd *exec.Cmd) error {
	startMu.RLock()
	defer startMu.RUnlock()
	prepare(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	started(cmd)
	mu.Lock()
	children[cmd.Process.Pid] = true
	if leadsGroup(cmd) {
		groups[cmd.Process.Pid] = true
	}
	mu.Unlock()
	return nil
}

// Done records that cmd was waited for. Its process group, if any of it
// is left, is still killed by KillAll.
func Done(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	mu.Lock()
	delete(children, cmd.Process.Pid)
	mu.Unlock()
	prune()
}

// prune forgets process groups that have ended, so that KillAll never
// signals a group whose ID was used again.
func prune() {
	mu.Lock()
	defer mu.Unlock()
	for g := range groups {
		if !children[g] && !groupAlive(g) {
			delete(groups, g)
			releaseGroup(g)
		}
	}
}

// Groups returns the process groups started and not known to have ended.
func Groups() []int {
	prune()
	mu.Lock()
	defer mu.Unlock()
	out := make([]int, 0, len(groups))
	for g := range groups {
		out = append(out, g)
	}
	return out
}

// KillGroup kills a program Start started together with everything it
// started: its process group, or on Windows its job object.
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		killGroup(cmd.Process.Pid)
	}
}

// KillAll kills what tools started that is still running: every process
// group they started and, on Linux after Supervise, every descendant of
// this process. It waits up to a second for them to go.
func KillAll() {
	for _, g := range Groups() {
		killGroup(g)
	}
	killDescendants()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && (len(Groups()) > 0 || descendants() > 0) {
		time.Sleep(10 * time.Millisecond)
	}
}
