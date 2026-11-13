//go:build !windows

package vm

import (
	"errors"
	"os"
	"syscall"
)

func truncateSparse(file *os.File, size int64) error {
	return file.Truncate(size)
}

// IsDiskFull recognizes disk-full errors through wrapped image-write failures.
func IsDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
