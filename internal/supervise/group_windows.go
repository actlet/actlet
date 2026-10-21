package supervise

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no process groups. Start puts each program in a job object
// of its own instead, before it runs a single instruction: the program is
// created suspended, assigned to the job and only then resumed, so that
// everything it starts belongs to the job too. Killing the job kills them
// all, as killing a process group does elsewhere. Git's bash.exe, for one,
// is a launcher that runs the real bash as its child: ending only the
// launcher would leave the script running.

// jobs are the job objects of programs started, by their process ID;
// guarded by mu.
var jobs = map[int]windows.Handle{}

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// prepare creates the program suspended, for started to put it in a job.
func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// started puts the suspended program in a job object of its own and
// resumes it. Without a job, it still runs: it is then killed alone.
func started(cmd *exec.Cmd) {
	pid := cmd.Process.Pid
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(pid))
	if err != nil {
		return // it can't be resumed either; Kill ends it
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if job, err := newJob(); err == nil {
		if windows.AssignProcessToJobObject(job, h) == nil {
			mu.Lock()
			jobs[pid] = job
			mu.Unlock()
		} else {
			_ = windows.CloseHandle(job)
		}
	}
	_, _, _ = ntResumeProcess.Call(uintptr(h))
}

// newJob creates a job object whose processes die when its last handle
// closes, as when Actlet itself exits.
func newJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// leadsGroup reports whether the program got a job object. The caller
// holds mu.
func leadsGroup(cmd *exec.Cmd) bool {
	_, ok := jobs[cmd.Process.Pid]
	return ok
}

// groupAlive reports whether a program's job has processes left. The
// caller holds mu.
func groupAlive(pid int) bool {
	job, ok := jobs[pid]
	if !ok {
		return false
	}
	var info jobAccounting
	err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	return err == nil && info.ActiveProcesses > 0
}

// jobAccounting is Windows' JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

// killGroup kills every process in a program's job.
func killGroup(pid int) {
	mu.Lock()
	job, ok := jobs[pid]
	mu.Unlock()
	if ok {
		_ = windows.TerminateJobObject(job, 1)
	}
}

// releaseGroup closes the job object of a program whose processes have
// all ended. The caller holds mu.
func releaseGroup(pid int) {
	if job, ok := jobs[pid]; ok {
		_ = windows.CloseHandle(job)
		delete(jobs, pid)
	}
}
