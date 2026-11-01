//go:build !linux

package agent

import "syscall"

// dieWithParent: only Linux has it; a worker also leaves when its input
// (the agent) closes.
func dieWithParent(*syscall.SysProcAttr) {}
