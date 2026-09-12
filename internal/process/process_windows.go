//go:build windows

package process

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobObject is the Windows process-tree control: every descendant of the child
// belongs to the same Job Object, so one call stops the whole tree.
//
// The job is created with kill-on-close, which is the safety net: if this
// process exits unexpectedly, the operating system terminates whatever the job
// still contains rather than leaving orphaned analyzers behind.
type jobObject struct {
	handle windows.Handle
}

func newJobControl() (jobControl, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(handle, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return &jobObject{handle: handle}, nil
}

// prepare creates the child suspended.
//
// Windows has no way to assign a process to a job atomically at creation
// through os/exec, so there is a window between creation and assignment. A
// child that ran during that window could spawn a descendant outside the job
// and escape termination entirely. Starting suspended closes the window: the
// child executes no instruction until it has already been assigned.
func (j *jobObject) prepare(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP
	return nil
}

// started assigns the suspended child to the job and only then lets it run.
func (j *jobObject) started(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return syscall.EINVAL
	}
	pid := uint32(cmd.Process.Pid)
	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(j.handle, handle); err != nil {
		return err
	}
	return resumeProcess(pid)
}

// resumeProcess resumes every thread of the newly created process.
//
// os/exec does not expose the primary thread handle that ResumeThread would
// normally take, so the threads are found through the Tool Help snapshot. A
// process created suspended has exactly one thread, but every thread owned by
// the process is resumed so the code does not depend on that.
func resumeProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	var firstErr error
	for {
		if entry.OwnerProcessID == pid {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
			} else {
				if _, err := windows.ResumeThread(thread); err != nil && firstErr == nil {
					firstErr = err
				}
				windows.CloseHandle(thread)
			}
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			// The walk ends with ERROR_NO_MORE_FILES, which is not a failure.
			return firstErr
		}
	}
}

// terminate stops the tree. The graceful attempt is a console control event to
// the child's process group, which a console application can handle; a process
// with no console never receives it, which is exactly why the runner follows
// with the forced path after the grace period.
//
// The forced path terminates the job, which reaches every descendant at once.
func (j *jobObject) terminate(cmd *exec.Cmd, force bool) {
	if force {
		windows.TerminateJobObject(j.handle, 1)
		return
	}
	if cmd.Process != nil {
		windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid))
	}
}

// close releases the job. Kill-on-close means anything still in it dies here,
// so no analyzer outlives the run that started it.
func (j *jobObject) close() {
	if j.handle != 0 {
		windows.CloseHandle(j.handle)
		j.handle = 0
	}
}

// signalOf has no Windows meaning: a process terminated by the job reports an
// ordinary exit code, not a signal.
func signalOf(*exec.ExitError) (int, bool) { return 0, false }
