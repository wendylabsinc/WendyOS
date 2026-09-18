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

	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// fakeTopicSource stands in for a DDS graph, so the command's probe selection and output
// are covered without a robot or a network.
type fakeTopicSource struct {
	topics   map[string][]string
	payloads map[string][][]byte
}

func (f fakeTopicSource) TopicsOfType(typeName string) []string { return f.topics[typeName] }

func (f fakeTopicSource) Sample(_ context.Context, topic, _ string, _ time.Duration, _ int) ([][]byte, error) {
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
	doc, err := probeRobot(context.Background(), g1CameraSource(), robotInspectOptions{
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
	doc, err := probeRobot(context.Background(), fakeTopicSource{}, robotInspectOptions{label: "bare"})
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

	doc, err := probeRobot(context.Background(), g1CameraSource(), robotInspectOptions{label: "unitree-g1-nx-2"})
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

	doc, err := probeRobot(context.Background(), fakeTopicSource{}, robotInspectOptions{})
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

func TestWriteRobotDocumentEmitsValidJSON(t *testing.T) {
	restore := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = restore }()

	doc, err := probeRobot(context.Background(), g1CameraSource(), robotInspectOptions{label: "unitree-g1-nx-2"})
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
