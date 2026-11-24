//go:build linux && regression_camera_fixture

package services

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
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
	stamp := b.timestampNanos()
	if err := r.ioctl(vidiocQbuf, unsafe.Pointer(&b)); err != nil {
		r.t.Fatalf("reader requeue: %v", err)
	}
	return frame, stamp
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
	if err := w.WriteFrame(frame, stampWithCaptureTime(ts)); err != nil {
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

	// Repeated open/read/close never exposes the previous session's picture.
	for attempt := 0; attempt < 3; attempt++ {
		late := openShareTestReader(t, path)
		if err := late.start(); err != nil {
			t.Fatalf("new reader STREAMON: %v", err)
		}
		expectCount(t, counts, 1, "new reader")
		late.expectNoFrame(150 * time.Millisecond)
		fresh := bytes.Repeat([]byte{byte(40 + attempt)}, mode.frameBytes())
		stamp := ts + int64(attempt+1)*1_000_000
		if err := w.WriteFrame(fresh, stampWithCaptureTime(stamp)); err != nil {
			t.Fatal(err)
		}
		got, gotStamp := late.next(2 * time.Second)
		if !bytes.Equal(got, fresh) || gotStamp != stamp {
			t.Fatal("FRESH_CAPTURE_CONTRACT: first frame was not the new source frame")
		}
		late.stop()
		expectCount(t, counts, 0, "new reader stopped")
		late.close()
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
	if err := w.WriteFrame(fill(1), stampWithCaptureTime(1_000_000_000)); err != nil {
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

	reader.expectNoFrame(200 * time.Millisecond)
	// The writer returns (an agent restart) and the same reader gets its first fresh frame.
	w2, err := openShareNode(zap.NewNop(), path, mode)
	if err != nil {
		t.Fatalf("re-opening the writer: %v", err)
	}
	t.Cleanup(func() { _ = w2.Close() })
	want := fill(9)
	if err := w2.WriteFrame(want, stampWithCaptureTime(2_000_000_000)); err != nil {
		t.Fatalf("WriteFrame after re-attach: %v", err)
	}
	frame, stamp := reader.next(2 * time.Second)
	if !bytes.Equal(frame, want) || stamp != 2_000_000_000 {
		t.Fatal("FRESH_CAPTURE_CONTRACT: first frame after writer restart was stale")
	}
}

// Check both readiness and nonblocking DQBUF. A driver must not advertise a
// retained image, nor silently return it to a caller that skips poll.
func (r *shareTestReader) expectNoFrame(timeout time.Duration) {
	r.t.Helper()
	fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, int(timeout.Milliseconds())); err != nil || n != 0 {
		r.t.Fatalf("FRESH_CAPTURE_CONTRACT: reader became ready before a new frame: n=%d revents=%#x err=%v", n, fds[0].Revents, err)
	}
	var b v4l2Buf
	b.setType(v4l2BufTypeVideoCapture)
	b.setMemory(v4l2MemoryMmap)
	if err := r.ioctl(vidiocDqbuf, unsafe.Pointer(&b)); !errors.Is(err, unix.EAGAIN) {
		r.t.Fatalf("FRESH_CAPTURE_CONTRACT: DQBUF before fresh frame = %v, want EAGAIN", err)
	}
}

func TestShareNode_FreshStartForLateAndRestartedReaders(t *testing.T) {
	requireShareModule(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	path := createShareFixtureNode(t, ctx, zap.NewNop())
	mode := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 30}
	w, err := openShareNode(zap.NewNop(), path, mode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	expectCount(t, w.Counts(), 0, "initial")
	first := openShareTestReader(t, path)
	if err := first.start(); err != nil {
		t.Fatal(err)
	}
	expectCount(t, w.Counts(), 1, "first reader")
	old := bytes.Repeat([]byte{20}, mode.frameBytes())
	if err := w.WriteFrame(old, stampWithCaptureTime(1_000_000_000)); err != nil {
		t.Fatal(err)
	}
	first.next(time.Second)
	// The driver grants one CAPTURE owner per node. Concurrent apps use
	// separate share nodes; here a late opener replaces the previous reader.
	first.stop()
	expectCount(t, w.Counts(), 0, "first reader stopped")
	first.close()
	late := openShareTestReader(t, path)
	if err := late.start(); err != nil {
		t.Fatal(err)
	}
	expectCount(t, w.Counts(), 1, "late reader")
	late.expectNoFrame(150 * time.Millisecond)
	fresh := bytes.Repeat([]byte{60}, mode.frameBytes())
	if err := w.WriteFrame(fresh, stampWithCaptureTime(2_000_000_000)); err != nil {
		t.Fatal(err)
	}
	// A duplicate STREAMON must not discard the frame already waiting.
	typ := int32(v4l2BufTypeVideoCapture)
	if err := late.ioctl(vidiocStreamon, unsafe.Pointer(&typ)); err != nil {
		t.Fatal(err)
	}
	got, stamp := late.next(time.Second)
	if !bytes.Equal(got, fresh) || stamp != 2_000_000_000 {
		t.Fatal("FRESH_CAPTURE_CONTRACT: late reader got stale pixels")
	}
	late.stop()
	expectCount(t, w.Counts(), 0, "late reader stopped")
	if err := w.WriteFrame(old, stampWithCaptureTime(3_000_000_000)); err != nil {
		t.Fatal(err)
	}
	if err := late.ioctl(vidiocStreamon, unsafe.Pointer(&typ)); err != nil {
		t.Fatal(err)
	}
	expectCount(t, w.Counts(), 1, "same descriptor restarted")
	late.expectNoFrame(150 * time.Millisecond)
	if err := w.WriteFrame(fresh, stampWithCaptureTime(4_000_000_000)); err != nil {
		t.Fatal(err)
	}
	got, stamp = late.next(time.Second)
	if !bytes.Equal(got, fresh) || stamp != 4_000_000_000 {
		t.Fatal("FRESH_CAPTURE_CONTRACT: restarted descriptor got old pixels")
	}
	t.Log("FRESH_CAPTURE_READER_EPOCHS_VERIFIED")
}

func TestShareNode_FreshStartSuppressesSyntheticFrames(t *testing.T) {
	requireShareModule(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	path := createShareFixtureNode(t, ctx, zap.NewNop())
	mode := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 30}
	w, err := openShareNode(zap.NewNop(), path, mode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	writer := w.(*shareNodeWriter)
	for _, setting := range []v4l2Control{{ID: 0x0098f901, Value: 1}, {ID: 0x0098f902, Value: 100}} {
		if err := writer.ioctl(vidiocSCtrl, unsafe.Pointer(&setting)); err != nil {
			t.Fatalf("set synthetic-frame control: %v", err)
		}
	}
	old := bytes.Repeat([]byte{20}, mode.frameBytes())
	if err := w.WriteFrame(old, stampWithCaptureTime(1_000_000_000)); err != nil {
		t.Fatal(err)
	}
	reader := openShareTestReader(t, path)
	if err := reader.start(); err != nil {
		t.Fatal(err)
	}
	reader.expectNoFrame(350 * time.Millisecond)
	fresh := bytes.Repeat([]byte{80}, mode.frameBytes())
	if err := w.WriteFrame(fresh, stampWithCaptureTime(2_000_000_000)); err != nil {
		t.Fatal(err)
	}
	got, stamp := reader.next(time.Second)
	if !bytes.Equal(got, fresh) || stamp != 2_000_000_000 {
		t.Fatal("FRESH_CAPTURE_CONTRACT: first frame was a timer repeat or timeout image")
	}
	t.Log("FRESH_CAPTURE_TIMERS_VERIFIED")
}

func TestShareNode_FreshStartForReadIO(t *testing.T) {
	requireShareModule(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	path := createShareFixtureNode(t, ctx, zap.NewNop())
	mode := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 30}
	w, err := openShareNode(zap.NewNop(), path, mode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	old := bytes.Repeat([]byte{20}, mode.frameBytes())
	if err := w.WriteFrame(old, stampWithCaptureTime(1_000_000_000)); err != nil {
		t.Fatal(err)
	}
	reader := openShareTestReader(t, path)
	buffer := make([]byte, mode.frameBytes())
	// read() lazily starts CAPTURE without explicit REQBUFS or STREAMON.
	if n, err := unix.Read(reader.fd, buffer); n > 0 || !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("FRESH_CAPTURE_CONTRACT: first read() returned retained data: n=%d err=%v", n, err)
	}
	fresh := bytes.Repeat([]byte{90}, mode.frameBytes())
	if err := w.WriteFrame(fresh, stampWithCaptureTime(2_000_000_000)); err != nil {
		t.Fatal(err)
	}
	if n, err := unix.Read(reader.fd, buffer); err != nil || n != len(fresh) || !bytes.Equal(buffer, fresh) {
		t.Fatalf("FRESH_CAPTURE_CONTRACT: read() did not return fresh source pixels: n=%d err=%v", n, err)
	}
	t.Log("FRESH_CAPTURE_READ_IO_VERIFIED")
}

func TestShareNodeWriter_CompressedNodeCarriesWholeFramesOfAnyLength(t *testing.T) {
	requireShareModule(t)
	for _, mode := range []sharedMode{
		{Width: 640, Height: 480, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes},
		{Width: 640, Height: 480, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatH264, MaxFrameBytes: maxFrameBytes},
		// 1920x1080 at the module's own depth is larger than the maximum frame.
		{Width: 1920, Height: 1080, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes},
	} {
		t.Run(mode.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			t.Cleanup(cancel)
			path := createShareFixtureNode(t, ctx, zap.NewNop())
			w, err := openShareNode(zap.NewNop(), path, mode)
			if err != nil {
				t.Fatalf("openShareNode: %v", err)
			}
			t.Cleanup(func() { _ = w.Close() })
			counts := w.Counts()
			expectCount(t, counts, 0, "initial count")

			probe := openShareTestReader(t, path)
			var got v4l2Format
			got.Type = v4l2BufTypeVideoCapture
			if err := probe.ioctl(vidiocGFmtForShareTest, unsafe.Pointer(&got)); err != nil {
				t.Fatalf("G_FMT: %v", err)
			}
			if got.Width != mode.Width || got.Height != mode.Height || got.PixelFormat != mode.pixelFormat() || got.SizeImage < mode.MaxFrameBytes {
				t.Fatalf("G_FMT(CAPTURE) = %dx%d %s, image of %d bytes; want %s with an image of at least %d",
					got.Width, got.Height, fourccName(got.PixelFormat), got.SizeImage, mode, mode.MaxFrameBytes)
			}
			t.Logf("%s: the node declares an image of %d bytes for a maximum frame of %d", mode, got.SizeImage, mode.MaxFrameBytes)
			probe.close()

			reader := openShareTestReader(t, path)
			if err := reader.start(); err != nil {
				t.Fatalf("reader STREAMON: %v", err)
			}
			expectCount(t, counts, 1, "after the reader's STREAMON")
			for i, size := range []int{17, 4096, 100_001, int(mode.MaxFrameBytes)} {
				frame := make([]byte, size)
				for j := range frame {
					frame[j] = byte(j*5 + i)
				}
				stamp := int64(i+1) * 1_000_000_000
				if err := w.WriteFrame(frame, stampWithCaptureTime(stamp)); err != nil {
					t.Fatalf("WriteFrame of %d bytes: %v", size, err)
				}
				gotFrame, gotStamp := reader.next(2 * time.Second)
				if !bytes.Equal(gotFrame, frame) || gotStamp != stamp {
					t.Fatalf("reader got %d bytes stamped %d, want the %d-byte frame stamped %d", len(gotFrame), gotStamp, size, stamp)
				}
			}

			err = w.WriteFrame(make([]byte, int(mode.MaxFrameBytes)+1), stampWithCaptureTime(9_000_000_000))
			if !errors.Is(err, errShareFrameTooLarge) || w.(*shareNodeWriter).oversizedFrames() != 1 {
				t.Fatalf("a frame over the maximum: error %v, counted %d", err, w.(*shareNodeWriter).oversizedFrames())
			}
			reader.expectNoFrame(150 * time.Millisecond)
		})
	}
}

func TestShareNodeWriter_BufferTimestampReadsBackAsTheCaptureTimeOrTheSampleID(t *testing.T) {
	requireShareModule(t)
	for _, mode := range []sharedMode{
		{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 30, Format: appconfig.CameraFormatPixels},
		{Width: 640, Height: 480, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes},
	} {
		t.Run(mode.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			t.Cleanup(cancel)
			path := createShareFixtureNode(t, ctx, zap.NewNop())
			w, err := openShareNode(zap.NewNop(), path, mode)
			if err != nil {
				t.Fatalf("openShareNode: %v", err)
			}
			t.Cleanup(func() { _ = w.Close() })
			expectCount(t, w.Counts(), 0, "initial count")
			reader := openShareTestReader(t, path)
			if err := reader.start(); err != nil {
				t.Fatalf("reader STREAMON: %v", err)
			}
			expectCount(t, w.Counts(), 1, "after the reader's STREAMON")
			frame := bytes.Repeat([]byte{0x42}, mode.frameBytes())
			if mode.compressed() {
				frame = []byte{0xFF, 0xD8, 0x42, 0xFF, 0xD9}
			}

			const captured = int64(123_456_789_123_456_000) // whole microseconds
			if err := w.WriteFrame(frame, stampWithCaptureTime(captured)); err != nil {
				t.Fatalf("WriteFrame with a capture time: %v", err)
			}
			if _, stamp := reader.next(2 * time.Second); stamp != captured {
				t.Fatalf("capture time read back as %d, want %d", stamp, captured)
			}

			// A reader recovers a sample ID as tv_sec*1e6 + tv_usec, which is
			// the dequeued timestamp in whole microseconds.
			for _, id := range []uint64{1, 999_999, 1_000_000, 1234567890123} {
				if err := w.WriteFrame(frame, stampWithSampleID(id)); err != nil {
					t.Fatalf("WriteFrame with sample ID %d: %v", id, err)
				}
				if _, stamp := reader.next(2 * time.Second); uint64(stamp/1000) != id || stamp%1000 != 0 {
					t.Fatalf("sample ID %d read back as %d µs (%d ns)", id, stamp/1000, stamp)
				}
			}

			if err := w.WriteFrame(frame, stampWithSampleID(0)); !errors.Is(err, errShareFrameWithoutSampleID) {
				t.Fatalf("a frame without a sample ID: error %v, want errShareFrameWithoutSampleID", err)
			}
			reader.expectNoFrame(150 * time.Millisecond)
		})
	}
}
