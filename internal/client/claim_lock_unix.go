//go:build linux || darwin

package client

import (
	"os"
	"syscall"
)

func lockClaim(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }
