//go:build unix

package profile

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f, blocking until it is free.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

// unlockFile releases a lock taken by lockFile.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
