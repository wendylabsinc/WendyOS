package camerapose

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func calibrationFixture(t *testing.T, cfg *Config, scope string) map[string]any {
	t.Helper()
	cfg.CalibrationBinding = &CalibrationBinding{HardwareID: "test-unit-1", Board: "seeed-xiao-esp32s3-sense", Sensor: "ov3660", Lens: "stock", CaptureMode: "full-frame-mirror0-flip0", Width: 64, Height: 48}
	camera := map[string]any{"board": cfg.CalibrationBinding.Board, "sensor": "ov3660", "lens": "stock", "capture_mode": cfg.CalibrationBinding.CaptureMode, "width": 64, "height": 48}
	if scope == "device" {
		camera["hardware_id"] = "test-unit-1"
	}
	p := map[string]any{
		"schema": "wendy.camera-intrinsics.v1", "id": "test-calibration", "name": "Synthetic test lens", "scope": scope, "origin": "simulated",
		"camera":     camera,
		"intrinsics": map[string]any{"fx": 50.0, "fy": 51.0, "cx": 32.0, "cy": 24.0, "distortion": []float64{.01, -.02, .001, -.002, .003}},
		"evidence":   map[string]any{"method": "opencv-checkerboard", "reference_device_ids": []string{"test-unit-1"}, "training_views": 12, "validation_views": 3, "training_rms_pixels": .15, "validation_rms_pixels": .2, "dataset_sha256": strings.Repeat("a", 64), "calibrated_at": "2026-11-22T12:00:00Z"},
	}
	cfg.CalibrationProfilePath = filepath.Join(t.TempDir(), "calibration.json")
	writeCalibrationFixture(t, cfg.CalibrationProfilePath, p)
	return p
}
func writeCalibrationFixture(t *testing.T, path string, p map[string]any) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func fixedLensResult(lens *Result) Result {
	r := result()
	r.FittedLens = 0
	r.FX = lens.FX
	r.FY = lens.FY
	r.CX = lens.CX
	r.CY = lens.CY
	r.Distortion = lens.Distortion
	return r
}

func TestConfiguredCalibrationUsedForBothFreshFrames(t *testing.T) {
	for _, scope := range []string{"model", "device"} {
		t.Run(scope, func(t *testing.T) {
			cfg, source := fixture(t)
			calibrationFixture(t, &cfg, scope)
			var modes []string
			cfg.solve = func(_ context.Context, _, _, mode string, lens *Result) (Result, error) {
				modes = append(modes, mode)
				if lens == nil || lens.FX != 50 || lens.FY != 51 || lens.Distortion[0] != .01 {
					t.Fatal("fixed calibration not supplied")
				}
				return fixedLensResult(lens), nil
			}
			c, err := New(cfg, source)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			initial := c.Snapshot()
			if initial.Calibration == nil || initial.Calibration.BindingSource != "operator" {
				t.Fatal("missing configured profile metadata")
			}
			initial.Calibration.Name = "mutated"
			// Changing the caller's binding after construction cannot change the loaded binding.
			cfg.CalibrationBinding.Width = 100
			if err = c.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := await(t, c)
			mode, quality := "assumed", "model_calibration"
			if scope == "device" {
				mode, quality = "measured", "measured"
			}
			if got.Pose == nil || !got.Fresh || got.Pose.Lens.Quality != quality || len(modes) != 2 || modes[0] != mode || modes[1] != mode {
				t.Fatalf("profile solve failed: %+v modes=%v", got, modes)
			}
			if got.Pose.Lens.Profile == nil || *got.Pose.Lens.Profile != *got.Calibration || got.Calibration.Name == "mutated" || got.Calibration.Origin != world.Simulated {
				t.Fatal("profile provenance lost or caller-mutated")
			}
			got.Pose.Lens.Profile.Name = "also mutated"
			if c.Snapshot().Pose.Lens.Profile.Name == "also mutated" {
				t.Fatal("snapshot exposes mutable provenance")
			}
			history, err := os.ReadFile(filepath.Join(cfg.WorkDir, "last-calibration.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved State
			if json.Unmarshal(history, &saved) != nil || saved.Pose.Lens.Profile == nil || saved.Pose.Lens.Profile.Scope != scope {
				t.Fatal("history lost calibration provenance")
			}
		})
	}
}

func TestInvalidCalibrationRejectedEvenWithoutSite(t *testing.T) {
	mutations := map[string]func(map[string]any, *Config){
		"missing binding":       func(_ map[string]any, c *Config) { c.CalibrationBinding = nil },
		"wrong sensor":          func(_ map[string]any, c *Config) { c.CalibrationBinding.Sensor = "ov2640" },
		"wrong lens":            func(_ map[string]any, c *Config) { c.CalibrationBinding.Lens = "wide-angle" },
		"wrong mode":            func(_ map[string]any, c *Config) { c.CalibrationBinding.CaptureMode = "cropped" },
		"wrong dimensions":      func(_ map[string]any, c *Config) { c.CalibrationBinding.Width = 128 },
		"wrong device":          func(_ map[string]any, c *Config) { c.CalibrationBinding.HardwareID = "other-unit" },
		"missing matrix centre": func(p map[string]any, _ *Config) { delete(p["intrinsics"].(map[string]any), "cx") },
		"too few distortion coefficients": func(p map[string]any, _ *Config) {
			p["intrinsics"].(map[string]any)["distortion"] = []float64{0, 0, 0, 0}
		},
		"too many distortion coefficients": func(p map[string]any, _ *Config) {
			p["intrinsics"].(map[string]any)["distortion"] = []float64{0, 0, 0, 0, 0, 0}
		},
		"bad focal length":   func(p map[string]any, _ *Config) { p["intrinsics"].(map[string]any)["fx"] = -1 },
		"unknown field":      func(p map[string]any, _ *Config) { p["guessed"] = true },
		"missing validation": func(p map[string]any, _ *Config) { p["evidence"].(map[string]any)["validation_views"] = 2 },
		"missing rms":        func(p map[string]any, _ *Config) { delete(p["evidence"].(map[string]any), "validation_rms_pixels") },
		"bad validation":     func(p map[string]any, _ *Config) { p["evidence"].(map[string]any)["validation_rms_pixels"] = 2.01 },
		"duplicate device evidence": func(p map[string]any, _ *Config) {
			p["evidence"].(map[string]any)["reference_device_ids"] = []string{"test-unit-1", "test-unit-1"}
		},
		"other device evidence": func(p map[string]any, _ *Config) {
			p["evidence"].(map[string]any)["reference_device_ids"] = []string{"other-unit"}
		},
		"invalid dataset digest": func(p map[string]any, _ *Config) { p["evidence"].(map[string]any)["dataset_sha256"] = "unknown" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cfg, s := fixture(t)
			p := calibrationFixture(t, &cfg, "device")
			mutate(p, &cfg)
			writeCalibrationFixture(t, cfg.CalibrationProfilePath, p)
			cfg.Helper = ""
			cfg.SiteDir = ""
			if _, err := New(cfg, s); err == nil {
				t.Fatal("invalid calibration accepted")
			}
		})
	}
	cfg, s := fixture(t)
	calibrationFixture(t, &cfg, "model")
	cfg.CalibrationProfilePath = ""
	cfg.Helper = ""
	cfg.SiteDir = ""
	if _, err := New(cfg, s); err == nil {
		t.Fatal("binding without profile accepted")
	}
}

type originOverrideSource struct {
	Source
	origin world.Origin
}

func (s originOverrideSource) State() stationarycamera.State {
	state := s.Source.State()
	r := *state.Latest
	r.Origin = s.origin
	state.Latest = &r
	return state
}
func TestSimulatedProfileNeverAuthorizesMeasuredInput(t *testing.T) {
	cfg, s := fixture(t)
	calibrationFixture(t, &cfg, "model")
	called := false
	cfg.solve = func(context.Context, string, string, string, *Result) (Result, error) {
		called = true
		return result(), nil
	}
	c, err := New(cfg, originOverrideSource{s, world.Measured})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Start(context.Background())
	got := await(t, c)
	if called || got.Pose != nil || got.Fresh || got.Status != "unavailable" {
		t.Fatal("simulated calibration used for physical image")
	}
}
func TestProfileMismatchAndChangeInvalidatePose(t *testing.T) {
	for _, change := range []string{"dimensions", "profile bytes"} {
		t.Run(change, func(t *testing.T) {
			cfg, s := fixture(t)
			calibrationFixture(t, &cfg, "model")
			cfg.solve = func(_ context.Context, _, _, _ string, lens *Result) (Result, error) {
				return fixedLensResult(lens), nil
			}
			c, err := New(cfg, s)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.Start(context.Background())
			if got := await(t, c); got.Pose == nil {
				t.Fatal("fixture not located")
			}
			if change == "dimensions" {
				s.mu.Lock()
				s.width = 128
				s.mu.Unlock()
			} else {
				before, _ := os.Stat(cfg.CalibrationProfilePath)
				data, _ := os.ReadFile(cfg.CalibrationProfilePath)
				data = []byte(strings.Replace(string(data), "Synthetic test lens", "Different test lens", 1))
				os.WriteFile(cfg.CalibrationProfilePath, data, 0600)
				os.Chtimes(cfg.CalibrationProfilePath, before.ModTime(), before.ModTime())
			}
			if got := c.Snapshot(); got.Pose != nil || got.Fresh {
				t.Fatal("changed calibration/image retained pose")
			}
			c.Start(context.Background())
			if got := await(t, c); got.Pose != nil || got.Fresh || got.Status != "unavailable" {
				t.Fatal("changed input silently reused old calibration")
			}
		})
	}
}
func TestSelectedCalibrationFailureDoesNotFallbackToGuessing(t *testing.T) {
	cfg, s := fixture(t)
	calibrationFixture(t, &cfg, "model")
	calls := 0
	cfg.solve = func(_ context.Context, _, _, mode string, lens *Result) (Result, error) {
		calls++
		if mode != "assumed" || lens == nil {
			t.Fatal("selected profile fell back to guessing")
		}
		r := fixedLensResult(lens)
		r.Success = false
		r.Reason = 2
		return r, nil
	}
	c, err := New(cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Start(context.Background())
	got := await(t, c)
	if calls != 1 || got.Pose != nil || got.Status != "no_match" {
		t.Fatalf("unexpected fallback: %d %+v", calls, got)
	}
}
func TestProfileLensCannotBeSilentlyRefitted(t *testing.T) {
	for _, badCall := range []int{1, 2} {
		t.Run(string(rune('0'+badCall)), func(t *testing.T) {
			cfg, s := fixture(t)
			calibrationFixture(t, &cfg, "model")
			calls := 0
			cfg.solve = func(_ context.Context, _, _, _ string, lens *Result) (Result, error) {
				calls++
				r := fixedLensResult(lens)
				if calls == badCall {
					r.FX += 1
				}
				return r, nil
			}
			c, err := New(cfg, s)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.Start(context.Background())
			if got := await(t, c); got.Pose != nil || got.Fresh {
				t.Fatal("refitted supplied lens published as calibrated")
			}
		})
	}
}
func TestDuplicateProfileFieldsRejected(t *testing.T) {
	cfg, s := fixture(t)
	calibrationFixture(t, &cfg, "model")
	b, _ := os.ReadFile(cfg.CalibrationProfilePath)
	b = []byte(strings.Replace(string(b), `"scope":"model"`, `"scope":"device","scope":"model"`, 1))
	os.WriteFile(cfg.CalibrationProfilePath, b, 0600)
	if _, err := New(cfg, s); err == nil {
		t.Fatal("ambiguous duplicate field accepted")
	}
}
func TestProfileChangeDuringSolveRejectsPublication(t *testing.T) {
	cfg, s := fixture(t)
	calibrationFixture(t, &cfg, "model")
	cfg.solve = func(_ context.Context, _, _, _ string, lens *Result) (Result, error) {
		os.WriteFile(cfg.CalibrationProfilePath, []byte("{}"), 0600)
		return fixedLensResult(lens), nil
	}
	c, err := New(cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Start(context.Background())
	if got := await(t, c); got.Pose != nil || got.Fresh {
		t.Fatal("changed profile published")
	}
}
