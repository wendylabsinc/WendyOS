//go:build darwin || linux

package commands

import (
	"fmt"
	"io/fs"
	"os"
)

// helperInputEUID is os.Geteuid; a variable so tests can run as root.
var helperInputEUID = os.Geteuid

// checkHelperInputOwner refuses a file not owned by the user whose sudo made
// the helper root, so the helper never reads what that user couldn't. Without
// such a user (no sudo, or sudo from root) there is nothing to guard.
func checkHelperInputOwner(path string, fi fs.FileInfo) error {
	uid, _, ok := sudoInvoker(helperInputEUID(), os.Getenv)
	if !ok {
		return nil
	}
	if owner, _ := statOwner(fi); owner != uid {
		return fmt.Errorf("%w %s: owned by uid %d, not by uid %d who ran sudo", errHelperInputRefused, path, owner, uid)
	}
	return nil
}
