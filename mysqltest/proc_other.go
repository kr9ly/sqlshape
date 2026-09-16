//go:build !linux

package mysqltest

import "syscall"

func sysProcAttr() *syscall.SysProcAttr { return nil }
