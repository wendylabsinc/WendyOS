package cdi

import (
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

func NewManager() *Manager {
	return &Manager{specPath: defaultCDISpecPath}
}

// LoadNVIDIACDISpec loads the NVIDIA CDI spec from YAML and reports which file
// it came from. It tries /var/run/cdi/nvidia.yaml first, then
// /etc/cdi/nvidia.yaml.
//
// The path is returned rather than kept private because the two candidates have
// very different lifetimes — /var/run is tmpfs, regenerated every boot, while
// /etc/cdi persists — and a stale file in the first silently shadows the
// second. When GPU provisioning goes wrong, which of the two was in play is the
// first thing worth knowing.
func (m *Manager) LoadNVIDIACDISpec() (*CDISpecification, string, error) {
	possiblePaths := []string{
		"/var/run/cdi/nvidia.yaml",
		filepath.Join(m.specPath, "nvidia.yaml"),
	}

	var specPath string
	for _, path := range possiblePaths {
		if _, err := os.Stat(path); err == nil {
			specPath = path
			break
		}
	}

	if specPath == "" {
		return nil, "", &CDIError{
			Message: fmt.Sprintf("CDI spec not found at %s", strings.Join(possiblePaths, ", ")),
		}
	}

	data, err := os.ReadFile(specPath)
	if err != nil {
		return nil, specPath, &CDIError{
			Message: fmt.Sprintf("cannot read CDI spec at %s: %v", specPath, err),
		}
	}

	var spec CDISpecification
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, specPath, fmt.Errorf("parsing NVIDIA CDI YAML spec at %s: %w", specPath, err)
	}

	return &spec, specPath, nil
}
