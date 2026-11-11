//go:build !linux

package native

import "syscall"

// procAttr starts the container detached from the terminal's signals.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
