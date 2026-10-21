package supervise

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// helperEnv makes the test binary a helper: "parent" starts a "child" and
// prints its process ID, and both then wait to be killed.
const helperEnv = "ACTLET_SUPERVISE_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "parent":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), helperEnv+"=child")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "child":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// exited waits up to d for a process to end.
func exited(pid int, d time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // gone already
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ev, _ := windows.WaitForSingleObject(h, uint32(d.Milliseconds()))
	return ev == windows.WAIT_OBJECT_0
}

// Killing a program's group kills what it started too: Start puts it in
// a job object before it runs, and its children join the job.
func TestKillGroupKillsChildren(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"=parent")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if got := Groups(); len(got) != 1 || got[0] != cmd.Process.Pid {
		t.Errorf("groups %v, want the program's job", got)
	}
	KillGroup(cmd)
	_ = cmd.Wait()
	Done(cmd)
	if !exited(child, 5*time.Second) {
		t.Fatal("the program's child outlived it")
	}
	if got := Groups(); len(got) != 0 {
		t.Errorf("groups %v after the job ended", got)
	}
}
