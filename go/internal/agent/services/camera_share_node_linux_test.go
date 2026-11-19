//go:build linux && regression_camera_fixture

package services

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const vidiocGFmtForShareTest = 0xC0D05604 // _IOWR('V', 4, struct v4l2_format)

// shareTestReader is a raw V4L2 capture client of a share node: the least an
// app does to read frames.
type shareTestReader struct {
	t    *testing.T
	fd   int
	bufs [][]byte
}

func openShareTestReader(t *testing.T, path string) *shareTestReader {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("opening %s as a reader: %v", path, err)
	}
	r := &shareTestReader{t: t, fd: fd}
	t.Cleanup(r.close)
	return r
}

func (r *shareTestReader) ioctl(req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(r.fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// start queues two buffers and runs STREAMON, returning STREAMON's error.
func (r *shareTestReader) start() error {
	var req v4l2ReqBuffers
	req.Count = 2
	req.Type = v4l2BufTypeVideoCapture
	req.Memory = v4l2MemoryMmap
	if err := r.ioctl(vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		r.t.Fatalf("reader REQBUFS: %v", err)
	}
	for i := uint32(0); i < req.Count; i++ {
		var b v4l2Buf
		b.setIndex(i)
		b.setType(v4l2BufTypeVideoCapture)
		b.setMemory(v4l2MemoryMmap)
		if err := r.ioctl(vidiocQuerybuf, unsafe.Pointer(&b)); err != nil {
			r.t.Fatalf("reader QUERYBUF: %v", err)
		}
		mem, err := unix.Mmap(r.fd, int64(b.offset()), int(v4l2BufLength(&b)), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			r.t.Fatalf("reader mmap: %v", err)
		}
		r.bufs = append(r.bufs, mem)
		if err := r.ioctl(vidiocQbuf, unsafe.Pointer(&b)); err != nil {
			r.t.Fatalf("reader QBUF: %v", err)
		}
	}
	typ := int32(v4l2BufTypeVideoCapture)
	return r.ioctl(vidiocStreamon, unsafe.Pointer(&typ))
}

// next waits for a frame and returns a copy of it and its timestamp.
func (r *shareTestReader) next(timeout time.Duration) ([]byte, int64) {
	r.t.Helper()
	fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, int(timeout.Milliseconds())); err != nil || n == 0 {
		r.t.Fatalf("no frame within %v (poll n=%d err=%v)", timeout, n, err)
	}
	var b v4l2Buf
	b.setType(v4l2BufTypeVideoCapture)
	b.setMemory(v4l2MemoryMmap)
	if err := r.ioctl(vidiocDqbuf, unsafe.Pointer(&b)); err != nil {
		r.t.Fatalf("reader DQBUF: %v", err)
	}
	frame := bytes.Clone(r.bufs[b.index()][:b.bytesUsed()])
	return frame, b.timestampNanos()
}

// tryNext is next without the failure: ok is false when no frame arrives in
// timeout.
func (r *shareTestReader) tryNext(timeout time.Duration) (frame []byte, ts int64, ok bool) {
	r.t.Helper()
	fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, int(timeout.Milliseconds())); err != nil || n == 0 {
		return nil, 0, false
	}
	frame, ts = r.next(time.Second)
	return frame, ts, true
}

func (r *shareTestReader) stop() {
	typ := int32(v4l2BufTypeVideoCapture)
	_ = r.ioctl(vidiocStreamoff, unsafe.Pointer(&typ))
}

func (r *shareTestReader) close() {
	if r.fd < 0 {
		return
	}
	for _, m := range r.bufs {
		_ = unix.Munmap(m)
	}
	_ = unix.Close(r.fd)
	r.fd = -1
}

func expectCount(t *testing.T, counts <-chan uint32, want uint32, what string) {
	t.Helper()
	select {
	case got, ok := <-counts:
		if !ok {
			t.Fatalf("%s: Counts() closed, want %d", what, want)
		}
		if got != want {
			t.Fatalf("%s: count %d, want %d", what, got, want)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("%s: no count within 100ms, want %d", what, want)
	}
}

func TestShareNodeWriter_DeclaresModeAndReportsReaders(t *testing.T) {
	requireShareModule(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	path := createShareFixtureNode(t, ctx, zap.NewNop())

	mode := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 30}
	w, err := openShareNode(zap.NewNop(), path, mode)
	if err != nil {
		t.Fatalf("openShareNode: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	counts := w.Counts()
	expectCount(t, counts, 0, "initial count")

	// The node is a capture device with the declared format.
	probe := openShareTestReader(t, path)
	var caps v4l2Capability
	if err := probe.ioctl(vidiocQueryCap, unsafe.Pointer(&caps)); err != nil {
		t.Fatalf("QUERYCAP: %v", err)
	}
	if !caps.hasVideoCapture() {
		t.Fatalf("node is not a capture device: caps %#x device caps %#x", caps.Capabilities, caps.DeviceCaps)
	}
	var got v4l2Format
	got.Type = v4l2BufTypeVideoCapture
	if err := probe.ioctl(vidiocGFmtForShareTest, unsafe.Pointer(&got)); err != nil {
		t.Fatalf("G_FMT: %v", err)
	}
	if got.Width != 640 || got.Height != 480 || got.PixelFormat != v4l2PixFmtYUYV || got.BytesPerLine != 1280 {
		t.Fatalf("G_FMT(CAPTURE) = %dx%d %s, %d bytes per line; want 640x480 YUYV, 1280", got.Width, got.Height, fourccName(got.PixelFormat), got.BytesPerLine)
	}
	probe.close() // a probe that never streams does not count as a reader

	reader := openShareTestReader(t, path)
	if err := reader.start(); err != nil {
		t.Fatalf("reader STREAMON: %v", err)
	}
	expectCount(t, counts, 1, "after the reader's STREAMON")

	frame := make([]byte, mode.frameBytes())
	for i := range frame {
		frame[i] = byte(i*7 + i/1280)
	}
	const ts = int64(123456789_123456_000) // whole microseconds
	if err := w.WriteFrame(frame, ts); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	gotFrame, gotTs := reader.next(2 * time.Second)
	if !bytes.Equal(gotFrame, frame) {
		t.Fatalf("reader got a different frame (%d bytes, want %d)", len(gotFrame), len(frame))
	}
	if gotTs/1000 != ts/1000 {
		t.Fatalf("timestamp %d µs, want %d µs", gotTs/1000, ts/1000)
	}

	reader.stop()
	expectCount(t, counts, 0, "after the reader's STREAMOFF")
	reader.close()

	const ts2 = int64(223456789_654321_000)
	if err := w.WriteBlank(ts2); err != nil {
		t.Fatalf("WriteBlank: %v", err)
	}
	late := openShareTestReader(t, path)
	if err := late.start(); err != nil {
		t.Fatalf("second reader STREAMON: %v", err)
	}
	blank, blankTs := late.next(2 * time.Second)
	if !bytes.Equal(blank, blankFrame(mode)) {
		t.Fatal("a new reader's first frame is not the black frame")
	}
	if blankTs/1000 != ts2/1000 {
		t.Fatalf("blank timestamp %d µs, want %d µs", blankTs/1000, ts2/1000)
	}
	// The ring is black throughout: the next frame, if any, is not the earlier one.
	if second, _, ok := late.tryNext(200 * time.Millisecond); ok && !bytes.Equal(second, blankFrame(mode)) {
		t.Fatal("a new reader's second frame is neither black nor absent (the earlier frame leaked)")
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close closes Counts(): drain what is buffered, then expect the close.
	deadline := time.After(2 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-counts:
		case <-deadline:
			t.Fatal("Counts() not closed after Close")
		}
	}
}

// requireShareModule skips, or fails under WENDY_GSTREAMER_REQUIRED=1, unless
// the real v4l2loopback module and root are available.
func requireShareModule(t *testing.T) {
	t.Helper()
	unavailable := func(format string, args ...any) {
		t.Helper()
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatalf("required "+format, args...)
		}
		t.Skipf(format+" (set WENDY_GSTREAMER_REQUIRED=1 to fail instead of skipping)", args...)
	}
	if os.Geteuid() != 0 {
		unavailable("root, to create a v4l2loopback node")
	}
	if info, err := os.Stat("/dev/v4l2loopback"); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		unavailable("loaded v4l2loopback module (/dev/v4l2loopback): %v", err)
	}
	if version, err := os.ReadFile("/sys/module/v4l2loopback/version"); err != nil || !strings.HasPrefix(strings.TrimSpace(string(version)), "0.15.") {
		unavailable("v4l2loopback 0.15.x: version=%q err=%v", bytes.TrimSpace(version), err)
	}
}

func TestShareNode_StaysACameraWhileTheWriterIsAway(t *testing.T) {
	requireShareModule(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	path := createShareFixtureNode(t, ctx, zap.NewNop())

	mode := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 30}
	fill := func(seed byte) []byte {
		f := make([]byte, mode.frameBytes())
		for i := range f {
			f[i] = byte(i*3) + seed
		}
		return f
	}
	w, err := openShareNode(zap.NewNop(), path, mode)
	if err != nil {
		t.Fatalf("openShareNode: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() }) // runs before the node's removal
	if err := w.WriteFrame(fill(1), 1_000_000_000); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The writer is away. The node must still look like a camera.
	reader := openShareTestReader(t, path)
	var caps v4l2Capability
	if err := reader.ioctl(vidiocQueryCap, unsafe.Pointer(&caps)); err != nil {
		t.Fatalf("QUERYCAP: %v", err)
	}
	if !caps.hasVideoCapture() {
		t.Fatalf("node is not a capture device while the writer is away: caps %#x device caps %#x", caps.Capabilities, caps.DeviceCaps)
	}
	var got v4l2Format
	got.Type = v4l2BufTypeVideoCapture
	if err := reader.ioctl(vidiocGFmtForShareTest, unsafe.Pointer(&got)); err != nil {
		t.Fatalf("G_FMT while the writer is away: %v", err)
	}
	if got.Width != 640 || got.Height != 480 || got.PixelFormat != v4l2PixFmtYUYV || got.BytesPerLine != 1280 {
		t.Fatalf("G_FMT = %dx%d %s, %d bytes per line, want 640x480 YUYV, 1280", got.Width, got.Height, fourccName(got.PixelFormat), got.BytesPerLine)
	}
	// An app asking for another size is told the locked mode.
	other := v4l2Format{Type: v4l2BufTypeVideoCapture, Width: 320, Height: 240, PixelFormat: v4l2PixFmtYUYV, Field: v4l2FieldNone}
	if err := reader.ioctl(vidiocSFmt, unsafe.Pointer(&other)); err != nil {
		t.Fatalf("S_FMT while the writer is away: %v", err)
	}
	if other.Width != 640 || other.Height != 480 || other.PixelFormat != v4l2PixFmtYUYV {
		t.Fatalf("S_FMT to 320x240 gave %dx%d %s, want the locked 640x480 YUYV", other.Width, other.Height, fourccName(other.PixelFormat))
	}
	if err := reader.start(); err != nil {
		t.Fatalf("reader STREAMON while the writer is away: %v", err)
	}

	// The writer returns (an agent restart) and the same reader gets its frame.
	w2, err := openShareNode(zap.NewNop(), path, mode)
	if err != nil {
		t.Fatalf("re-opening the writer: %v", err)
	}
	t.Cleanup(func() { _ = w2.Close() })
	want := fill(9)
	if err := w2.WriteFrame(want, 2_000_000_000); err != nil {
		t.Fatalf("WriteFrame after re-attach: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatal("the new frame did not reach the reader within 2s")
		}
		frame, _ := reader.next(remaining)
		if bytes.Equal(frame, want) {
			return
		}
		// Anything else must be the stale last frame from before the restart.
		if !bytes.Equal(frame, fill(1)) {
			t.Fatal("reader received a frame that is neither the stale one nor the new one")
		}
	}
}
