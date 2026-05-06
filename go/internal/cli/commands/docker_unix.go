//go:build darwin || linux

package commands

import "os"

// linkOrCopyDir links src into dst. On Unix os.Symlink is sufficient and
// requires no special privileges.
func linkOrCopyDir(src, dst string) error {
	return os.Symlink(src, dst)
}
