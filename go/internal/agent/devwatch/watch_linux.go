//go:build linux

package devwatch

import (
	"bytes"
	"context"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Watch calls onEvent for each videoN node created or deleted in dir until ctx
// ends, then returns nil. Attribute changes are deliberately not watched: a
// node being opened or chmodded is not a replug. onEvent runs on the watch loop and must not block.
func Watch(ctx context.Context, dir string, onEvent func(Event)) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify_init1: %w", err)
	}
	defer unix.Close(fd)
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_DELETE); err != nil {
		return fmt.Errorf("inotify_add_watch(%s): %w", dir, err)
	}

	buf := make([]byte, 4096)
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for ctx.Err() == nil {
		n, err := unix.Poll(pfd, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll: %w", err)
		}
		if n == 0 {
			continue
		}
		got, err := unix.Read(fd, buf)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("read inotify: %w", err)
		}
		for off := 0; off+unix.SizeofInotifyEvent <= got; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			end := off + unix.SizeofInotifyEvent + int(ev.Len)
			if end > got {
				break
			}
			name := string(bytes.TrimRight(buf[off+unix.SizeofInotifyEvent:end], "\x00"))
			off = end
			if !videoNode.MatchString(name) {
				continue
			}
			switch {
			case ev.Mask&unix.IN_CREATE != 0:
				onEvent(Event{Name: name, Created: true})
			case ev.Mask&unix.IN_DELETE != 0:
				onEvent(Event{Name: name, Created: false})
			}
		}
	}
	return nil
}
