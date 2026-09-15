// DRM vendor naming moved to gpudiscovery (see discovery_test.go); the Adreno
// arch probe stays here because it reads platform devices, not DRM nodes.
package services

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectAdrenoArch(t *testing.T) {
	// The device-tree property is a NUL-separated string list, exactly as the
	// Dragonwing reports it.
	for name, tc := range map[string]struct {
		device     string
		compatible string
		want       string
	}{
		"adreno 623": {"3d00000.gpu", "qcom,adreno-623.0\x00qcom,adreno\x00", "a623"},
		"adreno 650": {"5000000.gpu", "qcom,adreno-650.2\x00qcom,adreno\x00", "a650"},
		// The GMU sits alongside the GPU and must not be mistaken for it.
		"not a gpu suffix": {"3d6a000.gmu", "qcom,adreno-gmu-623.0\x00", ""},
		"unrelated":        {"1000000.gpu", "arm,mali-t760\x00", ""},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, tc.device, "of_node")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "compatible"), []byte(tc.compatible), 0o644); err != nil {
				t.Fatal(err)
			}
			old := platformDevicesRoot
			platformDevicesRoot = root
			defer func() { platformDevicesRoot = old }()

			if got := detectAdrenoArch(); got != tc.want {
				t.Errorf("detectAdrenoArch() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectAdrenoArchMissingSysfs(t *testing.T) {
	old := platformDevicesRoot
	platformDevicesRoot = filepath.Join(t.TempDir(), "absent")
	defer func() { platformDevicesRoot = old }()
	if got := detectAdrenoArch(); got != "" {
		t.Errorf("detectAdrenoArch() = %q, want empty", got)
	}
}

func TestDetectAdrenoArchIgnoresChipIDCompatible(t *testing.T) {
	// Some device trees name the GPU by chip id rather than version; parsing
	// "qcom,adreno-43050a01" as a version yields a nonsense "a43050".
	for name, tc := range map[string]struct{ compatible, want string }{
		"version form": {"qcom,adreno-623.0\x00qcom,adreno\x00", "a623"},
		"chip id form": {"qcom,adreno-43050a01\x00qcom,adreno\x00", ""},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "3d00000.gpu", "of_node")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "compatible"), []byte(tc.compatible), 0o644); err != nil {
				t.Fatal(err)
			}
			old := platformDevicesRoot
			platformDevicesRoot = root
			defer func() { platformDevicesRoot = old }()
			if got := detectAdrenoArch(); got != tc.want {
				t.Errorf("detectAdrenoArch() = %q, want %q", got, tc.want)
			}
		})
	}
}
