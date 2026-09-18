package commands

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/robotprobe"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// fakeTopicSource stands in for a DDS graph, so the command's probe selection and output
// are covered without a robot or a network.
type fakeTopicSource struct {
	topics   map[string][]string
	payloads map[string][][]byte
	dwell    time.Duration
}

func (f fakeTopicSource) TopicsOfType(typeName string) []string { return f.topics[typeName] }

func (f fakeTopicSource) Sample(_ context.Context, topic, _ string, window time.Duration, _ int) ([][]byte, error) {
	// Occupy the window as a real sampling pass would, so a measured rate is derived
	// from a span long enough to mean something.
	if f.dwell > 0 {
		time.Sleep(f.dwell)
	}
	_ = window
	return f.payloads[topic], nil
}

func g1CameraSource() fakeTopicSource {
	return fakeTopicSource{
		topics: map[string][]string{
			rosmsg.TypeCameraInfo: {"/camera/color/camera_info"},
		},
		payloads: map[string][][]byte{
			// The intrinsics measured on unitree-g1-nx-2: 43.4 vertical, 55.9 horizontal.
			"/camera/color/camera_info": {testCameraInfoPayload(640, 480, 603.33, 603.67)},
		},
	}
}

func TestProbeRobotReportsWhatTheCameraClaims(t *testing.T) {
	doc, err := probeRobot(context.Background(), g1CameraSource(), nil, robotInspectOptions{
		label: "unitree-g1-nx-2", vendorKind: "unitree-g1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if doc.Device != "unitree-g1-nx-2" || doc.VendorKind != "unitree-g1" {
		t.Errorf("document records %q/%q", doc.Device, doc.VendorKind)
	}
	if !doc.PassiveOnly {
		t.Error("PassiveOnly = false; inspection must only ever plan passive probes")
	}
	if got := doc.Summarise().Findings(); got != 0 {
		t.Errorf("findings = %d, want 0 for a working camera", got)
	}

	var vertical float64
	for _, p := range doc.Properties {
		if p.ID == "camera.color.fov.vertical" && len(p.Observations) == 1 {
			vertical = p.Observations[0].Quantity.Value()
		}
	}
	if math.Abs(vertical-43.4) > 0.05 {
		t.Errorf("vertical field of view = %.2f, want 43.4", vertical)
	}
}

// A robot with no cameras gets no camera rows, rather than a probe failure per topic.
func TestProbeRobotRegistersNothingForAnEmptyGraph(t *testing.T) {
	doc, err := probeRobot(context.Background(), fakeTopicSource{}, nil, robotInspectOptions{label: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Properties) != 0 {
		t.Errorf("got %d properties, want none", len(doc.Properties))
	}
	if len(doc.Skipped) != 0 {
		t.Errorf("got %v, want no skipped probes when none were registered", doc.Skipped)
	}
}

func TestWriteRobotDocumentRendersTheReportForAnOperator(t *testing.T) {
	restore := jsonOutput
	jsonOutput = false
	defer func() { jsonOutput = restore }()

	doc, err := probeRobot(context.Background(), g1CameraSource(), nil, robotInspectOptions{label: "unitree-g1-nx-2"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeRobotDocument(&out, doc, robotInspectOptions{}); err != nil {
		t.Fatal(err)
	}

	report := out.String()
	for _, want := range []string{"unitree-g1-nx-2", "camera", "vertical", "horizontal", "Nothing was commanded."} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	// Every angle must carry its axis; a bare degree figure is the original bug.
	for _, line := range strings.Split(report, "\n") {
		if strings.Contains(line, "deg") && !strings.Contains(line, "horizontal") && !strings.Contains(line, "vertical") {
			t.Errorf("angle printed without its axis: %q", line)
		}
	}
}

// An empty graph must say so. An empty report would read as "this robot has nothing",
// which is a claim the inspection has not earned.
func TestWriteRobotDocumentSaysWhenTheGraphWasEmpty(t *testing.T) {
	restore := jsonOutput
	jsonOutput = false
	defer func() { jsonOutput = restore }()

	doc, err := probeRobot(context.Background(), fakeTopicSource{}, nil, robotInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeRobotDocument(&out, doc, robotInspectOptions{domain: 3, settle: 8 * time.Second}); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	if !strings.Contains(report, "No ROS 2 writers found on domain 3") {
		t.Errorf("report does not name the empty graph:\n%s", report)
	}
	if !strings.Contains(report, "Nothing was commanded.") {
		t.Errorf("report drops the read-only assurance:\n%s", report)
	}
}

// The report an operator sees on a robot whose camera is calibrated at one resolution
// and streaming at another — the shape this command exists to produce.
func TestFullReportShowsTheDeclaredAgainstTheMeasured(t *testing.T) {
	restore := jsonOutput
	jsonOutput = false
	defer func() { jsonOutput = restore }()

	source := fakeTopicSource{
		dwell: 300 * time.Millisecond,
		topics: map[string][]string{
			rosmsg.TypeCameraInfo: {"/camera/color/camera_info"},
			rosmsg.TypeImage:      {"/camera/color/image_raw"},
		},
		payloads: map[string][][]byte{
			// Intrinsics calibrated at 640x480 ...
			"/camera/color/camera_info": {testCameraInfoPayload(640, 480, 603.33, 603.67)},
			// ... while the stream delivers 848x480.
			"/camera/color/image_raw": testImageFrames(9, 848, 480),
		},
	}

	doc, err := probeRobot(context.Background(), source, nil, robotInspectOptions{
		label: "unitree-g1-nx-2", vendorKind: "unitree-g1", window: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeRobotDocument(&out, doc, robotInspectOptions{}); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	t.Log("\n" + report)

	if got := doc.Summarise().Disagree; got != 1 {
		t.Errorf("disagreements = %d, want 1 (640 calibrated against 848 delivered)", got)
	}
	for _, want := range []string{"declared", "measured", "disagree", "Nothing was commanded."} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

func testImageFrames(n int, width, height uint32) [][]byte {
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, testImagePayload(width, height))
	}
	return out
}

func testImagePayload(width, height uint32) []byte {
	var buf []byte
	align := func(n int) {
		for len(buf)%n != 0 {
			buf = append(buf, 0)
		}
	}
	u32 := func(v uint32) { align(4); buf = binary.LittleEndian.AppendUint32(buf, v) }
	str := func(s string) {
		u32(uint32(len(s) + 1))
		buf = append(buf, s...)
		buf = append(buf, 0)
	}
	u32(1758207600)
	u32(250000000)
	str("camera_color_optical_frame")
	u32(height)
	u32(width)
	str("rgb8")
	buf = append(buf, 0)
	u32(width * 3)
	u32(width * height * 3)
	buf = append(buf, make([]byte, 32)...)
	return append([]byte{0x00, 0x01, 0x00, 0x00}, buf...)
}

func TestWriteRobotDocumentEmitsValidJSON(t *testing.T) {
	restore := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = restore }()

	doc, err := probeRobot(context.Background(), g1CameraSource(), nil, robotInspectOptions{label: "unitree-g1-nx-2"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeRobotDocument(&out, doc, robotInspectOptions{}); err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Schema     string `json:"schema"`
		Properties []struct {
			ID           string `json:"id"`
			Observations []struct {
				Axis string `json:"axis"`
			} `json:"observations"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("--json output does not parse: %v\n%s", err, out.String())
	}
	if decoded.Schema != "wendy.robot.inspection.v1" {
		t.Errorf("schema = %q", decoded.Schema)
	}
	for _, p := range decoded.Properties {
		if !strings.Contains(p.ID, "fov") {
			continue
		}
		if p.Observations[0].Axis == "" {
			t.Errorf("%s serialised without an axis", p.ID)
		}
	}
}

// testCameraInfoPayload encodes a CameraInfo through the K matrix, paying CDR alignment.
func testCameraInfoPayload(width, height uint32, fx, fy float64) []byte {
	var buf []byte
	align := func(n int) {
		for len(buf)%n != 0 {
			buf = append(buf, 0)
		}
	}
	u32 := func(v uint32) { align(4); buf = binary.LittleEndian.AppendUint32(buf, v) }
	f64 := func(v float64) { align(8); buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(v)) }
	str := func(s string) {
		u32(uint32(len(s) + 1))
		buf = append(buf, s...)
		buf = append(buf, 0)
	}

	u32(1758207600)
	u32(250000000)
	str("camera_color_optical_frame")
	u32(height)
	u32(width)
	str("plumb_bob")
	u32(5)
	for i := 0; i < 5; i++ {
		f64(0)
	}
	for _, v := range []float64{fx, 0, float64(width) / 2, 0, fy, float64(height) / 2, 0, 0, 1} {
		f64(v)
	}
	return append([]byte{0x00, 0x01, 0x00, 0x00}, buf...)
}

// A robot reachable through the cloud tunnel but with no visible ROS 2 graph still
// produces a real report. This is the path that works when DDS multicast cannot reach
// the machine running the command, which is most of the time.
func TestProbeRobotReportsTheHostWithNoGraphAtAll(t *testing.T) {
	doc, err := probeRobot(context.Background(), nil, &stubHostFacts{}, robotInspectOptions{
		label: "unitree-g1-nx-2", vendorKind: "unitree-g1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Properties) == 0 {
		t.Fatal("no properties from the agent alone; the host half must stand on its own")
	}
	if !doc.PassiveOnly {
		t.Error("PassiveOnly = false")
	}
	for _, id := range []string{
		"compute.board", "compute.architecture", "storage.free",
		"hardware.camera.devices", "hardware.can.count", "clock.drift",
	} {
		found := false
		for _, p := range doc.Properties {
			if p.ID == id {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q from an agent-only report", id)
		}
	}
	if _, err := doc.CanonicalJSON(); err != nil {
		t.Errorf("document does not serialise: %v", err)
	}
}

// Both halves together, which is what a reachable robot on its own LAN produces.
func TestProbeRobotCombinesHostAndGraph(t *testing.T) {
	doc, err := probeRobot(context.Background(), g1CameraSource(), &stubHostFacts{}, robotInspectOptions{
		label: "unitree-g1-nx-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawHost, sawCamera bool
	for _, p := range doc.Properties {
		if p.ID == "compute.board" {
			sawHost = true
		}
		if p.ID == "camera.color.fov.vertical" {
			sawCamera = true
		}
	}
	if !sawHost || !sawCamera {
		t.Errorf("host=%v camera=%v; a full pass needs both", sawHost, sawCamera)
	}
}

type stubHostFacts struct{}

func (stubHostFacts) Hardware(context.Context) ([]robotprobe.HardwareDevice, error) {
	return []robotprobe.HardwareDevice{
		{Category: "camera", DevicePath: "/dev/video4", Description: "RealSense color"},
		{Category: "can", DevicePath: "can0", Description: "CANable"},
	}, nil
}

func (stubHostFacts) DeviceClock(context.Context) (time.Time, time.Duration, error) {
	return time.Now().UTC().Add(800 * time.Microsecond), 3 * time.Millisecond, nil
}

func (stubHostFacts) HostFacts(context.Context) (*robotprobe.HostFacts, error) {
	return &robotprobe.HostFacts{
		Hostname: "unitree-g1-nx-2", DeviceType: "jetson-orin-nx",
		CPUArchitecture: "arm64", CPUCount: 8, MemoryTotalBytes: 16000000000,
		OS: "wendyos", OSVersion: "2026.08", AgentVersion: "dev",
		HasGPU: true, GPUVendor: "nvidia", GPUArch: "sm_87", CUDAVersion: "13.2",
		DiskTotalBytes: 120000000000, DiskUsedBytes: 40000000000, StorageMedium: "nvme",
		Interfaces: []robotprobe.HostInterface{{Name: "eth0", Addresses: []string{"192.168.1.40"}}},
	}, nil
}
