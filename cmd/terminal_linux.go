//go:build linux

package cmd

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/term"
)

// handoffTerminalToChild makes the child's process group the foreground group
// of the controlling terminal, returning a function that restores the parent
// group. Without this, a child in its own process group is a background job and
// gets SIGTTIN as soon as it reads from the terminal.
//
// The handoff only happens when stdin is a terminal and the parent is currently
// its foreground group. A background invocation is left alone so it keeps
// normal background-job semantics.
func handoffTerminalToChild(child *exec.Cmd) func() {
	noop := func() {}
	if child.Process == nil {
		return noop
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return noop
	}

	parentPgrp := syscall.Getpgrp()
	foreground, err := tcGetPgrp(fd)
	if err != nil || foreground != parentPgrp {
		return noop
	}

	// The parent is a background process group while the child owns the
	// terminal. Ignore SIGTTOU for that window so terminal ioctls and writes to
	// stderr (when TOSTOP is enabled) cannot stop the parent. The child has
	// already been exec'd, so it does not inherit this disposition.
	signal.Ignore(syscall.SIGTTOU)
	if err := tcSetPgrp(fd, child.Process.Pid); err != nil {
		signal.Reset(syscall.SIGTTOU)
		return noop
	}

	// Resume the child if it raced us and stopped on SIGTTIN before the
	// foreground handoff completed.
	_ = syscall.Kill(-child.Process.Pid, syscall.SIGCONT)

	return func() {
		_ = tcSetPgrp(fd, parentPgrp)
		signal.Reset(syscall.SIGTTOU)
	}
}

func tcGetPgrp(fd int) (int, error) {
	var pgrp int32
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(syscall.TIOCGPGRP),
		uintptr(unsafe.Pointer(&pgrp)),
	)
	if errno != 0 {
		return 0, errno
	}
	return int(pgrp), nil
}

func tcSetPgrp(fd int, pgrp int) error {
	value := int32(pgrp)
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(syscall.TIOCSPGRP),
		uintptr(unsafe.Pointer(&value)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
