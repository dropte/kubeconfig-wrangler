//go:build windows

package profile

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on f, blocking until it is free.
func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol)
}

// unlockFile releases a lock taken by lockFile.
func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
