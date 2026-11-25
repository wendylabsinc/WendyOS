package recording

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrInstallUncertain = errors.New("recording may be installed but directory durability could not be confirmed")

// Write installs a verified archive under a new name, without overwriting an
// existing file, symlink or directory. The destination parent is trusted storage.
func Write(bundle Bundle, destination string) error {
	return write(bundle, destination, renameNoReplace)
}
func write(bundle Bundle, destination string, rename func(int, string, string) error) error {
	b, e := Verify(bundle.Root, bundle.Blobs)
	if e != nil {
		return e
	}
	abs, e := filepath.Abs(destination)
	if e != nil {
		return e
	}
	name, parentName := filepath.Base(abs), filepath.Dir(abs)
	if destination == "" || abs == parentName || name == "." || name == ".." {
		return errors.New("invalid recording destination")
	}
	parent, e := os.OpenRoot(parentName)
	if e != nil {
		return e
	}
	defer parent.Close()
	fd, e := parent.Open(".")
	if e != nil {
		return e
	}
	defer fd.Close()
	if _, e = parent.Lstat(name); e == nil {
		return os.ErrExist
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	entropy := make([]byte, 12)
	if _, e = rand.Read(entropy); e != nil {
		return e
	}
	tmp := ".world-recording-" + hex.EncodeToString(entropy)
	if e = parent.Mkdir(tmp, 0700); e != nil {
		return e
	}
	defer parent.RemoveAll(tmp)
	stage, e := parent.OpenRoot(tmp)
	if e != nil {
		return e
	}
	defer stage.Close()
	if e = stage.Mkdir("blobs", 0700); e != nil {
		return e
	}
	put := func(name string, data []byte) error {
		f, e := stage.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(data)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return e
		}
		return closeErr
	}
	if e = put("recording.json", b.Root); e != nil {
		return e
	}
	for hash, data := range b.Blobs {
		if e = put("blobs/"+hash, data); e != nil {
			return e
		}
	}
	for _, dir := range []string{"blobs", "."} {
		f, e := stage.Open(dir)
		if e != nil {
			return e
		}
		e = f.Sync()
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if e = rename(int(fd.Fd()), tmp, name); e != nil {
		return e
	}
	if e = fd.Sync(); e != nil {
		return fmt.Errorf("%w: %v", ErrInstallUncertain, e)
	}
	return nil
}
