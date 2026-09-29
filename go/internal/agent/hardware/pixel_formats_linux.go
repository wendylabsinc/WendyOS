//go:build linux

package hardware

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// VIDIOC_ENUM_FMT is _IOWR('V', 2, struct v4l2_fmtdesc), 64 bytes.
	vidiocEnumFmt = 0xC0405602

	v4l2BufTypeVideoCapture       = 1 // V4L2_BUF_TYPE_VIDEO_CAPTURE
	v4l2BufTypeVideoCaptureMplane = 9 // V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE

	// maxFormatIndex stops the walk on a driver that never answers EINVAL.
	maxFormatIndex = 64
)

// v4l2Fmtdesc mirrors struct v4l2_fmtdesc from linux/videodev2.h (Video for
// Linux 2, V4L2).
type v4l2Fmtdesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]uint8
	PixelFormat uint32
	MbusCode    uint32
	Reserved    [3]uint32
}

// v4l2PixelFormats walks VIDIOC_ENUM_FMT for single-planar and multi-planar
// capture. Read-only open, so it answers while another process streams from
// the node; the descriptor is closed before returning.
func v4l2PixelFormats(path string) ([]uint32, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd) //nolint:errcheck

	var formats []uint32
	var firstErr error
	for _, bufType := range []uint32{v4l2BufTypeVideoCapture, v4l2BufTypeVideoCaptureMplane} {
		for index := uint32(0); index < maxFormatIndex; index++ {
			desc := v4l2Fmtdesc{Index: index, Type: bufType}
			if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocEnumFmt,
				uintptr(unsafe.Pointer(&desc))); errno != 0 {
				if index == 0 && firstErr == nil {
					firstErr = errno
				}
				break // EINVAL marks the end of the list
			}
			formats = append(formats, desc.PixelFormat)
		}
	}
	if len(formats) == 0 && firstErr != nil {
		return nil, fmt.Errorf("VIDIOC_ENUM_FMT on %s: %w", path, firstErr)
	}
	return formats, nil
}
