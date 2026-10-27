package services

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	tc001StableID  = "by-id:usb-Camera_USB_Camera_EA6935255-video-index0"
	tc001bStableID = "by-id:usb-Camera_USB_Camera_EA8164649-video-index0"
	webcamStableID = "by-id:usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-video-index0"
)

func TestValidateThermalCorrection(t *testing.T) {
	for _, tc := range []struct {
		gain, offset float64
		ok           bool
		why          string
	}{
		{0.48, 0, true, ""},
		{1, 0, true, ""},
		{0.1, -100, true, ""},
		{10, 100, true, ""},
		{0, 0, false, "greater than 0"},
		{-0.48, 0, false, "greater than 0"},
		{0.09, 0, false, "outside 0.1..10"},
		{10.5, 0, false, "outside 0.1..10"},
		{48, 0, false, "outside 0.1..10"},
		{math.NaN(), 0, false, "not a number"},
		{math.Inf(1), 0, false, "not a number"},
		{0.48, 100.5, false, "outside -100..100"},
		{0.48, -273.15, false, "outside -100..100"},
		{0.48, math.NaN(), false, "not a number"},
	} {
		err := validateThermalCorrection(tc.gain, tc.offset)
		if (err == nil) != tc.ok || (err != nil && !strings.Contains(err.Error(), tc.why)) {
			t.Errorf("gain %v offset %v: %v, want ok %v (%q)", tc.gain, tc.offset, err, tc.ok, tc.why)
		}
	}
}

// The store keeps each camera's pair by stable id, survives a reload, is
// private to the agent, and counts its changes so a running decoder notices.
func TestThermalCalibrationStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "thermal-calibration.json")
	store := newThermalCalibrationStore(path)
	if _, ok, err := store.get(tc001StableID); ok || err != nil {
		t.Fatalf("absent file: ok %v err %v", ok, err)
	}
	gen := store.generation.Load()
	when := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	if err := store.set(tc001StableID, storedThermalCalibration{Gain: 0.48, Offset: 0.5, Updated: when}); err != nil {
		t.Fatal(err)
	}
	if err := store.set(tc001bStableID, storedThermalCalibration{Gain: 1, Updated: when}); err != nil {
		t.Fatal(err)
	}
	if store.generation.Load() == gen {
		t.Error("a set did not change the generation")
	}

	reloaded := newThermalCalibrationStore(path)
	c, ok, err := reloaded.get(tc001StableID)
	if err != nil || !ok || c.Gain != 0.48 || c.Offset != 0.5 || !c.Updated.Equal(when) {
		t.Fatalf("reloaded %+v ok %v err %v", c, ok, err)
	}

	gen = reloaded.generation.Load()
	if cleared, err := reloaded.clear(tc001StableID); !cleared || err != nil {
		t.Fatalf("clear: %v %v", cleared, err)
	}
	if cleared, err := reloaded.clear(tc001StableID); cleared || err != nil {
		t.Fatalf("second clear: %v %v", cleared, err)
	}
	if reloaded.generation.Load() == gen {
		t.Error("a clear did not change the generation")
	}
	again := newThermalCalibrationStore(path)
	if _, ok, _ := again.get(tc001StableID); ok {
		t.Error("a cleared correction came back after a reload")
	}
	if _, ok, _ := again.get(tc001bStableID); !ok {
		t.Error("clearing one camera lost another's correction")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("store file mode %#o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("store dir mode %#o, want 0700", perm)
	}
}

// A file the store cannot trust fails closed: nothing reads a correction from
// it, and nothing overwrites the other cameras' entries in it. Once it is
// repaired, the store reads it without an agent restart.
func TestThermalCalibrationStoreFailsClosed(t *testing.T) {
	for name, contents := range map[string]string{
		"not JSON":     "{\"cameras\": {",
		"a wrong gain": `{"cameras": {"` + tc001bStableID + `": {"gain": 0, "offset": 0}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thermal-calibration.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			store := newThermalCalibrationStore(path)
			if _, _, err := store.get(tc001StableID); err == nil {
				t.Fatal("read a correction from an untrustworthy file")
			}
			if err := store.set(tc001StableID, storedThermalCalibration{Gain: 0.48}); err == nil {
				t.Fatal("set overwrote an untrustworthy file")
			}
			if _, err := store.clear(tc001StableID); err == nil {
				t.Fatal("clear overwrote an untrustworthy file")
			}
			if b, _ := os.ReadFile(path); string(b) != contents {
				t.Fatalf("file changed to %q", b)
			}

			if err := os.WriteFile(path, []byte(`{"cameras": {"`+tc001StableID+`": {"gain": 0.5, "offset": 1}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if c, ok, err := store.get(tc001StableID); err != nil || !ok || c.Gain != 0.5 {
				t.Fatalf("after repair: %+v %v %v", c, ok, err)
			}
		})
	}
}

// calibrationRig is a video service whose cameras are described, not
// opened: each node's stable id and whether it offers the TC001's stacked
// mode.
type calibrationRig struct {
	svc     *VideoService
	path    string
	stable  map[string]stableNames
	thermal map[string]bool
}

func newCalibrationRig(t *testing.T) *calibrationRig {
	t.Helper()
	rig := &calibrationRig{
		stable: map[string]stableNames{
			"/dev/video0": {byID: strings.TrimPrefix(webcamStableID, "by-id:")},
			"/dev/video4": {byID: strings.TrimPrefix(tc001StableID, "by-id:")},
			// A thermal camera udev gave no name: twin serials, or no rules.
			"/dev/video6": {},
		},
		thermal: map[string]bool{"/dev/video4": true, "/dev/video6": true},
	}
	prevModes, prevIdentity := gridYUYVModes, gridNodeIdentity
	gridYUYVModes = func(path string) ([][2]uint32, error) {
		if rig.thermal[path] {
			return tc001Modes, nil
		}
		return [][2]uint32{{640, 480}, {1920, 1080}}, nil
	}
	gridNodeIdentity = func(path string) (string, bool) {
		_, ok := rig.stable[path]
		return path, ok
	}
	t.Cleanup(func() { gridYUYVModes, gridNodeIdentity = prevModes, prevIdentity })

	rig.svc = NewVideoService(context.Background(), zap.NewNop(), nil)
	rig.path = filepath.Join(t.TempDir(), "thermal-calibration.json")
	rig.svc.thermalCalibration = newThermalCalibrationStore(rig.path)
	rig.svc.readStableNames = func() map[string]stableNames { return rig.stable }
	return rig
}

func (r *calibrationRig) set(id uint32, stableID string, gain, offset float64) (*agentpb.ThermalCalibration, error) {
	return r.svc.SetThermalCalibration(context.Background(), &agentpb.SetThermalCalibrationRequest{DeviceId: id, StableId: stableID, Gain: gain, Offset: offset})
}

func (r *calibrationRig) get(id uint32, stableID string) (*agentpb.ThermalCalibration, error) {
	return r.svc.GetThermalCalibration(context.Background(), &agentpb.GetThermalCalibrationRequest{DeviceId: id, StableId: stableID})
}

func (r *calibrationRig) clear(id uint32, stableID string) (*agentpb.ThermalCalibration, error) {
	return r.svc.ClearThermalCalibration(context.Background(), &agentpb.ClearThermalCalibrationRequest{DeviceId: id, StableId: stableID})
}

func TestThermalCalibrationSetGetClear(t *testing.T) {
	rig := newCalibrationRig(t)

	got, err := rig.get(4, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCalibrated() || got.GetGain() != 1 || got.GetOffset() != 0 || got.GetStableId() != tc001StableID || got.GetPath() != "/dev/video4" || got.GetDeviceId() != 4 {
		t.Fatalf("before a set: %v", got)
	}

	before := time.Now()
	set, err := rig.set(4, "", 0.48, -0.5)
	if err != nil {
		t.Fatal(err)
	}
	if !set.GetCalibrated() || set.GetGain() != 0.48 || set.GetOffset() != -0.5 || set.GetStableId() != tc001StableID || set.GetUpdatedUnixNanos() < before.UnixNano() {
		t.Fatalf("set replied %v", set)
	}
	corr, _, err := rig.svc.thermalCorrectionFor("/dev/video4")
	if err != nil || corr != (thermalCorrection{calibrated: true, gain: 0.48, offset: -0.5}) {
		t.Fatalf("the decoder would apply %+v (%v)", corr, err)
	}

	// Named by stable id, it is the same camera.
	if got, err := rig.get(0, tc001StableID); err != nil || !got.GetCalibrated() || got.GetGain() != 0.48 || got.GetDeviceId() != 4 {
		t.Fatalf("by stable id: %v %v", got, err)
	}
	// Setting again replaces it.
	if _, err := rig.set(0, tc001StableID, 0.5, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := rig.get(4, ""); got.GetGain() != 0.5 || got.GetOffset() != 0 {
		t.Fatalf("after a second set: %v", got)
	}

	cleared, err := rig.clear(4, "")
	if err != nil || cleared.GetCalibrated() || cleared.GetGain() != 1 || cleared.GetStableId() != tc001StableID {
		t.Fatalf("clear replied %v %v", cleared, err)
	}
	if corr, _, _ := rig.svc.thermalCorrectionFor("/dev/video4"); corr.calibrated {
		t.Fatal("the decoder still applies a cleared correction")
	}
	// Clearing a camera with none succeeds.
	if _, err := rig.clear(4, ""); err != nil {
		t.Fatal(err)
	}
}

// The correction belongs to the camera, not to its number: after a reboot
// renumbers the nodes, it follows the camera, and the camera that took the
// old number does not inherit it.
func TestThermalCalibrationFollowsTheCameraNotTheNumber(t *testing.T) {
	rig := newCalibrationRig(t)
	if _, err := rig.set(4, "", 0.48, 0); err != nil {
		t.Fatal(err)
	}

	// Rebooted: the calibrated camera is /dev/video2 now, and another TC001
	// is /dev/video4.
	rig.stable = map[string]stableNames{
		"/dev/video2": {byID: strings.TrimPrefix(tc001StableID, "by-id:")},
		"/dev/video4": {byID: strings.TrimPrefix(tc001bStableID, "by-id:")},
	}
	rig.thermal = map[string]bool{"/dev/video2": true, "/dev/video4": true}

	if got, err := rig.get(2, ""); err != nil || !got.GetCalibrated() || got.GetGain() != 0.48 || got.GetPath() != "/dev/video2" {
		t.Fatalf("the calibrated camera at its new number: %v %v", got, err)
	}
	if got, err := rig.get(4, ""); err != nil || got.GetCalibrated() || got.GetStableId() != tc001bStableID {
		t.Fatalf("the camera that took the old number: %v %v", got, err)
	}
	if corr, _, _ := rig.svc.thermalCorrectionFor("/dev/video4"); corr.calibrated {
		t.Fatal("the decoder applies the old number's correction to another camera")
	}
	if corr, _, _ := rig.svc.thermalCorrectionFor("/dev/video2"); !corr.calibrated || corr.gain != 0.48 {
		t.Fatalf("the decoder lost the camera's correction: %+v", corr)
	}
}

func TestThermalCalibrationRefusals(t *testing.T) {
	rig := newCalibrationRig(t)
	for _, tc := range []struct {
		name         string
		id           uint32
		stableID     string
		gain, offset float64
		code         codes.Code
		why          string
	}{
		{"a gain of zero", 4, "", 0, 0, codes.InvalidArgument, "greater than 0"},
		{"a negative gain", 4, "", -0.48, 0, codes.InvalidArgument, "greater than 0"},
		{"an absurd gain", 4, "", 48, 0, codes.InvalidArgument, "outside 0.1..10"},
		{"an absurd offset", 4, "", 0.48, 273.15, codes.InvalidArgument, "outside -100..100"},
		{"not a number", 4, "", math.NaN(), 0, codes.InvalidArgument, "not a number"},
		{"a webcam", 0, "", 0.48, 0, codes.FailedPrecondition, "not a radiometric thermal camera"},
		{"a webcam by stable id", 0, webcamStableID, 0.48, 0, codes.FailedPrecondition, "not a radiometric thermal camera"},
		{"a network camera", 200, "", 0.48, 0, codes.InvalidArgument, "network camera"},
		{"a ROS 2 topic", 128, "", 0.48, 0, codes.InvalidArgument, "ROS 2"},
		{"no such node", 9, "", 0.48, 0, codes.NotFound, "no camera at /dev/video9"},
		{"an unknown stable id", 0, "by-id:usb-Nobody-video-index0", 0.48, 0, codes.NotFound, tc001StableID},
		{"a camera without a stable id", 6, "", 0.48, 0, codes.FailedPrecondition, "no stable identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rig.set(tc.id, tc.stableID, tc.gain, tc.offset)
			if status.Code(err) != tc.code || !strings.Contains(status.Convert(err).Message(), tc.why) {
				t.Fatalf("got %v, want %v containing %q", err, tc.code, tc.why)
			}
		})
	}
	if _, err := os.Stat(rig.path); !os.IsNotExist(err) {
		t.Fatalf("a refused set wrote the store: %v", err)
	}
	if _, err := rig.get(0, ""); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("get on a webcam: %v", err)
	}
}

// A camera that is not attached can still have its stored correction cleared
// by stable id, for one that moved to another device. It cannot be set or
// read that way, because nothing can check that it is a thermal camera.
func TestThermalCalibrationClearsADetachedCamera(t *testing.T) {
	rig := newCalibrationRig(t)
	if _, err := rig.set(4, "", 0.48, 0); err != nil {
		t.Fatal(err)
	}
	delete(rig.stable, "/dev/video4")
	if _, err := rig.get(0, tc001StableID); status.Code(err) != codes.NotFound {
		t.Fatalf("get of a detached camera: %v", err)
	}
	got, err := rig.clear(0, tc001StableID)
	if err != nil || got.GetCalibrated() || got.GetStableId() != tc001StableID || got.GetPath() != "" {
		t.Fatalf("clear of a detached camera: %v %v", got, err)
	}
	if _, ok, _ := rig.svc.thermalCalibration.get(tc001StableID); ok {
		t.Fatal("the detached camera's correction is still stored")
	}
}

// An unreadable store is reported, and refuses writes, rather than letting a
// set replace every other camera's correction.
func TestThermalCalibrationReportsAnUnreadableStore(t *testing.T) {
	rig := newCalibrationRig(t)
	if err := os.WriteFile(rig.path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"get":   func() error { _, err := rig.get(4, ""); return err },
		"set":   func() error { _, err := rig.set(4, "", 0.48, 0); return err },
		"clear": func() error { _, err := rig.clear(4, ""); return err },
	} {
		if err := call(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Repair or remove it") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := rig.svc.thermalCorrectionFor("/dev/video4"); err == nil {
		t.Error("the decoder would publish temperatures without knowing the camera's correction")
	}
}
