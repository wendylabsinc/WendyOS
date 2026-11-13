package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// maxSymlinkHops bounds the manual symlink-chain walk below. It is Linux's
// own limit for one path lookup (MAXSYMLINKS, 40), not filepath.EvalSymlinks'
// (255): a file behind a dotfiles link needs one or two hops, and a loop
// should fail fast.
const maxSymlinkHops = 40

// ResolveWritePath follows path through any symlinks to its final target,
// including a dangling chain whose target does not exist yet, so a caller can
// Write there and leave the links intact: Write renames over whatever path it
// is given, and renaming over a symlink replaces the link with a regular file.
//
// filepath.EvalSymlinks requires the fully resolved path to exist, so it
// fails outright on a dangling symlink — e.g. a freshly cloned dotfiles repo
// whose config.json target hasn't been created. Falling back to path itself
// in that case (as a naive "try EvalSymlinks, else use path unchanged"
// would) makes the write rename over the symlink itself, replacing it with a
// regular file instead of creating its target — unlike os.WriteFile, which
// follows a symlink to a missing target and creates it there. Walking the
// chain by hand here reproduces that behavior.
func ResolveWritePath(path string) (string, error) {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		return target, nil
	}
	current := path
	for i := 0; i < maxSymlinkHops; i++ {
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				// current does not exist, not even as a symlink: this is the
				// final target (a fresh file, dangling or not).
				return current, nil
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			// Not a symlink: this is the final target. EvalSymlinks would
			// already have succeeded above in the common case; this only
			// matters if it failed for an unrelated, transient reason.
			return current, nil
		}
		target, err := os.Readlink(current)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		current = target
	}
	return "", fmt.Errorf("too many levels of symbolic links resolving %s", path)
}
