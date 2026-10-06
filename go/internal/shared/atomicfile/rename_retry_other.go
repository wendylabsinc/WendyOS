//go:build !windows

package atomicfile

// retryableRenameErr is false off Windows: a refused rename there is final,
// so replaceFile renames once.
func retryableRenameErr(error) bool { return false }
