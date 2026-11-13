package board

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// installPaths redirects the package-level detection paths into a tempdir
// and creates any of the fixture files listed in `files`. Missing keys are
// left absent.
func installPaths(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	origTegra := tegraReleasePath
	origSoC := socFamilyPath
	origDT := deviceTreeDir
	t.Cleanup(func() {
		tegraReleasePath = origTegra
		socFamilyPath = origSoC
		deviceTreeDir = origDT
		resetForTest()
	})
	resetForTest()
	tegraReleasePath = filepath.Join(dir, "nv_tegra_release")
	socFamilyPath = filepath.Join(dir, "soc_family")
	deviceTreeDir = dir
	pathMap := map[string]string{
		"tegra":      tegraReleasePath,
		"soc":        socFamilyPath,
		"model":      filepath.Join(dir, "model"),
		"compatible": filepath.Join(dir, "compatible"),
		"serial":     filepath.Join(dir, "serial-number"),
	}
	for key, content := range files {
		path := pathMap[key]
		if path == "" {
			t.Fatalf("unknown fixture key %q", key)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func TestDetect_Jetson_TegraRelease(t *testing.T) {
	installPaths(t, map[string]string{
		"tegra": "# R36 (release), REVISION: 4.0\n",
		"model": "NVIDIA Jetson Orin Nano Developer Kit\x00",
	})
	got := Detect()
	if !got.IsJetson() {
		t.Fatalf("expected Jetson, got %+v", got)
	}
	if got.Model != "NVIDIA Jetson Orin Nano Developer Kit" {
		t.Errorf("model: %q", got.Model)
	}
}

func TestDetect_Jetson_SoCFamily(t *testing.T) {
	installPaths(t, map[string]string{
		"soc": "tegra\n",
	})
	got := Detect()
	if !got.IsJetson() {
		t.Fatalf("expected Jetson, got %+v", got)
	}
}

func TestDetect_RaspberryPi(t *testing.T) {
	installPaths(t, map[string]string{
		"model": "Raspberry Pi 5 Model B Rev 1.0\x00",
	})
	got := Detect()
	if !got.IsRaspberryPi() {
		t.Fatalf("expected RaspberryPi, got %+v", got)
	}
	if got.Model != "Raspberry Pi 5 Model B Rev 1.0" {
		t.Errorf("model: %q", got.Model)
	}
}

func TestDetect_Generic(t *testing.T) {
	installPaths(t, nil)
	got := Detect()
	if got.Kind != Generic {
		t.Fatalf("expected Generic, got %+v", got)
	}
	if got.IsJetson() || got.IsRaspberryPi() {
		t.Errorf("predicates wrong: %+v", got)
	}
}

func TestDetect_CachingAcrossCalls(t *testing.T) {
	installPaths(t, map[string]string{"soc": "tegra\n"})
	first := Detect()
	// Mutate the underlying file after the first call.
	if err := os.WriteFile(socFamilyPath, []byte("not-tegra"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	second := Detect()
	if first.Kind != second.Kind {
		t.Errorf("expected cached result, got first=%v second=%v", first.Kind, second.Kind)
	}
}

// Device-tree fixtures are written exactly as the kernel exposes them:
// NUL-terminated strings, NUL-separated lists. The Thor strings follow the
// L4T r38 tegra264 tree; no Thor unit was available to capture them from.
func TestDetect_DeviceTree(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		model      string
		compatible []string
		serial     string
		arch       string
	}{
		{
			name: "thor",
			files: map[string]string{
				"tegra":      "# R38 (release), REVISION: 2.1\n",
				"model":      "NVIDIA Jetson AGX Thor Developer Kit\x00",
				"compatible": "nvidia,p4071-0000+p3834-0008\x00nvidia,p3834-0008\x00nvidia,tegra264\x00",
				"serial":     "1424325050123\x00",
			},
			model:      "NVIDIA Jetson AGX Thor Developer Kit",
			compatible: []string{"nvidia,p4071-0000+p3834-0008", "nvidia,p3834-0008", "nvidia,tegra264"},
			serial:     "1424325050123",
			arch:       "sm_110",
		},
		{
			name: "orin",
			files: map[string]string{
				"model":      "NVIDIA Jetson AGX Orin Developer Kit\x00",
				"compatible": "nvidia,p3737-0000+p3701-0000\x00nvidia,p3701-0000\x00nvidia,tegra234\x00",
			},
			model:      "NVIDIA Jetson AGX Orin Developer Kit",
			compatible: []string{"nvidia,p3737-0000+p3701-0000", "nvidia,p3701-0000", "nvidia,tegra234"},
			arch:       "sm_87",
		},
		{
			name: "raspberry pi",
			files: map[string]string{
				"model":      "Raspberry Pi 5 Model B Rev 1.0\x00",
				"compatible": "raspberrypi,5-model-b\x00brcm,bcm2712\x00",
				"serial":     "e3d1c2b4a5f60718\x00",
			},
			model:      "Raspberry Pi 5 Model B Rev 1.0",
			compatible: []string{"raspberrypi,5-model-b", "brcm,bcm2712"},
			serial:     "e3d1c2b4a5f60718",
		},
		{name: "no device tree"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installPaths(t, tt.files)
			got := Detect()
			if got.Model != tt.model || got.SerialNumber != tt.serial || !slices.Equal(got.Compatible, tt.compatible) {
				t.Errorf("got model=%q compatible=%q serial=%q, want %q %q %q",
					got.Model, got.Compatible, got.SerialNumber, tt.model, tt.compatible, tt.serial)
			}
			if arch := got.NvidiaGPUArch(); arch != tt.arch {
				t.Errorf("NvidiaGPUArch() = %q, want %q", arch, tt.arch)
			}
		})
	}
}
