package services

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// The writer side of an app's camera share node.
//
// Each camera app gets its own v4l2loopback node (the "share node"), and the
// agent keeps it open as the node's writer. The sequence below follows what was
// measured against the real module:
//
//   - On a node that has never had a writer, a reader's STREAMON fails with EIO
//     until a writer has run STREAMON on the OUTPUT side; STREAMON with no
//     buffer queued is enough. With announce_all_caps=1 the node always
//     announces CAPTURE, and the writer's format plus keep_format is what makes
//     the declared mode readable, including while the writer is away.
//   - The same descriptor serves writing and the event subscription.
//   - v4l2loopback's client-usage event reports how many readers are
//     streaming. There is one pending event per subscription (the newest
//     overwrites), and SEND_INITIAL delivers the current count at subscribe.
//   - A new reader first receives the node's last written frame, which is why
//     WriteBlank exists: it replaces a stale last frame with black.

const (
	// vidiocSubscribeEvent is _IOW('V', 90, struct v4l2_event_subscription).
	vidiocSubscribeEvent = 0x4020565A
	// vidiocDqevent is _IOR('V', 89, struct v4l2_event), 136 bytes.
	vidiocDqevent = 0x80885659

	// v4l2EventClientUsage is V4L2_EVENT_PRIVATE_START + 0x08E00000 + 1: the
	// v4l2loopback event that carries the number of streaming clients.
	v4l2EventClientUsage = 0x10E00001
	// v4l2EventSubFlSendInitial is V4L2_EVENT_SUB_FL_SEND_INITIAL.
	v4l2EventSubFlSendInitial = 1

	// vidiocSCtrl is _IOWR('V', 28, struct v4l2_control), 8 bytes.
	vidiocSCtrl = 0xC008561C
	// v4l2LoopbackCidKeepFormat is V4L2LOOPBACK_CID_BASE+0 ("keep_format").
	v4l2LoopbackCidKeepFormat = 0x0098f900

	// shareNodeBuffers is how many OUTPUT buffers the writer round-robins.
	shareNodeBuffers = 2

	// v4l2EventCountOffset and v4l2EventPendingOffset locate the client count
	// (first word of the event union, which starts at 8 on 64-bit) and the
	// number of events still queued behind this one in struct v4l2_event.
	v4l2EventCountOffset   = 8
	v4l2EventPendingOffset = 72
)

// v4l2EventSubscription matches struct v4l2_event_subscription (32 bytes).
type v4l2EventSubscription struct {
	Type  uint32
	ID    uint32
	Flags uint32
	_     [5]uint32
}

// v4l2Control matches struct v4l2_control (8 bytes).
type v4l2Control struct {
	ID    uint32
	Value int32
}

// v4l2Event is a struct v4l2_event (136 bytes), read through offsets like
// v4l2Buf so Go's layout rules never matter.
type v4l2Event [136]byte

var (
	_ [32 - unsafe.Sizeof(v4l2EventSubscription{})]byte
	_ [unsafe.Sizeof(v4l2EventSubscription{}) - 32]byte
	_ [136 - unsafe.Sizeof(v4l2Event{})]byte
	_ [unsafe.Sizeof(v4l2Event{}) - 136]byte
)

func (e *v4l2Event) count() uint32   { return *(*uint32)(unsafe.Pointer(&e[v4l2EventCountOffset])) }
func (e *v4l2Event) pending() uint32 { return *(*uint32)(unsafe.Pointer(&e[v4l2EventPendingOffset])) }

// shareNodeIO is what the pump needs of a share node.
//
// Contract: Counts reports how many readers are streaming (newest value only;
// the channel closes when the node goes away or the writer is closed).
// WriteFrame and WriteBlank may be called concurrently with each other and
// with Close; after Close they fail with an error. Close is idempotent and
// every call returns the first call's result.
type shareNodeIO interface {
	Counts() <-chan uint32
	WriteFrame(data []byte, monoNs int64) error
	WriteBlank(monoNs int64) error
	Close() error
}

// openShareNode is a variable so the pump's tests can substitute a fake.
var openShareNode = openShareNodeV4L2

var errShareNodeClosed = errors.New("share node writer is closed")

// blankFrames caches blankFrame by mode.
var blankFrames sync.Map // sharedMode -> []byte

// blankFrame is one black frame in mode. The slice is shared and cached:
// callers must not modify it.
func blankFrame(mode sharedMode) []byte {
	if v, ok := blankFrames.Load(mode); ok {
		return v.([]byte)
	}
	frame := make([]byte, mode.frameBytes())
	var pattern []byte
	switch mode.Fourcc {
	case "YUYV":
		pattern = []byte{0x10, 0x80, 0x10, 0x80}
	case "UYVY":
		pattern = []byte{0x80, 0x10, 0x80, 0x10}
	}
	for i := range pattern {
		for j := i; j < len(frame); j += len(pattern) {
			frame[j] = pattern[i]
		}
	}
	v, _ := blankFrames.LoadOrStore(mode, frame)
	return v.([]byte)
}

// shareNodeWriter is the V4L2 implementation of shareNodeIO.
type shareNodeWriter struct {
	logger *zap.Logger
	path   string
	mode   sharedMode
	fd     int

	// mu guards everything below and serialises writes against Close.
	mu        sync.Mutex
	buffers   [][]byte
	next      int
	streaming bool
	closed    bool
	closeErr  error

	wakeR, wakeW int // wake pipe: a byte on wakeW ends the event goroutine
	counts       chan uint32
	done         chan struct{} // closed when the event goroutine has exited
}

// openShareNodeV4L2 opens path as a share node writer in mode. A nil logger is
// treated as zap.NewNop().
func openShareNodeV4L2(logger *zap.Logger, path string, mode sharedMode) (shareNodeIO, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if mode.pixelFormat() == 0 {
		return nil, fmt.Errorf("share node %s: unknown pixel format %q", path, mode.Fourcc)
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("opening share node %s: %w", path, err)
	}
	w := &shareNodeWriter{logger: logger, path: path, mode: mode, fd: fd, wakeR: -1, wakeW: -1}
	if err := w.configure(); err != nil {
		_ = w.Close()
		return nil, err
	}
	return w, nil
}

func (w *shareNodeWriter) ioctl(req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(w.fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

func (w *shareNodeWriter) configure() error {
	mode := w.mode

	var vfmt v4l2Format
	vfmt.Type = v4l2BufTypeVideoOutput
	vfmt.Width = mode.Width
	vfmt.Height = mode.Height
	vfmt.PixelFormat = mode.pixelFormat()
	vfmt.Field = v4l2FieldNone
	vfmt.BytesPerLine = mode.BytesPerLine
	vfmt.SizeImage = uint32(mode.frameBytes())
	if err := w.ioctl(vidiocSFmt, unsafe.Pointer(&vfmt)); err != nil {
		return fmt.Errorf("VIDIOC_S_FMT on %s: %w", w.path, err)
	}
	if vfmt.Width != mode.Width || vfmt.Height != mode.Height || vfmt.PixelFormat != mode.pixelFormat() ||
		vfmt.BytesPerLine != mode.BytesPerLine || vfmt.SizeImage != uint32(mode.frameBytes()) {
		return fmt.Errorf("share node %s took %dx%d %s (%d bytes per line, %d bytes) instead of %s",
			w.path, vfmt.Width, vfmt.Height, fourccName(vfmt.PixelFormat), vfmt.BytesPerLine, vfmt.SizeImage, mode)
	}

	// Best effort. A refusal leaves the driver's default interval, which is a
	// cosmetic difference for readers, never a correctness one.
	if mode.FPS > 0 {
		parm := v4l2StreamParm{Type: v4l2BufTypeVideoOutput, TpfNumerator: 1, TpfDenominator: mode.FPS}
		if err := w.ioctl(vidiocSParm, unsafe.Pointer(&parm)); err != nil {
			w.logger.Warn("VIDIOC_S_PARM refused on share node; it keeps the driver default interval",
				zap.String("device", w.path), zap.Stringer("mode", mode), zap.Error(err))
		}
	}
	// keep_format=1 (with the node's announce_all_caps=1 from EnsureShareNode)
	// keeps the node a capture device with this format while no writer streams,
	// so an app can open it during an agent restart. It is required, not best
	// effort: without it an app that opens the node in that window can
	// reformat it and force its format on the returning writer.
	keep := v4l2Control{ID: v4l2LoopbackCidKeepFormat, Value: 1}
	if err := w.ioctl(vidiocSCtrl, unsafe.Pointer(&keep)); err != nil {
		return fmt.Errorf("setting keep_format on %s: %w", w.path, err)
	}

	var req v4l2ReqBuffers
	req.Count = shareNodeBuffers
	req.Type = v4l2BufTypeVideoOutput
	req.Memory = v4l2MemoryMmap
	if err := w.ioctl(vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		return fmt.Errorf("VIDIOC_REQBUFS on %s: %w", w.path, err)
	}
	if req.Count == 0 {
		return fmt.Errorf("VIDIOC_REQBUFS on %s allocated no buffers", w.path)
	}
	for i := uint32(0); i < req.Count; i++ {
		var qbuf v4l2Buf
		qbuf.setIndex(i)
		qbuf.setType(v4l2BufTypeVideoOutput)
		qbuf.setMemory(v4l2MemoryMmap)
		if err := w.ioctl(vidiocQuerybuf, unsafe.Pointer(&qbuf)); err != nil {
			return fmt.Errorf("VIDIOC_QUERYBUF %d on %s: %w", i, w.path, err)
		}
		mem, err := unix.Mmap(w.fd, int64(qbuf.offset()), int(v4l2BufLength(&qbuf)),
			unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("mmap buffer %d on %s: %w", i, w.path, err)
		}
		w.buffers = append(w.buffers, mem)
		if len(mem) < mode.frameBytes() {
			return fmt.Errorf("share node %s buffer %d holds %d bytes, a frame is %d", w.path, i, len(mem), mode.frameBytes())
		}
	}

	// STREAMON with no buffer queued: a reader can then STREAMON, and it sees
	// the declared format (the node already announces CAPTURE through
	// announce_all_caps, and keep_format holds the format).
	bufType := int32(v4l2BufTypeVideoOutput)
	if err := w.ioctl(vidiocStreamon, unsafe.Pointer(&bufType)); err != nil {
		return fmt.Errorf("VIDIOC_STREAMON on %s: %w", w.path, err)
	}
	w.streaming = true

	sub := v4l2EventSubscription{Type: v4l2EventClientUsage, Flags: v4l2EventSubFlSendInitial}
	if err := w.ioctl(vidiocSubscribeEvent, unsafe.Pointer(&sub)); err != nil {
		return fmt.Errorf("VIDIOC_SUBSCRIBE_EVENT on %s: %w", w.path, err)
	}

	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		return fmt.Errorf("wake pipe for %s: %w", w.path, err)
	}
	unix.CloseOnExec(pipe[0])
	unix.CloseOnExec(pipe[1])
	w.wakeR, w.wakeW = pipe[0], pipe[1]
	w.counts = make(chan uint32, 1)
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		pumpShareEvents(w.logger, w.path, w.fd, w.wakeR, unix.POLLPRI, w.dequeueEvent, w.counts)
	}()
	return nil
}

// dequeueEvent runs one VIDIOC_DQEVENT: the client count and how many events
// are still queued.
func (w *shareNodeWriter) dequeueEvent(fd int) (count, pending uint32, err error) {
	var ev v4l2Event
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocDqevent, uintptr(unsafe.Pointer(&ev))); errno != 0 {
		err = errno
		return 0, 0, err
	}
	return ev.count(), ev.pending(), nil
}

// pumpShareEvents is the event goroutine's body. It waits on the node for
// wantEvents and on wakeFd for Close, drains every pending event on each wake
// and hands only the newest count to out, a channel of capacity 1 in which the
// newest value replaces an unread one. It is the only sender on out and closes
// it on every exit: a wake byte, POLLERR/POLLHUP/POLLNVAL, or a dequeue error
// (ENODEV when the node is removed). The reason for an exit that was not asked
// for is logged once before out closes.
func pumpShareEvents(logger *zap.Logger, path string, nodeFd, wakeFd int, wantEvents int16, dequeue func(fd int) (count, pending uint32, err error), out chan uint32) {
	defer close(out)
	fds := []unix.PollFd{
		{Fd: int32(nodeFd), Events: wantEvents},
		{Fd: int32(wakeFd), Events: unix.POLLIN},
	}
	const dead = unix.POLLERR | unix.POLLHUP | unix.POLLNVAL
	for {
		fds[0].Revents, fds[1].Revents = 0, 0
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			logger.Warn("share node event poll failed; no more reader counts", zap.String("device", path), zap.Error(err))
			return
		}
		if fds[1].Revents != 0 {
			return
		}
		if fds[0].Revents&dead != 0 {
			logger.Info("share node went away; no more reader counts",
				zap.String("device", path), zap.Int16("revents", fds[0].Revents))
			return
		}
		if fds[0].Revents&wantEvents == 0 {
			continue
		}
		var newest uint32
		got := false
		for {
			count, pending, err := dequeue(nodeFd)
			if err != nil {
				if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EINTR) {
					break // nothing (more) queued
				}
				if errors.Is(err, unix.ENODEV) {
					logger.Info("share node removed; no more reader counts", zap.String("device", path))
				} else {
					logger.Warn("share node event dequeue failed; no more reader counts", zap.String("device", path), zap.Error(err))
				}
				return
			}
			newest, got = count, true
			if pending == 0 {
				break
			}
		}
		if !got {
			continue
		}
		select {
		case out <- newest:
		default:
			select {
			case <-out: // drop the unread older value
			default:
			}
			out <- newest // cannot block: this goroutine is the only sender
		}
	}
}

func (w *shareNodeWriter) Counts() <-chan uint32 { return w.counts }

func (w *shareNodeWriter) WriteFrame(data []byte, monoNs int64) error {
	if len(data) != w.mode.frameBytes() {
		return fmt.Errorf("share node %s: frame is %d bytes, mode %s needs %d", w.path, len(data), w.mode, w.mode.frameBytes())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errShareNodeClosed
	}
	if len(w.buffers) == 0 {
		return fmt.Errorf("share node %s has no buffers", w.path)
	}
	idx := w.next
	copy(w.buffers[idx], data)

	var qbuf v4l2Buf
	qbuf.setIndex(uint32(idx))
	qbuf.setType(v4l2BufTypeVideoOutput)
	qbuf.setMemory(v4l2MemoryMmap)
	v4l2BufSetBytesUsed(&qbuf, uint32(len(data)))
	v4l2BufSetField(&qbuf, v4l2FieldNone)
	qbuf.setFlags(v4l2BufFlagTimestampCopy)
	qbuf.setTimestampNanos(monoNs)
	if err := w.ioctl(vidiocQbuf, unsafe.Pointer(&qbuf)); err != nil {
		return fmt.Errorf("VIDIOC_QBUF on %s: %w", w.path, err)
	}
	w.next = (idx + 1) % len(w.buffers)
	return nil
}

func (w *shareNodeWriter) WriteBlank(monoNs int64) error {
	return w.WriteFrame(blankFrame(w.mode), monoNs)
}

// Close stops the event goroutine, turns the stream off, unmaps the buffers
// and closes the node. It is idempotent: concurrent and repeated calls wait for
// the first to finish and return its result. In-flight writes complete first;
// later ones fail with errShareNodeClosed.
func (w *shareNodeWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.closeErr
	}
	w.closed = true

	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// The goroutine must be gone before the descriptor is closed under it.
	if w.done != nil {
		if w.wakeW >= 0 {
			_, _ = unix.Write(w.wakeW, []byte{1})
		}
		<-w.done
	}
	if w.fd >= 0 {
		if w.streaming {
			bufType := int32(v4l2BufTypeVideoOutput)
			if err := w.ioctl(vidiocStreamoff, unsafe.Pointer(&bufType)); err != nil {
				note(fmt.Errorf("VIDIOC_STREAMOFF on %s: %w", w.path, err))
			}
			w.streaming = false
		}
		for _, mem := range w.buffers {
			note(unix.Munmap(mem))
		}
		w.buffers = nil
		note(unix.Close(w.fd))
		w.fd = -1
	}
	for _, p := range []*int{&w.wakeR, &w.wakeW} {
		if *p >= 0 {
			_ = unix.Close(*p)
			*p = -1
		}
	}
	w.closeErr = firstErr
	return firstErr
}
