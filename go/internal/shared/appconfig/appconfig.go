// Package appconfig provides parsing and validation of wendy.json application configuration files.
package appconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// EntitlementType enumerates the supported entitlement types.
const (
	EntitlementNetwork   = "network"
	EntitlementBluetooth = "bluetooth"
	EntitlementVideo     = "video"
	EntitlementGPU       = "gpu"
	EntitlementPersist   = "persist"
	EntitlementAudio     = "audio"
	EntitlementCamera    = "camera"
	EntitlementUSB       = "usb"
	EntitlementI2C       = "i2c"
	EntitlementGPIO      = "gpio"
	EntitlementSPI       = "spi"
	EntitlementInput     = "input"
)

// ValidEntitlementTypes is the set of all recognized entitlement type strings.
var ValidEntitlementTypes = []string{
	EntitlementNetwork,
	EntitlementBluetooth,
	EntitlementVideo,
	EntitlementGPU,
	EntitlementPersist,
	EntitlementAudio,
	EntitlementCamera,
	EntitlementUSB,
	EntitlementI2C,
	EntitlementGPIO,
	EntitlementSPI,
	EntitlementInput,
}

var deprecatedEntitlementReplacements = map[string]string{
	EntitlementVideo: EntitlementCamera,
}

// allowedKeys maps each entitlement type to the set of JSON keys that are valid for it.
var allowedKeys = map[string][]string{
	EntitlementNetwork:   {"type", "mode"},
	EntitlementBluetooth: {"type", "mode"},
	EntitlementVideo:     {"type", "mode", "allowlist"},
	EntitlementGPU:       {"type"},
	EntitlementPersist:   {"type", "name", "path"},
	EntitlementAudio:     {"type"},
	EntitlementCamera:    {"type", "mode", "allowlist"},
	EntitlementUSB:       {"type"},
	EntitlementI2C:       {"type", "device"},
	EntitlementGPIO:      {"type", "pins"},
	EntitlementSPI:       {"type"},
	EntitlementInput:     {"type"},
}

// Platform constants identify the target hardware family.
const (
	PlatformWendyOS   = "wendyos"
	PlatformWendyLite = "wendy-lite"
)

// AppConfig represents the wendy.json application configuration.
type AppConfig struct {
	AppID        string           `json:"appId"`
	Version      string           `json:"version,omitempty"`
	Platform     string           `json:"platform,omitempty"`
	Language     string           `json:"language,omitempty"`
	Entitlements []Entitlement    `json:"entitlements,omitempty"`
	Readiness    *ReadinessConfig `json:"readiness,omitempty"`
	Hooks        *HooksConfig     `json:"hooks,omitempty"`
	Python       *PythonConfig    `json:"python,omitempty"`
	Debug        bool             `json:"debug,omitempty"`
}

// ReadinessConfig defines a probe the CLI uses to determine when the app is ready.
type ReadinessConfig struct {
	TCPSocket      *TCPSocketProbe `json:"tcpSocket,omitempty"`
	TimeoutSeconds int             `json:"timeoutSeconds,omitempty"` // Default 30
}

// TCPSocketProbe checks readiness by dialing a TCP port.
type TCPSocketProbe struct {
	Port int `json:"port"`
}

// HooksConfig holds optional lifecycle hook commands.
type HooksConfig struct {
	PostStart *HookCommand `json:"postStart,omitempty"`
}

// HookCommand holds CLI and agent-side commands for a lifecycle hook.
type HookCommand struct {
	CLI   string `json:"cli,omitempty"`   // Command to run on the developer's machine
	Agent string `json:"agent,omitempty"` // Command to run on the device
}

// PythonConfig holds Python-specific configuration.
type PythonConfig struct {
	SourceRoot string `json:"sourceRoot,omitempty"`
}

// Entitlement represents a single entitlement entry in wendy.json.
type Entitlement struct {
	Type   string `json:"type"`
	Mode   string `json:"mode,omitempty"`   // Network, Bluetooth, Video
	Name   string `json:"name,omitempty"`   // Persist
	Path   string `json:"path,omitempty"`   // Persist
	Device string `json:"device,omitempty"` // I2C
	Pins   []int  `json:"pins,omitempty"`   // GPIO
}

// DeprecatedEntitlementReplacement reports the preferred replacement for a deprecated entitlement type.
func DeprecatedEntitlementReplacement(entType string) (string, bool) {
	replacement, ok := deprecatedEntitlementReplacements[entType]
	return replacement, ok
}

// HasEntitlement reports whether the config contains an entitlement of the given type.
func (c *AppConfig) HasEntitlement(entType string) bool {
	for _, e := range c.Entitlements {
		if e.Type == entType {
			return true
		}
	}
	return false
}

// LoadFromFile reads and parses a wendy.json file at the given path.
func LoadFromFile(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading wendy.json: %w", err)
	}
	return LoadFromBytes(data)
}

// LoadFromBytes parses a wendy.json from raw bytes.
func LoadFromBytes(data []byte) (*AppConfig, error) {
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing wendy.json: %w", err)
	}
	return &cfg, nil
}

// Validate checks the AppConfig for required fields and valid entitlement types.
func (c *AppConfig) Validate() error {
	if c.AppID == "" {
		return fmt.Errorf("appId is required")
	}

	for i, e := range c.Entitlements {
		if e.Type == "" {
			return fmt.Errorf("entitlement[%d]: type is required", i)
		}
		if !slices.Contains(ValidEntitlementTypes, e.Type) {
			return fmt.Errorf("entitlement[%d]: unknown type %q", i, e.Type)
		}

		switch e.Type {
		case EntitlementNetwork:
			if e.Mode != "" && e.Mode != "host" && e.Mode != "none" {
				return fmt.Errorf("entitlement[%d]: network mode must be \"host\" or \"none\", got %q", i, e.Mode)
			}
		case EntitlementPersist:
			if e.Name == "" {
				return fmt.Errorf("entitlement[%d]: persist entitlement requires a name", i)
			}
			if e.Path == "" {
				return fmt.Errorf("entitlement[%d]: persist entitlement requires a path", i)
			}
		case EntitlementI2C:
			if e.Device == "" {
				return fmt.Errorf("entitlement[%d]: i2c entitlement requires a device", i)
			}
		case EntitlementGPIO:
			// Pins are optional; omitting them grants access to all GPIO chips.
		}
	}

	if c.Readiness != nil {
		if c.Readiness.TCPSocket != nil {
			port := c.Readiness.TCPSocket.Port
			if port < 1 || port > 65535 {
				return fmt.Errorf("readiness.tcpSocket.port must be between 1 and 65535, got %d", port)
			}
		}
		if c.Readiness.TimeoutSeconds < 0 {
			return fmt.Errorf("readiness.timeoutSeconds must not be negative, got %d", c.Readiness.TimeoutSeconds)
		}
	}

	return nil
}

// ValidateJSON checks raw JSON data for unknown keys in entitlements and returns warnings.
// Call this after decoding to detect potential typos or invalid configuration.
func ValidateJSON(data []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	entRaw, ok := raw["entitlements"]
	if !ok {
		return nil
	}

	var entitlements []map[string]json.RawMessage
	if err := json.Unmarshal(entRaw, &entitlements); err != nil {
		return nil
	}

	var warnings []string
	for i, ent := range entitlements {
		typeRaw, ok := ent["type"]
		if !ok {
			continue
		}
		var entType string
		if err := json.Unmarshal(typeRaw, &entType); err != nil {
			continue
		}

		if replacement, ok := DeprecatedEntitlementReplacement(entType); ok {
			warnings = append(warnings, fmt.Sprintf(
				"entitlement[%d]: %q is deprecated; use %q instead",
				i, entType, replacement,
			))
		}

		allowed, ok := allowedKeys[entType]
		if !ok {
			continue
		}

		allowedSet := make(map[string]bool, len(allowed))
		for _, k := range allowed {
			allowedSet[k] = true
		}

		var unknown []string
		for k := range ent {
			if !allowedSet[k] {
				unknown = append(unknown, k)
			}
		}

		if len(unknown) > 0 {
			sort.Strings(unknown)
			sortedAllowed := make([]string, len(allowed))
			copy(sortedAllowed, allowed)
			sort.Strings(sortedAllowed)
			warnings = append(warnings, fmt.Sprintf(
				"Unknown key(s) in entitlement[%d] (%s): %s. Allowed keys are: %s",
				i, entType,
				strings.Join(unknown, ", "),
				strings.Join(sortedAllowed, ", "),
			))
		}
	}

	return warnings
}
