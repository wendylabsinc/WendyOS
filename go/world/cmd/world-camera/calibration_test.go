package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/camerapose"
)

func TestCalibrationFlagsRequireExplicitPerCameraBinding(t *testing.T) {
	for _, args := range [][]string{
		{"--calibration-profile", "/unused.json"},
		{"--calibration-binding", "/unused.json"},
		{"--camera-config", "/unused.json", "--calibration-profile", "/profile.json", "--calibration-binding", "/binding.json"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("ambiguous calibration arguments accepted: %v: %v", args, err)
		}
	}
}

func TestReadCalibrationBindingRejectsUnknownAndTrailingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binding.json")
	for _, data := range []string{`null`, `{"typo_sensor":"ov3660"}`, `{} {}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readCalibrationBinding(path); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	data := `{"hardware_id":"fixture-one","board":"test-board","sensor":"test-sensor","lens":"test-lens","capture_mode":"test-full-frame","width":640,"height":480}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := readCalibrationBinding(path)
	if err != nil || b.HardwareID != "fixture-one" || b.Width != 640 {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestTwinCalibrationIsScopedToConfiguredCamera(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profile.json")
	// Deliberate simulated contract fixture; these values make no hardware claim.
	profile := map[string]any{
		"schema": "wendy.camera-intrinsics.v1", "id": "fixture-model", "name": "Synthetic reference camera", "scope": "model", "origin": "simulated",
		"camera":     map[string]any{"board": "test-board", "sensor": "test-sensor", "lens": "test-lens", "capture_mode": "test-full-frame", "width": 640, "height": 480},
		"intrinsics": map[string]any{"fx": 500, "fy": 500, "cx": 320, "cy": 240, "distortion": []float64{0, 0, 0, 0, 0}},
		"evidence":   map[string]any{"method": "opencv-checkerboard", "reference_device_ids": []string{"fixture-one"}, "training_views": 12, "validation_views": 3, "training_rms_pixels": 0.2, "validation_rms_pixels": 0.3, "dataset_sha256": strings.Repeat("a", 64), "calibrated_at": "2026-11-22T00:00:00Z"},
	}
	b, _ := json.Marshal(profile)
	if err := os.WriteFile(profilePath, b, 0600); err != nil {
		t.Fatal(err)
	}
	binding := &camerapose.CalibrationBinding{HardwareID: "fixture-one", Board: "test-board", Sensor: "test-sensor", Lens: "test-lens", CaptureMode: "test-full-frame", Width: 640, Height: 480}
	configs := []cameraConfig{
		{ID: "one", Name: "Profile camera", Device: "wendy-lite:test-one", CalibrationProfile: profilePath, CalibrationBinding: binding},
		{ID: "two", Name: "Uncalibrated camera", Device: "wendy-lite:test-two"},
	}
	service, err := newTwin(context.Background(), configs, filepath.Join(dir, "state"), "/usr/bin/false", "", "", "test-token", false)
	if err != nil {
		t.Fatal(err)
	}
	defer service.close()
	state := service.snapshot()
	if state.Cameras[0].Pose.Calibration == nil || state.Cameras[0].Pose.Calibration.ID != "fixture-model" || state.Cameras[1].Pose.Calibration != nil {
		t.Fatalf("calibration leaked or disappeared: %+v", state.Cameras)
	}
}
