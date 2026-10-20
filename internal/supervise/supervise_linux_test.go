package supervise

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper modes, by ACTLET_SUPERVISE_HELPER: the test binary runs
// again as a supervisor, an init process, or a program that leaves
// processes behind.
const helperEnv = "ACTLET_SUPERVISE_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "sleep":
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	case "spawn":
		os.Exit(spawn())
	case "supervisor":
		os.Exit(supervisor())
	case "init":
		if os.Getenv(InitChildEnv) != "" {
			os.Exit(child()) // started by the init process
		}
		os.Exit(initHelper())
	}
	os.Exit(2)
}

// helper returns the test binary as a helper.
func helper(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	return cmd
}

// spawn leaves a daemon, in a session of its own, and a process that
// exits at once, and prints the daemon's PID.
func spawn() int {
	d := helper("sleep")
	d.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := d.Start(); err != nil {
		return 1
	}
	q := exec.Command("true")
	q.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = q.Start()
	fmt.Println(d.Process.Pid)
	return 0
}

// descendantsOf returns a process's descendants, zombies included, and
// whether any is a zombie.
func descendantsOf(root int) (pids []int, zombie bool) {
	ps := procs()
	queue := []int{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for pid, pr := range ps {
			if pr.ppid == p {
				pids = append(pids, pid)
				zombie = zombie || pr.zombie
				queue = append(queue, pid)
			}
		}
	}
	return pids, zombie
}

// supervisor supervises a program that leaves a daemon, adopts the
// daemon, and kills it with KillAll.
func supervisor() int {
	stop := Supervise()
	cmd := helper("spawn")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	cmd.Stdout = &out
	if err := Start(cmd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = cmd.Wait()
	Done(cmd)
	daemon, _ := strconv.Atoi(strings.TrimSpace(out.String()))
	time.Sleep(200 * time.Millisecond)
	if p := procs()[daemon]; p.ppid != os.Getpid() {
		fmt.Fprintf(os.Stderr, "the daemon %d wasn't adopted: parent %d\n", daemon, p.ppid)
		return 1
	}
	KillAll()
	stop()
	if pids, _ := descendantsOf(os.Getpid()); len(pids) > 0 {
		for _, p := range pids {
			stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p))
			fmt.Fprintf(os.Stderr, "left: %s\n", stat)
		}
		return 1
	}
	return 0
}

// A daemon a tool's program leaves behind is adopted, killed by KillAll,
// and reaped: nothing is left, not even a zombie.
func TestSupervisorKillsDaemons(t *testing.T) {
	cmd := helper("supervisor")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

// child is an init process's child: it leaves a daemon and a process
// that exits, waits for SIGTERM, and exits 7.
func child() int {
	cmd := helper("spawn")
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		return 1
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	fmt.Println("ready")
	<-sig
	return 7
}

// initHelper is an init process, as PID 1 is, but by being a subreaper.
// It checks that nothing is left after its child exits.
func initHelper() int {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		return 1
	}
	code := runInit([]string{os.Args[0], "-test.run=^$"}, killDescendants)
	if pids, zombie := descendantsOf(os.Getpid()); len(pids) > 0 || zombie {
		fmt.Fprintf(os.Stderr, "left: %v (zombies %v)\n", pids, zombie)
		return 1
	}
	return code
}

// The init process passes a signal on to its child, returns the child's
// exit code, and reaps and kills what the child left behind.
func TestInit(t *testing.T) {
	cmd := helper("init")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	var got strings.Builder
	for !strings.Contains(got.String(), "ready") {
		n, err := stdout.Read(buf)
		if err != nil {
			t.Fatalf("the child never got ready: %q, %v", got.String(), err)
		}
		got.Write(buf[:n])
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Fatalf("init exited with %v, want 7", err)
	}
}

// As a container's PID 1, actlet runs itself as its child: the child's
// orphans are reaped by it, and once the child exits, what the child left
// is killed and its exit code returned. Run by make test-linux, which
// starts the test binary as a container's first process.
func TestAsPID1(t *testing.T) {
	if os.Getenv(InitChildEnv) != "" {
		// The child: leave a daemon and a process that exits at once.
		out, err := helper("spawn").Output()
		if err != nil {
			t.Fatal(err)
		}
		daemon, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		time.Sleep(300 * time.Millisecond)
		for pid, p := range procs() {
			if p.ppid == 1 && p.zombie {
				fmt.Fprintf(os.Stderr, "zombie %d not reaped by the init process\n", pid)
				os.Exit(1)
			}
		}
		if procs()[daemon].ppid != 1 {
			fmt.Fprintf(os.Stderr, "the daemon %d wasn't adopted by the init process\n", daemon)
			os.Exit(1)
		}
		os.Exit(7)
	}
	if os.Getpid() != 1 {
		t.Skip("only as a container's PID 1: make test-linux")
	}
	if !ShouldInit() {
		t.Fatal("PID 1 doesn't run as an init process")
	}
	if code := Init(); code != 7 {
		t.Fatalf("exit code %d, want 7", code)
	}
	if ps := procs(); len(ps) != 1 {
		t.Errorf("processes left: %v", ps)
	}
}
