package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const thermalStable = "by-id:usb-Camera_USB_Camera_EA6935255-video-index0"

// thermalCalibrationClient is a device with a webcam at 0, a network camera
// at 200 and a TC001 at 4.
type thermalCalibrationClient struct {
	agentpb.WendyVideoServiceClient
	set     *agentpb.SetThermalCalibrationRequest
	cleared *agentpb.ClearThermalCalibrationRequest
	asked   []uint32
	stored  *agentpb.ThermalCalibration
	err     error
}

func (c *thermalCalibrationClient) ListVideoDevices(context.Context, *agentpb.ListVideoDevicesRequest, ...grpc.CallOption) (*agentpb.ListVideoDevicesResponse, error) {
	return &agentpb.ListVideoDevicesResponse{Devices: []*agentpb.VideoDevice{
		{Id: 0, Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB, Path: "/dev/video0"},
		{Id: 4, Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB, Path: "/dev/video4", StableId: thermalStable},
		{Id: 200, Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_IP},
	}}, nil
}

func (c *thermalCalibrationClient) GetThermalCalibration(_ context.Context, req *agentpb.GetThermalCalibrationRequest, _ ...grpc.CallOption) (*agentpb.ThermalCalibration, error) {
	c.asked = append(c.asked, req.GetDeviceId())
	if c.err != nil {
		return nil, c.err
	}
	if req.GetDeviceId() != 4 && req.GetStableId() != thermalStable {
		return nil, status.Error(codes.FailedPrecondition, "camera 0 is not a radiometric thermal camera")
	}
	return c.stored, nil
}

func (c *thermalCalibrationClient) SetThermalCalibration(_ context.Context, req *agentpb.SetThermalCalibrationRequest, _ ...grpc.CallOption) (*agentpb.ThermalCalibration, error) {
	c.set = req
	if c.err != nil {
		return nil, c.err
	}
	return &agentpb.ThermalCalibration{StableId: thermalStable, Path: "/dev/video4", DeviceId: 4, Calibrated: true, Gain: req.GetGain(), Offset: req.GetOffset(), UpdatedUnixNanos: 1_793_000_000_000_000_000}, nil
}

func (c *thermalCalibrationClient) ClearThermalCalibration(_ context.Context, req *agentpb.ClearThermalCalibrationRequest, _ ...grpc.CallOption) (*agentpb.ThermalCalibration, error) {
	c.cleared = req
	if c.err != nil {
		return nil, c.err
	}
	return &agentpb.ThermalCalibration{StableId: thermalStable, Path: "/dev/video4", DeviceId: 4, Gain: 1}, nil
}

func runThermalCalibrationCmd(t *testing.T, client *thermalCalibrationClient, asJSON bool, args ...string) (string, error) {
	t.Helper()
	previousResolve, previousJSON := resolveCameraTargetFn, jsonOutput
	t.Cleanup(func() { resolveCameraTargetFn, jsonOutput = previousResolve, previousJSON })
	jsonOutput = asJSON
	resolveCameraTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		return &SelectedDevice{Agent: &grpcclient.AgentConnection{VideoService: client}}, nil
	}
	var out bytes.Buffer
	cmd := newCameraThermalCalibrationCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestThermalCalibrationSetSendsThePairToTheNamedCamera(t *testing.T) {
	client := &thermalCalibrationClient{}
	out, err := runThermalCalibrationCmd(t, client, false, "set", "--id", "4", "--gain", "0.48", "--offset", "-0.5")
	if err != nil {
		t.Fatal(err)
	}
	if client.set.GetDeviceId() != 4 || client.set.GetStableId() != "" || client.set.GetGain() != 0.48 || client.set.GetOffset() != -0.5 {
		t.Fatalf("request %v", client.set)
	}
	if !strings.Contains(out, "camera 4 ("+thermalStable+"): °C = 0.48 × camera °C - 0.5") || !strings.Contains(out, "from the next frame") {
		t.Fatalf("output %q", out)
	}

	// By stable id, and the offset defaults to 0.
	if _, err := runThermalCalibrationCmd(t, client, false, "set", "--stable-id", thermalStable, "--gain", "0.5"); err != nil {
		t.Fatal(err)
	}
	if client.set.GetStableId() != thermalStable || client.set.GetGain() != 0.5 || client.set.GetOffset() != 0 {
		t.Fatalf("request %v", client.set)
	}
}

func TestThermalCalibrationClearAndShow(t *testing.T) {
	client := &thermalCalibrationClient{stored: &agentpb.ThermalCalibration{StableId: thermalStable, Path: "/dev/video4", DeviceId: 4, Calibrated: true, Gain: 0.48, UpdatedUnixNanos: 1_793_000_000_000_000_000}}
	out, err := runThermalCalibrationCmd(t, client, false, "clear", "--stable-id", thermalStable)
	if err != nil {
		t.Fatal(err)
	}
	if client.cleared.GetStableId() != thermalStable || !strings.Contains(out, "no correction") {
		t.Fatalf("request %v, output %q", client.cleared, out)
	}

	out, err = runThermalCalibrationCmd(t, client, false, "show", "--id", "4")
	if err != nil || !strings.Contains(out, "°C = 0.48 × camera °C + 0, set 2026-10-26") {
		t.Fatalf("show: %q %v", out, err)
	}

	out, err = runThermalCalibrationCmd(t, client, true, "show", "--id", "4")
	if err != nil {
		t.Fatal(err)
	}
	var view thermalCalibrationJSON
	if err := json.Unmarshal([]byte(out), &view); err != nil || !view.Calibrated || view.Gain != 0.48 || view.StableID != thermalStable || view.Updated == "" {
		t.Fatalf("json %q: %+v %v", out, view, err)
	}
}

// Bare, and for show without a camera, it lists the thermal cameras: it asks
// about every local camera, skips the ones the agent refuses, and never asks
// about a network camera.
func TestThermalCalibrationListsTheThermalCameras(t *testing.T) {
	for _, args := range [][]string{nil, {"show"}} {
		client := &thermalCalibrationClient{stored: &agentpb.ThermalCalibration{StableId: thermalStable, Path: "/dev/video4", DeviceId: 4, Gain: 1}}
		out, err := runThermalCalibrationCmd(t, client, false, args...)
		if err != nil {
			t.Fatal(err)
		}
		if len(client.asked) != 2 || client.asked[0] != 0 || client.asked[1] != 4 {
			t.Fatalf("asked about cameras %v, want 0 and 4", client.asked)
		}
		if !strings.Contains(out, thermalStable) || !strings.Contains(out, "none (the camera's own reading)") || strings.Contains(out, "/dev/video0") {
			t.Fatalf("output %q", out)
		}
	}

	client := &thermalCalibrationClient{stored: &agentpb.ThermalCalibration{StableId: thermalStable, DeviceId: 4, Gain: 1}}
	out, err := runThermalCalibrationCmd(t, client, true, "show")
	if err != nil {
		t.Fatal(err)
	}
	var views []thermalCalibrationJSON
	if err := json.Unmarshal([]byte(out), &views); err != nil || len(views) != 1 || views[0].Calibrated || views[0].Gain != 1 {
		t.Fatalf("json %q: %+v %v", out, views, err)
	}
}

func TestThermalCalibrationRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		err  error
		want string
	}{
		{"set without a camera lists them", []string{"set", "--gain", "0.48"}, nil, "name the camera to set with --id or --stable-id"},
		{"clear without a camera", []string{"clear"}, nil, "name the camera to clear"},
		{"set without a gain", []string{"set", "--id", "4"}, nil, `required flag(s) "gain" not set`},
		{"two names", []string{"set", "--id", "4", "--stable-id", thermalStable, "--gain", "0.48"}, nil, "pass one"},
		{"a stray argument", []string{"show", "4"}, nil, "unknown command"},
		{"the agent's reason", []string{"set", "--id", "4", "--gain", "0"}, status.Error(codes.InvalidArgument, "gain 0 must be greater than 0"), "thermal calibration set: gain 0 must be greater than 0"},
		{"not a thermal camera", []string{"show", "--id", "0"}, nil, "not a radiometric thermal camera"},
		{"an old agent", []string{"set", "--id", "4", "--gain", "0.48"}, status.Error(codes.Unimplemented, "unknown method"), "update the agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &thermalCalibrationClient{err: tc.err, stored: &agentpb.ThermalCalibration{StableId: thermalStable, DeviceId: 4, Gain: 1}}
			out, err := runThermalCalibrationCmd(t, client, false, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q (output %q)", err, tc.want, out)
			}
			if strings.Contains(out, "from the next frame") {
				t.Fatalf("a refused command printed success: %q", out)
			}
		})
	}
}
