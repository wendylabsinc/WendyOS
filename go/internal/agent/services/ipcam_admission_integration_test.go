//go:build regression_camera_fixture

package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type admissionVideoStream struct {
	grpc.ServerStreamingServer[agentpb.VideoFrame]
	ctx    context.Context
	frames chan *agentpb.VideoFrame
}

func (s *admissionVideoStream) Context() context.Context { return s.ctx }
func (s *admissionVideoStream) Send(frame *agentpb.VideoFrame) error {
	select {
	case s.frames <- frame:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func TestCameraPairingControlsRealONVIFAndRTSPConnections(t *testing.T) {
	launch, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("GStreamer is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	server := newCameraAdmissionServer(t, ctx, launch)
	stateDir := t.TempDir()
	const mac = "ec:71:db:2a:ae:7e"
	probeReply := []byte(fmt.Sprintf(`<Envelope><Body><ProbeMatches><ProbeMatch><Types>dn:NetworkVideoTransmitter</Types><Scopes>onvif://www.onvif.org/name/fixture</Scopes><XAddrs>%s/onvif/device_service</XAddrs></ProbeMatch></ProbeMatches></Body></Envelope>`, server.http.URL))
	newService := func() *VideoService {
		s := newIPTestService(t)
		s.registry = ipcam.NewRegistry(filepath.Join(stateDir, "cameras.json"))
		s.credentials = ipcam.NewCredentialStore(filepath.Join(stateDir, "credentials.json"))
		s.loadNetworkCameras()
		// Substitute physical multicast/ARP and port assignments only. The real
		// discovery round still decides whether to contact the counted server.
		s.discoverer = ipcam.NewRegressionDiscoverer(s.registry, zap.NewNop(), func(context.Context) ([][]byte, error) {
			return [][]byte{probeReply}, nil
		}, "127.0.0.1 0x1 0x2 "+mac+" * fixture\n", func(string) bool {
			conn, err := net.DialTimeout("tcp", server.http.Listener.Addr().String(), time.Second)
			if err == nil {
				_ = conn.Close()
			}
			return err == nil
		})
		s.discoverer.Credentials = s.credentials.Get
		s.learnStreamPaths = s.discoverer.LearnStreamPaths
		// This fixture exercises direct network capture. Kernel loopback nodes
		// are outside its scope and must never be created on the test host.
		s.loopback = nil
		s.cameraReachable = func(string) bool {
			conn, err := net.DialTimeout("tcp", server.rtsp.Addr().String(), time.Second)
			if err == nil {
				_ = conn.Close()
			}
			return err == nil
		}
		s.runGStreamer = func(ctx context.Context, args []string, emit func([]byte)) error {
			args = append([]string{}, args...)
			for i, arg := range args {
				if strings.HasPrefix(arg, "location=") {
					u, err := url.Parse(strings.TrimPrefix(arg, "location="))
					if err != nil {
						return err
					}
					// Preserve the production credential and learned stream path;
					// use an ephemeral local port instead of privileged port 554.
					u.Host = server.rtsp.Addr().String()
					args[i] = "location=" + u.String()
				}
			}
			cmd := exec.CommandContext(ctx, launch, append([]string{"-q"}, args...)...)
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				return err
			}
			if err := cmd.Start(); err != nil {
				return err
			}
			buf := make([]byte, 65536)
			for {
				n, err := stdout.Read(buf)
				if n > 0 {
					emit(bytes.Clone(buf[:n]))
				}
				if err != nil {
					if !errors.Is(err, io.EOF) {
						_ = cmd.Process.Kill()
					}
					break
				}
			}
			err = cmd.Wait()
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return fmt.Errorf("RTSP capture failed: %v: %s", err, ipcam.RedactText(diagnostics.String(), "camera-password"))
			}
			return nil
		}
		return s
	}
	refresh := func(s *VideoService) uint32 {
		t.Helper()
		response, err := s.RefreshCameras(ctx, &agentpb.RefreshCamerasRequest{})
		if err != nil || len(response.GetDevices()) != 1 {
			t.Fatalf("discover camera: response=%v err=%v", response, err)
		}
		return response.GetDevices()[0].GetId()
	}
	counts := func() [3]int32 {
		// A liveness TCP connect can complete before the HTTP accept callback.
		// All production discovery work is synchronous; allow it to be counted.
		time.Sleep(100 * time.Millisecond)
		return [3]int32{server.httpConns.Load(), server.httpRequests.Load(), server.rtspConns.Load()}
	}
	assertUnpaired := func(s *VideoService, id uint32, before [3]int32) {
		t.Helper()
		response, err := s.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
		if err != nil || len(response.GetDevices()) != 0 {
			t.Fatalf("unpaired camera was listed: %v, %v", response, err)
		}
		stream := &admissionVideoStream{ctx: ctx, frames: make(chan *agentpb.VideoFrame, 16)}
		if err := s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: id}, stream); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("unpaired capture error = %v", err)
		}
		if got := counts(); got != before {
			t.Fatalf("unpaired camera opened network connections: before=%v after=%v (HTTP connections, ONVIF requests, RTSP connections)", before, got)
		}
	}
	capture := func(s *VideoService, id uint32) {
		t.Helper()
		beforeConns, beforePlays := server.rtspConns.Load(), server.rtspPlays.Load()
		streamCtx, stop := context.WithCancel(ctx)
		stream := &admissionVideoStream{ctx: streamCtx, frames: make(chan *agentpb.VideoFrame, 16)}
		done := make(chan error, 1)
		go func() { done <- s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: id}, stream) }()
		defer func() {
			stop()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("paired camera stream did not stop")
			}
		}()
		var encoded []byte
		for len(annexBKeyframeStarts(encoded)) < 4 {
			select {
			case frame := <-stream.frames:
				if frame.GetCodec() != agentpb.VideoCodec_VIDEO_CODEC_H264 {
					t.Fatalf("paired stream codec = %v", frame.GetCodec())
				}
				encoded = append(encoded, frame.GetData()...)
			case err := <-done:
				done <- err
				t.Fatalf("paired camera failed before decoded capture: %v", err)
			case <-ctx.Done():
				t.Fatal("paired camera did not deliver decodable capture before deadline")
			}
		}
		assertSensorLinkDecodedColor(t, ctx, launch, encoded, 0)
		if server.rtspPlays.Load() != beforePlays+1 || server.rtspConns.Load() != beforeConns+2 || server.authenticated.Load() != beforePlays+1 {
			t.Fatalf("capture did not use one authenticated RTSP session and one preflight connection: connections=%d plays=%d authenticated=%d", server.rtspConns.Load()-beforeConns, server.rtspPlays.Load()-beforePlays, server.authenticated.Load())
		}
	}

	s := newService()
	id := refresh(s)
	refresh(s)
	assertUnpaired(s, id, [3]int32{})
	if _, err := s.SetCameraCredentials(ctx, &agentpb.SetCameraCredentialsRequest{DeviceId: id, Username: "camera-user", Password: "camera-password"}); err != nil {
		t.Fatal(err)
	}
	cam, _ := s.registry.Get(id)
	if cam.StreamMain != "/measurement" || cam.StreamSub != "/measurement" || server.httpRequests.Load() == 0 {
		t.Fatalf("pairing did not learn real ONVIF stream paths: %+v", cam)
	}
	capture(s, id)
	s.Shutdown()
	// Recreate the service from its actual saved files, without pairing again.
	s = newService()
	beforeRequests := server.httpRequests.Load()
	if got := refresh(s); got != id || !s.credentials.Has(mac) {
		t.Fatalf("restart lost the paired camera: id=%d credential=%v", got, s.credentials.Has(mac))
	}
	if server.httpRequests.Load() != beforeRequests {
		t.Fatal("restart discarded persisted stream paths and probed ONVIF again")
	}
	capture(s, id)
	if _, err := s.ForgetCamera(ctx, &agentpb.ForgetCameraRequest{DeviceId: id}); err != nil {
		t.Fatal(err)
	}
	if s.credentials.Has(mac) {
		t.Fatal("forget left the camera credential in memory")
	}
	before := counts()
	id = refresh(s)
	refresh(s)
	assertUnpaired(s, id, before)
	s.Shutdown()
	s = newService()
	if s.credentials.Has(mac) || len(s.registry.List()) != 0 {
		t.Fatal("restart restored a forgotten camera or its credentials")
	}
	id = refresh(s)
	assertUnpaired(s, id, before)
}
