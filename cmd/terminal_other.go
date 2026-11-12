//go:build !linux

package cmd

import "os/exec"

// handoffTerminalToChild is a no-op outside Linux. The Linux implementation
// performs the terminal foreground process group handoff required for
// interactive children.
func handoffTerminalToChild(child *exec.Cmd) func() {
	return func() {}
}
