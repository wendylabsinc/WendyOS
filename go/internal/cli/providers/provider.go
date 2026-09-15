// Package providers defines the pluggable device provider system for building
// and running apps on different targets (local, Docker, ADB, WASM, etc.).
package providers

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// DeviceProvider is implemented by each target backend (local, Docker, ADB, etc.).
type DeviceProvider interface {
	// Key returns a unique short identifier for this provider (e.g. "local", "docker").
	Key() string
	// DisplayName returns a human-friendly name shown in the CLI.
	DisplayName() string
	// IsAvailable reports whether this provider's toolchain is installed.
	IsAvailable(ctx context.Context) bool
	// CheckRequirements returns a detailed error if prerequisites are missing.
	CheckRequirements(ctx context.Context) error
	// DiscoverDevices returns the devices currently reachable through this
	// provider. It fits a scan that has an end — enumerating USB devices,
	// listing the local Docker containers — where "the scan is finished" is a
	// well-defined moment, and the returned set is complete.
	//
	// mDNS and Bluetooth have no such moment: the scan runs until it is
	// stopped, and nothing announces that the last device has been seen. A
	// provider on those transports has nothing to define when this method
	// should return, so it simply waits some time before returning, giving
	// devices a chance to get discovered — and accepts that a slow responder
	// is missed. Such a provider can also implement ContinuousDiscoverer,
	// which drops that arbitrary wait.
	DiscoverDevices(ctx context.Context) ([]models.ExternalDevice, error)
	// SupportedBuildTypes returns the build type keys (e.g. "docker", "swift")
	// that this provider can handle. Used to filter the build-type picker.
	SupportedBuildTypes() []string
	// CanBuild reports whether this provider can build the project at projectPath.
	CanBuild(projectPath string) bool
	// Build compiles the project for the given device and returns a BuiltApp handle.
	Build(ctx context.Context, device models.ExternalDevice, projectPath, projectType, product string, debug bool) (*BuiltApp, error)
	// Run starts the built application, streaming output to the channel until done.
	Run(ctx context.Context, app *BuiltApp, detach bool, output chan<- RunOutput) error
	// Stop terminates a running application.
	Stop(ctx context.Context, app *BuiltApp) error
	// GetDeviceInfo queries the device for identifying information.
	// Providers that cannot report device info return (nil, nil).
	GetDeviceInfo(ctx context.Context, device models.ExternalDevice) (*ProviderDeviceInfo, error)
}

// ProviderDeviceInfo describes a device as reported by its provider.
type ProviderDeviceInfo struct {
	AgentVersion     string
	OS               string
	OSVersion        string
	CPUArchitecture  string // e.g. "riscv" for wendy-lite ESP32 boards
	DeviceType       string // e.g. the SoC/IDF target name for wendy-lite
	WasmAppSupport   bool
	NativeAppSupport bool
}

// BuiltApp is the result of a successful Build. The opaque Context field
// carries provider-specific artifacts (binary path, container ID, etc.).
type BuiltApp struct {
	ProviderKey  string
	Device       models.ExternalDevice
	AppName      string
	Entitlements []appconfig.Entitlement // from wendy.json; may be nil for Compose projects
	Context      interface{}             // provider-specific build artifact
}

// RunOutputType classifies a line of output from a running app.
type RunOutputType int

// Provider key constants for the built-in providers.
const (
	ProviderKeyAppleContainer = "apple-container"
	ProviderKeyDocker         = "docker"
	ProviderKeyLocal          = "local"
)

// LocalProviderKeys are the providers whose "devices" are really this computer
// or a local container runtime (the local machine, Docker/OrbStack, Apple
// Container) rather than a separate WendyOS device. The interactive device
// picker and `wendy discover` hide these from the default device list unless
// ShowLocalDevices reports true. Real external hardware providers (e.g.
// android, wendy-lite) are not local and are always shown.
func LocalProviderKeys() []string {
	return []string{ProviderKeyLocal, ProviderKeyDocker, ProviderKeyAppleContainer}
}

// IsLocalProviderKey reports whether key names a local run target (see
// LocalProviderKeys).
func IsLocalProviderKey(key string) bool {
	return slices.Contains(LocalProviderKeys(), key)
}

// ShowLocalDevicesEnv is the environment variable that opts local run targets
// (see LocalProviderKeys) back into the device picker and `wendy discover`.
// They are hidden by default so those surfaces list separate WendyOS devices.
const ShowLocalDevicesEnv = "WENDY_SHOW_LOCAL_DEVICES"

// ShowLocalDevices reports whether local run targets should be shown in the
// interactive device picker and non-JSON `wendy discover` output. It is false
// unless ShowLocalDevicesEnv is set to a truthy value ("1", "true", "yes", "on";
// case-insensitive).
func ShowLocalDevices() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ShowLocalDevicesEnv))) {
	case "yes", "on":
		return true
	}
	b, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(ShowLocalDevicesEnv)))
	return err == nil && b
}

const (
	RunOutputStarted RunOutputType = iota
	RunOutputStdout
	RunOutputStderr
)

// RunOutput is a single chunk of output from a running application.
type RunOutput struct {
	Type RunOutputType
	Data []byte
}

// ImageBuilder is optionally implemented by providers that can create a
// BuiltApp from a pre-built container image (e.g. after cross-compilation).
type ImageBuilder interface {
	BuildFromImage(device models.ExternalDevice, product, imageName string) *BuiltApp
}

// ContinuousDiscoverer is optionally implemented by providers whose scan has no
// end of its own (mDNS, Bluetooth — see DeviceProvider.DiscoverDevices), so a
// caller can watch discovery unfold instead of paying a fixed wait for one
// DiscoverDevices snapshot. Implementing it is optional: a caller must fall
// back to polling DiscoverDevices for providers that do not.
type ContinuousDiscoverer interface {
	// DiscoverDevicesContinuous starts continuous discovery and returns a
	// channel of snapshots. It finds the same devices DiscoverDevices does —
	// only the scan methodology differs.
	//
	// Every emission is the full list of devices discovered so far, not the
	// single device that just turned up, so a consumer can render a received
	// snapshot as-is without accumulating across emissions. A device drops out
	// of later snapshots once its source stops reporting it (a board
	// unplugged, a BLE device out of range); mDNS rows are cumulative, since a
	// browse only ever announces arrivals.
	//
	// The channel stays open until ctx is cancelled (or the stream fails),
	// then is closed. A close before ctx is done means discovery died, not
	// that discovery is complete — the caller should fall back to polling
	// DiscoverDevices.
	DiscoverDevicesContinuous(ctx context.Context) (<-chan []models.ExternalDevice, error)
}

// TypedBuilder is optionally implemented by providers that can disambiguate
// between multiple buildable markers in the same project using the caller's
// resolved build type (e.g. "docker" vs "compose"). When the build type is
// empty, the provider should apply its default heuristic.
type TypedBuilder interface {
	BuildWithType(ctx context.Context, device models.ExternalDevice, projectPath, product, buildType string, debug bool) (*BuiltApp, error)
}

// DockerfileBuilder is optionally implemented by providers that support
// building from a specific Dockerfile/Containerfile (e.g. Dockerfile.prod).
// When dockerfile is empty, the provider uses its default build-file resolution.
type DockerfileBuilder interface {
	BuildWithDockerfile(ctx context.Context, device models.ExternalDevice, projectPath, product, buildType, dockerfile string, debug bool) (*BuiltApp, error)
}

// ContainerManager is optionally implemented by providers that support
// managing container lifecycle (list, start, stop, remove).
type ContainerManager interface {
	ListContainers(ctx context.Context) ([]ContainerInfo, error)
	StartContainer(ctx context.Context, name string) error
	StopContainer(ctx context.Context, name string) error
	RemoveContainer(ctx context.Context, name string) error
}

// WifiManager is optionally implemented by providers that can manage the
// WiFi connection of their devices.
type WifiManager interface {
	WifiConnect(ctx context.Context, device models.ExternalDevice, ssid, password string) error
	WifiDisconnect(ctx context.Context, device models.ExternalDevice) error
}

// ContainerInfo describes a container managed by a provider.
type ContainerInfo struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
}
