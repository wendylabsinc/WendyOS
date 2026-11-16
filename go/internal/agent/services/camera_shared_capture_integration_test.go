package services

import (
	"context"
	"errors"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This fixture starts at the app-owner feed boundary. The owner fd links are
// synthetic, but HTTP requests, JPEG validation, GStreamer encoding, subscriber
// fan-out and downstream pixel decoding are real. Linux CI traces the test and
// codec children to reject opening a camera. It does not emulate V4L2 EBUSY or
// establish hardware-driver contention behavior.
func TestSharedCameraSubscribersDecodeFreshOwnerPixelsWithoutCameraOpen(t *testing.T) {
	launch, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("GStreamer is not installed")
	}
	const cameraPath = "/dev/video44"
	if _, err := os.Lstat(cameraPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture requires absent %s, got %v", cameraPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service := NewVideoService(ctx, zap.NewNop(), nil)
	defer service.Shutdown()
	red := sensorLinkColorFrame(t, color.RGBA{R: 240, A: 255}).Data
	green := sensorLinkColorFrame(t, color.RGBA{G: 240, A: 255}).Data
	var phase, calls, starts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/frame.jpg" || r.Method != http.MethodGet {
			http.Error(w, "unexpected feed request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Captured-At-Unix-Ns", strconv.FormatInt(time.Now().UnixNano(), 10))
		if phase.Load() == 0 {
			_, _ = w.Write(red)
		} else {
			_, _ = w.Write(green)
		}
	}))
	defer server.Close()
	endpoint := sharedTestEndpoint(t, server.URL+"/frame.jpg")
	// Point the synthetic ownership evidence at the same absent device whose
	// opens are prohibited by the hosted syscall trace.
	if err := os.Remove(endpoint.cameraFD); err != nil {
		t.Fatal(err)
	}
	endpoint.cameraLink = cameraPath
	if err := os.Symlink(cameraPath, endpoint.cameraFD); err != nil {
		t.Fatal(err)
	}
	service.startProducer = func(ctx context.Context, h *deviceHub, path string, req *agentpb.StreamVideoRequest) {
		starts.Add(1)
		service.wg.Add(1)
		go func() {
			defer service.wg.Done()
			client := sharedCameraHTTPClient()
			defer client.CloseIdleConnections()
			first, err := readSharedCameraJPEG(ctx, client, endpoint)
			if err == nil {
				err = service.streamSharedJPEG(ctx, func(payload []byte, stamp frameTimestamp, codec agentpb.VideoCodec) bool {
					return h.produce(&videoFrame{data: payload, tsNs: stamp.wallNs, codec: codec,
						nativeNs: stamp.nativeNs, nativeClock: stamp.nativeClock, nativeFlags: stamp.nativeFlags,
						sequence: stamp.sequence, sequenceValid: stamp.sequenceValid, auAligned: stamp.auAligned})
				}, path, req, h, client, endpoint, first)
			}
			// The existing producer-start seam requires its caller to perform
			// runProducer's lifecycle bookkeeping. No frame or codec is mocked.
			service.mu.Lock()
			if service.hubs[path] == h {
				delete(service.hubs, path)
			}
			service.mu.Unlock()
			h.mu.Lock()
			if err != nil && ctx.Err() == nil {
				h.err = err
			}
			for _, sub := range h.subs {
				if !sub.closed {
					sub.closed = true
					close(sub.ch)
				}
			}
			h.mu.Unlock()
			close(h.done)
		}()
	}
	first, firstID, firstFrames, err := service.getOrCreateHub(ctx, cameraPath, &agentpb.StreamVideoRequest{DeviceId: 44}, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer first.unsubscribe(firstID)
	second, secondID, secondFrames, err := service.getOrCreateHub(ctx, cameraPath, &agentpb.StreamVideoRequest{DeviceId: 44}, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer second.unsubscribe(secondID)
	if first != second || starts.Load() != 1 {
		t.Fatal("app-owned feed did not share exactly one producer")
	}
	_, _, _, err = service.getOrCreateHub(ctx, cameraPath, &agentpb.StreamVideoRequest{DeviceId: 44, Width: 1280, Height: 720}, hubHolderStreamClient)
	if status.Code(err) != codes.InvalidArgument || starts.Load() != 1 {
		t.Fatalf("incompatible shared-camera request disrupted the producer: %v", err)
	}
	appendFrame := func(stream []byte, frame *videoFrame, ok bool) []byte {
		if !ok {
			t.Fatalf("app-owned subscribers did not receive decodable frames: %v", first.terminalErr())
		}
		if frame.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || len(frame.data) == 0 {
			t.Fatalf("app-owned subscriber codec=%v bytes=%d", frame.codec, len(frame.data))
		}
		return append(stream, frame.data...)
	}
	var initial [2][]byte
	for len(annexBKeyframeStarts(initial[0])) < 4 || len(annexBKeyframeStarts(initial[1])) < 4 {
		select {
		case frame, ok := <-firstFrames:
			initial[0] = appendFrame(initial[0], frame, ok)
		case frame, ok := <-secondFrames:
			initial[1] = appendFrame(initial[1], frame, ok)
		case <-ctx.Done():
			t.Fatal("app-owned subscribers did not receive decodable frames before deadline")
		}
	}
	for _, stream := range initial {
		assertSensorLinkDecodedColor(t, ctx, launch, stream, 0)
	}
	first.unsubscribe(firstID)
	phase.Store(1)
	if second.ctx.Err() != nil {
		t.Fatal("canceling the first viewer stopped the remaining viewer")
	}
	var after []byte
	for len(annexBKeyframeStarts(after)) < 5 {
		select {
		case frame, ok := <-secondFrames:
			after = appendFrame(after, frame, ok)
		case <-ctx.Done():
			t.Fatal("remaining app-owned subscriber stopped receiving fresh pixels")
		}
	}
	assertSensorLinkDecodedColor(t, ctx, launch, after, 1)
	second.unsubscribe(secondID)
	select {
	case <-second.done:
	case <-time.After(3 * time.Second):
		t.Fatal("last viewer did not stop the shared HTTP pump and encoder")
	}
	count := calls.Load()
	time.Sleep(3 * sharedJPEGPeriod)
	if calls.Load() != count || starts.Load() != 1 {
		t.Fatalf("feed requests continued after teardown or another producer started: calls %d -> %d, starts %d", count, calls.Load(), starts.Load())
	}
	// Stopping subscribers must not release or alter the owner's descriptors.
	if !endpoint.stillOwned() {
		t.Fatal("subscriber teardown altered app-owned camera/listener ownership")
	}
}
