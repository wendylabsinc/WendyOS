package appconfig

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// MCUConfig names the Arduino sketch that `wendy run` compiles and flashes to
// the board's microcontroller before the app starts.
type MCUConfig struct {
	Sketch  string `json:"sketch"`
	Profile string `json:"profile,omitempty"`
}

var mcuProfilePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func validateMCU(m *MCUConfig) error {
	if m == nil {
		return nil
	}
	if m.Sketch == "" {
		return fmt.Errorf("mcu.sketch is required: the sketch directory, relative to the project")
	}
	clean := path.Clean(strings.ReplaceAll(m.Sketch, "\\", "/"))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("mcu.sketch must be a directory inside the project, got %q", m.Sketch)
	}
	if m.Profile != "" && !mcuProfilePattern.MatchString(m.Profile) {
		return fmt.Errorf("mcu.profile must use letters, digits, '.', '_' or '-', got %q", m.Profile)
	}
	return nil
}
