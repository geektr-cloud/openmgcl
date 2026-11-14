//go:build !unix

package cmd

import (
	"os"
	"os/exec"
)

var shutdownSignals = []os.Signal{os.Interrupt}

func configureChildProcess(cmd *exec.Cmd) {}

func forwardSignal(cmd *exec.Cmd, sig os.Signal) {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(sig)
	}
}

func killChildProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func processGroupAlive(cmd *exec.Cmd) bool { return false }

func reapAdoptedChildren() {}

func signalExitCode(sig os.Signal) int {
	if sig == os.Interrupt {
		return 130
	}
	return 1
}

func processExitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	code := ps.ExitCode()
	if code < 0 {
		return 1
	}
	return code
}

func processSignaled(ps *os.ProcessState) bool { return false }
