// Package services implements the gRPC service handlers for the wendy-agent.
package services

import (
	"context"
	"io"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// NetworkManager abstracts WiFi management operations (typically backed by nmcli).
type NetworkManager interface {
	ListWiFiNetworks(ctx context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error)
	ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) error
	GetWiFiStatus(ctx context.Context) (connected bool, ssid string, err error)
	DisconnectWiFi(ctx context.Context) error
	ListKnownWiFiNetworks(ctx context.Context) ([]*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, error)
	SetWiFiNetworkPriority(ctx context.Context, ssid string, priority int32) error
	ReorderKnownWiFiNetworks(ctx context.Context, orderedSSIDs []string) error
	ForgetWiFiNetwork(ctx context.Context, ssid string) error
}

// HardwareDiscoverer discovers hardware capabilities by probing sysfs, /dev, /proc, etc.
type HardwareDiscoverer interface {
	Discover(ctx context.Context, categoryFilter string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error)
}

// BluetoothManager abstracts Bluetooth peripheral management.
type BluetoothManager interface {
	Scan(ctx context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error)
	Connect(ctx context.Context, address string, pair, trust bool) error
	Disconnect(ctx context.Context, address string) error
	Forget(ctx context.Context, address string) error
}

// ProgressFunc is called by CreateContainer to report progress during
// image pull, unpack, and container creation. The caller may be nil.
type ProgressFunc func(progress *agentpb.CreateContainerProgress)

// ContainerdClient abstracts interactions with the containerd runtime.
type ContainerdClient interface {
	ListLayers(ctx context.Context) ([]*agentpb.LayerHeader, error)
	WriteLayer(ctx context.Context, digest string, reader io.Reader, size int64) error
	AssembleImage(ctx context.Context, imageName string, layers []*agentpb.RunContainerLayerHeader) error
	CreateContainer(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig) error
	CreateContainerWithProgress(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig, onProgress ProgressFunc) error
	StartContainer(ctx context.Context, appName, postStartAgentCommand string) (<-chan ContainerOutput, error)
	StartContainerWithStdin(ctx context.Context, appName string, stdin io.Reader, postStartAgentCommand string) (<-chan ContainerOutput, error)
	StopContainer(ctx context.Context, appName string) error
	DeleteContainer(ctx context.Context, appName string, deleteImage bool) error
	ListContainers(ctx context.Context) ([]*agentpb.AppContainer, error)
	GetContainerStats(ctx context.Context) ([]*agentpb.ContainerStats, error)
	GetContainerMetrics(ctx context.Context, appName string) (ContainerMetrics, error)
}

// ContainerOutput represents a chunk of output from a running container.
type ContainerOutput struct {
	Stdout []byte
	Stderr []byte
	Done   bool
}

// ContainerMetrics holds a point-in-time CPU and memory snapshot for a container.
type ContainerMetrics struct {
	UserCPUNanos int64 // cumulative user-mode CPU time in nanoseconds
	SysCPUNanos  int64 // cumulative kernel-mode CPU time in nanoseconds
	MemBytes     int64 // current memory usage in bytes
}
