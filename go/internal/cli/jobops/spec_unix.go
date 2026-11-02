//go:build unix

package jobops

import (
	"os"

	"golang.org/x/sys/unix"
)

const regularReadFlags = unix.O_NONBLOCK | unix.O_NOFOLLOW

func openSpec(name string) (*os.File, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
