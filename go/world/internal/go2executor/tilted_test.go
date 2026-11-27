package go2executor

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
)

func tiltedController(t *testing.T) (*Controller, *fakeAdapter) {
	t.Helper()
	c, _, a, _ := fixture(t)
	var p Profile
	if e := json.Unmarshal(c.cfg.ProfileJSON, &p); e != nil {
		t.Fatal(e)
	}
	p.Schema = "wendy.world.go2-profile.v2"
	p.MaxAbsRollRad = .12
	p.MaxAbsPitchRad = .12
	p.TiltedScan = &go2nav.TiltedScanProfile{Model: "solid-vertical-slab-v1", BaseHeightMinM: .15, BaseHeightMaxM: .6, ObstacleMinHeightM: .8, SlabMarginM: .02, BodyHalfHeightM: .35, MaxSupportRangeM: 8, RangeErrorM: .02, AttitudeErrorRad: .001, MaxTranslationMPerWallS: 1, MaxRotationRadPerWallS: 2, HeightCalibrationErrorM: .005, OdomFloorZM: 0}
	cfg := c.cfg
	cfg.ProfileJSON, _ = json.Marshal(p)
	cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
	var e error
	c, e = New(c.gateway, a, cfg)
	if e != nil {
		t.Fatal(e)
	}
	a.state.Binding = cfg.Binding
	a.fresh()
	// Start setup is reused from the fixture's explicit owner state.
	c.session = a.state.Session
	c.boot = a.state.BridgeBootID
	c.odomPublisher = a.state.OdomPublisher
	c.scanPublisher = a.state.ScanPublisher
	c.commandPublisher = a.state.CommandPublisher
	a.state.Scan.TiltedPose = &go2nav.TiltedPose{QuaternionWXYZ: [4]float64{1, 0, 0, 0}, OdomPositionXYZ: [3]float64{a.state.Pose.X, a.state.Pose.Y, .33}, MountTranslationXYZ: [3]float64{.28, 0, .08}, MountQuaternionWXYZ: [4]float64{1, 0, 0, 0}, OdomCapturedUnixNS: a.state.OdomCapturedUnixNS, BaseHeightMinM: .275, BaseHeightMaxM: .385}
	return c, a
}

func TestTiltedProfileExplicitVersionAndCalibrationKeys(t *testing.T) {
	c, _ := tiltedController(t)
	for _, name := range []string{"missing_floor", "null_floor", "missing_nested", "legacy_version", "unknown_version", "unknown_model"} {
		t.Run(name, func(t *testing.T) {
			cfg := c.cfg
			var m map[string]any
			_ = json.Unmarshal(cfg.ProfileJSON, &m)
			v := m["tilted_scan"].(map[string]any)
			switch name {
			case "missing_floor":
				delete(v, "odom_floor_z_m")
			case "null_floor":
				v["odom_floor_z_m"] = nil
			case "missing_nested":
				delete(m, "tilted_scan")
			case "legacy_version":
				m["schema"] = "wendy.world.go2-profile.v1"
			case "unknown_version":
				m["schema"] = "wendy.world.go2-profile.v3"
			case "unknown_model":
				v["model"] = "leveled"
			}
			cfg.ProfileJSON, _ = json.Marshal(m)
			cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
			if _, _, e := DecodeConfiguration(cfg); e == nil {
				t.Fatal("unsupported or implicit calibration admitted")
			}
		})
	}
}
func TestTiltedStateValidationAndDeepCopy(t *testing.T) {
	c, a := tiltedController(t)
	if e := c.validateState(a.state); e != nil {
		t.Fatal(e)
	}
	original := a.state
	for _, name := range []string{"missing_pose", "wrong_age", "wrong_stamp", "height_departure", "wrong_roll", "missing_pair_bound", "wrong_raw_xy"} {
		t.Run(name, func(t *testing.T) {
			s, _ := diagnosticState(original)
			switch name {
			case "missing_pose":
				s.Scan.TiltedPose = nil
			case "wrong_age":
				s.Scan.TiltedPose.OdometryEpochAgeS = 1
			case "wrong_stamp":
				s.Scan.TiltedPose.OdomCapturedUnixNS--
			case "height_departure":
				s.Scan.TiltedPose.OdomPositionXYZ[2] = .7
				s.Scan.TiltedPose.BaseHeightMinM = .645
				s.Scan.TiltedPose.BaseHeightMaxM = .755
			case "wrong_roll":
				s.RollRad = .1
			case "missing_pair_bound":
				s.OdomCapturedUnixNS -= 10000000
				s.SampleUnixMS -= 10
				s.Scan.TiltedPose.OdomCapturedUnixNS = s.OdomCapturedUnixNS
			case "wrong_raw_xy":
				s.Scan.TiltedPose.OdomPositionXYZ[0] += .1
			}
			if e := c.validateState(s); e == nil {
				t.Fatal("contradictory original evidence accepted")
			}
		})
	}
	c.observedState = original
	obs, ok := c.Observation()
	if !ok {
		t.Fatal("missing observation")
	}
	obs.Scan.TiltedPose.OdomPositionXYZ[2] = 99
	if original.Scan.TiltedPose.OdomPositionXYZ[2] != .33 {
		t.Fatal("observation aliases original pose")
	}
	copy, _ := diagnosticState(original)
	copy.Scan.TiltedPose.OdomPositionXYZ[2] = 99
	if original.Scan.TiltedPose.OdomPositionXYZ[2] != .33 {
		t.Fatal("diagnostic aliases pose")
	}
	shot := FaultSnapshot{Profile: c.profile, State: original}
	second := copyFault(shot)
	second.Profile.TiltedScan.OdomFloorZM = 99
	if c.profile.TiltedScan.OdomFloorZM != 0 {
		t.Fatal("fault profile aliases commissioned config")
	}
}
func TestTiltedStateWireShapesAndDiagnosticRoundTrip(t *testing.T) {
	c, a := tiltedController(t)
	shot := FaultSnapshot{Profile: c.profile, State: a.state}
	b, e := json.Marshal(shot)
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	s, e := DecodeState(m["state"])
	if e != nil {
		t.Fatal(e)
	}
	if e = c.validateState(s); e != nil {
		t.Fatal(e)
	}
	var state map[string]any
	_ = json.Unmarshal(m["state"], &state)
	for _, key := range []string{"quaternion_wxyz", "odom_position_xyz", "mount_translation_xyz", "mount_quaternion_wxyz", "base_height_min_m", "odom_captured_unix_ns"} {
		t.Run(key, func(t *testing.T) {
			raw := append([]byte(nil), m["state"]...)
			var x map[string]any
			_ = json.Unmarshal(raw, &x)
			tilt := x["scan"].(map[string]any)["tilted_pose"].(map[string]any)
			delete(tilt, key)
			raw, _ = json.Marshal(x)
			if _, e := DecodeState(raw); e == nil {
				t.Fatal("missing nested field accepted")
			}
		})
	}
	raw := bytes.Replace(m["state"], []byte(`"quaternion_wxyz":[1,0,0,0]`), []byte(`"quaternion_wxyz":[1,0,0]`), 1)
	if bytes.Equal(raw, m["state"]) {
		t.Fatal("fixture replacement missed")
	}
	if _, e := DecodeState(raw); e == nil {
		t.Fatal("short quaternion invented zero")
	}
	// Original nonfinite raw pose cannot silently become a valid zero diagnostic.
	shot.State.Scan.TiltedPose.OdomPositionXYZ[2] = math.NaN()
	b, e = json.Marshal(shot)
	if e != nil {
		t.Fatal(e)
	}
	_ = json.Unmarshal(b, &m)
	if _, e := DecodeState(m["state"]); e == nil {
		t.Fatal("null invalid height became known")
	}
}

func TestPythonTiltedStateAndConfigurationFixture(t *testing.T) {
	read := func(n string) []byte {
		b, e := os.ReadFile("testdata/" + n)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	raw := read("python-state-v2.json")
	s, e := DecodeState(raw)
	if e != nil {
		t.Fatal(e)
	}
	_, g, a, _ := fixture(t)
	cfg := Config{TaskID: "fixture", Site: "fixture", Binding: s.Binding, AlignmentJSON: read("python-alignment-v2.json"), ProfileJSON: read("python-profile-v2.json"), Now: func() time.Time { return time.UnixMilli(s.SampleUnixMS + 10) }, Monotonic: func() time.Duration { return 0 }}
	c, e := New(g, a, cfg)
	if e != nil {
		t.Fatal(e)
	}
	c.session, c.boot, c.sequence = s.Session, s.BridgeBootID, s.Sequence
	c.odomPublisher, c.scanPublisher, c.commandPublisher = s.OdomPublisher, s.ScanPublisher, s.CommandPublisher
	if e = c.validateState(s); e != nil {
		t.Fatal(e)
	}
	if _, e = go2nav.Build(s.Scan, go2nav.Bounds{Min: go2nav.Point{X: -2, Y: -2}, Max: go2nav.Point{X: 2, Y: 2}}, c.profile.navigation()); e != nil {
		t.Fatal(e)
	}
	if s.RollRad <= .05 || s.PitchRad <= .05 {
		t.Fatal("fixture never exercises v2 full-rotation path")
	}
}
