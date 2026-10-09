package commands

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// This metadata supplies labels and flags. appconfig remains the authority for
// manifest validation; the same fields drive both direct commands and forms.
type projectField struct {
	key, flag, label, hint, kind, defaultValue string
	required                                   bool
}

type projectFeature struct {
	name, label, description string
	fields                   []projectField
}

func projectCatalog() []projectFeature {
	features := make([]projectFeature, 0, len(appconfig.ValidEntitlementTypes)+1)
	labels := map[string]string{
		"network": "Network access", "camera": "Camera access", "persist": "Persistent storage",
		"gpu": "GPU access", "npu": "NPU access", "audio": "Microphone and speakers",
		"i2c": "I2C bus", "gpio": "GPIO pins", "spi": "SPI bus", "usb": "USB access",
		"serial": "USB serial device", "http": "Web interface", "mcp": "MCP server",
		"input": "Input devices", "display": "Display access", "bluetooth": "Bluetooth access",
		"episode-write": "Episode recording", "notifications": "Notifications", "admin": "Agent administration",
		"build": "Container builds", "video": "Video access (deprecated; use camera)",
	}
	for _, name := range appconfig.ValidEntitlementTypes {
		features = append(features, projectFeature{name: name, label: labels[name], description: entitlementDescriptions[name], fields: projectFields(name)})
	}
	return append(features, projectFeature{name: "ros2", label: "ROS 2", description: "Configure middleware, discovery, and domain ID", fields: projectFields("ros2")})
}

func projectFields(name string) []projectField {
	switch name {
	case "app":
		return []projectField{
			{"appId", "app-id", "App ID", "Letters, digits, dots, underscores, or hyphens", "string", "", true},
			{"language", "language", "Language", "For example python, swift, rust, or cpp", "string", "", false},
			{"platform", "platform", "Platform", "linux, darwin, or wendy-lite", "string", "linux", false},
			{"version", "version", "Version", "For example 0.1.0", "string", "", false},
		}
	case "persist":
		return []projectField{
			{"name", "name", "Storage namespace", "Apps with this namespace share data", "string", "", true},
			{"path", "path", "Mount path", "Absolute path inside the container", "string", "/data", true},
		}
	case "http", "mcp":
		return []projectField{{"port", "port", "Port", "A port between 1 and 65535", "int", "", true}}
	case "i2c":
		return []projectField{{"device", "bus", "I2C bus", "Bare device name, for example i2c-1", "string", "i2c-1", true}}
	case "serial":
		return []projectField{{"device", "serial-device", "Serial device", "Bare USB tty name, for example ttyACM0 or ttyUSB0", "string", "ttyACM0", true}}
	case "gpio":
		return []projectField{{"pins", "pins", "GPIO pins", "Comma-separated pins, or empty for all GPIO chips", "pins", "", false}}
	case "network":
		return []projectField{
			{"mode", "mode", "Network mode", "bridge: outbound internet; host: host network; mesh: mesh peers; none: no network; host-admin: host network administration", "string", "bridge", false},
			{"serviceCIDR", "service-cidr", "Mesh service CIDR", "Required only for mesh mode, for example 10.42.0.0/16", "string", "", false},
			{"ports", "ports", "Mesh port mappings", "JSON array, for example [{\"host\":8080,\"container\":8080}]; empty for none", "array", "", false},
		}
	case "bluetooth":
		return []projectField{{"mode", "mode", "Bluetooth mode", "Optional Bluetooth access mode", "string", "", false}}
	case "camera", "video":
		return []projectField{{"allowlist", "allowlist", "Allowed cameras", "Comma-separated camera names or paths; empty for all", "list", "", false}}
	case "episode-write":
		return []projectField{{"streams", "streams", "Recording streams", "JSON object of recording streams; use edit --raw for larger configurations", "object", "", false}}
	case "ros2":
		return []projectField{
			{"domainId", "domain-id", "Domain ID", "0 to 232; empty derives a stable ID from appId", "int", "", false},
			{"rmw", "rmw", "Middleware", "cyclonedds, fastrtps, connextdds, or gurumdds", "string", appconfig.ROS2DefaultRMW, false},
			{"distro", "distro", "ROS distribution", "For example humble or jazzy", "string", appconfig.ROS2DefaultDistro, false},
			{"discoveryScope", "discovery-scope", "Discovery scope", "app: this app group; host: device network", "string", "app", false},
		}
	}
	return nil
}

func findProjectFeature(name string) (projectFeature, bool) {
	if name == "storage" {
		name = "persist"
	}
	if name == "app" {
		return projectFeature{name: "app", label: "App settings", fields: projectFields("app")}, true
	}
	for _, f := range projectCatalog() {
		if f.name == name {
			return f, true
		}
	}
	return projectFeature{}, false
}

func projectFieldFlags(cmd *cobra.Command) {
	features := projectCatalog()
	if cmd.Name() == "edit" {
		features = append(features, projectFeature{fields: projectFields("app")})
	}
	for _, f := range features {
		for _, field := range f.fields {
			if cmd.Flags().Lookup(field.flag) == nil {
				cmd.Flags().String(field.flag, "", field.label+". "+field.hint)
			}
		}
	}
}

func parseProjectField(field projectField, value string) (any, error) {
	if value == "" {
		if field.required {
			return nil, fmt.Errorf("%s is required (--%s)", field.label, field.flag)
		}
		return nil, nil
	}
	switch field.kind {
	case "int":
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer", field.label)
		}
		if field.key == "port" && (n < 1 || n > 65535) {
			return nil, fmt.Errorf("port must be between 1 and 65535")
		}
		if field.key == "domainId" && (n < 0 || n > 232) {
			return nil, fmt.Errorf("domain ID must be between 0 and 232")
		}
		return json.Number(strconv.Itoa(n)), nil
	case "pins":
		pins, err := parsePins(value)
		if err != nil {
			return nil, err
		}
		for _, pin := range pins {
			if pin < 0 {
				return nil, fmt.Errorf("GPIO pins must not be negative")
			}
		}
		return pins, nil
	case "list":
		values := strings.Split(value, ",")
		for i := range values {
			values[i] = strings.TrimSpace(values[i])
			if values[i] == "" {
				return nil, fmt.Errorf("%s must not contain empty entries", field.label)
			}
		}
		return values, nil
	case "array", "object":
		var parsed any
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		if !json.Valid([]byte(value)) || decoder.Decode(&parsed) != nil {
			return nil, fmt.Errorf("%s must be valid JSON", field.label)
		}
		_, array := parsed.([]any)
		_, object := parsed.(map[string]any)
		if field.kind == "array" && !array || field.kind == "object" && !object {
			return nil, fmt.Errorf("%s must be a JSON %s", field.label, field.kind)
		}
		return parsed, nil
	}
	return value, nil
}

func projectFieldValue(field projectField, obj projectObject) string {
	v, exists := obj[field.key]
	if !exists {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	data, _ := json.Marshal(v)
	if field.kind == "pins" || field.kind == "list" {
		var items []any
		_ = json.Unmarshal(data, &items)
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = fmt.Sprint(item)
		}
		return strings.Join(parts, ",")
	}
	return string(data)
}

func checkProjectFieldFlags(cmd *cobra.Command, f projectFeature) (bool, error) {
	allowed := map[string]bool{}
	for _, field := range f.fields {
		allowed[field.flag] = true
	}
	changed := false
	var invalid []string
	features := append(projectCatalog(), projectFeature{fields: projectFields("app")})
	for _, feature := range features {
		for _, field := range feature.fields {
			if cmd.Flags().Changed(field.flag) {
				changed = true
				if !allowed[field.flag] {
					invalid = append(invalid, "--"+field.flag)
				}
			}
		}
	}
	if len(invalid) > 0 {
		slices.Sort(invalid)
		return changed, fmt.Errorf("%s does not use %s", f.name, strings.Join(slices.Compact(invalid), ", "))
	}
	return changed, nil
}
