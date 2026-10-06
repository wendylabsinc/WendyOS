//go:build !unix

package commands

import "os"

// fileOwner reports no owner: there are no POSIX uids/gids to keep here.
func fileOwner(os.FileInfo) (uid, gid int, ok bool) {
	return 0, 0, false
}
