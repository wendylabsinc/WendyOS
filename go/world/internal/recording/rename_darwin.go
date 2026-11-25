package recording

import "golang.org/x/sys/unix"

func renameNoReplace(parent int, from, to string) error {
	return unix.RenameatxNp(parent, from, parent, to, unix.RENAME_EXCL)
}
