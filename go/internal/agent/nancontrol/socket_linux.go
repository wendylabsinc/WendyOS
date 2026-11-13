//go:build linux

package nancontrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const permissionReceiptPath = "/run/wendy-nan-app-socket.json"

// SocketToken identifies a socket incarnation, including changes made by host
// administrators. Only our recorded permission mutations may retain its old
// identity; arbitrary ctime changes must still invalidate provider ownership.
type SocketToken struct {
	Dev, Ino            uint64
	CtimeSec, CtimeNsec int64
}

type permissionReceipt struct {
	Path              string
	Original, Current SocketToken
	UID, GID          int
	Mode              os.FileMode
	Granted           bool
}

func socketToken(info os.FileInfo) (SocketToken, error) {
	if info.Mode()&os.ModeSocket == 0 {
		return SocketToken{}, errors.New("NAN control path is not a Unix socket")
	}
	s := info.Sys().(*syscall.Stat_t)
	return SocketToken{uint64(s.Dev), s.Ino, s.Ctim.Sec, s.Ctim.Nsec}, nil
}

func readReceipt(path string) (permissionReceipt, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return permissionReceipt{}, err
	}
	var r permissionReceipt
	if err := json.Unmarshal(b, &r); err != nil || r.Path == "" || r.Original.Ino == 0 || r.Current.Ino == 0 {
		return r, fmt.Errorf("invalid NAN socket permission receipt %s", path)
	}
	return r, nil
}

func writeReceipt(path string, r permissionReceipt) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".wendy-nan-permissions-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// SocketIdentity returns the original incarnation across exactly recorded
// permission changes. The receipt remains after restoration because chmod
// changes ctime again, while existing provider journals retain this identity.
func SocketIdentity(path string) (SocketToken, error) {
	return socketIdentity(path, permissionReceiptPath)
}

func socketIdentity(path, receipt string) (SocketToken, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return SocketToken{}, err
	}
	current, err := socketToken(info)
	if err != nil {
		return SocketToken{}, err
	}
	r, err := readReceipt(receipt)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return SocketToken{}, err
	}
	if err == nil && r.Path == path && r.Current == current {
		return r.Original, nil
	}
	return current, nil
}

// GrantSocketAccess and RestoreSocketAccess require Lock. Containerd's app
// reservations decide when the last user leaves; the provider shares this
// same short lock around management ownership checks and destructive changes.
func GrantSocketAccess(path string, gid int) error {
	return grantSocketAccess(path, permissionReceiptPath, gid)
}

func grantSocketAccess(path, receipt string, gid int) (grantErr error) {
	file, err := openSocket(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	token, err := socketToken(info)
	if err != nil {
		return err
	}
	r, err := readReceipt(receipt)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err != nil || r.Path != path || r.Current != token {
		s := info.Sys().(*syscall.Stat_t)
		r = permissionReceipt{Path: path, Original: token, Current: token, UID: int(s.Uid), GID: int(s.Gid), Mode: info.Mode().Perm()}
	}
	if r.Granted {
		return nil
	}
	// Confirm that the receipt is writable before widening host access. If a
	// subsequent operation fails, roll permissions back on this pinned inode.
	if err := writeReceipt(receipt, r); err != nil {
		return err
	}
	defer func() {
		if grantErr == nil {
			return
		}
		s := info.Sys().(*syscall.Stat_t)
		rollbackErr := changeSocketPermissions(file, int(s.Uid), int(s.Gid), info.Mode().Perm())
		if rollbackErr == nil {
			r.Granted = false
			if after, err := file.Stat(); err == nil {
				r.Current, rollbackErr = socketToken(after)
				if rollbackErr == nil {
					rollbackErr = writeReceipt(receipt, r)
				}
			} else {
				rollbackErr = err
			}
		}
		grantErr = errors.Join(grantErr, rollbackErr)
	}()
	if err := changeSocketPermissions(file, 0, gid, 0770); err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, after) {
		return errors.New("NAN socket replaced while granting access")
	}
	r.Current, err = socketToken(after)
	if err != nil {
		return err
	}
	r.Granted = true
	return writeReceipt(receipt, r)
}

func RestoreSocketAccess(path string) error {
	return restoreSocketAccess(path, permissionReceiptPath)
}

func restoreSocketAccess(path, receipt string) error {
	r, err := readReceipt(receipt)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Path != path || !r.Granted {
		return nil
	}
	file, err := openSocket(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := socketToken(info)
	if err != nil {
		return err
	}
	if current != r.Current {
		return nil // A replaced socket or administrator change is foreign.
	}
	if err := changeSocketPermissions(file, r.UID, r.GID, r.Mode); err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, after) {
		return errors.New("NAN socket replaced while restoring access")
	}
	r.Current, err = socketToken(after)
	if err != nil {
		return err
	}
	r.Granted = false
	return writeReceipt(receipt, r)
}

// Pin the inode before checking or changing access. The supplicant can replace
// its pathname independently; never chown/chmod the replacement through a
// stale pathname check. O_PATH opens Unix sockets without connecting to them.
func openSocket(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil {
		_, err = socketToken(info)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func changeSocketPermissions(file *os.File, uid, gid int, mode os.FileMode) error {
	if err := unix.Fchownat(int(file.Fd()), "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
		return err
	}
	// chmod through the pinned descriptor works on kernels predating fchmodat2.
	return os.Chmod(fmt.Sprintf("/proc/self/fd/%d", file.Fd()), mode)
}
