package supervisor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// SnapshotDir is config put back when a trial fails.
type SnapshotDir struct {
	Path string
	// Only limits it to these names in Path, so identity, leases and other
	// state that must never go back are left alone. Empty means the whole tree.
	Only []string
}

// snapshot copies each dir into dst before a trial, so a failed agent can't
// leave behind config the previous agent can't read. It is built aside and
// renamed into place: a partial snapshot would delete what it is missing on restore.
func snapshot(dst string, dirs []SnapshotDir) error {
	partial := dst + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return err
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := snapshotDir(partial, dir); err != nil {
			os.RemoveAll(partial)
			return err
		}
	}
	return os.Rename(partial, dst)
}

// onlySuffix marks an allowlist snapshot, which a whole-tree restore must
// never find; its names file says what it captured.
const onlySuffix = ".only"

func snapshotDir(root string, dir SnapshotDir) error {
	if len(dir.Only) == 0 {
		target := filepath.Join(root, snapshotKey(dir.Path))
		if _, err := os.Stat(dir.Path); errors.Is(err, os.ErrNotExist) {
			return os.MkdirAll(target, 0o700)
		}
		return copyTree(dir.Path, target)
	}
	target := filepath.Join(root, snapshotKey(dir.Path)+onlySuffix)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(target+".names", []byte(strings.Join(dir.Only, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	for _, name := range dir.Only {
		from := filepath.Join(dir.Path, name)
		if _, err := os.Lstat(from); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := copyTree(from, filepath.Join(target, name)); err != nil {
			return err
		}
	}
	return nil
}

// restore puts the snapshot back in place. Files are replaced one by one and
// extras removed last, because the dirs are bind-mounted and can't be swapped.
func restore(src string, dirs []SnapshotDir) error {
	for _, dir := range dirs {
		if len(dir.Only) == 0 {
			from := filepath.Join(src, snapshotKey(dir.Path))
			if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err := copyTree(from, dir.Path); err != nil {
				return err
			}
			if err := removeExtras(dir.Path, from); err != nil {
				return err
			}
			continue
		}
		from := filepath.Join(src, snapshotKey(dir.Path)+onlySuffix)
		names, err := os.ReadFile(from + ".names")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		captured := strings.Fields(string(names))
		for _, name := range dir.Only {
			if !slices.Contains(captured, name) {
				// Not captured, so nothing says what it was before the trial.
				continue
			}
			saved, live := filepath.Join(from, name), filepath.Join(dir.Path, name)
			fi, err := os.Lstat(saved)
			switch {
			case errors.Is(err, os.ErrNotExist):
				err = os.RemoveAll(live)
			case err != nil:
			default:
				if err = copyTree(saved, live); err == nil && fi.IsDir() {
					err = removeExtras(live, saved)
				}
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func snapshotKey(dir string) string {
	return strings.ReplaceAll(strings.Trim(dir, "/"), "/", "_")
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		dst := filepath.Join(to, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		// A trial may have turned a file into a directory or the reverse, or put
		// a symlink where a directory was; clear whatever is in the way.
		if cur, err := os.Lstat(dst); err == nil && (cur.IsDir() != d.IsDir() || (cur.Mode()|d.Type())&fs.ModeSymlink != 0) {
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, dst)
		case d.Type().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return atomicfile.Write(dst, data, info.Mode().Perm())
		}
		return nil
	})
}

func removeExtras(live, snap string) error {
	entries, err := os.ReadDir(live)
	if err != nil {
		return err
	}
	for _, e := range entries {
		want := filepath.Join(snap, e.Name())
		got := filepath.Join(live, e.Name())
		fi, err := os.Lstat(want)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.RemoveAll(got); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if fi.IsDir() && e.IsDir() {
			if err := removeExtras(got, want); err != nil {
				return err
			}
		}
	}
	return nil
}
