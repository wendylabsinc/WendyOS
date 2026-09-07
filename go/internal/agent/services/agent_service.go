package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
	"github.com/wendylabsinc/wendy/go/internal/agent/oshealth"
	"github.com/wendylabsinc/wendy/go/internal/shared/sigverify"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type AgentService struct {
	agentpb.UnimplementedWendyAgentServiceServer
	logger                   *zap.Logger
	networkManager           NetworkManager
	hardwareDiscoverer       HardwareDiscoverer
	discoverGPUs             func() []gpudiscovery.Device
	discoverContainerStorage func() (partitionUsage, bool)
	bluetoothManager         BluetoothManager
	installer                *AgentInstaller
	isWendyOSHost            func() bool
	osUpdateStateDir         string

	// verifier checks the update binary's signature before install. Defaults
	// to sigverify.DefaultVerifier (disabled until a real pinned key is
	// embedded); settable within-package so tests can inject an enabled
	// verifier without changing the exported constructor signature. See
	// AgentUpdateService (the v2 sibling handler) for the same pattern.
	verifier *sigverify.Verifier

	execPathCache

	// restartFn is invoked once the binary is committed to schedule the
	// process exit that lets systemd restart into the new binary. Defaults
	// to scheduleAgentRestartExit (a real os.Exit(0) after a grace period);
	// settable within-package so tests exercising a successful install don't
	// kill the test process.
	restartFn func()

	// binaryHashOnce/binaryHash cache the hex SHA-256 of the executable this
	// process was started from; see binarySHA256.
	binaryHashOnce sync.Once
	binaryHash     string
}

func NewAgentService(
	logger *zap.Logger,
	nm NetworkManager,
	hd HardwareDiscoverer,
	bm BluetoothManager,
	installer *AgentInstaller,
) *AgentService {
	return &AgentService{
		logger:             logger,
		networkManager:     nm,
		hardwareDiscoverer: hd,
		bluetoothManager:   bm,
		installer:          installer,
		isWendyOSHost:      defaultIsWendyOSHost,
		osUpdateStateDir:   oshealth.DefaultStateDir,
		verifier:           sigverify.DefaultVerifier,
		restartFn:          scheduleAgentRestartExit,
	}
}

func (s *AgentService) GetAgentVersion(_ context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	resp := &agentpb.GetAgentVersionResponse{
		Version:         version.Version,
		Os:              detectOS(),
		CpuArchitecture: runtime.GOARCH,
		Featureset:      detectFeatureset(),
		BinarySha256:    s.binarySHA256(),
	}

	if hn, err := os.Hostname(); err == nil {
		resp.Hostname = hn
	}

	if v, ok := wendyOSVersion(); ok {
		resp.OsVersion = &v
	} else if _, distroVer := detectDistro(); distroVer != "" {
		resp.OsVersion = &distroVer
	}

	if data, err := os.ReadFile("/etc/wendyos/device-type"); err == nil {
		deviceType, storageMedium := parseDeviceType(string(data))
		if deviceType != "" {
			resp.DeviceType = &deviceType
		}
		if storageMedium != "" {
			resp.StorageMedium = &storageMedium
		}
	}

	gpuProbe := s.discoverGPUs
	if gpuProbe == nil {
		gpuProbe = gpudiscovery.Host
	}
	gpuInfo := detectGPUInfoFrom(gpuProbe())
	resp.GpuCapabilities = &agentpb.GpuCapabilities{ComputeBackends: gpuInfo.computeBackends}
	resp.HasGpu = &gpuInfo.hasGPU
	if gpuInfo.vendor != "" {
		resp.GpuVendor = &gpuInfo.vendor
	}
	if gpuInfo.jetpackVersion != "" {
		resp.JetpackVersion = &gpuInfo.jetpackVersion
	}
	if gpuInfo.cudaVersion != "" {
		resp.CudaVersion = &gpuInfo.cudaVersion
	}
	if gpuInfo.gpuArch != "" {
		resp.GpuArch = &gpuInfo.gpuArch
	}

	if usage, ok := rootDiskUsage(); ok {
		resp.DiskUsedBytes = &usage.usedBytes
		resp.DiskTotalBytes = &usage.totalBytes
	}

	storageProbe := s.discoverContainerStorage
	if storageProbe == nil {
		storageProbe = containerStorageUsage
	}
	if p, ok := storageProbe(); ok {
		resp.ContainerStorage = &agentpb.DiskPartition{Mountpoint: p.mountpoint, Filesystem: p.filesystem, Device: p.device, UsedBytes: p.usedBytes, TotalBytes: p.totalBytes}
	}

	resp.MemTotalBytes, resp.CpuCount = hostMemAndCPUCount()

	for _, p := range listDiskPartitions() {
		resp.Partitions = append(resp.Partitions, &agentpb.DiskPartition{
			Mountpoint: p.mountpoint,
			Filesystem: p.filesystem,
			Device:     p.device,
			UsedBytes:  p.usedBytes,
			TotalBytes: p.totalBytes,
		})
	}

	resp.NetworkInterfaces = listNetworkInterfaces()

	// sysfs when the host has its own pack, else a registered fallback (a
	// robot's BMS read over DDS). nil on the mains-powered devices that make up
	// most of the fleet, which leaves the field absent so `wendy device info`
	// prints no battery line.
	resp.Battery = batteryToProto(hoststats.ResolveBattery())

	return resp, nil
}

// WarmBinaryHash starts computing the executable hash in the background so
// the first GetAgentVersion — which doubles as the CLI's liveness probe with
// a ~3s budget — doesn't pay a full read of the binary (tens of MB, possibly
// off cold SD-card storage) inside that window. Call once at startup, after
// any execPathResolver override; GetAgentVersion still blocks on (or itself
// performs) the same one-time computation.
func (s *AgentService) WarmBinaryHash() {
	go s.binarySHA256()
}

// binarySHA256 returns the hex SHA-256 of the executable this process was
// started from, computed once and cached: after an update commits, the file
// at the exec path is already the NEW binary while this process still runs
// the old one, so re-hashing on demand would misreport until the restart.
// Empty when the executable cannot be resolved or read.
func (s *AgentService) binarySHA256() string {
	s.binaryHashOnce.Do(func() {
		path, _, err := s.resolvedExecPath()
		if err != nil {
			s.logger.Warn("could not resolve own executable for hashing", zap.Error(err))
			return
		}
		f, err := os.Open(path)
		if err != nil {
			s.logger.Warn("could not open own executable for hashing", zap.Error(err))
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			s.logger.Warn("could not hash own executable", zap.Error(err))
			return
		}
		s.binaryHash = hex.EncodeToString(h.Sum(nil))
	})
	return s.binaryHash
}

// SetHostname sets the device's hostname (and mDNS name) to a literal value,
// applies it live, and persists it across reboots. See applyHostname.
func (s *AgentService) SetHostname(_ context.Context, req *agentpb.SetHostnameRequest) (*agentpb.SetHostnameResponse, error) {
	hostname := req.GetHostname()
	s.logger.Info("SetHostname requested", zap.String("hostname", hostname))

	if !s.isWendyOSHost() {
		s.logger.Warn("SetHostname rejected: host is not a WendyOS device", zap.String("hostname", hostname))
		return nil, status.Error(codes.Unavailable, "setting the hostname is only supported on WendyOS devices")
	}

	if err := applyHostname(s.logger, hostname); err != nil {
		s.logger.Warn("SetHostname failed", zap.String("hostname", hostname), zap.Error(err))
		if !validHostname(hostname) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "applying hostname: %v", err)
	}

	return &agentpb.SetHostnameResponse{Hostname: hostname}, nil
}

type gpuInfo struct {
	hasGPU          bool
	vendor          string
	jetpackVersion  string
	cudaVersion     string
	gpuArch         string
	computeBackends []string
}

// detectGPUInfo probes on every call rather than caching. /dev/dri and the DRM
// sysfs tree are live state: the first RPC can land before udev has settled,
// and installing a driver add-on makes a GPU appear without restarting the
// agent — so a cached "no GPU" would never heal, and would contradict
// detectFeatureset, which re-probes.
func detectGPUInfo() gpuInfo { return detectGPUInfoFrom(gpudiscovery.Host()) }

func detectGPUInfoFrom(devices []gpudiscovery.Device) gpuInfo {
	vendor, backends := gpudiscovery.Summary(devices)
	info := gpuInfo{hasGPU: len(devices) > 0, vendor: vendor, computeBackends: backends}

	switch info.vendor {
	case "nvidia":
		info.jetpackVersion = detectJetPackVersion()
		info.cudaVersion = detectCUDAVersion()
		info.gpuArch = detectNvidiaGPUArch()
	case "qualcomm":
		info.gpuArch = detectAdrenoArch()
	}

	return info
}

// adrenoCompatibleRe pulls the model out of "qcom,adreno-623.0" -> "623".
var adrenoCompatibleRe = regexp.MustCompile(`qcom,adreno-(\d+)\.\d+`)

// platformDevicesRoot is behind a var so tests can use a fixture tree.
var platformDevicesRoot = "/sys/bus/platform/devices"

// detectAdrenoArch reports the Adreno model, e.g. "a623". The DRM device on
// this SoC family is the display controller, so its compatible string names the
// DPU; the GPU is a separate platform device bound to the adreno driver.
func detectAdrenoArch() string {
	entries, err := os.ReadDir(platformDevicesRoot)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".gpu") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(platformDevicesRoot, e.Name(), "of_node", "compatible"))
		if err != nil {
			continue
		}
		if m := adrenoCompatibleRe.FindSubmatch(data); len(m) > 1 {
			return "a" + string(m[1])
		}
	}
	return ""
}

var tegraReleaseRe = regexp.MustCompile(`R(\d+)\s+\([^)]+\),\s+REVISION:\s+([\d.]+)`)

// Falls back to "L4T-{version}" when no mapping is found.
func detectJetPackVersion() string {
	data, err := os.ReadFile("/etc/nv_tegra_release")
	if err != nil {
		return ""
	}
	return jetPackVersionFromTegraRelease(data)
}

func jetPackVersionFromTegraRelease(data []byte) string {
	m := tegraReleaseRe.FindSubmatch(data)
	if len(m) < 3 {
		return ""
	}
	major := string(m[1])
	revision := string(m[2]) // e.g. "4.4"

	// Some JetPack releases share an L4T major.minor family, so prefer the
	// complete L4T version before falling back to the family mapping.
	l4tVersion := major + "." + revision
	jetpackExact := map[string]string{
		"36.4.4": "6.2.1",
		"36.4.3": "6.2",
		"36.4.0": "6.1",
	}
	if jp, ok := jetpackExact[l4tVersion]; ok {
		return jp
	}

	// Use major.minor for releases where the patch version is not significant.
	minor := strings.SplitN(revision, ".", 2)[0]
	key := major + "." + minor

	// L4T → JetPack version table.
	// https://developer.nvidia.com/embedded/jetpack-archive
	jetpack := map[string]string{
		"39.2": "7.2",
		"36.3": "6.0",
		"36.2": "6.0",
		"35.5": "5.1.3",
		"35.4": "5.1.2",
		"35.3": "5.1.1",
		"35.2": "5.1",
		"35.1": "5.0.2",
		"34.1": "5.0.1",
		"32.7": "4.6",
		"32.6": "4.6",
		"32.5": "4.5",
		"32.4": "4.4",
		"32.3": "4.3",
		"32.2": "4.2",
		"32.1": "4.1",
	}
	if jp, ok := jetpack[key]; ok {
		return jp
	}
	return "L4T-" + l4tVersion
}

var cudaVersionFileRe = regexp.MustCompile(`(?i)CUDA[^0-9]*([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)

func detectCUDAVersion() string {
	return detectCUDAVersionIn("/usr/local", exec.LookPath, runDetectCommand)
}

// runDetectCommand runs a detection helper binary with a timeout so detection
// cannot block an RPC handler.
func runDetectCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// detectCUDAVersionIn probes for the installed CUDA toolkit version under
// usrLocal (normally /usr/local). lookPath and runCmd are injectable for tests.
//
// Probe order:
//  1. version.txt / version.json under the well-known cuda symlink (legacy
//     layouts; CUDA 11.1+ dropped version.txt, newer releases also dropped
//     version.json).
//  2. nvcc on PATH.
//  3. nvcc inside versioned toolkit dirs (cuda-X.Y/bin/nvcc) — the JetPack 7
//     layout ships no cuda symlink, no version files, and does not put nvcc
//     on PATH.
func detectCUDAVersionIn(usrLocal string, lookPath func(string) (string, error), runCmd func(string, ...string) ([]byte, error)) string {
	for _, name := range []string{"version.txt", "version.json"} {
		if data, err := os.ReadFile(filepath.Join(usrLocal, "cuda", name)); err == nil {
			if m := cudaVersionFileRe.FindSubmatch(data); len(m) > 1 {
				return string(m[1])
			}
		}
	}

	if nvcc, err := lookPath("nvcc"); err == nil {
		if v := cudaVersionFromNvcc(nvcc, runCmd); v != "" {
			return v
		}
	}

	matches, _ := filepath.Glob(filepath.Join(usrLocal, "cuda-*", "bin", "nvcc"))
	for _, nvcc := range matches {
		if v := cudaVersionFromNvcc(nvcc, runCmd); v != "" {
			return v
		}
	}

	return ""
}

func cudaVersionFromNvcc(nvcc string, runCmd func(string, ...string) ([]byte, error)) string {
	out, err := runCmd(nvcc, "--version")
	if err != nil {
		return ""
	}
	if m := cudaVersionFileRe.FindSubmatch(out); len(m) > 1 {
		return string(m[1])
	}
	return ""
}

var computeCapRe = regexp.MustCompile(`^\s*(\d+)\.(\d+)\s*$`)

func detectNvidiaGPUArch() string {
	if nvidiaSmi, err := exec.LookPath("nvidia-smi"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, nvidiaSmi, "--query-gpu=compute_cap", "--format=csv,noheader,nounits").Output()
		if err == nil {
			if m := computeCapRe.FindSubmatch(out); len(m) > 2 {
				return "sm_" + string(m[1]) + string(m[2])
			}
		}
	}
	return ""
}

func detectFeatureset() []string {
	var features []string

	if _, err := os.Stat("/dev/nvidia0"); err == nil {
		features = append(features, "gpu")
	} else if matches, _ := os.ReadDir("/dev/dri"); len(matches) > 0 {
		features = append(features, "gpu")
	}

	if _, err := os.Stat("/proc/asound/cards"); err == nil {
		features = append(features, "audio")
	} else if _, err := exec.LookPath("pactl"); err == nil {
		features = append(features, "audio")
	}

	if _, err := os.Stat("/sys/class/bluetooth"); err == nil {
		if entries, _ := os.ReadDir("/sys/class/bluetooth"); len(entries) > 0 {
			features = append(features, "bluetooth")
		}
	}

	if entries, _ := os.ReadDir("/dev"); len(entries) > 0 {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "video") {
				features = append(features, "video")
				break
			}
		}
	}

	if _, err := os.Stat("/dev/video0"); err == nil {
		features = append(features, "camera")
	}

	if _, hasWendyOS := resolveWendyOSBinary(); hasWendyOS {
		// The in-house wendyos-update engine, the OS update backend. See
		// selectUpdater.
		features = append(features, "wendyos-update")
		// "os-healthcheck": OS updates are verified by post-reboot service
		// healthchecks with automatic rollback (and GetOSUpdateStatus reports
		// the outcome). See oshealth.Gate.
		features = append(features, "os-healthcheck")
	}

	return features
}

// parseDeviceType parses /etc/wendyos/device-type, which may be either a plain
// string (legacy) or a KEY=VALUE file (new format).
// Returns (deviceType, storageMedium); either may be empty.
//
// BOARD is the stable board identity that matches the OTA manifest keys
// (e.g. "jetson-orin-nano"), whereas MACHINE is the full Yocto machine name
// (e.g. "jetson-orin-nano-devkit-nvme-wendyos"). The manifest is keyed by the
// board id, so BOARD is preferred; MACHINE is only a fallback for images that
// don't emit a BOARD line.
func parseDeviceType(content string) (deviceType, storageMedium string) {
	content = strings.TrimSpace(content)
	if !strings.Contains(content, "=") {
		return content, ""
	}
	var board, machine string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "BOARD":
			board = strings.TrimSpace(v)
		case "MACHINE":
			machine = strings.TrimSpace(v)
		case "STORAGE":
			storageMedium = strings.TrimSpace(v)
		}
	}
	deviceType = board
	if deviceType == "" {
		deviceType = machine
	}
	// Fall back to inferring the storage medium from the Yocto machine name
	// (e.g. "...-nvme-wendyos") when the image didn't emit an explicit STORAGE
	// line. The OTA manifest uses this to pick the storage-specific artifact;
	// an unknown/absent medium is fine (the default artifact is used).
	if storageMedium == "" {
		storageMedium = inferStorageFromMachine(machine)
	}
	return deviceType, storageMedium
}

// inferStorageFromMachine derives a storage medium from a Yocto machine name
// when no explicit STORAGE was provided. Only the media that need a dedicated
// OTA artifact are recognized; everything else returns "" (the default
// artifact applies).
func inferStorageFromMachine(machine string) string {
	m := strings.ToLower(machine)
	switch {
	case strings.Contains(m, "nvme"):
		return "nvme"
	case strings.Contains(m, "emmc"):
		return "emmc"
	default:
		return ""
	}
}

// RunContainer is deprecated. Clients should use WendyContainerService.RunContainer
// or WendyContainerService.CreateContainer + StartContainer instead.
func (s *AgentService) RunContainer(stream grpc.BidiStreamingServer[agentpb.RunContainerRequest, agentpb.RunContainerResponse]) error {
	s.logger.Warn("RunContainer called on deprecated WendyAgentService.RunContainer")
	return status.Error(codes.Unimplemented,
		"RunContainer is deprecated. Use WendyContainerService.RunContainer or CreateContainer + StartContainer instead. Please update your CLI.")
}

func (s *AgentService) UpdateAgent(stream grpc.BidiStreamingServer[agentpb.UpdateAgentRequest, agentpb.UpdateAgentResponse]) error {
	if !s.installer.TryLock() {
		return status.Error(codes.FailedPrecondition, "an update is already in progress")
	}
	// committed is declared before the defer so the closure captures it.
	// On success the lock is intentionally NOT released: the process exits
	// within 500 ms and holding the lock prevents a concurrent update from
	// racing on the just-installed binary during that shutdown window.
	committed := false
	defer func() {
		if !committed {
			s.installer.Unlock()
		}
	}()

	s.logger.Info("UpdateAgent stream started")

	execPath, originalPerm, err := s.resolvedExecPath()
	if err != nil {
		return err
	}

	tmpFile, tmpPath, cleanupTmp, err := createUpdateTempFile(execPath)
	if err != nil {
		return err
	}
	defer func() {
		if !committed {
			cleanupTmp()
		}
	}()

	hasher := sha256.New()
	var written int64

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "error receiving update data: %v", err)
		}

		if chunk := msg.GetChunk(); chunk != nil {
			data := chunk.GetData()
			written += int64(len(data))
			if written > maxAgentBinarySize {
				return status.Errorf(codes.ResourceExhausted,
					"update stream exceeds maximum agent binary size (%d MiB)", maxAgentBinarySize>>20)
			}
			if _, err := tmpFile.Write(data); err != nil {
				return status.Errorf(codes.Internal, "failed to write update chunk: %v", err)
			}
			hasher.Write(data)
			continue
		}

		if ctrl := msg.GetControl(); ctrl != nil {
			if ctrl.GetUpdate() != nil {
				digest := hasher.Sum(nil)
				computedHash := hex.EncodeToString(digest)
				expectedHash := ctrl.GetUpdate().GetSha256()
				if expectedHash != "" && computedHash != expectedHash {
					return status.Errorf(codes.DataLoss,
						"SHA256 mismatch: expected %s, got %s", expectedHash, computedHash)
				}

				// Verify the binary's signature over its SHA256 digest before
				// installing. When the verifier is disabled (no pinned key
				// embedded yet), Verify is a fail-safe no-op and install
				// proceeds exactly as before this check existed.
				if err := s.verifier.Verify(digest, ctrl.GetUpdate().GetSignature()); err != nil {
					switch {
					case errors.Is(err, sigverify.ErrUnsigned):
						return status.Error(codes.FailedPrecondition, "agent update binary is unsigned; refusing install")
					case errors.Is(err, sigverify.ErrBadSignature):
						return status.Error(codes.DataLoss, "agent update binary signature verification failed; refusing install")
					default:
						return status.Errorf(codes.Internal, "agent update binary signature verification error: %v", err)
					}
				}

				if _, err := commitBinaryUpdate(tmpFile, tmpPath, execPath, computedHash, originalPerm, s.logger); err != nil {
					if errors.Is(err, ErrDirFsync) {
						// Binary is installed; only directory-entry durability is at risk.
						s.logger.Warn("Update dir fsync failed; binary installed but rename may not survive power loss", zap.Error(err))
					} else {
						return err
					}
				}
				committed = true

				return finishCommittedUpdate(s.logger, func() error {
					return stream.Send(&agentpb.UpdateAgentResponse{
						ResponseType: &agentpb.UpdateAgentResponse_Updated_{
							Updated: &agentpb.UpdateAgentResponse_Updated{},
						},
					})
				}, s.restartFn)
			}
		}
	}

	return status.Error(codes.InvalidArgument, "update stream ended without update control command")
}

// finishCommittedUpdate runs the tail of an UpdateAgent stream once the binary
// is committed: schedule the restart exit FIRST, then best-effort ack the
// client. The restart must never depend on the ack — the client's transport
// can already be gone by the time the update lands, and returning an error
// here would leave the old binary running while the installer lock is held
// forever (it is deliberately kept on success), wedging every retry on
// "an update is already in progress" until a manual reboot. The exit delay in
// scheduleExit gives a successful ack time to flush before the process dies.
func finishCommittedUpdate(logger *zap.Logger, sendAck func() error, scheduleExit func()) error {
	scheduleExit()
	if err := sendAck(); err != nil {
		logger.Warn("agent update committed but the ack was not delivered; restarting into the new binary anyway",
			zap.Error(err))
	}
	return nil
}

// scheduleAgentRestartExit exits the process after a short grace period so
// systemd restarts the agent on the just-committed binary.
func scheduleAgentRestartExit() {
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
}

func (s *AgentService) ListWiFiNetworks(ctx context.Context, _ *agentpb.ListWiFiNetworksRequest) (*agentpb.ListWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	networks, err := s.networkManager.ListWiFiNetworks(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list WiFi networks: %v", err)
	}
	return &agentpb.ListWiFiNetworksResponse{Networks: networks}, nil
}

func (s *AgentService) ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) (*agentpb.ConnectToWiFiResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.ConnectToWiFi(ctx, req); err != nil {
		errMsg := err.Error()
		return &agentpb.ConnectToWiFiResponse{Success: false, ErrorMessage: &errMsg}, nil
	}
	return &agentpb.ConnectToWiFiResponse{Success: true}, nil
}

func (s *AgentService) ListKnownWiFiNetworks(ctx context.Context, _ *agentpb.ListKnownWiFiNetworksRequest) (*agentpb.ListKnownWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	known, err := s.networkManager.ListKnownWiFiNetworks(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list known WiFi networks: %v", err)
	}
	return &agentpb.ListKnownWiFiNetworksResponse{Networks: known}, nil
}

func (s *AgentService) SetWiFiNetworkPriority(ctx context.Context, req *agentpb.SetWiFiNetworkPriorityRequest) (*agentpb.SetWiFiNetworkPriorityResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.SetWiFiNetworkPriority(ctx, req.GetSsid(), req.GetPriority()); err != nil {
		msg := err.Error()
		return &agentpb.SetWiFiNetworkPriorityResponse{Success: false, ErrorMessage: &msg}, nil
	}
	return &agentpb.SetWiFiNetworkPriorityResponse{Success: true}, nil
}

func (s *AgentService) ReorderKnownWiFiNetworks(ctx context.Context, req *agentpb.ReorderKnownWiFiNetworksRequest) (*agentpb.ReorderKnownWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.ReorderKnownWiFiNetworks(ctx, req.GetOrderSsids()); err != nil {
		msg := err.Error()
		return &agentpb.ReorderKnownWiFiNetworksResponse{Success: false, ErrorMessage: &msg}, nil
	}
	return &agentpb.ReorderKnownWiFiNetworksResponse{Success: true}, nil
}

func (s *AgentService) ForgetWiFiNetwork(ctx context.Context, req *agentpb.ForgetWiFiNetworkRequest) (*agentpb.ForgetWiFiNetworkResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.ForgetWiFiNetwork(ctx, req.GetSsid()); err != nil {
		msg := err.Error()
		return &agentpb.ForgetWiFiNetworkResponse{Success: false, ErrorMessage: &msg}, nil
	}
	return &agentpb.ForgetWiFiNetworkResponse{Success: true}, nil
}

func (s *AgentService) GetWiFiStatus(ctx context.Context, _ *agentpb.GetWiFiStatusRequest) (*agentpb.GetWiFiStatusResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	connected, ssid, err := s.networkManager.GetWiFiStatus(ctx)
	if err != nil {
		errMsg := err.Error()
		return &agentpb.GetWiFiStatusResponse{ErrorMessage: &errMsg}, nil
	}
	return &agentpb.GetWiFiStatusResponse{Connected: connected, Ssid: &ssid}, nil
}

func (s *AgentService) DisconnectWiFi(ctx context.Context, _ *agentpb.DisconnectWiFiRequest) (*agentpb.DisconnectWiFiResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.DisconnectWiFi(ctx); err != nil {
		errMsg := err.Error()
		return &agentpb.DisconnectWiFiResponse{Success: false, ErrorMessage: &errMsg}, nil
	}
	return &agentpb.DisconnectWiFiResponse{Success: true}, nil
}

func (s *AgentService) ListHardwareCapabilities(ctx context.Context, req *agentpb.ListHardwareCapabilitiesRequest) (*agentpb.ListHardwareCapabilitiesResponse, error) {
	caps, err := s.hardwareDiscoverer.Discover(ctx, req.GetCategoryFilter())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hardware discovery failed: %v", err)
	}
	return &agentpb.ListHardwareCapabilitiesResponse{Capabilities: caps}, nil
}

func (s *AgentService) ScanBluetoothPeripherals(stream grpc.BidiStreamingServer[agentpb.ScanBluetoothPeripheralsRequest, agentpb.ScanBluetoothPeripheralsResponse]) error {
	ctx := stream.Context()

	ch, err := s.bluetoothManager.Scan(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to start bluetooth scan: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case peripherals, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&agentpb.ScanBluetoothPeripheralsResponse{
				DiscoveredDevices: peripherals,
			}); err != nil {
				return err
			}
		}
	}
}

func (s *AgentService) ConnectBluetoothPeripheral(ctx context.Context, req *agentpb.ConnectBluetoothPeripheralRequest) (*agentpb.ConnectBluetoothPeripheralResponse, error) {
	paired, err := s.bluetoothManager.Connect(ctx, req.GetAddress(), req.GetPair(), req.GetTrust())
	if err != nil {
		return nil, btStatusError("connect bluetooth peripheral", err)
	}
	return &agentpb.ConnectBluetoothPeripheralResponse{Paired: &paired}, nil
}

func (s *AgentService) DisconnectBluetoothPeripheral(ctx context.Context, req *agentpb.DisconnectBluetoothPeripheralRequest) (*agentpb.DisconnectBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Disconnect(ctx, req.GetAddress()); err != nil {
		return nil, btStatusError("disconnect bluetooth peripheral", err)
	}
	return &agentpb.DisconnectBluetoothPeripheralResponse{}, nil
}

func (s *AgentService) ForgetBluetoothPeripheral(ctx context.Context, req *agentpb.ForgetBluetoothPeripheralRequest) (*agentpb.ForgetBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Forget(ctx, req.GetAddress()); err != nil {
		return nil, btStatusError("forget bluetooth peripheral", err)
	}
	return &agentpb.ForgetBluetoothPeripheralResponse{}, nil
}

const osUpdateUnsupportedForHostMessage = "This setup cannot be updated with wendy os update. Use this machine’s normal OS update tools instead. To use WendyOS OTA updates, install WendyOS on supported hardware with wendy os install."

// systemctlFn runs systemctl; overridable in tests.
var systemctlFn = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
}

const (
	updaterTimerUnit   = "wendyos-agent-updater.timer"
	updaterServiceUnit = "wendyos-agent-updater.service"
)

// inhibitAutoUpdater stops the agent auto-updater (timer+service) and returns a
// defer-able restore func that re-enables the timer. The updater's "systemctl
// stop wendyos-agent" would otherwise SIGTERM the in-flight wendyos-update
// install via the shared service cgroup (default KillMode=control-group) and
// abort the OTA. Best-effort: failures are logged, not fatal.
func inhibitAutoUpdater(logger *zap.Logger) func() {
	sysctl := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return systemctlFn(ctx, args...)
	}
	run := func(args ...string) {
		if out, err := sysctl(args...); err != nil {
			logger.Warn("OTA updater-inhibit: systemctl failed",
				zap.Strings("args", args), zap.Error(err),
				zap.String("output", strings.TrimSpace(string(out))))
		}
	}
	// Hosts without the auto-updater (Jetson/QEMU) have nothing to stop — no-op
	// instead of logging a spurious failure on every OTA. `cat` exits non-zero
	// when the unit is absent.
	if _, err := sysctl("cat", updaterTimerUnit); err != nil {
		return func() {}
	}
	run("stop", updaterTimerUnit, updaterServiceUnit)
	// Restore only the timer: it re-triggers the oneshot service itself, and a
	// stopped timer re-arms on the next boot regardless.
	return func() { run("start", updaterTimerUnit) }
}

func defaultIsWendyOSHost() bool {
	// Older WendyOS builds did not write /etc/wendyos/device-type, so keep the
	// version file as a compatibility marker alongside the newer device type.
	if v, ok := wendyOSVersion(); ok {
		return strings.HasPrefix(v, "WendyOS-")
	}
	// Newer WendyOS images report a board/device type used for OTA artifact
	// selection. This file is absent on generic Linux agent installs.
	if _, err := os.Stat("/etc/wendyos/device-type"); err == nil {
		return true
	}
	return false
}

func wendyOSVersion() (string, bool) {
	return readWendyOSVersionFrom("/etc/wendyos/version.txt", "/etc/wendy/version.txt")
}

func readWendyOSVersionFrom(paths ...string) (string, bool) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(string(data)); v != "" {
			return v, true
		}
	}
	return "", false
}

func (s *AgentService) UpdateOS(req *agentpb.UpdateOSRequest, stream grpc.ServerStreamingServer[agentpb.UpdateOSResponse]) error {
	s.logger.Info("UpdateOS started",
		zap.String("artifact_url", req.GetArtifactUrl()), zap.String("updater", req.GetUpdaterBackend()))

	if !s.isWendyOSHost() {
		s.logger.Warn("UpdateOS rejected: host is not a WendyOS OTA target", zap.String("artifact_url", req.GetArtifactUrl()))
		return sendOSUpdateFailure(stream, osUpdateUnsupportedForHostMessage)
	}

	// Stop the auto-updater so it can't SIGTERM the in-flight install mid-OTA;
	// see inhibitAutoUpdater. Restored on return.
	restoreUpdater := inhibitAutoUpdater(s.logger)
	defer restoreUpdater()

	updater, err := selectUpdater(s.logger, req.GetUpdaterBackend(), req.GetArtifactUrl())
	if err != nil {
		s.logger.Warn("UpdateOS rejected: no usable updater backend", zap.Error(err))
		return sendOSUpdateFailure(stream, err.Error())
	}
	s.logger.Info("UpdateOS using backend", zap.String("backend", updater.name()))

	sendProgress := func(phase string, percent int32) {
		_ = stream.Send(&agentpb.UpdateOSResponse{
			ResponseType: &agentpb.UpdateOSResponse_Progress_{
				Progress: &agentpb.UpdateOSResponse_Progress{
					Phase:   phase,
					Percent: percent,
				},
			},
		})
	}

	if err := updater.install(stream.Context(), req.GetArtifactUrl(), sendProgress); err != nil {
		return sendOSUpdateFailure(stream, err.Error())
	}

	recordPendingOSUpdate(s.logger, s.osUpdateStateDir, req.GetArtifactUrl(), updater.name())

	sendProgress("finalizing", 100)

	if err := stream.Send(&agentpb.UpdateOSResponse{
		ResponseType: &agentpb.UpdateOSResponse_Completed_{
			Completed: &agentpb.UpdateOSResponse_Completed{
				RebootRequired: true,
			},
		},
	}); err != nil {
		return err
	}

	// The running OS decides how to reboot: an image whose wendyos-update leaves
	// the staged capsule non-durable needs the flush-and-unmount path, or the
	// update just installed is lost on restart (WDY-2200).
	osVersion, _ := wendyOSVersion()
	if err := rebootAfterOSUpdate(newRebooter(s.logger), osVersion); err != nil {
		s.logger.Error("Failed to reboot after OS update", zap.Error(err))
	}

	return nil
}

// sendOSUpdateFailure sends a terminal Failed response on the v1 UpdateOS stream.
func sendOSUpdateFailure(stream grpc.ServerStreamingServer[agentpb.UpdateOSResponse], msg string) error {
	return stream.Send(&agentpb.UpdateOSResponse{
		ResponseType: &agentpb.UpdateOSResponse_Failed_{
			Failed: &agentpb.UpdateOSResponse_Failed{ErrorMessage: msg},
		},
	})
}

// envWithPath returns os.Environ() with the PATH entry replaced by the given value.
// This ensures PATH is set exactly once (not duplicated), which matters because
// getenv on Linux returns the first match — appending would leave the original in place.
func envWithPath(path string) []string {
	env := os.Environ()
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			env[i] = "PATH=" + path
			return env
		}
	}
	return append(env, "PATH="+path)
}

func CleanupOldBackups(logger *zap.Logger) {
	execPath, err := os.Executable()
	if err != nil {
		logger.Debug("CleanupOldBackups: failed to get executable path", zap.Error(err))
		return
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		logger.Debug("CleanupOldBackups: failed to resolve symlinks", zap.Error(err))
		return
	}
	backupPath := execPath + ".backup"

	info, err := os.Stat(backupPath)
	if err != nil {
		// No backup file exists; nothing to do.
		return
	}

	if time.Since(info.ModTime()) > 48*time.Hour {
		if err := os.Remove(backupPath); err != nil {
			logger.Warn("Failed to remove old backup", zap.String("path", backupPath), zap.Error(err))
			return
		}
		logger.Info("Removed old backup", zap.String("path", backupPath))
	}
}
