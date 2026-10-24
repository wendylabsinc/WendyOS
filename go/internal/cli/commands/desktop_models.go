package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/modelhub"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Multi-platform Linux amd64/arm64 manifest, Ollama 0.40.10. Pinning avoids
// changing the runtime silently when another model is downloaded.
const desktopOllamaImage = "docker.io/ollama/ollama@sha256:4c5b27871a3239a717048f9b93057680a51b60c8119a7016aebb57f938e25e35"

// DesktopModelPorts discovers installed, running runtimes without starting or
// changing any apps. Runtime versions need not match the current installer.
func DesktopModelPorts(ctx context.Context, conn *grpcclient.AgentConnection) ([]uint32, error) {
	apps, err := listContainersOnConn(ctx, conn)
	if err != nil {
		return nil, err
	}
	var ports []uint32
	seen := map[uint32]bool{}
	for _, app := range apps {
		port := app.GetHttpPort()
		if port > 0 && port <= 65535 && !seen[port] && app.GetRunningState() == pb.AppRunningState_RUNNING && app.GetAppName() == fmt.Sprintf("sh.wendy.models.ollama.%d", port) {
			seen[port] = true
			ports = append(ports, port)
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports, nil
}

func DesktopModelHardware(ctx context.Context, conn *grpcclient.AgentConnection) modelhub.Hardware {
	var h modelhub.Hardware
	probe, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	info, err := conn.AgentService.GetAgentVersion(probe, &pb.GetAgentVersionRequest{})
	if err != nil {
		h.Warning = "Device hardware details are unavailable. "
	} else {
		h.OS, h.Architecture, h.JetPack = desktopModelOS(info.GetOs()), info.GetCpuArchitecture(), info.GetJetpackVersion()
		h.MemoryTotal = info.GetMemTotalBytes()
		h.GPU = info.GetGpuVendor()
		h.SharedMemory = h.JetPack != ""
		for _, g := range info.GetGpuCapabilities() {
			for _, backend := range g.GetComputeBackends() {
				if g.GetVendor() == "nvidia" && backend == "cuda" {
					h.CUDA = true
				}
			}
		}
		if info.GetGpuVendor() == "nvidia" && info.GetCudaVersion() != "" {
			h.CUDA = true
		}
		disk := info.GetContainerStorage()
		if disk == nil {
			for _, d := range info.GetPartitions() {
				if d.GetMountpoint() == "/data" {
					disk = d
					break
				}
			}
		}
		if disk != nil && disk.GetTotalBytes() > 0 {
			h.DiskKnown = true
			h.DiskFree = max(0, disk.GetTotalBytes()-disk.GetUsedBytes())
		}
	}
	sample, stop := context.WithTimeout(ctx, 6*time.Second)
	defer stop()
	stats, err := conn.ContainerService.GetResourceStats(sample, &pb.GetResourceStatsRequest{})
	if err != nil || stats.GetHost() == nil {
		h.Warning += "Available memory could not be measured. "
		h.MemoryTotal = 0
	} else {
		host := stats.GetHost()
		h.MemoryTotal, h.MemoryAvailable = host.GetMemTotalBytes(), host.GetMemAvailableBytes()
		// Do not add discrete GPUs together: a single model may not span devices.
		for _, g := range host.GetGpus() {
			if g.GetMemTotalBytes() > h.GPUTotal {
				h.GPU = g.GetName()
				h.GPUTotal = g.GetMemTotalBytes()
				h.GPUAvailable = max(0, g.GetMemTotalBytes()-g.GetMemUsedBytes())
			}
		}
	}
	if !h.DiskKnown {
		h.Warning += "Model storage free space is unknown."
	}
	h.Warning = strings.TrimSpace(h.Warning)
	return h
}

// Linux Agents return /etc/os-release's distribution ID, while other builds
// return GOOS. Preserve known non-Linux platforms and normalize distro IDs.
func desktopModelOS(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "", "darwin", "ios", "windows", "freebsd", "openbsd", "netbsd", "dragonfly", "solaris", "illumos", "aix", "plan9", "android", "js", "wasip1":
		return name
	default:
		return "linux"
	}
}

func desktopModelRuntime(port uint32, accelerator uint32) (*pb.CreateContainerRequest, error) {
	if port == 0 || port > 65535 {
		return nil, errors.New("enter a port from 1 to 65535")
	}
	profiles := []string{"cpu", "nvidia", "jetpack5", "jetpack6"}
	if int(accelerator) >= len(profiles) {
		return nil, errors.New("unsupported accelerator")
	}
	profile := profiles[accelerator]
	cfg := appconfig.AppConfig{AppID: fmt.Sprintf("sh.wendy.models.ollama.%d", port), Version: "0.40.10-" + profile + "-loopback", Platform: "linux", Entitlements: []appconfig.Entitlement{
		{Type: "network", Mode: "host"}, {Type: "http", Port: int(port)}, {Type: "persist", Name: "ollama-models", Path: "/root/.ollama"},
	}}
	env := []string{fmt.Sprintf("OLLAMA_HOST=127.0.0.1:%d", port), "OLLAMA_CONTEXT_LENGTH=4096", "OLLAMA_MAX_LOADED_MODELS=1", "OLLAMA_NUM_PARALLEL=1"}
	if accelerator != 0 {
		cfg.Entitlements = append(cfg.Entitlements, appconfig.Entitlement{Type: "gpu"})
	}
	if accelerator == 2 {
		env = append(env, "JETSON_JETPACK=5")
	}
	if accelerator == 3 {
		env = append(env, "JETSON_JETPACK=6")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return &pb.CreateContainerRequest{ImageName: desktopOllamaImage, AppName: cfg.AppID, AppConfig: raw, Env: env, RestartPolicy: &pb.RestartPolicy{Mode: pb.RestartPolicyMode_UNLESS_STOPPED}}, nil
}

// DesktopEnsureModelRuntime reuses the same profile, or upgrades its known
// wildcard-listening predecessor in place while retaining the model volume.
// Unrelated apps and profiles are never replaced.
func DesktopEnsureModelRuntime(ctx context.Context, conn *grpcclient.AgentConnection, port, accelerator uint32, portOccupied func() bool, progress func(string)) error {
	req, err := desktopModelRuntime(port, accelerator)
	if err != nil {
		return err
	}
	apps, err := listContainersOnConn(ctx, conn)
	if err != nil {
		return err
	}
	var cfg appconfig.AppConfig
	if err = json.Unmarshal(req.AppConfig, &cfg); err != nil {
		return err
	}
	upgrade := false
	for _, app := range apps {
		if app.GetAppName() == req.AppName {
			upgrade = app.GetAppVersion() == strings.TrimSuffix(cfg.Version, "-loopback")
			if (!upgrade && app.GetAppVersion() != cfg.Version) || app.GetHttpPort() != port || len(app.GetServices()) != 0 {
				return errors.New("an app already uses this runtime name with different settings; choose another port")
			}
			if upgrade {
				progress("Updating Ollama to listen only on device loopback")
				break
			}
			if app.GetRunningState() == pb.AppRunningState_RUNNING {
				progress("Using the installed Ollama runtime")
				return nil
			}
			if portOccupied() {
				return errors.New("this port is already in use; choose another port or use the existing server")
			}
			progress("Starting the installed Ollama runtime")
			return DesktopStartApp(ctx, conn, req.AppName)
		}
	}
	if !upgrade && portOccupied() {
		return errors.New("this port is already in use; choose another port or use the existing server")
	}
	if !upgrade {
		if err = registerCloudApps(ctx, conn, []string{req.AppName}, false); err != nil {
			return err
		}
	}
	progress("Downloading the Ollama runtime on the device")
	stream, err := conn.ContainerService.CreateContainerWithProgress(ctx, req)
	if err != nil {
		return err
	}
	completed := false
	for {
		r, e := stream.Recv()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		if r.GetCompleted() != nil {
			completed = true
		}
		if p := r.GetProgress(); p != nil {
			progress("Preparing runtime: " + p.GetPhase().String())
		}
	}
	if !completed {
		return errors.New("the Agent ended runtime installation without a completion acknowledgement")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	progress("Starting Ollama on the device")
	return DesktopStartApp(ctx, conn, req.AppName)
}

func DesktopVerifyModelRuntime(ctx context.Context, conn *grpcclient.AgentConnection, port uint32) error {
	apps, err := listContainersOnConn(ctx, conn)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("sh.wendy.models.ollama.%d", port)
	for _, app := range apps {
		if app.GetAppName() == name && app.GetRunningState() == pb.AppRunningState_RUNNING {
			return nil
		}
	}
	return errors.New("the managed model runtime is not running; check its app logs before retrying")
}

// DesktopSelectModelPort reuses a matching managed runtime before reserving a
// new name/port. A failed probe is not evidence that a port is available.
func DesktopSelectModelPort(ctx context.Context, conn *grpcclient.AgentConnection, accelerator uint32, occupied func(context.Context, uint32) (bool, error)) (uint32, error) {
	apps, err := listContainersOnConn(ctx, conn)
	if err != nil {
		return 0, err
	}
	used := map[uint32]bool{}
	for _, app := range apps {
		port := app.GetHttpPort()
		if port > 0 {
			used[port] = true
		}
		if port < 1024 || port > 65535 || app.GetAppName() != fmt.Sprintf("sh.wendy.models.ollama.%d", port) {
			continue
		}
		req, e := desktopModelRuntime(port, accelerator)
		if e != nil {
			return 0, e
		}
		var cfg appconfig.AppConfig
		if e = json.Unmarshal(req.AppConfig, &cfg); e != nil {
			return 0, e
		}
		if (app.GetAppVersion() != cfg.Version && app.GetAppVersion() != strings.TrimSuffix(cfg.Version, "-loopback")) || len(app.GetServices()) != 0 {
			continue
		}
		if app.GetRunningState() == pb.AppRunningState_RUNNING {
			return port, nil
		}
		busy, e := occupied(ctx, port)
		if e != nil {
			return 0, e
		}
		if !busy {
			return port, nil
		}
	}
	for port := uint32(11434); port < 11534; port++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if used[port] {
			continue
		}
		// A conflicting app name may exist without an HTTP entitlement.
		taken := false
		for _, app := range apps {
			if app.GetAppName() == fmt.Sprintf("sh.wendy.models.ollama.%d", port) {
				taken = true
				break
			}
		}
		if taken {
			continue
		}
		busy, e := occupied(ctx, port)
		if e != nil {
			return 0, e
		}
		if !busy {
			return port, nil
		}
	}
	return 0, errors.New("no available model runtime port between 11434 and 11533")
}
