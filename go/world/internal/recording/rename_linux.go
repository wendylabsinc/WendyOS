package recording

import "golang.org/x/sys/unix"

func renameNoReplace(parent int, from, to string) error {
	return unix.Renameat2(parent, from, parent, to, unix.RENAME_NOREPLACE)
}
