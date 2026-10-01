//go:build !darwin && !linux

package commands

import "io/fs"

// fileIdentityOf reports false: this platform's os.FileInfo carries no inode
// or ctime (Windows), so a same-size rewrite with a preserved mtime could not
// be told apart from an unchanged file. Every context file is read instead.
func fileIdentityOf(fs.FileInfo) (fileIdentity, bool) {
	return fileIdentity{}, false
}
