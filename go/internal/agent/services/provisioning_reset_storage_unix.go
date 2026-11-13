//go:build unix

package services

import (
	"os"
	"syscall"
)

const supportsTrustedResetStorage = true

// Saved HTTPS-authenticated keys are authority only in protected LOCAL state.
// Root on devices (the current account in native tests) must own the file and
// directory; neither another account nor a group may replace that authority.
func trustedResetStorage(file, directory os.FileInfo) bool {
	f, ok := file.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	d, ok := directory.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return file.Mode().IsRegular() && file.Mode().Perm() == 0600 && directory.IsDir() && directory.Mode().Perm()&0022 == 0 && int(f.Uid) == os.Geteuid() && int(d.Uid) == os.Geteuid()
}
