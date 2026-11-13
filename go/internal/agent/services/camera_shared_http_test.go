package services

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

func sharedTestJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sharedTestEndpoint(t *testing.T, url string) sharedCameraEndpoint {
	t.Helper()
	dir := t.TempDir()
	e := sharedCameraEndpoint{url: url, cameraFD: filepath.Join(dir, "camera"), socketFD: filepath.Join(dir, "socket"), cameraLink: "/dev/video4", socketLink: "socket:[99]"}
	for path, link := range map[string]string{e.cameraFD: e.cameraLink, e.socketFD: e.socketLink} {
		if err := os.Symlink(link, path); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func TestSharedCameraDiscoveryUsesOnlyOwnerListeners(t *testing.T) {
	proc := t.TempDir()
	for _, path := range []string{"self/ns", "90001/ns", "90001/fd", "90001/net", "90002/ns", "90002/fd", "90002/net"} {
		if err := os.MkdirAll(filepath.Join(proc, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	self := filepath.Join(proc, "self/ns/net")
	if err := os.WriteFile(self, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []string{"90001", "90002"} {
		if err := os.Link(self, filepath.Join(proc, pid, "ns/net")); err != nil {
			t.Fatal(err)
		}
		for fd, target := range map[string]string{"1": "/dev/video4", "2": "/dev/video0", "3": "socket:[99]"} {
			if err := os.Symlink(target, filepath.Join(proc, pid, "fd", fd)); err != nil {
				t.Fatal(err)
			}
		}
		// Both processes can see the same namespace-wide TCP table. Only
		// inode 99 is held by these processes; 100 must never be probed.
		table := "0: 00000000:1F41 00000000:0000 0A 0 0 0 0 0 99\n" +
			"1: 00000000:2328 00000000:0000 0A 0 0 0 0 0 100\n" +
			"2: 0100007F:1F42 00000000:0000 01 0 0 0 0 0 99\n"
		if err := os.WriteFile(filepath.Join(proc, pid, "net/tcp"), []byte(table), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An otherwise identical process in a private namespace must be ignored.
	if err := os.Remove(filepath.Join(proc, "90002/ns/net")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "90002/ns/net"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	color := func(path string) bool { return path == "/dev/video4" }
	got := sharedCameraEndpoints(context.Background(), proc, "/dev/video4", color)
	if len(got) != 1 || got[0].url != "http://127.0.0.1:8001/frame.jpg" || !got[0].stillOwned() {
		t.Fatalf("endpoints = %+v", got)
	}
	if got := sharedCameraEndpoints(context.Background(), proc, "/dev/video0", color); len(got) != 0 {
		t.Fatalf("depth node must not return color pictures: %+v", got)
	}
	if got := sharedCameraEndpoints(context.Background(), proc, "/dev/video4", func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("ambiguous camera owner must not select an image: %+v", got)
	}
	if err := os.Remove(got[0].socketFD); err != nil {
		t.Fatal(err)
	}
	if got[0].stillOwned() {
		t.Fatal("lost listener retained ownership")
	}
}

func TestSharedCameraJPEGValidation(t *testing.T) {
	valid := sharedTestJPEG(t)
	for _, tc := range []struct {
		name   string
		status int
		media  string
		stamp  string
		body   []byte
		want   string
	}{
		{"fresh", 200, "image/jpeg", "fresh", valid, ""},
		{"stale", 200, "image/jpeg", "1", valid, "stale"},
		{"no timestamp", 200, "image/jpeg", "", valid, "timestamp"},
		{"locked", 423, "application/json", "fresh", nil, "423"},
		{"redirect", 302, "image/jpeg", "fresh", valid, "302"},
		{"not image", 200, "text/html", "fresh", valid, "image/jpeg"},
		{"truncated", 200, "image/jpeg", "fresh", valid[:len(valid)-20], "invalid"},
		{"too large", 200, "image/jpeg", "fresh", bytes.Repeat([]byte{0}, sharedJPEGMaxBytes+1), "8 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var redirected atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/frame.jpg" {
					redirected.Store(true)
				}
				w.Header().Set("Content-Type", tc.media)
				stamp := tc.stamp
				if stamp == "fresh" {
					stamp = strconv.FormatInt(time.Now().UnixNano(), 10)
				}
				w.Header().Set("X-Captured-At-Unix-Ns", stamp)
				w.Header().Set("Location", "/elsewhere")
				w.WriteHeader(tc.status)
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()
			client := sharedCameraHTTPClient()
			defer client.CloseIdleConnections()
			frame, err := readSharedCameraJPEG(context.Background(), client, sharedTestEndpoint(t, srv.URL+"/frame.jpg"))
			if tc.want == "" {
				if err != nil || frame.width != 64 || frame.height != 48 || !bytes.Equal(frame.data, valid) {
					t.Fatalf("frame %dx%d: %v", frame.width, frame.height, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if redirected.Load() {
				t.Fatal("followed redirect outside the camera feed")
			}
		})
	}
}

func TestSharedCameraPumpSkipsDuplicateFramesAndCancels(t *testing.T) {
	data := sharedTestJPEG(t)
	stamp := time.Now().UnixNano()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		n := calls.Add(1)
		captured := stamp
		if n >= 2 {
			captured++
		}
		w.Header().Set("X-Captured-At-Unix-Ns", strconv.FormatInt(captured, 10))
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := sharedCameraHTTPClient()
	defer client.CloseIdleConnections()
	r, w := io.Pipe()
	defer r.Close()
	done := make(chan error, 1)
	go func() {
		err := pumpSharedJPEG(ctx, w, client, sharedTestEndpoint(t, srv.URL), sharedCameraJPEG{data: data, width: 64, height: 48, captured: stamp})
		_ = w.CloseWithError(err)
		done <- err
	}()
	buf := make([]byte, len(data)*2)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("duplicate frame was sent, calls = %d", calls.Load())
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pump error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pump did not stop after cancellation")
	}
}

func TestSharedCameraPipelineClosesInput(t *testing.T) {
	// The subprocess emits a frame without draining stdin. Ending the viewer
	// must still unblock the HTTP feeder and exec's internal stdin copier.
	dir := t.TempDir()
	program := filepath.Join(dir, "gst-test")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf encoded\nexec /bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	svc := &VideoService{logger: zap.NewNop()}
	err := svc.runCameraPipeline(ctx, func(b []byte, _ frameTimestamp, _ agentpb.VideoCodec) bool { return false }, "/dev/video4", gstEncoderResult{element: "x264enc"}, gstPipelinePlan{args: []string{program}}, nil, nil, r)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("pipeline teardown waited for its deadline")
	}
	if _, err := w.Write([]byte("next frame")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("input was not closed: %v", err)
	}
}

func TestSharedJPEGPipelineDoesNotOpenCamera(t *testing.T) {
	if _, err := planSharedJPEGPipeline("gst", "", &agentpb.StreamVideoRequest{Framerate: 30}, gstEncoderResult{element: "x264enc"}, sharedCameraJPEG{width: 640, height: 480}); err == nil {
		t.Fatal("must not promise a requested rate the camera owner does not guarantee")
	}
	plan, err := planSharedJPEGPipeline("gst", "", &agentpb.StreamVideoRequest{Width: 320, Height: 240}, gstEncoderResult{element: "x264enc", hasH264Parse: true}, sharedCameraJPEG{width: 640, height: 480})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(plan.args, " ")
	for _, want := range []string{"fdsrc", "jpegparse", "jpegdec", "videoscale ! video/x-raw,width=320,height=240", "h264parse"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %s in %s", want, args)
		}
	}
	if strings.Contains(args, "v4l2src") || strings.Contains(args, "pipewiresrc") || plan.raw != nil {
		t.Fatal("shared JPEG path opened physical capture or promised raw frames")
	}
}

func TestSharedJPEGPipelineGStreamer(t *testing.T) {
	gst, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		t.Skip("GStreamer is not installed")
	}
	data := sharedTestJPEG(t)
	plan, err := planSharedJPEGPipeline(gst, "", &agentpb.StreamVideoRequest{}, gstEncoderResult{element: "x264enc", hasH264Parse: true}, sharedCameraJPEG{width: 64, height: 48})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, plan.args[0], plan.args[1:]...)
	cmd.Stdin = bytes.NewReader(bytes.Repeat(data, 4))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	encoded, err := cmd.Output()
	if err != nil {
		t.Fatalf("JPEG pipeline: %v: %s", err, stderr.String())
	}
	if len(encoded) == 0 {
		t.Fatal("JPEG pipeline produced no video")
	}
	// Decode the resulting H.264 back to a JPEG, just as a snapshot client
	// does. A successful process exit alone does not prove a usable image.
	decode := exec.CommandContext(ctx, gst, "-q", "fdsrc", "fd=0", "!", "h264parse", "!", "avdec_h264", "!", "videoconvert", "!", "jpegenc", "snapshot=true", "!", "fdsink", "fd=1")
	decode.Stdin = bytes.NewReader(encoded)
	stderr.Reset()
	decode.Stderr = &stderr
	decoded, err := decode.Output()
	if err != nil {
		t.Fatalf("snapshot decode: %v: %s", err, stderr.String())
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || cfg.Width != 64 || cfg.Height != 48 {
		t.Fatalf("snapshot = %dx%d, %v", cfg.Width, cfg.Height, err)
	}
}
