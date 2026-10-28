//go:build !unix

package context

import "os"

// hasMultipleLinks reports whether the file has more than one hard link. The
// link count is not available here, so the atomic rename is always used.
func hasMultipleLinks(os.FileInfo) bool { return false }
