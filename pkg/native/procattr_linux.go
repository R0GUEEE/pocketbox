//go:build linux

package native

import "syscall"

// procAttr puts the container in its own process group so a signal reaches
// everything it started. It deliberately does not set Pdeathsig: the iSH host
// kernel rejects the clone flags Go uses to wire up parent-death signals
// (EINVAL on exec), and the engine reconciles orphans on start instead.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// chrootSupported reports whether the runner can hand a process the image as its
// root. On a Linux host it can; inside iSH the host syscall layer refuses, and
// processes run with the image as their working directory instead.
func chrootSupported() bool { return true }
