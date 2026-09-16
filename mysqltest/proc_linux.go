package mysqltest

import "syscall"

// sysProcAttr asks the kernel to SIGTERM mysqld when the test binary is gone (a panic, an
// os.Exit), so a server shared under Main never outlives the process that booted it.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM} }
