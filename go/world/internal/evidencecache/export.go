package evidencecache

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Export stages verified bytes in a private anchored directory. The destination
// is linked without replacement only after the caller's final authority check.
// The caller must supply already digest-verified bytes and the original expiry.
func Export(path string, data []byte, until int64, authorize func() error) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(data) == 0 || len(data) > 64<<20 || authorize == nil {
		return errors.New("absolute new output path and bounded verified bytes required")
	}
	parent, name := filepath.Split(path)
	if name == "" || name == "." || name == ".." {
		return errors.New("output file name required")
	}
	info, err := os.Lstat(filepath.Clean(parent))
	if err != nil {
		return err
	}
	if err = check(info, true); err != nil {
		return err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err = root.Stat(".")
	if err != nil {
		return err
	}
	if err = check(info, true); err != nil {
		return err
	}
	if _, err = root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return errors.New("output already exists or cannot be checked")
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return err
	}
	temp := ".evidence-export-" + hex.EncodeToString(token[:])
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	defer f.Close()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		return err
	}
	if err = authorize(); err != nil {
		return err
	}
	// The authorization callback may wait on a network request. Recheck the
	// exact staged descriptor and path after that wait, before linking it.
	staged, e := f.Stat()
	if e != nil {
		return e
	}
	if e = check(staged, false); e != nil {
		return e
	}
	current, e := root.Lstat(temp)
	if e != nil {
		return e
	}
	if !os.SameFile(staged, current) || current.Mode()&os.ModeSymlink != 0 {
		return errors.New("staged export changed during authorization")
	}
	// Refuse a replaced FIFO/symlink without blocking and compare actual bytes.
	verify, e := root.OpenFile(temp, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	verified, e := verify.Stat()
	verify.Close()
	if e != nil || !os.SameFile(staged, verified) {
		return errors.New("staged export descriptor changed")
	}
	got := make([]byte, len(data))
	if _, e = f.ReadAt(got, 0); e != nil && e != io.EOF {
		return e
	}
	if !bytes.Equal(got, data) || staged.Size() != int64(len(data)) {
		return errors.New("staged export bytes changed")
	}
	if time.Now().UnixMilli() >= until {
		return errors.New("evidence expired before export")
	}
	if err = root.Link(temp, name); err != nil {
		return err
	}
	if err = root.Remove(temp); err != nil {
		return err
	}
	return syncRoot(root)
}
