//go:build !unix && !windows

package profile

import "os"

// lockFile is a no-op on platforms without advisory file locking.
func lockFile(*os.File) error { return nil }

// unlockFile is a no-op on platforms without advisory file locking.
func unlockFile(*os.File) error { return nil }
