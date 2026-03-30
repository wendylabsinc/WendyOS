// Package providers defines the pluggable device provider system for building
// and running apps on different targets (local, Docker, ADB, WASM, etc.).
package providers

import (
	"context"

	"github.com/wendylabsinc/wendy/internal/shared/models"
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
	// DiscoverDevices returns the devices currently reachable through this provider.
	DiscoverDevices(ctx context.Context) ([]models.ExternalDevice, error)
	// SupportedBuildTypes returns the build type keys (e.g. "docker", "swift")
	// that this provider can handle. Used to filter the build-type picker.
	SupportedBuildTypes() []string
	// CanBuild reports whether this provider can build the project at projectPath.
	CanBuild(projectPath string) bool
	// Build compiles the project for the given device and returns a BuiltApp handle.
	Build(ctx context.Context, device models.ExternalDevice, projectPath, product string, debug bool) (*BuiltApp, error)
	// Run starts the built application, streaming output to the channel until done.
	Run(ctx context.Context, app *BuiltApp, detach bool, output chan<- RunOutput) error
	// Stop terminates a running application.
	Stop(ctx context.Context, app *BuiltApp) error
}

// BuiltApp is the result of a successful Build. The opaque Context field
// carries provider-specific artifacts (binary path, container ID, etc.).
type BuiltApp struct {
	ProviderKey string
	Device      models.ExternalDevice
	AppName     string
	Context     interface{} // provider-specific build artifact
}

// RunOutputType classifies a line of output from a running app.
type RunOutputType int

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

// ContainerManager is optionally implemented by providers that support
// managing container lifecycle (list, start, stop, remove).
type ContainerManager interface {
	ListContainers(ctx context.Context) ([]ContainerInfo, error)
	StartContainer(ctx context.Context, name string) error
	StopContainer(ctx context.Context, name string) error
	RemoveContainer(ctx context.Context, name string) error
}

// ContainerInfo describes a container managed by a provider.
type ContainerInfo struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
}
