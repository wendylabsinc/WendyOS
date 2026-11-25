//go:build darwin

package replicaprobe

import "golang.org/x/sys/unix"

func renameNoReplace(fd int, from, to string) error {
	return unix.RenameatxNp(fd, from, fd, to, unix.RENAME_EXCL)
}
