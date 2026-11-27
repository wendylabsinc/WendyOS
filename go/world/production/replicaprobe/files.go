package replicaprobe

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"golang.org/x/sys/unix"
)

// The owner and root remain trusted. Pinned directory descriptors and no-follow
// opens prevent commands or receipts from traversing links to other objects.
type directory struct{ file *os.File }

func openDirectory(path string) (*directory, error) {
	if hostpath.EnsureDirectory(path, 0755, false) != nil {
		return nil, ErrConfig
	}
	fd, e := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		_ = f.Close()
		return nil, ErrConfig
	}
	return &directory{f}, nil
}
func (d *directory) close() { _ = d.file.Close() }
func (d *directory) read(name string, limit int) ([]byte, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, ErrConfig
	}
	fd, e := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var before, after unix.Stat_t
	if unix.Fstat(fd, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Mode&0022 != 0 || before.Uid != uint32(os.Geteuid()) || before.Nlink != 1 || before.Size < 1 || before.Size > int64(limit) {
		return nil, ErrConfig
	}
	raw, e := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if e != nil || len(raw) > limit || int64(len(raw)) != before.Size || unix.Fstat(fd, &after) != nil || after.Size != before.Size || after.Mode != before.Mode || after.Uid != before.Uid || after.Nlink != 1 {
		return nil, ErrConfig
	}
	return raw, nil
}
func (d *directory) write(name string, raw []byte, limit int) error {
	return d.writeWithPublisher(name, raw, limit, renameNoReplace)
}

// The per-call publisher seam lets tests pause after the actual namespace
// transition. Production always uses the platform's atomic no-replace rename.
func (d *directory) writeWithPublisher(name string, raw []byte, limit int, publish func(int, string, string) error) error {
	if filepath.Base(name) != name || name == "." || name == ".." || len(raw) == 0 || len(raw) > limit {
		return ErrConfig
	}
	fd := int(d.file.Fd())
	tmp := ".pending-" + name
	n, e := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return e
	}
	f := os.NewFile(uintptr(n), tmp)
	_, e = f.Write(raw)
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	// Move the completed single-link file in one operation. Link+unlink would
	// briefly expose Nlink=2 and make strict concurrent readers refuse it.
	// Unsupported kernels/filesystems fail; there is no replacing fallback.
	if e = publish(fd, tmp, name); e != nil {
		return e
	}
	return d.file.Sync()
}
func LoadConfig(path string) (Config, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Config{}, ErrConfig
	}
	d, e := openDirectory(filepath.Dir(path))
	if e != nil {
		return Config{}, ErrConfig
	}
	defer d.close()
	raw, e := d.read(filepath.Base(path), MaxConfigBytes)
	if e != nil {
		return Config{}, ErrConfig
	}
	var c Config
	if jws.DecodeStrict(raw, &c) != nil {
		return c, ErrConfig
	}
	return c.normalized()
}
