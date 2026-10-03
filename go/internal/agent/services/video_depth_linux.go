//go:build linux

package services

import (
	"context"
	"fmt"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// captureDepthFramesNative is the Linux implementation of captureDepthFrames:
// open the node, negotiate Z16 at exactly width x height with VIDIOC_S_FMT, then
// hand the rest to the shared mmap queue and dequeue loop (video_v4l2_mmap.go).
// Error conventions follow streamV4L2Native: a held camera is errCameraInUse,
// other setup failures are logged with their errno and reported without it.
func captureDepthFramesNative(ctx context.Context, s *VideoService, path string, width, height uint32, offered func(bytesPerLine uint32), deliver func(frame []byte) bool) error {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		s.logger.Error("failed to open depth device", zap.String("device", path), zap.Error(err))
		if isBusyErrno(err) {
			return errCameraInUse(path)
		}
		return status.Errorf(codes.Internal, "failed to open video device")
	}
	defer unix.Close(fd) //nolint:errcheck

	var vfmt v4l2Format
	vfmt.Type = v4l2BufTypeVideoCapture
	vfmt.Width = width
	vfmt.Height = height
	vfmt.PixelFormat = v4l2PixFmtZ16
	vfmt.Field = v4l2FieldNone
	sfmt := func() unix.Errno {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocSFmt, uintptr(unsafe.Pointer(&vfmt)))
		return errno
	}
	if errno := retryWhileBusy(ctx, sfmt); errno != 0 {
		if errno == unix.EINVAL {
			return errRawUnavailable(fmt.Sprintf("depth camera rejected Z16 at %dx%d", width, height))
		}
		return s.errCaptureSetup("VIDIOC_S_FMT", path, errno)
	}
	// V4L2 lets a driver adjust what it was asked for. A frame whose layout is
	// not the one RawFormat will describe must never reach a subscriber.
	if vfmt.PixelFormat != v4l2PixFmtZ16 || vfmt.Width != width || vfmt.Height != height {
		return errRawUnavailable(fmt.Sprintf("depth camera switched Z16 %dx%d to %s %dx%d",
			width, height, fourccString(vfmt.PixelFormat), vfmt.Width, vfmt.Height))
	}
	bytesPerLine := vfmt.BytesPerLine
	if bytesPerLine == 0 {
		bytesPerLine = twoBytesPerPixel(width)
	}
	// Consumers read a depth sample as packed rows of width values (the sensor
	// sample carries no stride), so a padded row layout is refused rather than
	// handed over as something it is not. UVC drivers report packed rows.
	if bytesPerLine != twoBytesPerPixel(width) {
		return errRawUnavailable(fmt.Sprintf("depth camera reports %d bytes per line for a %d-pixel Z16 row; only packed rows are supported", bytesPerLine, width))
	}
	frameBytes := uint64(bytesPerLine) * uint64(height)
	if frameBytes > maxRawFrameBytes {
		return errRawUnavailable(fmt.Sprintf("a %dx%d Z16 frame of %d bytes exceeds the %d-byte raw frame limit",
			width, height, frameBytes, maxRawFrameBytes))
	}

	queue, err := s.startV4L2MmapQueue(fd, path, depthCaptureBuffers)
	if err != nil {
		return err
	}
	defer queue.stop()
	offered(bytesPerLine)

	var partial uint64
	err = s.dequeueLoop(ctx, queue, path, nil, func(dqbuf *v4l2Buf, mem []byte) bool {
		// An uncompressed frame is only usable whole: a short or flagged buffer
		// would be decoded against a layout it does not have. Drop it before
		// publishRaw, so it consumes no sample id; the count is logged when the
		// capture ends.
		if n := uint64(dqbuf.bytesUsed()); n != frameBytes || uint64(len(mem)) < frameBytes || dqbuf.flags()&v4l2BufFlagError != 0 {
			partial++
			return true
		}
		return deliver(mem[:frameBytes])
	})
	if partial > 0 {
		s.logger.Debug("depth capture dropped incomplete frames",
			zap.String("device", path), zap.Uint64("frames", partial))
	}
	return err
}

// fourccString renders a V4L2 pixel format code as its four characters.
func fourccString(f uint32) string {
	return string([]byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)})
}
