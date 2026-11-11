//go:build linux

package native

import "syscall"

// procAttr puts the container in its own process group (so a signal reaches
// everything it started) and ties its life to the engine.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
