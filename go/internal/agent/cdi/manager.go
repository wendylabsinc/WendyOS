package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultCDISpecPath = "/etc/cdi"

// Manager loads and manages CDI specifications.
type Manager struct {
	specPath string
}

// NewManager creates a new CDI Manager with the default spec path (/etc/cdi).
func NewManager() *Manager {
	return &Manager{specPath: defaultCDISpecPath}
}

// NewManagerWithPath creates a new CDI Manager with a custom spec path.
func NewManagerWithPath(specPath string) *Manager {
	return &Manager{specPath: specPath}
}

// GetAvailableCDIDevices scans the CDI spec directory for JSON specs and returns
// all available CDI device identifiers.
func (m *Manager) GetAvailableCDIDevices() ([]CDIDeviceInfo, error) {
	var devices []CDIDeviceInfo

	info, err := os.Stat(m.specPath)
	if err != nil || !info.IsDir() {
		return devices, nil
	}

	entries, err := os.ReadDir(m.specPath)
	if err != nil {
		return nil, fmt.Errorf("reading CDI spec directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		filePath := filepath.Join(m.specPath, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var spec CDISpecification
		if err := json.Unmarshal(data, &spec); err != nil {
			continue
		}

		for _, device := range spec.Devices {
			identifier := fmt.Sprintf("%s=%s", spec.Kind, device.Name)
			var devicePaths []string
			for _, node := range device.ContainerEdits.DeviceNodes {
				devicePaths = append(devicePaths, node.Path)
			}

			devices = append(devices, CDIDeviceInfo{
				Identifier:  identifier,
				Category:    extractCategoryFromDeviceName(device.Name),
				Description: device.Name,
				DevicePaths: devicePaths,
			})
		}
	}

	return devices, nil
}

// GetCDIDevices returns CDI devices filtered by the given categories.
func (m *Manager) GetCDIDevices(categories []string) ([]CDIDeviceInfo, error) {
	allDevices, err := m.GetAvailableCDIDevices()
	if err != nil {
		return nil, err
	}

	var filtered []CDIDeviceInfo
	for _, device := range allDevices {
		for _, category := range categories {
			if strings.EqualFold(device.Category, category) {
				filtered = append(filtered, device)
				break
			}
		}
	}

	return filtered, nil
}

// LoadNVIDIACDISpec loads the NVIDIA CDI spec from YAML.
// It tries /etc/cdi/nvidia.yaml first, then /var/run/cdi/nvidia.yaml.
func (m *Manager) LoadNVIDIACDISpec() (*CDISpecification, error) {
	possiblePaths := []string{
		filepath.Join(m.specPath, "nvidia.yaml"),
		"/var/run/cdi/nvidia.yaml",
	}

	var specPath string
	for _, path := range possiblePaths {
		if _, err := os.Stat(path); err == nil {
			specPath = path
			break
		}
	}

	if specPath == "" {
		return nil, &CDIError{
			Message: fmt.Sprintf("CDI spec not found at %s", strings.Join(possiblePaths, ", ")),
		}
	}

	data, err := os.ReadFile(specPath)
	if err != nil {
		return nil, &CDIError{
			Message: fmt.Sprintf("cannot read CDI spec at %s: %v", specPath, err),
		}
	}

	var spec CDISpecification
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parsing NVIDIA CDI YAML spec: %w", err)
	}

	return &spec, nil
}

// extractCategoryFromDeviceName extracts the category prefix from a device name
// like "gpio-gpiochip0" -> "GPIO".
func extractCategoryFromDeviceName(deviceName string) string {
	if idx := strings.Index(deviceName, "-"); idx >= 0 {
		return strings.ToUpper(deviceName[:idx])
	}
	return "UNKNOWN"
}
