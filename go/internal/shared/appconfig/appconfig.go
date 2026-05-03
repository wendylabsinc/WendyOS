// Package appconfig provides parsing and validation of wendy.json application configuration files.
package appconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	EntitlementMCP       = "mcp"
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
	EntitlementMCP,
}

var deprecatedEntitlementReplacements = map[string]string{
	EntitlementVideo: EntitlementCamera,
}

// allowedKeys maps each entitlement type to the set of JSON keys that are valid for it.
var allowedKeys = map[string][]string{
	EntitlementNetwork:   {"type", "mode", "ports"},
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
	EntitlementMCP:       {"type", "port"},
}

// Platform constants identify the target hardware family.
const (
	PlatformWendyOS   = "wendyos"
	PlatformWendyLite = "wendy-lite"
)

// FileSyncEntry describes a file or directory to sync to the device's app
// working directory before the app starts. Path is relative to wendy.json.
// To is the destination path relative to the app working directory; it
// defaults to Path (with any leading ./ stripped) when omitted.
type FileSyncEntry struct {
	Path string `json:"path"`
	To   string `json:"to,omitempty"`
}

// RunConfig holds runtime configuration applied when the app is started.
type RunConfig struct {
	Args []string `json:"args,omitempty"`
}

// AppConfig represents the wendy.json application configuration.
type AppConfig struct {
	AppID        string           `json:"appId"`
	Version      string           `json:"version,omitempty"`
	Platform     string           `json:"platform,omitempty"`
	Language     string           `json:"language,omitempty"`
	Xcode        *XcodeConfig     `json:"xcode,omitempty"`
	Run          *RunConfig       `json:"run,omitempty"`
	Entitlements []Entitlement    `json:"entitlements,omitempty"`
	Readiness    *ReadinessConfig `json:"readiness,omitempty"`
	Hooks        *HooksConfig     `json:"hooks,omitempty"`
	Python       *PythonConfig    `json:"python,omitempty"`
	Debug        bool             `json:"debug,omitempty"`
	Files        []FileSyncEntry  `json:"files,omitempty"`
}

// XcodeConfig holds Xcode-specific build settings.
type XcodeConfig struct {
	Scheme string `json:"scheme,omitempty"`
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

// PostStartAgentHookMetadataKey carries hooks.postStart.agent on start RPCs
// that should run the agent-side postStart hook.
const PostStartAgentHookMetadataKey = "wendy-post-start-agent-command"

// HookCommand holds CLI and agent-side commands for a lifecycle hook.
type HookCommand struct {
	CLI   string `json:"cli,omitempty"`   // Command to run on the developer's machine
	Agent string `json:"agent,omitempty"` // Command to run on the device
}

// PythonConfig holds Python-specific configuration.
type PythonConfig struct {
	SourceRoot string `json:"sourceRoot,omitempty"`
}

// PortMapping maps a host port to a container port for network entitlements.
type PortMapping struct {
	Host      uint16 `json:"host"`
	Container uint16 `json:"container"`
}

// Entitlement represents a single entitlement entry in wendy.json.
type Entitlement struct {
	Type   string        `json:"type"`
	Mode   string        `json:"mode,omitempty"`   // Network, Bluetooth, Video
	Name   string        `json:"name,omitempty"`   // Persist
	Path   string        `json:"path,omitempty"`   // Persist
	Device string        `json:"device,omitempty"` // I2C
	Pins   []int         `json:"pins,omitempty"`   // GPIO
	Ports  []PortMapping `json:"ports,omitempty"`  // Network
	Port   int           `json:"port,omitempty"`   // MCP
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
			if !filepath.IsAbs(e.Path) {
				return fmt.Errorf("entitlement[%d]: persist path must be absolute, got %q", i, e.Path)
			}
			if containsDotDot(e.Path) {
				return fmt.Errorf("entitlement[%d]: persist path must not contain '..' components", i)
			}
		case EntitlementI2C:
			if e.Device == "" {
				return fmt.Errorf("entitlement[%d]: i2c entitlement requires a device", i)
			}
			if !isValidI2CDevice(e.Device) {
				return fmt.Errorf("entitlement[%d]: i2c device must be in i2c-N format, got %q", i, e.Device)
			}
		case EntitlementGPIO:
			// Pins are optional; omitting them grants access to all GPIO chips.
		case EntitlementMCP:
			if e.Port < 1 || e.Port > 65535 {
				return fmt.Errorf("entitlement[%d]: mcp port must be between 1 and 65535, got %d", i, e.Port)
			}
		}
	}

	mcpCount := 0
	for _, e := range c.Entitlements {
		if e.Type == EntitlementMCP {
			mcpCount++
		}
	}
	if mcpCount > 1 {
		return fmt.Errorf("at most one mcp entitlement is allowed, found %d", mcpCount)
	}

	for i, f := range c.Files {
		if f.Path == "" {
			return fmt.Errorf("files[%d]: path is required", i)
		}
		if strings.HasPrefix(f.Path, "/") {
			return fmt.Errorf("files[%d]: path must not be absolute", i)
		}
		if containsDotDot(f.Path) {
			return fmt.Errorf("files[%d]: path must not contain '..' components", i)
		}
		if f.To != "" {
			if strings.HasPrefix(f.To, "/") {
				return fmt.Errorf("files[%d]: to must not be absolute", i)
			}
			if containsDotDot(f.To) {
				return fmt.Errorf("files[%d]: to must not contain '..' components", i)
			}
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

// containsDotDot reports whether p has a path component equal to "..".
func containsDotDot(p string) bool {
	for _, component := range strings.Split(p, "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

// isValidI2CDevice reports whether device is a safe I2C device name (i2c-N).
func isValidI2CDevice(device string) bool {
	if !strings.HasPrefix(device, "i2c-") {
		return false
	}
	suffix := device[len("i2c-"):]
	if suffix == "" {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
