package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func unpairedListingFixture() []*agentpb.VideoDevice {
	return []*agentpb.VideoDevice{
		{Id: 0, Name: "Logitech Brio 100", Path: "/dev/video0", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB},
		{Id: 200, Name: "RLC-520A", Address: "10.98.0.50", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_IP, HasCredentials: true, Online: true},
		{Id: 201, Name: "Tapo C200", Address: "192.168.0.42", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_IP, Online: true},
	}
}

// A network camera without a stored login may be a neighbour's camera on a
// shared network, so `camera list` leaves it out and says how many it hid and
// how to pair or list them.
func TestRenderCameraListHidesUnpairedNetworkCameras(t *testing.T) {
	var out bytes.Buffer
	if err := renderCameraList(&out, unpairedListingFixture(), false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Logitech Brio 100", "RLC-520A", "1 network camera without a login is hidden",
		"wendy device pair", "wendy device camera list --all"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Tapo C200") {
		t.Errorf("unpaired camera listed:\n%s", got)
	}
}

// With only unpaired network cameras around, the listing still says why it
// looks empty rather than claiming there are no cameras at all.
func TestRenderCameraListExplainsAnEmptyListing(t *testing.T) {
	var out bytes.Buffer
	devices := unpairedListingFixture()[2:]
	devices = append(devices, &agentpb.VideoDevice{Id: 202, Name: "C320WS", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_IP})
	if err := renderCameraList(&out, devices, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "No cameras found.") || !strings.Contains(got, "2 network cameras without a login are hidden") {
		t.Fatalf("listing = %q", got)
	}
}

func TestRenderCameraListAllShowsUnpairedNetworkCameras(t *testing.T) {
	var out bytes.Buffer
	if err := renderCameraList(&out, unpairedListingFixture(), true); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Tapo C200") || !strings.Contains(got, "needs login") {
		t.Fatalf("--all listing missing the unpaired camera:\n%s", got)
	}
	if strings.Contains(got, "hidden") {
		t.Fatalf("--all listing still reports hidden cameras:\n%s", got)
	}
}

// JSON output stays a bare array, with the same cameras the table shows.
func TestRenderCameraListJSONHidesUnpairedNetworkCameras(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true

	for _, tc := range []struct {
		all  bool
		want []uint32
	}{{false, []uint32{0, 200}}, {true, []uint32{0, 200, 201}}} {
		var out bytes.Buffer
		if err := renderCameraList(&out, unpairedListingFixture(), tc.all); err != nil {
			t.Fatal(err)
		}
		var listed []struct {
			ID uint32 `json:"id"`
		}
		if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
			t.Fatalf("all=%v: output is not a JSON array: %v\n%s", tc.all, err, out.String())
		}
		var ids []uint32
		for _, d := range listed {
			ids = append(ids, d.ID)
		}
		if len(ids) != len(tc.want) {
			t.Fatalf("all=%v: listed %v, want %v", tc.all, ids, tc.want)
		}
		for i := range ids {
			if ids[i] != tc.want[i] {
				t.Fatalf("all=%v: listed %v, want %v", tc.all, ids, tc.want)
			}
		}
	}
}

type recordingCameraListService struct {
	agentpb.UnimplementedWendyVideoServiceServer
	list, refresh chan bool
}

func (s *recordingCameraListService) ListVideoDevices(_ context.Context, req *agentpb.ListVideoDevicesRequest) (*agentpb.ListVideoDevicesResponse, error) {
	s.list <- req.GetIncludeUnpaired()
	return &agentpb.ListVideoDevicesResponse{}, nil
}

func (s *recordingCameraListService) RefreshCameras(_ context.Context, req *agentpb.RefreshCamerasRequest) (*agentpb.RefreshCamerasResponse, error) {
	s.refresh <- req.GetIncludeUnpaired()
	return &agentpb.RefreshCamerasResponse{}, nil
}

// `camera list` always asks for unpaired cameras and filters them itself, so it
// can count what it hides, and an older agent that ignores the flag (and lists
// everything) is shown the same way.
func TestCameraListAsksForUnpairedCameras(t *testing.T) {
	svc := &recordingCameraListService{list: make(chan bool, 1), refresh: make(chan bool, 1)}
	startUDSAgentWithOS(t, "linux", func(s *grpc.Server) {
		agentpb.RegisterWendyVideoServiceServer(s, svc)
	})
	for _, args := range [][]string{nil, {"--refresh"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := newCameraListCmd()
		cmd.SetContext(ctx)
		cmd.SetOut(&bytes.Buffer{})
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("camera list %v: %v", args, err)
		}
		cancel()
		var asked bool
		if len(args) == 0 {
			asked = <-svc.list
		} else {
			asked = <-svc.refresh
		}
		if !asked {
			t.Fatalf("camera list %v did not ask for unpaired cameras", args)
		}
	}
}
