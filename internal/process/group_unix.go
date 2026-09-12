//go:build !windows

package process

import (
	"io/fs"
	"os/exec"
	"syscall"
)

// processGroup puts the child in its own process group so that every descendant
// it starts is signalled together with it. Signalling only the child would leave
// a tool's own workers running with the materialized inputs still open.
type processGroup struct{}

func newJobControl() (jobControl, error) { return processGroup{}, nil }

func (processGroup) prepare(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Setpgid makes the child a group leader, so its group ID is its PID and
	// the whole tree can be addressed as one negative PID.
	cmd.SysProcAttr.Setpgid = true
	return nil
}

func (processGroup) started(*exec.Cmd) error { return nil }

// terminate signals the whole group: SIGTERM first so a tool can shut down and
// remove its own temporary files, SIGKILL when it did not.
func (processGroup) terminate(cmd *exec.Cmd, force bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	// A negative PID addresses the group. The caller guarantees the child has
	// not been reaped yet, which is what keeps the group alive and its
	// identifier unambiguous.
	syscall.Kill(-cmd.Process.Pid, sig)
}

func (processGroup) close() {}

// signalOf reports the signal that killed a child, if it was killed by one.
func signalOf(err *exec.ExitError) (int, bool) {
	var status syscall.WaitStatus
	sys, ok := err.Sys().(syscall.WaitStatus)
	if !ok {
		return 0, false
	}
	status = sys
	if !status.Signaled() {
		return 0, false
	}
	return int(status.Signal()), true
}

// isExecutable reports whether the file carries an execute bit. A tool the
// operator approved but did not make executable is a configuration error, not
// a run to attempt.
func isExecutable(info fs.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}
