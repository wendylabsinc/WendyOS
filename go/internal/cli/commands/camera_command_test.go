package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type cameraViewCommandClient struct {
	agentpb.WendyVideoServiceClient
	devices []*agentpb.VideoDevice
	listed  bool
	request *agentpb.StreamVideoRequest
	err     error
}

func (c *cameraViewCommandClient) ListVideoDevices(context.Context, *agentpb.ListVideoDevicesRequest, ...grpc.CallOption) (*agentpb.ListVideoDevicesResponse, error) {
	c.listed = true
	return &agentpb.ListVideoDevicesResponse{Devices: c.devices}, nil
}

func (c *cameraViewCommandClient) StreamVideo(_ context.Context, req *agentpb.StreamVideoRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.VideoFrame], error) {
	c.request = req
	return nil, c.err
}

func TestCameraViewNonInteractiveSelection(t *testing.T) {
	streamErr := errors.New("test stream unavailable")
	for _, tc := range []struct {
		name     string
		args     []string
		devices  []*agentpb.VideoDevice
		wantID   uint32
		stableID string
		listed   bool
		wantErr  string
	}{
		{name: "one camera", devices: []*agentpb.VideoDevice{usbCam(42, "front")}, wantID: 42, listed: true},
		{name: "explicit zero", args: []string{"--id", "0"}},
		{name: "stable identity", args: []string{"--stable-id", "usb-front"}, stableID: "usb-front"},
		{name: "ambiguous cameras", devices: []*agentpb.VideoDevice{usbCam(0, "front"), usbCam(1, "rear")}, listed: true, wantErr: "multiple cameras found; pass --id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Even when launched from a TTY, the flag must prohibit the picker.
			stubInteractive(t)
			client := &cameraViewCommandClient{devices: tc.devices, err: streamErr}
			previous := connectCameraStreamFn
			connectCameraStreamFn = func(_ context.Context, opts ...resolveOption) (*grpcclient.AgentConnection, error) {
				var cfg resolveConfig
				for _, opt := range opts {
					opt(&cfg)
				}
				if !cfg.nonInteractive || !cfg.suppressUpdateCheck || !cfg.disableSessionBroker {
					t.Fatalf("background connection options = %+v", cfg)
				}
				return &grpcclient.AgentConnection{VideoService: client}, nil
			}
			t.Cleanup(func() { connectCameraStreamFn = previous })

			cmd := newCameraViewCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"--non-interactive"}, tc.args...))
			err := cmd.ExecuteContext(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if client.request != nil {
					t.Fatal("ambiguous camera selection started a stream")
				}
			} else {
				if !errors.Is(err, streamErr) {
					t.Fatalf("error = %v, want stream error", err)
				}
				if client.request == nil || client.request.GetDeviceId() != tc.wantID || client.request.GetStableId() != tc.stableID {
					t.Fatalf("stream request = %v", client.request)
				}
			}
			if client.listed != tc.listed {
				t.Fatalf("listed = %v, want %v", client.listed, tc.listed)
			}
		})
	}
}

func TestCameraViewClosedTerminalDoesNotOpenPicker(t *testing.T) {
	stubNonInteractive(t)
	previous := connectCameraStreamFn
	connectCameraStreamFn = func(context.Context, ...resolveOption) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{VideoService: &cameraViewCommandClient{
			devices: []*agentpb.VideoDevice{usbCam(0, "front"), usbCam(1, "rear")},
		}}, nil
	}
	t.Cleanup(func() { connectCameraStreamFn = previous })
	cmd := newCameraViewCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "multiple cameras found; pass --id") {
		t.Fatalf("error = %v, want explicit camera selection", err)
	}
}

func TestCameraPlaybackNonInteractiveNeverInstalls(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stubGSTFallback(t, nil)
	stubInteractive(t)
	stubConfirmFn(t, func(string) bool {
		t.Fatal("background playback must not prompt for installation")
		return false
	})
	stubInstallGStreamer(t, func(context.Context) error {
		t.Fatal("background playback must not install GStreamer")
		return nil
	})
	stream := &mockVideoStream{frames: []*agentpb.VideoFrame{{Codec: agentpb.VideoCodec_VIDEO_CODEC_H264}}}
	err := playVideoWithGStreamer(context.Background(), stream, false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want missing GStreamer", err)
	}
}
