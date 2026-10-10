//go:build linux

package ros2camera

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	v4l2BufTypeVideoOutput = 2
	v4l2PixFmtMJPEG        = 0x47504a4d // MJPG
	v4l2PixFmtH264         = 0x34363248 // H264
	v4l2PixFmtBGR24        = 0x33524742 // BGR3 -- raw packed 24-bit BGR
	v4l2FieldNone          = 1
	vidiocSFmt             = 0xc0d05605
)

type v4l2Format struct {
	Type         uint32
	_            [4]byte // align the v4l2_format union to 8 bytes on 64-bit Linux
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	Colorspace   uint32
	Priv         uint32
	Flags        uint32
	Enc          uint32
	Quantization uint32
	XferFunc     uint32
	_            [152]byte
}

// VIDIOC_S_FMT encodes a 208-byte argument, and the anonymous format union
// starts at byte 8 on Linux's 64-bit UAPI. Keep both properties compile-time
// checked: a subtly wrong layout can make the ioctl succeed with shifted
// dimensions and is otherwise very difficult to diagnose on a device.
var (
	_ [208 - unsafe.Sizeof(v4l2Format{})]byte
	_ [unsafe.Sizeof(v4l2Format{}) - 208]byte
	_ [8 - unsafe.Offsetof(v4l2Format{}.Width)]byte
	_ [unsafe.Offsetof(v4l2Format{}.Width) - 8]byte
)

type frameWriter struct {
	path          string
	file          *os.File
	width, height int
	codec         Codec
}

func newFrameWriter(path string) cameraWriter { return &frameWriter{path: path} }

func (w *frameWriter) WriteFrame(frame Frame) error {
	width, height := frame.Width, frame.Height
	if width <= 0 || height <= 0 || width > 8192 || height > 8192 {
		return fmt.Errorf("invalid frame dimensions %dx%d", width, height)
	}
	pixelFormat, err := v4l2PixelFormat(frame.Codec)
	if err != nil {
		return err
	}
	if w.file == nil {
		fd, err := unix.Open(w.path, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("opening ROS 2 camera loopback %s: %w", w.path, err)
		}
		w.file = os.NewFile(uintptr(fd), w.path)
	}
	if w.width != width || w.height != height || w.codec != frame.Codec {
		sizeImage := width * height * 3
		if len(frame.Data) > sizeImage {
			sizeImage = len(frame.Data)
		}
		format := v4l2Format{
			Type:        v4l2BufTypeVideoOutput,
			Width:       uint32(width),
			Height:      uint32(height),
			PixelFormat: pixelFormat,
			Field:       v4l2FieldNone,
			// Compressed frame sizes vary with scene complexity. Reserve the
			// uncompressed RGB bound rather than sizing the loopback buffer from
			// the first (possibly unusually small) JPEG.
			SizeImage: uint32(sizeImage),
		}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, w.file.Fd(), vidiocSFmt, uintptr(unsafe.Pointer(&format))); errno != 0 {
			return fmt.Errorf("configuring ROS 2 camera loopback %s: %w", w.path, errno)
		}
		w.width, w.height, w.codec = width, height, frame.Codec
	}
	data := frame.Data
	for len(data) > 0 {
		n, err := w.file.Write(data)
		if err != nil {
			return fmt.Errorf("writing ROS 2 camera frame: %w", err)
		}
		if n == 0 {
			return errors.New("writing ROS 2 camera frame made no progress")
		}
		data = data[n:]
	}
	return nil
}

func v4l2PixelFormat(codec Codec) (uint32, error) {
	switch codec {
	case CodecMJPEG:
		return v4l2PixFmtMJPEG, nil
	case CodecH264:
		return v4l2PixFmtH264, nil
	case CodecBGR24:
		return v4l2PixFmtBGR24, nil
	default:
		return 0, fmt.Errorf("unsupported ROS 2 loopback codec %d", codec)
	}
}

func (w *frameWriter) Close() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
