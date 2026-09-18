package robotprobe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

type fakeHardware struct {
	devices []HardwareDevice
	err     error
}

func (f fakeHardware) Hardware(context.Context) ([]HardwareDevice, error) {
	return f.devices, f.err
}

// The RealSense on unitree-g1-nx-2 presents three video nodes, and the arms rig carries
// CAN interfaces. Both are the kind of thing an operator needs enumerated.
func g1Hardware() []HardwareDevice {
	return []HardwareDevice{
		{Category: "camera", DevicePath: "/dev/video4", Description: "RealSense D435i color"},
		{Category: "camera", DevicePath: "/dev/video0", Description: "RealSense D435i depth"},
		{Category: "camera", DevicePath: "/dev/video2", Description: "RealSense D435i infrared"},
		{Category: "can", DevicePath: "can0", Description: "CANable"},
		{Category: "usb", DevicePath: "/dev/bus/usb/001/004", Description: "Intel RealSense"},
	}
}

func hardwareEnv(source HardwareSource) *robotinspect.Env {
	return robotinspect.NewEnv().Offer(robotinspect.RequirementHostStats, source)
}

func TestHardwareCountsAndNamesEachCategory(t *testing.T) {
	properties, err := Hardware{}.Observe(context.Background(), hardwareEnv(fakeHardware{devices: g1Hardware()}))
	if err != nil {
		t.Fatal(err)
	}

	if got := findIn(t, properties, "hardware.devices").Observations[0].Quantity.Value(); got != 5 {
		t.Errorf("device count = %v, want 5", got)
	}
	if got := findIn(t, properties, "hardware.camera.count").Observations[0].Quantity.Value(); got != 3 {
		t.Errorf("camera count = %v, want 3", got)
	}
	if got := findIn(t, properties, "hardware.can.count").Observations[0].Quantity.Value(); got != 1 {
		t.Errorf("can count = %v, want 1", got)
	}

	// Paths are sorted so the same robot renders identically twice, and a camera that
	// disappears shows up as a disagreement on this row rather than as a new row.
	cameras := findIn(t, properties, "hardware.camera.devices").Observations[0]
	if !cameras.IsText() {
		t.Fatal("a device list is text, not a number")
	}
	if cameras.Text != "/dev/video0 /dev/video2 /dev/video4" {
		t.Errorf("camera devices = %q, want them sorted", cameras.Text)
	}
}

// Inspect the same robot twice with a camera missing, and the device list disagrees.
// That is the question an operator asks when a sensor stops working.
func TestHardwareDeviceListDisagreesWhenACameraDisappears(t *testing.T) {
	before, err := Hardware{}.Observe(context.Background(), hardwareEnv(fakeHardware{devices: g1Hardware()}))
	if err != nil {
		t.Fatal(err)
	}
	after, err := Hardware{}.Observe(context.Background(),
		hardwareEnv(fakeHardware{devices: g1Hardware()[1:]})) // color node gone
	if err != nil {
		t.Fatal(err)
	}

	merged := robotinspect.Property{
		ID: "hardware.camera.devices",
		Observations: []robotinspect.Observation{
			findIn(t, before, "hardware.camera.devices").Observations[0],
			findIn(t, after, "hardware.camera.devices").Observations[0],
		},
	}
	assessment := merged.Assess()
	if assessment.Verdict != robotinspect.VerdictDisagree {
		t.Fatalf("verdict = %q, want %q", assessment.Verdict, robotinspect.VerdictDisagree)
	}
	if !strings.Contains(assessment.Detail, "/dev/video4") {
		t.Errorf("detail %q should name the device that changed", assessment.Detail)
	}
}

func TestHardwareReportsAnEmptyEnumerationAsUnknown(t *testing.T) {
	properties, err := Hardware{}.Observe(context.Background(), hardwareEnv(fakeHardware{}))
	if err != nil {
		t.Fatal(err)
	}
	devices := findIn(t, properties, "hardware.devices")
	if devices.Unknown == nil || devices.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Errorf("devices = %+v, want an unknown", devices.Unknown)
	}
}

func TestHardwareFallsBackToDescriptionWhenThereIsNoPath(t *testing.T) {
	properties, err := Hardware{}.Observe(context.Background(), hardwareEnv(fakeHardware{
		devices: []HardwareDevice{{Category: "gpu", Description: "NVIDIA Orin integrated"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := findIn(t, properties, "hardware.gpu.devices").Observations[0].Text; got != "NVIDIA Orin integrated" {
		t.Errorf("gpu devices = %q", got)
	}
}

func TestHardwareGroupsUncategorisedDevices(t *testing.T) {
	properties, err := Hardware{}.Observe(context.Background(), hardwareEnv(fakeHardware{
		devices: []HardwareDevice{{DevicePath: "/dev/mystery"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	findIn(t, properties, "hardware.other.devices")
}

func TestHardwareSurfacesAnEnumerationFailure(t *testing.T) {
	if _, err := (Hardware{}).Observe(context.Background(),
		hardwareEnv(fakeHardware{err: errors.New("agent unreachable")})); err == nil {
		t.Error("an enumeration failure was swallowed")
	}
}

func TestHardwareIsPassive(t *testing.T) {
	if got := (Hardware{}).Class(); got != robotinspect.ClassPassive {
		t.Errorf("class = %q, want passive", got)
	}
}
