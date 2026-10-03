package services

import (
	"context"
	"math"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Memory-mapped Video for Linux 2 (V4L2) capture, shared by every producer that
// reads a node directly rather than through GStreamer: the native H.264 path
// (streamV4L2Native) and the native depth path (video_depth_linux.go). Both
// negotiate their own format with VIDIOC_S_FMT; from there on the buffer
// handling is identical, so it lives here once.

// v4l2BufFlagError is V4L2_BUF_FLAG_ERROR: the driver dequeued the buffer but
// its contents are known to be corrupt.
const v4l2BufFlagError = 0x00000040

// v4l2MmapQueue is a streaming capture queue of mmap buffers on an open node.
type v4l2MmapQueue struct {
	fd        int
	mapped    [][]byte
	streaming bool
}

// startV4L2MmapQueue requests count mmap buffers (VIDIOC_REQBUFS), maps and
// queues each one (VIDIOC_QUERYBUF, mmap, VIDIOC_QBUF) and starts streaming
// (VIDIOC_STREAMON). A driver may grant fewer buffers than asked; fewer than two
// cannot keep one queued while another is being read, and is refused.
//
// On failure everything set up so far is released and the error follows the
// errCaptureSetup convention (a held camera reads as errCameraInUse, anything
// else is logged and reported without the errno). On success the caller must
// call stop exactly once.
func (s *VideoService) startV4L2MmapQueue(fd int, path string, count uint32) (q *v4l2MmapQueue, err error) {
	var reqbufs v4l2ReqBuffers
	reqbufs.Count = count
	reqbufs.Type = v4l2BufTypeVideoCapture
	reqbufs.Memory = v4l2MemoryMmap
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocReqbufs, uintptr(unsafe.Pointer(&reqbufs))); errno != 0 {
		return nil, s.errCaptureSetup("VIDIOC_REQBUFS", path, errno)
	}
	if reqbufs.Count < 2 {
		return nil, status.Errorf(codes.Internal, "insufficient buffer memory on device")
	}

	q = &v4l2MmapQueue{fd: fd, mapped: make([][]byte, 0, reqbufs.Count)}
	defer func() {
		if err != nil {
			q.stop()
			q = nil
		}
	}()
	for i := uint32(0); i < reqbufs.Count; i++ {
		var qbuf v4l2Buf
		qbuf.setIndex(i)
		qbuf.setType(v4l2BufTypeVideoCapture)
		qbuf.setMemory(v4l2MemoryMmap)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQuerybuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return q, s.errCaptureSetup("VIDIOC_QUERYBUF", path, errno)
		}
		length := *(*uint32)(unsafe.Pointer(&qbuf[72])) // length at offset 72 in v4l2_buffer
		data, mmapErr := unix.Mmap(fd, int64(qbuf.offset()), int(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if mmapErr != nil {
			return q, status.Errorf(codes.Internal, "mmap buffer %d: %v", i, mmapErr)
		}
		q.mapped = append(q.mapped, data)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQbuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return q, s.errCaptureSetup("VIDIOC_QBUF", path, errno)
		}
	}

	bufType := uint32(v4l2BufTypeVideoCapture)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocStreamon, uintptr(unsafe.Pointer(&bufType))); errno != 0 {
		return q, s.errCaptureSetup("VIDIOC_STREAMON", path, errno)
	}
	q.streaming = true
	return q, nil
}

// stop ends streaming (VIDIOC_STREAMOFF) and unmaps every buffer, in that order,
// so the driver has released the buffers before their mappings go away. The
// descriptor itself belongs to the caller.
func (q *v4l2MmapQueue) stop() {
	if q.streaming {
		bufType := uint32(v4l2BufTypeVideoCapture)
		unix.Syscall(unix.SYS_IOCTL, uintptr(q.fd), vidiocStreamoff, uintptr(unsafe.Pointer(&bufType))) //nolint:errcheck
		q.streaming = false
	}
	for _, data := range q.mapped {
		unix.Munmap(data) //nolint:errcheck
	}
	q.mapped = nil
}

// dequeueLoop dequeues filled buffers (VIDIOC_DQBUF), hands each to handle, and
// requeues it (VIDIOC_QBUF), until ctx ends, handle returns false (no
// subscribers remain; the loop returns nil), or the device fails.
//
// handle receives the dequeued buffer descriptor and that buffer's whole mapped
// region. The region is the driver's memory and is refilled once requeued, so
// handle must copy out anything it keeps before returning.
//
// A DQBUF failure other than an interrupted call or a held camera is passed to
// onDequeueErr when it is set, so a caller can give it its own meaning (the
// H.264 path reads one before the first frame as "fall back to GStreamer");
// otherwise it is reported through errCaptureSetup.
func (s *VideoService) dequeueLoop(ctx context.Context, q *v4l2MmapQueue, path string, onDequeueErr func(unix.Errno) error, handle func(buf *v4l2Buf, mem []byte) bool) error {
	if q.fd > math.MaxInt32 {
		return status.Errorf(codes.Internal, "file descriptor value out of range for poll")
	}
	pollFds := []unix.PollFd{{Fd: int32(q.fd), Events: unix.POLLIN}}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Poll with a short timeout so context cancellation is noticed quickly.
		// VIDIOC_DQBUF blocks until a buffer arrives; without this a cancelled
		// context can wait up to one full frame period before the producer exits,
		// holding the device fd and delaying the next StreamVideo caller.
		ready, err := unix.Poll(pollFds, 100)
		if err == unix.EINTR || (err == nil && ready == 0) {
			continue // timeout or signal: re-check ctx.Done
		}
		if err != nil {
			s.logger.Error("poll failed on video device", zap.String("device", path), zap.Error(err))
			return status.Errorf(codes.Internal, "video device poll error")
		}

		var dqbuf v4l2Buf
		dqbuf.setType(v4l2BufTypeVideoCapture)
		dqbuf.setMemory(v4l2MemoryMmap)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(q.fd), vidiocDqbuf, uintptr(unsafe.Pointer(&dqbuf))); errno != 0 {
			if errno == unix.EINTR || errno == unix.EAGAIN {
				continue
			}
			// EBUSY is a held camera, whatever the caller makes of other failures:
			// it must reach the sharing path, not a fallback.
			if isBusyErrno(errno) {
				return errCameraInUse(path)
			}
			if onDequeueErr != nil {
				return onDequeueErr(errno)
			}
			return s.errCaptureSetup("VIDIOC_DQBUF", path, errno)
		}

		idx := dqbuf.index()
		if int(idx) >= len(q.mapped) {
			// A driver naming a buffer it was never given would otherwise index
			// past the mapping table.
			return s.errCaptureSetup("VIDIOC_DQBUF", path, unix.EINVAL)
		}
		if !handle(&dqbuf, q.mapped[idx]) {
			return nil
		}

		var qbuf v4l2Buf
		qbuf.setIndex(idx)
		qbuf.setType(v4l2BufTypeVideoCapture)
		qbuf.setMemory(v4l2MemoryMmap)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(q.fd), vidiocQbuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return s.errCaptureSetup("VIDIOC_QBUF", path, errno)
		}
	}
}
