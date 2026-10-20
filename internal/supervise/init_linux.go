package supervise

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Environment variables of the init process.
const (
	// InitChildEnv marks the child an init process starts, which mustn't
	// start another.
	InitChildEnv = "ACTLET_INIT_CHILD"
	// NoInitEnv set to "true" turns the init process off, as when the
	// container already has one, such as docker run --init.
	NoInitEnv = "ACTLET_NO_INIT"
)

// forwarded are the signals the init process passes on to its child.
var forwarded = []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT,
	syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH}

// ShouldInit reports whether this process should be an init process: it
// is a container's first process, PID 1, which the kernel gives orphans
// to reap and no default signal handling.
func ShouldInit() bool {
	return os.Getpid() == 1 && os.Getenv(InitChildEnv) == "" && os.Getenv(NoInitEnv) != "true"
}

// Init runs this program again as its child, with the same arguments,
// as tini does: it passes signals on to the child, reaps every process
// that exits, and when the child exits kills every process left in the
// container and returns the child's exit code.
func Init() int {
	return runInit(os.Args, func() { _ = syscall.Kill(-1, syscall.SIGKILL) })
}

// runInit is Init, with killRest killing what the child left.
func runInit(argv []string, killRest func()) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "actlet init: %v\n", err)
		return 1
	}
	sigs := make(chan os.Signal, 32)
	signal.Notify(sigs, append([]os.Signal{syscall.SIGCHLD}, forwarded...)...)
	defer signal.Stop(sigs)
	child, err := os.StartProcess(exe, argv, &os.ProcAttr{
		Env:   append(os.Environ(), InitChildEnv+"=1"),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "actlet init: starting %s: %v\n", exe, err)
		return 1
	}
	code := -1
	for code < 0 {
		sig := <-sigs
		if sig != syscall.SIGCHLD {
			_ = child.Signal(sig)
		}
		code = reapAll(child.Pid)
	}
	killRest()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) {
			break
		}
		if pid <= 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return code
}

// reapAll reaps every child that has exited, and returns the exit code of
// the one with PID child once it has, or -1.
func reapAll(child int) int {
	code := -1
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err != nil || pid <= 0 {
			return code
		}
		if pid == child {
			code = ws.ExitStatus()
			if ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
		}
	}
}
