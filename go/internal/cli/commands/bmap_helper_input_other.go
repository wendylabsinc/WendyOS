//go:build !darwin && !linux

package commands

import "io/fs"

// checkHelperInputOwner allows every file off macOS and Linux, where the bmap
// write runs in-process instead of in a helper under sudo.
func checkHelperInputOwner(string, fs.FileInfo) error { return nil }
