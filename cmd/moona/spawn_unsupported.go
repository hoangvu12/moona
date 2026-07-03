//go:build !windows

package main

import "syscall"

// detachedSysProcAttr is a no-op on non-Windows platforms; moona only runs its
// daemon on Windows, but the stub keeps cross-platform builds compiling.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
