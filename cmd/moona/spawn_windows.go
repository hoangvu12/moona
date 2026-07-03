//go:build windows

package main

import "syscall"

// detachedSysProcAttr makes a spawned child survive its parent and run without a
// console window of its own. DETACHED_PROCESS (0x8) gives it no console (the
// daemon is a server; ConPTY sessions create their own pseudoconsoles), and
// CREATE_NEW_PROCESS_GROUP (0x200) keeps a Ctrl-C in the launching terminal from
// propagating to it.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200,
	}
}
