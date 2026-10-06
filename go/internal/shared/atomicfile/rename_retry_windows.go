//go:build windows

package atomicfile

import (
	"errors"

	"golang.org/x/sys/windows"
)

// retryableRenameErr reports whether a refused rename may succeed if retried:
// the target is open in another process without delete sharing, or a scanner
// briefly holds (or hides) the temp file. These are the errors cmd/go's
// robustio treats as ephemeral on Windows.
func retryableRenameErr(err error) bool {
	var errno windows.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION, windows.ERROR_FILE_NOT_FOUND:
		return true
	}
	return false
}
