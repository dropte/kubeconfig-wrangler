package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// lockPathFor returns the advisory lock file that guards storePath and the
// key material next to it: profiles.json -> profiles.lock.
func lockPathFor(storePath string) string {
	return strings.TrimSuffix(storePath, filepath.Ext(storePath)) + ".lock"
}

// withFileLock runs fn while holding an exclusive cross-process advisory lock
// on lockPath. Every process that uses the same profile store (the desktop
// app, "serve", CLI commands) takes this lock around key creation and around
// each read-modify-write of the profiles file, so concurrent writers do not
// lose each other's changes or create two different keys.
func withFileLock(lockPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	defer f.Close()
	if err := lockFile(f); err != nil {
		return fmt.Errorf("failed to lock %s: %w", lockPath, err)
	}
	defer func() { _ = unlockFile(f) }() // closing the file releases it too
	return fn()
}
