package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

func (e *Env) shellTool() *tool {
	return &tool{
		spec: Spec{
			Name: Shell,
			Description: fmt.Sprintf("Run a command with %s in workspace/ and return its exit code, stdout and stderr. "+
				"Each call starts in workspace/ with a fresh shell. Commands run until they exit or time out "+
				"(default %v, at most %v); output is capped at %d KiB per stream. A command is never run again "+
				"automatically.", e.shellName(), e.shellCfg.timeout, e.shellCfg.maxTimeout, e.shellCfg.outputCap>>10),
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"command":{"type":"string","description":"the command line"},` +
				`"timeout_seconds":{"type":"integer","minimum":1},` + writesSchema + `},` +
				`"required":["command"],"additionalProperties":false}`),
			Policy: PolicyNever,
		},
		writes: e.programWrites,
		prepare: func(in json.RawMessage) (*Call, error) {
			var args struct {
				Command string   `json:"command"`
				Timeout int      `json:"timeout_seconds"`
				Writes  []string `json:"writes"`
			}
			if err := decode(in, &args); err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.Command) == "" {
				return nil, errors.New("command is required")
			}
			timeout, err := e.timeout(args.Timeout)
			if err != nil {
				return nil, err
			}
			writes, err := e.writeSet(args.Writes)
			if err != nil {
				return nil, err
			}
			argv := append(slices.Clone(e.shell), args.Command)
			return &Call{writes: writes, writesAll: writes == nil, run: func(ctx RunContext) Result {
				return e.runProgram(ctx, argv, nil, timeout).result()
			}}, nil
		},
	}
}

func (e *Env) shellName() string {
	if len(e.shell) == 0 {
		return "the shell"
	}
	return e.shell[0]
}

func (e *Env) timeout(seconds int) (time.Duration, error) {
	if seconds < 0 {
		return 0, errors.New("timeout_seconds must be positive")
	}
	if seconds == 0 {
		return e.shellCfg.timeout, nil
	}
	d := time.Duration(seconds) * time.Second
	if d > e.shellCfg.maxTimeout {
		return 0, fmt.Errorf("timeout_seconds may be at most %d", int(e.shellCfg.maxTimeout/time.Second))
	}
	return d, nil
}

// programRun is how a program run went.
type programRun struct {
	stdout, stderr *capBuffer
	exitCode       int
	took           time.Duration
	startErr       error
	timedOut       bool
	interrupted    bool // asked to stop by a pause or stop
	killed         bool
}

// runProgram runs argv in the workspace, in a process group of its own.
// The soft context's end sends the group SIGTERM and the hard one's
// SIGKILL; a timeout sends SIGTERM, then SIGKILL after a grace period.
func (e *Env) runProgram(ctx RunContext, argv, extraEnv []string, timeout time.Duration) *programRun {
	r := &programRun{stdout: newCapBuffer(e.shellCfg.outputCap), stderr: newCapBuffer(e.shellCfg.outputCap), exitCode: -1}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = e.workspace
	cmd.Env = append(toolEnv(), extraEnv...)
	cmd.Stdout, cmd.Stderr = r.stdout, r.stderr
	// Children left running in the background may hold the output open.
	cmd.WaitDelay = 2 * time.Second
	newProcessGroup(cmd)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		r.startErr = err
		return r
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	soft, hard := ctx.Soft.Done(), ctx.Hard.Done()
	var grace <-chan time.Time
	for {
		select {
		case err := <-done:
			r.took = time.Since(start)
			var ee *exec.ExitError
			switch {
			case err == nil:
				r.exitCode = 0
			case errors.As(err, &ee):
				r.exitCode = ee.ExitCode()
			default:
				r.exitCode = cmd.ProcessState.ExitCode()
			}
			return r
		case <-timer.C:
			r.timedOut = true
			terminate(cmd)
			grace = time.After(e.shellCfg.termGrace)
		case <-soft:
			soft = nil
			r.interrupted = true
			terminate(cmd)
		case <-hard:
			hard = nil
			r.killed = true
			kill(cmd)
		case <-grace:
			r.killed = true
			kill(cmd)
		}
	}
}

// result describes the run for the model.
func (r *programRun) result() Result {
	if r.startErr != nil {
		return errorResult("couldn't start: %v", r.startErr)
	}
	var sb strings.Builder
	failed := r.exitCode != 0
	switch {
	case r.timedOut:
		fmt.Fprintf(&sb, "timed out after %v and was stopped", r.took.Round(time.Second))
	case r.interrupted || r.killed:
		fmt.Fprintf(&sb, "interrupted after %v: the session is pausing or stopping. It may have run partly", r.took.Round(100*time.Millisecond))
		failed = true
	default:
		fmt.Fprintf(&sb, "exit code %d", r.exitCode)
	}
	if r.exitCode >= 0 && (r.timedOut || r.interrupted || r.killed) {
		fmt.Fprintf(&sb, " (exit code %d)", r.exitCode)
	}
	sb.WriteString("\n")
	writeStream(&sb, "stdout", r.stdout)
	writeStream(&sb, "stderr", r.stderr)
	return Result{Output: sb.String(), IsError: failed || r.timedOut}
}

func writeStream(sb *strings.Builder, name string, b *capBuffer) {
	s := b.String()
	if s == "" {
		return
	}
	fmt.Fprintf(sb, "%s:\n%s", name, s)
	if !strings.HasSuffix(s, "\n") {
		sb.WriteString("\n")
	}
}

// capBuffer keeps the start and the end of what is written to it, within
// a limit.
type capBuffer struct {
	mu    sync.Mutex
	limit int
	head  []byte
	tail  []byte // a ring once full
	pos   int    // next write position in tail
	total int64
}

func newCapBuffer(limit int) *capBuffer { return &capBuffer{limit: limit} }

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	written := len(p)
	c.total += int64(written)
	headMax := c.limit * 2 / 3
	tailMax := c.limit - headMax
	if n := min(headMax-len(c.head), len(p)); n > 0 {
		c.head = append(c.head, p[:n]...)
		p = p[n:]
	}
	for len(p) > 0 {
		if len(c.tail) < tailMax {
			n := min(tailMax-len(c.tail), len(p))
			c.tail = append(c.tail, p[:n]...)
			p = p[n:]
			continue
		}
		n := copy(c.tail[c.pos:], p)
		c.pos = (c.pos + n) % tailMax
		p = p[n:]
	}
	return written, nil
}

func (c *capBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	tail := slices.Concat(c.tail[c.pos:], c.tail[:c.pos])
	kept := int64(len(c.head) + len(tail))
	if kept == c.total {
		return string(c.head) + string(tail)
	}
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", validPrefix(string(c.head)), c.total-kept, validSuffix(string(tail)))
}
