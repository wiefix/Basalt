//go:build !linux

package privhelper

import "syscall"

// dieWithParent: only Linux has it; a worker also leaves when its input
// closes.
func dieWithParent(*syscall.SysProcAttr) {}
