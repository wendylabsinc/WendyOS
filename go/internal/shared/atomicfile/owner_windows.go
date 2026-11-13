//go:build windows

package atomicfile

// lookupOwner is a no-op on Windows: file ownership there isn't chown-based,
// so there is nothing meaningful to report, and WritePreservingOwner's
// geteuid()==0 check never fires on this platform anyway.
func lookupOwner(string) (uid, gid int, ok bool) {
	return 0, 0, false
}
