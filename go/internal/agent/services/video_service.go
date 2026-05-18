package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// V4L2 ioctl constants for Linux kernel video capture interface.
const (
	v4l2BufTypeVideoCapture = 1
	v4l2MemoryMmap          = 1
	v4l2PixFmtH264          = 0x34363248 // 'H264' little-endian FourCC
	v4l2FieldNone           = 1

	v4l2CapVideoCapture = 0x00000001
	v4l2CapMetaCapture  = 0x00800000
	v4l2CapDeviceCaps   = 0x80000000

	vidiocQueryCap  = 0x80685600
	vidiocSFmt      = 0xC0D05605
	vidiocReqbufs   = 0xC0145608
	vidiocQuerybuf  = 0xC0585609
	vidiocQbuf      = 0xC058560F
	vidiocDqbuf     = 0xC0585611
	vidiocStreamon  = 0x40045612
	vidiocStreamoff = 0x40045613
)

// v4l2Format matches struct v4l2_format (208 bytes) for V4L2_BUF_TYPE_VIDEO_CAPTURE.
type v4l2Format struct {
	Type         uint32
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
	_            [156]byte
}

// v4l2ReqBuffers matches struct v4l2_requestbuffers (20 bytes).
type v4l2ReqBuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Flags        uint32
}

// v4l2Buf is a fixed-size byte array matching struct v4l2_buffer (88 bytes on 64-bit Linux).
// Accessor methods read/write fields at their known offsets to avoid C-struct alignment surprises.
type v4l2Buf [88]byte

func (b *v4l2Buf) index() uint32      { return *(*uint32)(unsafe.Pointer(&b[0])) }
func (b *v4l2Buf) setIndex(i uint32)  { *(*uint32)(unsafe.Pointer(&b[0])) = i }
func (b *v4l2Buf) setType(t uint32)   { *(*uint32)(unsafe.Pointer(&b[4])) = t }
func (b *v4l2Buf) bytesUsed() uint32  { return *(*uint32)(unsafe.Pointer(&b[8])) }
func (b *v4l2Buf) setMemory(m uint32) { *(*uint32)(unsafe.Pointer(&b[60])) = m }
func (b *v4l2Buf) offset() uint32     { return *(*uint32)(unsafe.Pointer(&b[64])) }

// v4l2Capability matches struct v4l2_capability (104 bytes).
type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

func (c *v4l2Capability) hasVideoCapture() bool {
	caps := c.Capabilities
	if caps&v4l2CapDeviceCaps != 0 {
		caps = c.DeviceCaps
	}
	// Require VIDEO_CAPTURE and exclude metadata-only nodes (e.g. the UVC
	// metadata companion device that some drivers expose on /dev/video1).
	return caps&v4l2CapVideoCapture != 0 && caps&v4l2CapMetaCapture == 0
}

// nativeH264NotSupported is returned when the V4L2 device does not expose H.264 output.
type nativeH264NotSupported struct{ msg string }

func (e nativeH264NotSupported) Error() string { return e.msg }

// videoFrame carries a single encoded video frame from a producer to subscribers.
type videoFrame struct {
	data  []byte
	tsNs  uint64
	codec agentpb.VideoCodec
}

// deviceHub multiplexes one camera producer to multiple gRPC subscribers.
type deviceHub struct {
	mu     sync.Mutex
	subs   map[int]chan videoFrame
	nextID int
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // closed by runProducer after the device fd is released
}

// subscribe adds a new subscriber and returns its channel and integer ID.
func (h *deviceHub) subscribe() (int, chan videoFrame) {
	ch := make(chan videoFrame, 4)
	h.mu.Lock()
	id := h.nextID
	h.nextID++
	h.subs[id] = ch
	h.mu.Unlock()
	return id, ch
}

// unsubscribe removes a subscriber. When the last subscriber leaves it cancels the producer.
func (h *deviceHub) unsubscribe(id int) {
	h.mu.Lock()
	delete(h.subs, id)
	empty := len(h.subs) == 0
	h.mu.Unlock()
	if empty {
		h.cancel()
	}
}

// broadcast delivers a frame to all subscribers, dropping for slow consumers.
// Returns false when there are no subscribers left (producer should stop).
func (h *deviceHub) broadcast(frame videoFrame) bool {
	h.mu.Lock()
	if len(h.subs) == 0 {
		h.mu.Unlock()
		return false
	}
	for _, ch := range h.subs {
		select {
		case ch <- frame:
		default:
		}
	}
	h.mu.Unlock()
	return true
}

// VideoService implements agentpb.WendyVideoServiceServer.
type VideoService struct {
	agentpb.UnimplementedWendyVideoServiceServer
	logger          *zap.Logger
	globDevices     func() ([]string, error)
	readDeviceName  func(base string) (string, error)
	hasVideoCapture func(path string) bool

	mu   sync.Mutex
	hubs map[string]*deviceHub
}

// NewVideoService creates a new VideoService.
func NewVideoService(logger *zap.Logger) *VideoService {
	return &VideoService{
		logger: logger,
		hubs:   make(map[string]*deviceHub),
		globDevices: func() ([]string, error) {
			return filepath.Glob("/dev/video*")
		},
		readDeviceName: func(base string) (string, error) {
			b, err := os.ReadFile(fmt.Sprintf("/sys/class/video4linux/%s/name", base))
			return strings.TrimSpace(string(b)), err
		},
		hasVideoCapture: func(path string) bool {
			fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
			if err != nil {
				return false
			}
			defer unix.Close(fd) //nolint:errcheck
			var cap v4l2Capability
			_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQueryCap, uintptr(unsafe.Pointer(&cap)))
			return errno == 0 && cap.hasVideoCapture()
		},
	}
}

// listV4L2Devices enumerates /dev/video* and reads human-readable names from sysfs.
func (s *VideoService) listV4L2Devices() ([]*agentpb.VideoDevice, error) {
	paths, err := s.globDevices()
	if err != nil {
		return nil, err
	}
	var devices []*agentpb.VideoDevice
	for _, path := range paths {
		base := filepath.Base(path)
		numStr := strings.TrimPrefix(base, "video")
		id, err := strconv.ParseUint(numStr, 10, 32)
		if err != nil {
			continue
		}
		if !s.hasVideoCapture(path) {
			continue
		}
		name, err := s.readDeviceName(base)
		if err != nil {
			name = base
		}
		devices = append(devices, &agentpb.VideoDevice{
			Id:   uint32(id),
			Name: name,
			Path: path,
		})
	}
	return devices, nil
}

// ListVideoDevices enumerates V4L2 video capture devices.
func (s *VideoService) ListVideoDevices(ctx context.Context, _ *agentpb.ListVideoDevicesRequest) (*agentpb.ListVideoDevicesResponse, error) {
	devices, err := s.listV4L2Devices()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to enumerate video devices: %v", err)
	}
	return &agentpb.ListVideoDevicesResponse{Devices: devices}, nil
}

// getOrCreateHub returns the existing hub for path, or starts a new producer and hub.
// The caller receives a hub with at least one subscriber already registered (the returned id/ch).
func (s *VideoService) getOrCreateHub(path string, req *agentpb.StreamVideoRequest) (h *deviceHub, id int, ch chan videoFrame) {
	for {
		s.mu.Lock()
		h, exists := s.hubs[path]
		if !exists {
			break
		}
		if h.ctx.Err() == nil {
			id, ch = h.subscribe()
			s.mu.Unlock()
			return h, id, ch
		}
		// Hub is cancelling. Evict it and wait for the producer to release
		// the device fd before opening a new one — otherwise VIDIOC_S_FMT
		// returns EBUSY while the old streaming session is still active.
		delete(s.hubs, path)
		done := h.done
		s.mu.Unlock()
		<-done
	}
	// s.mu is held here (broke out of loop with no hub in map).

	ctx, cancel := context.WithCancel(context.Background())
	h = &deviceHub{
		subs:   make(map[int]chan videoFrame),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	id, ch = h.subscribe()
	s.hubs[path] = h
	s.mu.Unlock()

	go s.runProducer(ctx, h, path, req)
	return h, id, ch
}

// runProducer drives the capture loop for a single device hub.
// It tries native V4L2 H.264 first, falling back to GStreamer when unsupported.
// When the hub loses its last subscriber the context is cancelled and this goroutine exits.
func (s *VideoService) runProducer(ctx context.Context, h *deviceHub, path string, req *agentpb.StreamVideoRequest) {
	broadcast := func(data []byte, tsNs uint64, codec agentpb.VideoCodec) bool {
		return h.broadcast(videoFrame{data: data, tsNs: tsNs, codec: codec})
	}

	err := s.streamV4L2Native(ctx, broadcast, path, req)
	if _, ok := err.(nativeH264NotSupported); ok {
		s.logger.Info("native H.264 not supported, falling back to GStreamer", zap.String("device", path))
		err = s.streamGStreamer(ctx, broadcast, path, req)
	}
	if err != nil && ctx.Err() == nil {
		s.logger.Error("video producer exited with error", zap.String("device", path), zap.Error(err))
	}

	// Remove hub so the next StreamVideo call spawns a fresh producer.
	s.mu.Lock()
	if s.hubs[path] == h {
		delete(s.hubs, path)
	}
	s.mu.Unlock()

	// Close all subscriber channels so their loops unblock.
	h.mu.Lock()
	for _, ch := range h.subs {
		close(ch)
	}
	h.mu.Unlock()

	// Signal that the device fd is fully released. getOrCreateHub waits on
	// this before opening a new producer to avoid EBUSY on reconnect.
	close(h.done)
}

// StreamVideo streams H.264 frames from a V4L2 camera.
// Multiple concurrent callers for the same device share one producer via a deviceHub.
func (s *VideoService) StreamVideo(req *agentpb.StreamVideoRequest, stream grpc.ServerStreamingServer[agentpb.VideoFrame]) error {
	ctx := stream.Context()
	path := fmt.Sprintf("/dev/video%d", req.GetDeviceId())

	if _, err := os.Stat(path); err != nil {
		return status.Errorf(codes.NotFound, "video device %s not found", path)
	}

	h, id, ch := s.getOrCreateHub(path, req)
	defer h.unsubscribe(id)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-ch:
			if !ok {
				// Producer exited.
				return status.Errorf(codes.Internal, "video producer for %s stopped", path)
			}
			if err := stream.Send(&agentpb.VideoFrame{
				Data:        frame.data,
				TimestampNs: frame.tsNs,
				Codec:       frame.codec,
			}); err != nil {
				return err
			}
		}
	}
}

// streamV4L2Native opens the V4L2 device, configures H.264 output via VIDIOC_S_FMT,
// allocates mmap buffers, and streams frames until ctx is cancelled or an error occurs.
// Each captured frame is delivered via the broadcast callback; if the callback returns
// false the loop exits cleanly (no subscribers remain).
// Returns nativeH264NotSupported if the device rejects the H.264 pixel format.
func (s *VideoService) streamV4L2Native(ctx context.Context, broadcast func([]byte, uint64, agentpb.VideoCodec) bool, path string, req *agentpb.StreamVideoRequest) error {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return status.Errorf(codes.Internal, "open %s: %v", path, err)
	}
	defer unix.Close(fd) //nolint:errcheck

	// Configure H.264 output format.
	var vfmt v4l2Format
	vfmt.Type = v4l2BufTypeVideoCapture
	if req.GetWidth() > 0 {
		vfmt.Width = req.GetWidth()
	}
	if req.GetHeight() > 0 {
		vfmt.Height = req.GetHeight()
	}
	vfmt.PixelFormat = v4l2PixFmtH264
	vfmt.Field = v4l2FieldNone

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocSFmt, uintptr(unsafe.Pointer(&vfmt))); errno != 0 {
		if errno == unix.EINVAL {
			return nativeH264NotSupported{msg: fmt.Sprintf("VIDIOC_S_FMT H264 rejected: %v", errno)}
		}
		return status.Errorf(codes.Internal, "VIDIOC_S_FMT failed for %s: %v", path, errno)
	}
	if vfmt.PixelFormat != v4l2PixFmtH264 {
		return nativeH264NotSupported{msg: "device switched pixel format away from H264"}
	}

	// Two buffers: one dequeued/in-flight, one queued for the camera to fill.
	// More buffers increase kernel-side lag when the broadcast lags the camera.
	const numBuffers = 2
	var req4 v4l2ReqBuffers
	req4.Count = numBuffers
	req4.Type = v4l2BufTypeVideoCapture
	req4.Memory = v4l2MemoryMmap

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocReqbufs, uintptr(unsafe.Pointer(&req4))); errno != 0 {
		return status.Errorf(codes.Internal, "VIDIOC_REQBUFS: %v", errno)
	}
	if req4.Count < 2 {
		return status.Errorf(codes.Internal, "insufficient buffer memory on device")
	}

	// Map and queue each buffer.
	type mappedBuf struct {
		data []byte
	}
	mapped := make([]mappedBuf, req4.Count)

	for i := uint32(0); i < req4.Count; i++ {
		var qbuf v4l2Buf
		qbuf.setIndex(i)
		qbuf.setType(v4l2BufTypeVideoCapture)
		qbuf.setMemory(v4l2MemoryMmap)

		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQuerybuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return status.Errorf(codes.Internal, "VIDIOC_QUERYBUF[%d]: %v", i, errno)
		}

		length := uint32(*(*uint32)(unsafe.Pointer(&qbuf[72]))) // length at offset 72 in v4l2_buffer
		data, err := unix.Mmap(fd, int64(qbuf.offset()), int(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return status.Errorf(codes.Internal, "mmap buffer %d: %v", i, err)
		}
		mapped[i].data = data

		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQbuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return status.Errorf(codes.Internal, "VIDIOC_QBUF[%d]: %v", i, errno)
		}
	}
	defer func() {
		for _, m := range mapped {
			unix.Munmap(m.data) //nolint:errcheck
		}
	}()

	// Start streaming.
	bufType := uint32(v4l2BufTypeVideoCapture)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocStreamon, uintptr(unsafe.Pointer(&bufType))); errno != 0 {
		return status.Errorf(codes.Internal, "VIDIOC_STREAMON: %v", errno)
	}
	defer func() {
		unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocStreamoff, uintptr(unsafe.Pointer(&bufType))) //nolint:errcheck
	}()

	pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	var framesSent int
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
			continue // timeout or signal — re-check ctx.Done
		}
		if err != nil {
			return status.Errorf(codes.Internal, "poll %s: %v", path, err)
		}

		var dqbuf v4l2Buf
		dqbuf.setType(v4l2BufTypeVideoCapture)
		dqbuf.setMemory(v4l2MemoryMmap)

		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocDqbuf, uintptr(unsafe.Pointer(&dqbuf))); errno != 0 {
			if errno == unix.EINTR || errno == unix.EAGAIN {
				continue
			}
			// Device accepted H264 format but failed before delivering any frame —
			// fall back to the GStreamer software encoder path.
			if framesSent == 0 {
				return nativeH264NotSupported{msg: fmt.Sprintf("VIDIOC_DQBUF failed before first frame: %v", errno)}
			}
			return status.Errorf(codes.Internal, "VIDIOC_DQBUF: %v", errno)
		}

		idx := dqbuf.index()
		n := dqbuf.bytesUsed()
		if n == 0 {
			// Empty buffer — requeue and skip.
			var qbuf v4l2Buf
			qbuf.setIndex(idx)
			qbuf.setType(v4l2BufTypeVideoCapture)
			qbuf.setMemory(v4l2MemoryMmap)
			if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQbuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
				return status.Errorf(codes.Internal, "VIDIOC_QBUF: %v", errno)
			}
			continue
		}
		data := make([]byte, n)
		copy(data, mapped[idx].data[:n])

		if !broadcast(data, uint64(time.Now().UnixNano()), agentpb.VideoCodec_VIDEO_CODEC_H264) {
			return nil
		}
		framesSent++

		// Re-queue the buffer.
		var qbuf v4l2Buf
		qbuf.setIndex(idx)
		qbuf.setType(v4l2BufTypeVideoCapture)
		qbuf.setMemory(v4l2MemoryMmap)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQbuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return status.Errorf(codes.Internal, "VIDIOC_QBUF requeue[%d]: %v", idx, errno)
		}
	}
}

// gstFallbackDirs is the list of directories searched for GStreamer binaries
// when they are not on PATH. wendy-agent runs as a systemd service whose
// inherited PATH may omit the standard system bin directories (observed on
// wendyOS, where a CUDA setup file leaves PATH=/usr/local/cuda-XX/bin:$PATH
// — the literal "$PATH" not being expanded). Declared as a var so tests can
// override it.
var gstFallbackDirs = []string{"/usr/bin", "/usr/local/bin", "/usr/sbin"}

// resolveGSTBinary looks up a GStreamer binary on PATH first, then falls back
// to known system locations.
func resolveGSTBinary(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, dir := range gstFallbackDirs {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found; install GStreamer on the device", name)
}

// streamGStreamer spawns gst-launch-1.0 on the device to encode via the best available
// encoder and pipes the resulting stream back as videoFrame chunks via the broadcast callback.
func (s *VideoService) streamGStreamer(ctx context.Context, broadcast func([]byte, uint64, agentpb.VideoCodec) bool, path string, req *agentpb.StreamVideoRequest) (runErr error) {
	gstPath, err := resolveGSTBinary("gst-launch-1.0")
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	inspectPath, err := resolveGSTBinary("gst-inspect-1.0")
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}

	enc, err := findGStreamerEncoder(inspectPath)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	s.logger.Info("GStreamer encoder selected", zap.String("encoder", enc.element), zap.String("codec", enc.codec.String()))

	args := buildGStreamerArgs(gstPath, path, req, enc.element, enc.hasH264Parse)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create GStreamer pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "failed to start GStreamer: %v", err)
	}

	defer func() {
		cmd.Process.Kill()          //nolint:errcheck
		io.Copy(io.Discard, stdout) // drain so Wait's internal goroutine can exit
		waitErr := cmd.Wait()
		if runErr == nil {
			msg := strings.TrimSpace(stderrBuf.String())
			if msg != "" {
				runErr = status.Errorf(codes.Internal, "gstreamer exited with error: %s", msg)
			} else if waitErr != nil {
				runErr = status.Errorf(codes.Internal, "gstreamer exited with error: %v", waitErr)
			}
		}
	}()

	const chunkSize = 16 * 1024
	buf := make([]byte, chunkSize)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, readErr := stdout.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			if !broadcast(data, uint64(time.Now().UnixNano()), enc.codec) {
				return nil
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil // normal termination; defer surfaces stderr/exit errors
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return status.Errorf(codes.Internal, "failed to read GStreamer output: %v", readErr)
		}
	}
}

// gstEncoderResult describes a found GStreamer encoder and the codec it produces.
type gstEncoderResult struct {
	element      string
	codec        agentpb.VideoCodec
	hasH264Parse bool // whether h264parse is available on this device
}

// findGStreamerEncoder probes available encoders by listing all elements once via
// gst-inspect-1.0 (no args) and building a lookup set. Per-element subprocess calls
// are unreliable on some builds; the list command is authoritative.
func findGStreamerEncoder(inspectPath string) (gstEncoderResult, error) {
	available, err := listGSTElements(inspectPath)
	if err != nil {
		// If listing fails, attempt x264enc and let gst-launch fail with a clear message.
		return gstEncoderResult{element: "x264enc", codec: agentpb.VideoCodec_VIDEO_CODEC_H264}, nil
	}

	hasElem := func(name string) bool { return available[name] }
	h264Parse := hasElem("h264parse")

	h264Encoders := []string{
		"nvv4l2h264enc", // NVIDIA V4L2 hardware (Jetson L4T, gstreamer1.0-plugins-nvvideo4linux2)
		"v4l2h264enc",   // V4L2 M2M hardware (gst-plugins-good)
		"omxh264enc",    // OpenMAX hardware (Broadcom, Qualcomm)
		"avenc_h264",    // libavcodec bridge (gst-libav)
		"x264enc",       // software (gst-plugins-ugly)
		"openh264enc",   // software (gst-plugins-bad)
		"vaapih264enc",  // Intel VA-API
		"nvh264enc",     // NVIDIA NVENC (desktop)
		"msdkh264enc",   // Intel Media SDK
	}

	// H.264 is preferred when h264parse is available to normalize output to Annex B
	// byte-stream. Without h264parse, encoders like x264enc emit stream-format=avc
	// which discards SPS/PPS when piped raw over gRPC, making the stream undecodable.
	if h264Parse {
		for _, enc := range h264Encoders {
			if hasElem(enc) {
				return gstEncoderResult{element: enc, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, hasH264Parse: true}, nil
			}
		}
		for name := range available {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "h264") && strings.Contains(lower, "enc") {
				return gstEncoderResult{element: name, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, hasH264Parse: true}, nil
			}
		}
	}

	// VP8 preferred over raw H.264 when h264parse is absent: vp8enc+webmmux (both
	// in gst-plugins-good) produce a self-describing WebM container that requires no
	// stream-format negotiation and is always decodable by the client.
	if hasElem("vp8enc") && hasElem("webmmux") {
		return gstEncoderResult{element: "vp8enc", codec: agentpb.VideoCodec_VIDEO_CODEC_VP8}, nil
	}

	// Last resort: attempt H.264 without normalization. Hardware encoders such as
	// nvv4l2h264enc and v4l2h264enc typically emit byte-stream natively; x264enc may
	// produce AVC which the client's h264parse may or may not be able to decode.
	for _, enc := range h264Encoders {
		if hasElem(enc) {
			return gstEncoderResult{element: enc, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, hasH264Parse: false}, nil
		}
	}
	for name := range available {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "h264") && strings.Contains(lower, "enc") {
			return gstEncoderResult{element: name, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}, nil
		}
	}

	return gstEncoderResult{}, fmt.Errorf(
		"no supported GStreamer encoder found (checked %d elements); install gst-plugins-good (vp8enc+webmmux) or gst-plugins-bad (h264parse)+gst-plugins-ugly (x264enc)",
		len(available),
	)
}

// listGSTElements runs gst-inspect-1.0 once and returns a set of all available element names.
// Each output line has the form "plugin:  element: description".
func listGSTElements(inspectPath string) (map[string]bool, error) {
	out, err := exec.Command(inspectPath).Output()
	if err != nil {
		return nil, fmt.Errorf("gst-inspect-1.0: %w", err)
	}
	elements := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// Split on ": " to get plugin and element name (first two fields).
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[1])
		if name != "" && !strings.ContainsAny(name, " \t") {
			elements[name] = true
		}
	}
	return elements, nil
}

// buildGStreamerArgs constructs the gst-launch-1.0 argument list for V4L2 encode.
func buildGStreamerArgs(gstPath, devicePath string, req *agentpb.StreamVideoRequest, encoder string, hasH264Parse bool) []string {
	src := fmt.Sprintf("v4l2src device=%s", devicePath)

	var capsParts []string
	if req.GetWidth() > 0 {
		capsParts = append(capsParts, fmt.Sprintf("width=%d", req.GetWidth()))
	}
	if req.GetHeight() > 0 {
		capsParts = append(capsParts, fmt.Sprintf("height=%d", req.GetHeight()))
	}
	if req.GetFramerate() > 0 {
		capsParts = append(capsParts, fmt.Sprintf("framerate=%d/1", req.GetFramerate()))
	}

	var pipeline string
	if len(capsParts) > 0 {
		caps := "video/x-raw," + strings.Join(capsParts, ",")
		pipeline = fmt.Sprintf("%s ! %s ! %s ! fdsink fd=1", src, caps, encoderSegment(encoder, hasH264Parse))
	} else {
		pipeline = fmt.Sprintf("%s ! %s ! fdsink fd=1", src, encoderSegment(encoder, hasH264Parse))
	}
	// -q suppresses gst-launch's status messages (e.g. "Setting pipeline to PLAYING")
	// from being written to stdout and corrupting the binary H264 stream.
	return append([]string{gstPath, "-q"}, strings.Fields(pipeline)...)
}

// h264ByteStream normalizes any encoder's H.264 output to Annex B byte-stream
// with in-band, per-keyframe SPS/PPS (config-interval=-1).
//
// Without it, encoders such as x264enc default to stream-format=avc when piped
// to fdsink (its src caps list "avc" before "byte-stream", and fdsink imposes no
// constraint). AVC carries SPS/PPS out-of-band in the caps codec_data, which is
// discarded when the elementary stream is piped raw over gRPC. The client's
// `fdsrc ! typefind ! h264parse` pipeline then sees length-prefixed NALs with no
// start codes and fails with "Could not determine type of stream". Annex B with
// repeated SPS/PPS also lets the client sync mid-stream.
const h264ByteStream = " ! h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au"

// encoderSegment returns the GStreamer pipeline segment for the given encoder element.
// H.264 encoders force I420 (4:2:0) input to avoid 4:4:4 output paths that can make
// encoders such as x264enc select profile 244 (High 4:4:4 Predictive), which
// VideoToolbox and most hardware decoders reject. This input cap does not by itself
// enforce a specific H.264 output profile; explicit profile caps are added only where needed
// (for example, v4l2h264enc is capped to baseline below).
func encoderSegment(encoder string) string {
	switch encoder {
	case "v4l2h264enc":
		return "videoconvert ! video/x-raw,format=I420 ! v4l2h264enc ! video/x-h264,profile=baseline"
	case "x264enc":
		return "videoconvert ! video/x-raw,format=I420 ! x264enc tune=zerolatency"
	case "openh264enc":
		return "videoconvert ! video/x-raw,format=I420 ! openh264enc"
	case "avenc_h264":
		return "videoconvert ! video/x-raw,format=I420 ! avenc_h264"
	case "vp8enc":
		// webmmux streamable=true writes headers that matroskademux can parse from a pipe.
		return "videoconvert ! vp8enc deadline=1 ! webmmux streamable=true"
	}

	var enc string
	switch encoder {
	case "nvv4l2h264enc":
		// Jetson L4T hardware encoder; NV12 is its preferred input format.
		enc = "videoconvert ! video/x-raw,format=NV12 ! nvv4l2h264enc"
	case "v4l2h264enc":
		enc = "videoconvert ! video/x-raw,format=I420 ! v4l2h264enc ! video/x-h264,profile=baseline"
	case "x264enc":
		enc = "videoconvert ! video/x-raw,format=I420 ! x264enc tune=zerolatency ! video/x-h264,profile=high"
	case "openh264enc":
		enc = "videoconvert ! video/x-raw,format=I420 ! openh264enc"
	case "avenc_h264":
		enc = "videoconvert ! video/x-raw,format=I420 ! avenc_h264"
	default:
		// For other H.264-family encoders, force I420 to avoid 4:4:4 profile selection.
		enc = "videoconvert ! video/x-raw,format=I420 ! " + encoder
	}
	if hasH264Parse {
		return enc + h264ByteStream
	}
	return enc
}
