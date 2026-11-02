//go:build !windows

// Package useripc provides the private local transport and durable path policy
// for one user's session broker. Its streams carry existing authenticated RPC
// ownership; it does not replace the broker's remote TLS or device identity.
package useripc

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
)

func owner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("session broker path is not owned by the current user")
	}
	return nil
}
func OwnedPath(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && owner(info) == nil
}
func EnsureDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	return ValidateDirectory(path)
}
func ValidateDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("session directory is not private")
	}
	return owner(info)
}
func ValidateFile(path string, socket bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	expected := info.Mode().IsRegular()
	if socket {
		expected = info.Mode()&os.ModeSocket != 0
	}
	if !expected || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("unsafe session broker file %s", path)
	}
	return owner(info)
}
func Listen(path string) (net.Listener, error) {
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
func Dial(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
