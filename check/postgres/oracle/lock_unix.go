//go:build !windows

package oracle

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f, waiting for it.
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }
