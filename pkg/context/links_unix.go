//go:build unix

package context

import (
	"os"
	"syscall"
)

// hasMultipleLinks reports whether the file has more than one hard link.
func hasMultipleLinks(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}
