package privhelper

import "syscall"

// dieWithParent has the kernel kill the process when the helper goes away.
func dieWithParent(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
