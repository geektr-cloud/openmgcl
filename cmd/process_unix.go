//go:build unix

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// shutdownSignals are forwarded to the child's process group. SIGHUP and
// SIGQUIT are included so closing a terminal or pressing Ctrl-\ does not leave
// the child behind.
var shutdownSignals = []os.Signal{
	syscall.SIGHUP,
	syscall.SIGINT,
	syscall.SIGQUIT,
	syscall.SIGTERM,
}

// configureChildProcess puts the child in its own process group so a signal can
// be forwarded to the whole subtree instead of only the direct child.
func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// forwardSignal sends sig to the child's process group. If the group no longer
// exists, it falls back to signaling the direct child.
func forwardSignal(cmd *exec.Cmd, sig os.Signal) {
	if cmd.Process == nil {
		return
	}
	unixSig, ok := sig.(syscall.Signal)
	if !ok {
		_ = cmd.Process.Signal(sig)
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, unixSig); err != nil {
		_ = cmd.Process.Signal(unixSig)
	}
}

// killChildProcessGroup force-kills every process in the child's group.
func killChildProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// processGroupAlive reports whether any process remains in the child's group.
func processGroupAlive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	err := syscall.Kill(-cmd.Process.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// reapAdoptedChildren reaps zombies reparented to us when running as PID 1.
// Without this, a dead grandchild keeps its process group visible and
// processGroupAlive would wait out the entire grace period. It is a no-op for
// ordinary processes and never blocks.
func reapAdoptedChildren() {
	if os.Getpid() != 1 {
		return
	}
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}

// signalExitCode returns the conventional shell exit code for a signal.
func signalExitCode(sig os.Signal) int {
	if unixSig, ok := sig.(syscall.Signal); ok {
		return 128 + int(unixSig)
	}
	return 1
}

// processExitCode returns the exit code for a finished child, mapping signal
// termination to 128+signal.
func processExitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	if status, ok := ps.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	code := ps.ExitCode()
	if code < 0 {
		return 1
	}
	return code
}
